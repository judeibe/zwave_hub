package driver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
	faulted := rep.waitFor(t, "faulted", 60*time.Second)
	if faulted.LastError == nil || faulted.LastError.Code == "" || faulted.NextRetryAt != nil {
		t.Fatalf("faulted report = %+v", faulted)
	}
}
