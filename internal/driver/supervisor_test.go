package driver

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/judeibe/zwave_hub/internal/event"
)

// sidecarDir finds the repository's sidecar and skips when npm ci has not run.
func sidecarDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../../sidecar")
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dir, "node_modules", "@zwave-js", "server", "dist-esm", "bin", "server.js")
	if _, err := os.Stat(entry); err != nil {
		t.Skip("sidecar/node_modules is not installed; run npm ci in sidecar/")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
	return dir
}

// nodeShim writes an executable that stands in for the device: it fails while
// the block file exists and otherwise runs the real node. Failures are what a
// stick that cannot be opened looks like to the supervisor, and removing the
// block file is a device-set change the way a re-plug is.
func nodeShim(t *testing.T) (path, block string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	dir := t.TempDir()
	block = filepath.Join(dir, "blocked")
	if err := os.WriteFile(block, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "node")
	script := "#!/bin/sh\n" +
		"if [ -f " + block + " ]; then\n" +
		"  echo 'no device at this path' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"exec " + node + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, block
}

type reports struct {
	mu   sync.Mutex
	list []Report
	ch   chan Report
}

func newReports() *reports { return &reports{ch: make(chan Report, 32)} }

func (r *reports) add(_ context.Context, rep Report) {
	r.mu.Lock()
	r.list = append(r.list, rep)
	r.mu.Unlock()
	r.ch <- rep
}

func (r *reports) waitFor(t *testing.T, connection string, within time.Duration) Report {
	t.Helper()
	timeout := time.After(within)
	for {
		select {
		case rep := <-r.ch:
			if rep.Connection == connection {
				return rep
			}
		case <-timeout:
			t.Fatalf("no %q report within %v", connection, within)
		}
	}
}

// states returns the connection state of every report received so far, in the
// order they arrived.
func (r *reports) states() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.list))
	for i, rep := range r.list {
		out[i] = rep.Connection
	}
	return out
}

// quiet fails the test when a further report arrives within d. It is how a test
// proves the state settled: a paused or ready driver says nothing more.
func (r *reports) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case rep := <-r.ch:
		t.Fatalf("a settled supervisor reported %q", rep.Connection)
	case <-time.After(d):
	}
}

func TestSupervisorMockReachesReadyAndStops(t *testing.T) {
	sidecar := sidecarDir(t)
	rep := newReports()
	sup, err := NewSupervisor(Options{
		SidecarDir: sidecar,
		Mock:       true,
		RunDir:     t.TempDir(),
		CacheDir:   t.TempDir(),
		Report:     rep.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	sup.Start(context.Background(), Target{CoordinatorID: "usb:10c4:ea60:abc"})
	ready := rep.waitFor(t, "ready", 30*time.Second)
	if ready.State == nil || ready.State.HomeID != "0x00000001" {
		t.Fatalf("ready report = %+v", ready)
	}
	if ready.SchemaVersion < MinimumSchema {
		t.Fatalf("schema = %d", ready.SchemaVersion)
	}
	if _, ok := sup.Current(); !ok {
		t.Fatal("no current target while running")
	}
	sup.Stop()
	if _, ok := sup.Current(); ok {
		t.Fatal("target still set after Stop")
	}
	sup.Stop()
}

func TestSupervisorFaultsAfterRepeatedFailures(t *testing.T) {
	rep := newReports()
	sup, err := NewSupervisor(Options{
		SidecarDir: t.TempDir(), // no server.js: node exits at once
		Mock:       true,
		RunDir:     t.TempDir(),
		CacheDir:   t.TempDir(),
		Report:     rep.add,
		Backoff:    func(int) time.Duration { return time.Millisecond },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
	sup.Start(context.Background(), Target{CoordinatorID: "usb:10c4:ea60:abc"})
	defer sup.Stop()
	faulted := rep.waitFor(t, ConnectionFaulted, 60*time.Second)
	if faulted.LastError == nil || faulted.LastError.Code == "" || faulted.NextRetryAt != nil {
		t.Fatalf("faulted report = %+v", faulted)
	}

	// Five attempts, each announced before it ran, is what reaches faulted, and
	// nothing follows it: retrying is paused until the device set changes
	// (R14 §8). The pause is the "no oscillation" half of the rule.
	want := []string{
		ConnectionConnecting, ConnectionRetrying,
		ConnectionConnecting, ConnectionRetrying,
		ConnectionConnecting, ConnectionRetrying,
		ConnectionConnecting, ConnectionRetrying,
		ConnectionConnecting, ConnectionFaulted,
	}
	if got := rep.states(); !slices.Equal(got, want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	rep.quiet(t, 200*time.Millisecond)
}

// TestDefaultBackoffIsExponential pins the retry schedule: it doubles from one
// second and caps at thirty, so a stick that cannot be opened is retried soon
// and then rarely, and never hammers the host.
func TestDefaultBackoffIsExponential(t *testing.T) {
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second,
		30 * time.Second, 30 * time.Second,
	}
	for failures, expected := range want {
		if got := defaultBackoff(failures + 1); got != expected {
			t.Errorf("backoff after %d failures = %v, want %v", failures+1, got, expected)
		}
	}
	if got := defaultBackoff(0); got != time.Second {
		t.Errorf("backoff before any failure = %v, want 1s", got)
	}
}

// TestSupervisorRetryingCarriesNextRetry proves a retrying report always says
// when the next attempt is and that the wait grows, which is what an operator
// reads to tell "about to retry" from "given up".
func TestSupervisorRetryingCarriesNextRetry(t *testing.T) {
	rep := newReports()
	sup, err := NewSupervisor(Options{
		SidecarDir: t.TempDir(),
		Mock:       true,
		RunDir:     t.TempDir(),
		CacheDir:   t.TempDir(),
		Report:     rep.add,
		Backoff:    func(failures int) time.Duration { return time.Duration(failures) * 10 * time.Millisecond },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
	started := time.Now()
	sup.Start(context.Background(), Target{CoordinatorID: "usb:10c4:ea60:abc"})
	defer sup.Stop()
	rep.waitFor(t, ConnectionFaulted, 60*time.Second)

	rep.mu.Lock()
	defer rep.mu.Unlock()
	type retry struct {
		at   time.Time
		wait time.Duration
	}
	var retries []retry
	for _, report := range rep.list {
		if report.Connection != ConnectionRetrying {
			continue
		}
		if report.NextRetryAt == nil {
			t.Fatalf("retrying report without next_retry_at: %+v", report)
		}
		if report.NextRetryAt.Before(started) {
			t.Fatalf("next_retry_at %v is before the run began", report.NextRetryAt)
		}
		if report.LastError == nil {
			t.Fatalf("retrying report without a last error: %+v", report)
		}
		retries = append(retries, retry{at: *report.NextRetryAt, wait: report.NextRetryAt.Sub(report.LastError.At)})
	}
	if len(retries) != MaxConsecutiveFailures-1 {
		t.Fatalf("%d retrying reports, want %d", len(retries), MaxConsecutiveFailures-1)
	}
	for i, r := range retries {
		if r.wait < time.Duration(i+1)*10*time.Millisecond {
			t.Errorf("retry %d waits %v, want at least %v", i+1, r.wait, time.Duration(i+1)*10*time.Millisecond)
		}
		if i > 0 && !r.at.After(retries[i-1].at) {
			t.Errorf("retry %d is scheduled at %v, not after %v", i+1, r.at, retries[i-1].at)
		}
	}
}

// TestSupervisorResumeAfterFault proves the recovery half of R14 §8: retrying
// pauses after five failures, and a settled device-set change restarts the same
// target, which reaches ready once the device answers. The selection is never
// touched here — Resume carries the target it was given.
func TestSupervisorResumeAfterFault(t *testing.T) {
	sidecar := sidecarDir(t)
	nodePath, block := nodeShim(t)
	rep := newReports()
	sup, err := NewSupervisor(Options{
		SidecarDir: sidecar,
		NodePath:   nodePath,
		Mock:       true,
		RunDir:     t.TempDir(),
		CacheDir:   t.TempDir(),
		Report:     rep.add,
		Backoff:    func(int) time.Duration { return time.Millisecond },
	})
	if err != nil {
		t.Fatal(err)
	}
	target := Target{CoordinatorID: "usb:10c4:ea60:abc"}
	sup.Start(context.Background(), target)
	defer sup.Stop()
	rep.waitFor(t, ConnectionFaulted, 60*time.Second)
	if current, ok := sup.Current(); !ok || current != target {
		t.Fatalf("current = %+v, %t; want the paused target", current, ok)
	}

	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	sup.Resume(context.Background())
	ready := rep.waitFor(t, ConnectionReady, 60*time.Second)
	if ready.State == nil || ready.State.HomeID != "0x00000001" {
		t.Fatalf("ready after resume = %+v", ready)
	}
	if current, ok := sup.Current(); !ok || current != target {
		t.Fatalf("current = %+v, %t after resume; want the same target", current, ok)
	}
}

// TestSupervisorResumeWithoutTarget is the guard that keeps a stray device-set
// notification from starting a driver nothing asked for.
func TestSupervisorResumeWithoutTarget(t *testing.T) {
	rep := newReports()
	sup, err := NewSupervisor(Options{
		SidecarDir: t.TempDir(),
		Mock:       true,
		RunDir:     t.TempDir(),
		CacheDir:   t.TempDir(),
		Report:     rep.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	sup.Resume(context.Background())
	if _, ok := sup.Current(); ok {
		t.Fatal("Resume started a target when none was set")
	}
	rep.quiet(t, 100*time.Millisecond)
}

// TestDecideTransitions walks every transition of data-model.md §9.3 without a
// child: each row is one observation and the action it must produce.
func TestDecideTransitions(t *testing.T) {
	const (
		id      = "usb:10c4:ea60:abc"
		byID    = "/dev/serial/by-id/usb-Silicon_Labs_abc-if00-port0"
		ttyPath = "/dev/ttyUSB1"
	)
	cases := []struct {
		name string
		in   Situation
		want Action
	}{
		{
			name: "nothing selected and nothing running",
			in:   Situation{},
			want: ActionLeave,
		},
		{
			name: "a selection appears",
			in:   Situation{Selected: id, Attached: true, Usable: true, Path: byID},
			want: ActionStart,
		},
		{
			name: "the same target is ready",
			in: Situation{
				Selected: id, Attached: true, Usable: true, Path: byID,
				Running: id, RunningPath: byID, Connection: ConnectionReady,
			},
			want: ActionLeave,
		},
		{
			name: "the same identity returns at a new path",
			in: Situation{
				Selected: id, Attached: true, Usable: true, Path: ttyPath,
				Running: id, RunningPath: byID, Connection: ConnectionReady,
			},
			want: ActionStart,
		},
		{
			name: "the selection is unplugged",
			in: Situation{
				Selected: id, Attached: false, Usable: true,
				Running: id, RunningPath: byID, Connection: ConnectionReady,
			},
			want: ActionStop,
		},
		{
			name: "unplugged with nothing running",
			in:   Situation{Selected: id, Connection: ConnectionNotAttached},
			want: ActionLeave,
		},
		{
			name: "faulted with the device set unchanged",
			in: Situation{
				Selected: id, Attached: true, Usable: true, Path: byID,
				Running: id, RunningPath: byID, Connection: ConnectionFaulted,
			},
			want: ActionLeave,
		},
		{
			name: "faulted and the device set changed",
			in: Situation{
				Selected: id, Attached: true, Usable: true, Path: byID,
				Running: id, RunningPath: byID, Connection: ConnectionFaulted,
				DeviceSetChanged: true,
			},
			want: ActionResume,
		},
		{
			name: "the selection is attached but unsupported",
			in: Situation{
				Selected: id, Attached: true, Usable: false,
				Running: id, RunningPath: byID, Connection: ConnectionNotAttached,
			},
			want: ActionStop,
		},
		{
			name: "the selection moved to another device",
			in: Situation{
				Selected: id, Attached: true, Usable: true, Path: byID,
				Running: "usb:10c4:ea60:old", RunningPath: byID, Connection: ConnectionReady,
			},
			want: ActionStart,
		},
	}
	for _, tc := range cases {
		if got := Decide(tc.in); got != tc.want {
			t.Errorf("%s: Decide = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestDecideDoesNotOscillate proves the settling half of R14 §7: a ready
// situation asked twice asks for nothing the second time, so a reconcile tick
// cannot restart a healthy driver and the status has nothing to flip between.
func TestDecideDoesNotOscillate(t *testing.T) {
	in := Situation{
		Selected:    "usb:10c4:ea60:abc",
		Attached:    true,
		Usable:      true,
		Path:        "/dev/serial/by-id/usb-abc",
		Running:     "usb:10c4:ea60:abc",
		RunningPath: "/dev/serial/by-id/usb-abc",
		Connection:  ConnectionReady,
	}
	for i := 0; i < 3; i++ {
		if got := Decide(in); got != ActionLeave {
			t.Fatalf("tick %d asked for %s, want a settled leave", i, got)
		}
	}
}

// TestDetachedReportsNotAttached pins the unplug transition of
// data-model.md §9.3: a device that leaves is reported not_attached whatever it
// was before, and a device that is already not_attached is not reported again,
// so the status settles instead of oscillating.
func TestDetachedReportsNotAttached(t *testing.T) {
	for _, current := range []string{ConnectionReady, ConnectionConnecting, ConnectionRetrying, ConnectionFaulted} {
		state, changed := Detached(current)
		if state != ConnectionNotAttached || !changed {
			t.Errorf("Detached(%q) = (%q, %t), want (not_attached, true)", current, state, changed)
		}
	}
	if state, changed := Detached(ConnectionNotAttached); state != ConnectionNotAttached || changed {
		t.Errorf("Detached(not_attached) = (%q, %t), want (not_attached, false)", state, changed)
	}
}

// TestSupervisorRecordsChildOutputRedacted proves the last stderr line of a
// failed attempt reaches the event stream, while a line carrying key material
// is dropped rather than masked (events.md §3.4, R14 §8).
func TestSupervisorRecordsChildOutputRedacted(t *testing.T) {
	var buf bytes.Buffer
	emitter := event.New(&buf, slog.LevelInfo)

	nodePath, _ := nodeShim(t)
	script := "#!/bin/sh\n" +
		"echo 'cannot open /dev/ttyUSB0: permission denied' >&2\n" +
		"echo 'securityKeys.S0_Legacy = 00112233445566778899aabbccddeeff' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(nodePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	rep := newReports()
	sup, err := NewSupervisor(Options{
		SidecarDir: t.TempDir(), // never reached: node exits at once
		NodePath:   nodePath,
		Mock:       true,
		RunDir:     t.TempDir(),
		CacheDir:   t.TempDir(),
		Emitter:    emitter,
		Report:     rep.add,
		Backoff:    func(int) time.Duration { return time.Millisecond },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
	sup.Start(context.Background(), Target{CoordinatorID: "usb:10c4:ea60:abc"})
	defer sup.Stop()
	rep.waitFor(t, ConnectionFaulted, 60*time.Second)

	logs := buf.String()
	if !strings.Contains(logs, "cannot open /dev/ttyUSB0") {
		t.Fatalf("the safe stderr line did not reach the event stream: %q", logs)
	}
	if strings.Contains(logs, "00112233445566778899aabbccddeeff") {
		t.Fatalf("the secret-looking line leaked into the event stream: %q", logs)
	}
}
