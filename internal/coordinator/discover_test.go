package coordinator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const discoverTimeout = 10 * time.Second

// newTestDiscovery builds a poller over the fixture trees. access stands in for
// unix.Access so the permission verdict does not depend on the test's user.
func newTestDiscovery(t *testing.T, sysRoot string, access func(string) error, publish func(context.Context, []Device) error) *Discovery {
	t.Helper()
	d, err := NewDiscovery(DiscoveryOptions{
		SysRoot:  sysRoot,
		DevRoot:  "testdata/dev",
		Interval: 10 * time.Millisecond,
		Debounce: 300 * time.Millisecond,
		Publish:  publish,
	})
	if err != nil {
		t.Fatalf("create the poller: %v", err)
	}
	if access != nil {
		d.access = access
	}
	return d
}

func allowAll(string) error { return nil }

func byTTY(devices []Device, tty string) (Device, bool) {
	for _, d := range devices {
		if d.TTY == tty {
			return d, true
		}
	}
	return Device{}, false
}

// TestPollCrossChecksDev proves the /dev cross-check of R5: a device sysfs lists
// and /dev shows is attached at its by-id link, while one sysfs lists but the
// container cannot see is reported not_passed_through and not attached.
func TestPollCrossChecksDev(t *testing.T) {
	d := newTestDiscovery(t, "testdata/sys", allowAll, func(context.Context, []Device) error { return nil })

	devices, err := d.poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}

	attached, ok := byTTY(devices, "ttyUSB0")
	if !ok {
		t.Fatal("poll did not report ttyUSB0")
	}
	if !attached.Attached || !attached.Accessible {
		t.Errorf("ttyUSB0 attached=%t accessible=%t, want both true", attached.Attached, attached.Accessible)
	}
	if attached.PathKind != PathByID || !strings.Contains(attached.Path, "serial/by-id/") {
		t.Errorf("ttyUSB0 path = %q (%s), want the by-id link", attached.Path, attached.PathKind)
	}
	if attached.Identity.ID != "usb:10c4:ea60:00a1b2c3" {
		t.Errorf("ttyUSB0 id = %q, want usb:10c4:ea60:00a1b2c3", attached.Identity.ID)
	}
	if !Selectable(attached) {
		t.Error("ttyUSB0 is not selectable, want it selectable")
	}

	missing, ok := byTTY(devices, "ttyUSB1")
	if !ok {
		t.Fatal("poll did not report ttyUSB1")
	}
	if missing.Attached || missing.Reason != ReasonNotPassedThrough {
		t.Errorf("ttyUSB1 attached=%t reason=%q, want detached with not_passed_through", missing.Attached, missing.Reason)
	}
	if missing.Identity.Kind != IdentityTopology || missing.Identity.ID != "usb:0658:0200:port:1-2.3" {
		t.Errorf("ttyUSB1 identity = %+v, want the topology identity", missing.Identity)
	}
	if Selectable(missing) {
		t.Error("ttyUSB1 is selectable, want it not selectable while detached")
	}
}

// TestPollPermissionDenied proves a readable-by-nobody node is reported
// permission_denied, distinct from not_passed_through, and stays selectable
// because it is a connection fault rather than a verdict (R14 §6).
func TestPollPermissionDenied(t *testing.T) {
	deny := func(string) error { return errors.New("permission denied") }
	d := newTestDiscovery(t, "testdata/sys", deny, func(context.Context, []Device) error { return nil })

	devices, err := d.poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	dev, _ := byTTY(devices, "ttyUSB0")
	if !dev.Attached || dev.Accessible {
		t.Errorf("ttyUSB0 attached=%t accessible=%t, want attached and inaccessible", dev.Attached, dev.Accessible)
	}
	if dev.Reason != ReasonPermissionDenied {
		t.Errorf("ttyUSB0 reason = %q, want permission_denied", dev.Reason)
	}
	if dev.Support != SupportUnverified {
		t.Errorf("ttyUSB0 support = %q, want unverified, not unsupported", dev.Support)
	}
}

// TestSettleDebounce proves R14 §7: the first poll publishes at once, a changed
// set waits out the debounce, and a set that flaps back inside the window
// publishes nothing at all.
func TestSettleDebounce(t *testing.T) {
	s := settleState{debounce: 300 * time.Millisecond}
	t0 := time.Unix(1000, 0)
	one := []Device{{TTY: "ttyUSB0", Attached: true}}
	two := []Device{{TTY: "ttyUSB0", Attached: true}, {TTY: "ttyUSB1", Attached: true}}

	got, ok := s.observe(t0, one)
	if !ok || len(got) != 1 {
		t.Fatalf("first poll published ok=%t len=%d, want an immediate publish", ok, len(got))
	}
	s.commit(got)

	if _, ok := s.observe(t0.Add(time.Second), one); ok {
		t.Error("an unchanged set was published")
	}
	if _, ok := s.observe(t0.Add(2*time.Second), two); ok {
		t.Error("a changed set published before the debounce elapsed")
	}
	if _, ok := s.observe(t0.Add(2*time.Second+100*time.Millisecond), one); ok {
		t.Error("a set that flapped back inside the window was published")
	}
	if _, ok := s.observe(t0.Add(3*time.Second), two); ok {
		t.Error("a re-changed set published on the poll that first saw it")
	}
	got, ok = s.observe(t0.Add(3*time.Second+300*time.Millisecond), two)
	if !ok || len(got) != 2 {
		t.Fatalf("a settled change published ok=%t len=%d, want 2 devices", ok, len(got))
	}
}

// TestSettleRetriesFailedPublish proves a failed publish does not advance the
// state, so the next poll offers the same set again.
func TestSettleRetriesFailedPublish(t *testing.T) {
	s := settleState{debounce: 0}
	set := []Device{{TTY: "ttyUSB0", Attached: true}}

	if _, ok := s.observe(time.Unix(1, 0), set); !ok {
		t.Fatal("first poll did not offer the set")
	}
	if _, ok := s.observe(time.Unix(2, 0), set); !ok {
		t.Error("the set was not offered again after an uncommitted publish")
	}
}

// TestRunPublishesDebouncedSnapshot runs the poller against the fixture trees
// and proves it reports the first snapshot, then stops on cancellation.
func TestRunPublishesDebouncedSnapshot(t *testing.T) {
	var mu sync.Mutex
	var published [][]Device
	first := make(chan struct{})
	var once sync.Once

	d := newTestDiscovery(t, "testdata/sys", allowAll, func(_ context.Context, devices []Device) error {
		mu.Lock()
		published = append(published, devices)
		mu.Unlock()
		once.Do(func() { close(first) })
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx)
	}()

	select {
	case <-first:
	case <-time.After(discoverTimeout):
		t.Fatal("no snapshot was published")
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(discoverTimeout):
		t.Fatal("the poller did not stop on cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("the unchanged fixture published %d snapshots, want exactly 1", len(published))
	}
	if len(published[0]) != 2 {
		t.Errorf("the snapshot holds %d devices, want the 2 USB devices", len(published[0]))
	}
}

// TestNewDiscoveryRejectsBadOptions proves construction fails fast on a missing
// dependency instead of panicking inside Run.
func TestNewDiscoveryRejectsBadOptions(t *testing.T) {
	ok := func(context.Context, []Device) error { return nil }
	tests := map[string]DiscoveryOptions{
		"no sys root":  {DevRoot: "d", Interval: time.Second, Publish: ok},
		"no dev root":  {SysRoot: "s", Interval: time.Second, Publish: ok},
		"no interval":  {SysRoot: "s", DevRoot: "d", Publish: ok},
		"no publisher": {SysRoot: "s", DevRoot: "d", Interval: time.Second},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDiscovery(opts); err == nil {
				t.Error("NewDiscovery returned no error")
			}
		})
	}
}
