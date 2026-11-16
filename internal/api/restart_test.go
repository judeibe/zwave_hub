package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/hub"
	"github.com/judeibe/zwave_hub/internal/store"
)

// restartFixture is one process lifetime of the hub over a fixed data
// directory. It embeds handlerFixture so requests go through the same helpers as
// the other API tests; the shutdown it adds is what lets a test close one
// lifetime and open the next over the same directory, which is the full restart
// of SC-003 and SC-006.
type restartFixture struct {
	handlerFixture
	cancel context.CancelFunc
	done   chan struct{}
	stop   sync.Once
}

// openRestartFixture opens dataDir and starts a hub over it. The master key is
// the same bytes in every lifetime, because keys one run sealed have to open in
// the next, and supplying it keeps Open from creating a master.key file.
func openRestartFixture(t *testing.T, dataDir string) *restartFixture {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DataDir:   dataDir,
		MasterKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.New(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()

	rt := NewRouter(Options{})
	NewCoordinators(h).Mount(rt)
	NewNetwork(h).Mount(rt)
	NewKeys(h).Mount(rt)
	NewAudit(st).Mount(rt)

	f := &restartFixture{
		handlerFixture: handlerFixture{hub: h, store: st, router: rt},
		cancel:         cancel,
		done:           done,
	}
	t.Cleanup(func() { f.close(t) })
	return f
}

// close stops the hub and releases the database. It is idempotent because a test
// closes the earlier lifetime explicitly and cleanup closes it again.
func (f *restartFixture) close(t *testing.T) {
	t.Helper()
	f.stop.Do(func() {
		f.cancel()
		<-f.done
		if err := f.store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
}

// wantField is one member a response body must carry.
type wantField struct {
	name  string
	value any
}

// assertBody fails unless rec is 200 and its decoded body holds every want, and
// returns the body for the checks that need a nested member.
func assertBody(t *testing.T, what string, rec *httptest.ResponseRecorder, wants ...wantField) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", what, rec.Code, rec.Body)
	}
	body := decode(t, rec)
	for _, want := range wants {
		if got := body[want.name]; got != want.value {
			t.Errorf("%s: %s = %v, want %v", what, want.name, got, want.value)
		}
	}
	return body
}

// TestRestartRestoresSelectionAndReady drives SC-003 and SC-006 through the HTTP
// surface: a selection committed before a full restart comes back with the
// network it adopted, and re-attaching the same device at a different path
// returns the coordinator to ready without the operator selecting it again.
func TestRestartRestoresSelectionAndReady(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	dev := device("aaa")
	const homeID = "0x00000001"

	// First lifetime: the only mutating request of the whole test is the one
	// accepted configuration change, the operator's selection.
	first := openRestartFixture(t, dataDir)
	if err := first.hub.SyncDiscovery(ctx, []coordinator.Device{dev}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if rec := first.do("PUT", "/api/v1/active-coordinator", `{"coordinator_id":"`+dev.Identity.ID+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("select status %d: %s", rec.Code, rec.Body)
	}
	if err := first.hub.Adopt(ctx, dev.Identity.ID, hub.Identified{
		Network: hub.Network{HomeID: homeID},
	}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	assertBody(t, "before restart", first.do("GET", "/api/v1/network", ""),
		wantField{"state", "degraded"},
		wantField{"ready_for_secure_inclusion", false},
		wantField{"active_coordinator_id", dev.Identity.ID},
		wantField{"home_id", homeID},
	)
	first.close(t)

	// Second lifetime: a new store handle and a new hub over the same directory.
	// No request has been served yet, so what the snapshot holds here was read
	// back from the database by hub.New, not rebuilt by a client.
	second := openRestartFixture(t, dataDir)
	if snap := second.hub.Snapshot(); snap.ActiveID != dev.Identity.ID || snap.HomeID != homeID {
		t.Fatalf("restored snapshot active %q home %q, want %q and %q",
			snap.ActiveID, snap.HomeID, dev.Identity.ID, homeID)
	}

	// The selected device is not attached yet, so nothing may have been started
	// for it (FR-016) and the network waits for it.
	active := assertBody(t, "after restart", second.do("GET", "/api/v1/active-coordinator", ""),
		wantField{"id", dev.Identity.ID},
		wantField{"connection", "not_attached"},
		wantField{"support", "supported"},
		wantField{"attached", false},
	)
	if active["network"] != nil {
		t.Errorf("after restart: network %v shown for a coordinator that is not ready", active["network"])
	}
	assertBody(t, "after restart", second.do("GET", "/api/v1/network", ""),
		wantField{"state", "awaiting_coordinator"},
		wantField{"active_coordinator_id", dev.Identity.ID},
		wantField{"home_id", homeID},
	)

	// The same stick reappears at a different node, and a stranger appears
	// beside it. Discovery records both and changes neither's selection: the
	// identity, not the path, is what the selection is bound to.
	replugged := dev
	replugged.TTY = "ttyUSB1"
	replugged.Path = "/dev/ttyUSB1"
	replugged.PathKind = coordinator.PathTTY
	stranger := device("zzz")
	if err := second.hub.SyncDiscovery(ctx, []coordinator.Device{replugged, stranger}); err != nil {
		t.Fatalf("re-plug sync: %v", err)
	}
	list := decode(t, second.do("GET", "/api/v1/coordinators", ""))
	if list["active_coordinator_id"] != dev.Identity.ID {
		t.Errorf("a new device changed the selection to %v", list["active_coordinator_id"])
	}
	if items, _ := list["items"].([]any); len(items) != 2 {
		t.Errorf("coordinators = %v, want the selected device and the stranger", list["items"])
	}

	// The driver identifies the re-plugged stick and reports it ready. Nothing
	// here selects it again: the identity is the one the store restored.
	if err := second.hub.Adopt(ctx, dev.Identity.ID, hub.Identified{
		Network: hub.Network{HomeID: homeID},
	}); err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	active = assertBody(t, "after re-plug", second.do("GET", "/api/v1/active-coordinator", ""),
		wantField{"id", dev.Identity.ID},
		wantField{"connection", "ready"},
		wantField{"attached", true},
		wantField{"path", "/dev/ttyUSB1"},
		wantField{"path_kind", "tty"},
	)
	if network, _ := active["network"].(map[string]any); network["home_id"] != homeID {
		t.Errorf("after re-plug: network %v, want home id %s", active["network"], homeID)
	}
	assertBody(t, "after re-plug", second.do("GET", "/api/v1/network", ""),
		wantField{"state", "degraded"},
		wantField{"ready_for_secure_inclusion", false},
		wantField{"active_coordinator_id", dev.Identity.ID},
		wantField{"home_id", homeID},
	)

	// Exactly one selection was ever committed: the durable trail across both
	// lifetimes holds the one operator request and no second one disguised as a
	// restore.
	page := decode(t, second.do("GET", "/api/v1/audit?action=coordinator.selected", ""))
	if items, _ := page["items"].([]any); len(items) != 1 {
		t.Errorf("coordinator.selected audit rows = %v, want exactly the one committed before the restart", page["items"])
	}

	// The keys.in_effect clause of T058 has nowhere to land yet: this repository
	// has no HTTP key API (internal/api/keys.go and the keys block of the network
	// body), and no driver apply path that renders the stored keys into the
	// child's config and sets provisioning.applied_revision. With nothing able
	// to change a revision or report one in effect, the assertion could only wait
	// for a field that never turns true. Add it here, against this network
	// body's keys.in_effect and bounded by the 20 s p95 of plan.md §Performance
	// Goals, once those three pieces land.
}

// TestRemovalAndReturnKeepsReady is the US3 independent test of the playbook's
// completion section: configure once, restart the hub with the device still
// attached, and the coordinator returns to ready without a second API call;
// then the same node leaves and returns and the states repeat, with the
// selection untouched throughout.
func TestRemovalAndReturnKeepsReady(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	dev := device("aaa")
	const homeID = "0x00000001"

	adopt := func(f *restartFixture, what string) {
		t.Helper()
		if err := f.hub.Adopt(ctx, dev.Identity.ID, hub.Identified{Network: hub.Network{HomeID: homeID}}); err != nil {
			t.Fatalf("%s adopt: %v", what, err)
		}
	}
	// ready asserts the one view the whole test keeps returning to.
	ready := func(f *restartFixture, what string) {
		t.Helper()
		active := assertBody(t, what, f.do("GET", "/api/v1/active-coordinator", ""),
			wantField{"id", dev.Identity.ID},
			wantField{"connection", "ready"},
			wantField{"attached", true},
			wantField{"support", "supported"},
		)
		if network, _ := active["network"].(map[string]any); network["home_id"] != homeID {
			t.Errorf("%s: network %v, want home id %s", what, active["network"], homeID)
		}
		// The coordinator is ready but no keys are configured, so the derived
		// state is degraded and secure inclusion is not ready yet.
		assertBody(t, what, f.do("GET", "/api/v1/network", ""),
			wantField{"state", "degraded"},
			wantField{"ready_for_secure_inclusion", false},
			wantField{"active_coordinator_id", dev.Identity.ID},
			wantField{"home_id", homeID},
		)
	}

	// First lifetime: the operator's selection, the only configuration change
	// of the test, and the driver's first ready report.
	first := openRestartFixture(t, dataDir)
	if err := first.hub.SyncDiscovery(ctx, []coordinator.Device{dev}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if rec := first.do("PUT", "/api/v1/active-coordinator", `{"coordinator_id":"`+dev.Identity.ID+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("select status %d: %s", rec.Code, rec.Body)
	}
	adopt(first, "before restart")
	ready(first, "before restart")
	first.close(t)

	// Second lifetime: the device is still plugged in at the same path, so
	// discovery finds it and the stored selection names it. Restore is what the
	// bridge asks before starting a child (FR-015, FR-016).
	second := openRestartFixture(t, dataDir)
	if err := second.hub.SyncDiscovery(ctx, []coordinator.Device{dev}); err != nil {
		t.Fatalf("restart sync: %v", err)
	}
	if got, ok := second.hub.Snapshot().Restore(); !ok || got.ID != dev.Identity.ID {
		t.Fatalf("Restore after restart = %+v, %t; want %q", got, ok, dev.Identity.ID)
	}
	adopt(second, "after restart")
	ready(second, "after restart")

	// The node leaves. The selection stays, nothing is started, and the network
	// waits: not_attached is what the bridge reports for a departed device
	// (data-model.md §9.3). The adapter call stands in for that report, exactly
	// as Adopt stands in for the driver's ready report.
	if err := second.hub.SyncDiscovery(ctx, nil); err != nil {
		t.Fatalf("removal sync: %v", err)
	}
	if got, ok := second.hub.Snapshot().Restore(); ok {
		t.Fatalf("Restore returned %q after removal; want none", got.ID)
	}
	if err := second.hub.SetConnection(ctx, dev.Identity.ID, hub.ConnectionNotAttached, nil); err != nil {
		t.Fatalf("report not_attached: %v", err)
	}
	assertBody(t, "after removal", second.do("GET", "/api/v1/active-coordinator", ""),
		wantField{"id", dev.Identity.ID},
		wantField{"connection", "not_attached"},
		wantField{"attached", false},
	)
	assertBody(t, "after removal", second.do("GET", "/api/v1/network", ""),
		wantField{"state", "awaiting_coordinator"},
		wantField{"active_coordinator_id", dev.Identity.ID},
		wantField{"home_id", homeID},
	)

	// The same node returns. It is the stored identity, so it is adopted without
	// the operator selecting it again, and it is ready as before.
	if err := second.hub.SyncDiscovery(ctx, []coordinator.Device{dev}); err != nil {
		t.Fatalf("return sync: %v", err)
	}
	if got, ok := second.hub.Snapshot().Restore(); !ok || got.ID != dev.Identity.ID {
		t.Fatalf("Restore after return = %+v, %t; want %q", got, ok, dev.Identity.ID)
	}
	adopt(second, "after return")
	ready(second, "after return")

	// One selection across both lifetimes and the removal: restore, adoption and
	// re-attachment never wrote one.
	page := decode(t, second.do("GET", "/api/v1/audit?action=coordinator.selected", ""))
	if items, _ := page["items"].([]any); len(items) != 1 {
		t.Errorf("coordinator.selected audit rows = %v, want exactly the one committed before the restart", page["items"])
	}
}
