package hub

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/event"
	"github.com/judeibe/zwave_hub/internal/store"
)

// newTestHub starts a hub over a fresh store and stops it when the test ends.
// Cleanup runs last-in-first-out, so registering the store first leaves the
// hub's stop ahead of the store's close.
func newTestHub(t *testing.T) (*Hub, *store.Store) {
	t.Helper()
	return newTestHubWith(t, nil)
}

// newTestHubWith is newTestHub with the emitter the caller supplies, so a test
// can read the event stream the hub writes.
func newTestHubWith(t *testing.T, emitter *event.Emitter) (*Hub, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	h, err := New(st, emitter)
	if err != nil {
		t.Fatalf("create the hub: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Errorf("the hub did not stop within %s", testTimeout)
		}
	})
	return h, st
}

// testDevice builds one attached, unverified discovery device whose identity is
// already resolved, so these tests do not depend on coordinator.Resolve (a stub
// until T021). tty, serial and topology only have to be distinct per device.
func testDevice(tty, serial, topology string) coordinator.Device {
	return coordinator.Device{
		TTY: tty,
		Attributes: coordinator.Attributes{
			VID:      "10c4",
			PID:      "ea60",
			Serial:   serial,
			Topology: topology,
		},
		Identity: coordinator.Identity{
			ID:     "usb:10c4:ea60:" + serial,
			Kind:   coordinator.IdentitySerial,
			Serial: serial,
		},
		Path:       "/dev/serial/by-id/usb-Silicon_Labs_" + serial + "-if00-port0",
		PathKind:   coordinator.PathByID,
		Attached:   true,
		Accessible: true,
		Support:    coordinator.SupportUnverified,
	}
}

// testSelection builds one operator selection. Every call passes a request id
// and an actor address, because the store refuses an audit row without them.
func testSelection(id, requestID string) SelectionRequest {
	return SelectionRequest{
		CoordinatorID: id,
		RequestID:     requestID,
		ActorAddr:     testActorAddr,
	}
}

// mustSync applies one discovery poll and fails the test if the hub refuses it.
func mustSync(t *testing.T, h *Hub, devices ...coordinator.Device) {
	t.Helper()
	if err := h.SyncDiscovery(context.Background(), devices); err != nil {
		t.Fatalf("sync %d discovered devices: %v", len(devices), err)
	}
}

// mustSetConnection records one driver connection report.
func mustSetConnection(t *testing.T, h *Hub, id string, conn Connection, network *Network) {
	t.Helper()
	if err := h.SetConnection(context.Background(), id, conn, network); err != nil {
		t.Fatalf("record a %s connection for %s: %v", conn, id, err)
	}
}

// mustRecordVerdict records the identification verdict the driver returned.
func mustRecordVerdict(t *testing.T, h *Hub, id string, verdict Verdict) {
	t.Helper()
	if err := h.RecordVerdict(context.Background(), id, verdict); err != nil {
		t.Fatalf("record the verdict for %s: %v", id, err)
	}
}

// mustSelect selects id and fails the test unless the hub commits it.
func mustSelect(t *testing.T, h *Hub, id, requestID string) {
	t.Helper()
	snap, err := h.Select(context.Background(), testSelection(id, requestID))
	if err != nil {
		t.Fatalf("select %s: %v", id, err)
	}
	if snap.ActiveID != id {
		t.Fatalf("Select(%s) returned active coordinator %q", id, snap.ActiveID)
	}
}

// findCoordinator returns the snapshot's entry for id, failing the test when the
// hub does not list it.
func findCoordinator(t *testing.T, snap Snapshot, id string) Coordinator {
	t.Helper()
	for _, c := range snap.Coordinators {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("the snapshot does not list coordinator %s", id)
	return Coordinator{}
}

// assertSelectionAudit proves the newest audit row is the one a selection leaves
// behind: its action and outcome, the coordinator it names, and the actor and
// request details every row carries (FR-019, data-model.md §7).
func assertSelectionAudit(t *testing.T, st *store.Store, action store.AuditAction, outcome store.AuditOutcome, coordinatorID, requestID string) store.AuditEntry {
	t.Helper()
	rows := auditRows(t, st)
	if len(rows) == 0 {
		t.Fatalf("the audit trail is empty, want a %s row", action)
	}
	row := rows[0]
	if row.Action != action {
		t.Errorf("the newest audit row is %s, want %s", row.Action, action)
	}
	if row.Outcome != outcome {
		t.Errorf("the newest audit row has outcome %s, want %s", row.Outcome, outcome)
	}
	if row.CoordinatorID != coordinatorID {
		t.Errorf("the newest audit row names coordinator %q, want %q", row.CoordinatorID, coordinatorID)
	}
	if row.RequestID != requestID {
		t.Errorf("the newest audit row carries request %q, want %q", row.RequestID, requestID)
	}
	if row.ActorAddr != testActorAddr {
		t.Errorf("the newest audit row records actor %q, want %q", row.ActorAddr, testActorAddr)
	}
	return row
}

// assertAuditDetail proves a row's detail carries exactly want: the refusal
// cause and, for a network mismatch, both identifiers (data-model.md §7).
func assertAuditDetail(t *testing.T, row store.AuditEntry, want store.AuditDetail) {
	t.Helper()
	if !reflect.DeepEqual(row.Detail, want) {
		t.Errorf("the audit detail is %+v, want %+v", row.Detail, want)
	}
}

// TestSelectRejectsUnsupportedCoordinator proves an unsupported device cannot be
// activated: Select refuses with coordinator_unsupported, no coordinator becomes
// active, and the refusal is audited with its reason (FR-003, FR-019).
func TestSelectRejectsUnsupportedCoordinator(t *testing.T) {
	h, st := newTestHub(t)
	a := testDevice("ttyUSB0", "aaa", "1-2.3")
	mustSync(t, h, a)
	mustRecordVerdict(t, h, a.Identity.ID, Verdict{Supported: false, Reason: coordinator.ReasonNotZWaveController})

	const requestID = "req-select-unsupported"
	_, err := h.Select(context.Background(), testSelection(a.Identity.ID, requestID))

	var selErr *SelectionError
	if !errors.As(err, &selErr) {
		t.Fatalf("Select returned %v, want a *SelectionError", err)
	}
	if selErr.Code != CodeCoordinatorUnsupported {
		t.Errorf("the refusal code is %q, want %q", selErr.Code, CodeCoordinatorUnsupported)
	}
	if got := h.Snapshot().ActiveID; got != "" {
		t.Errorf("the snapshot has active coordinator %q after a refused selection, want none", got)
	}

	row := assertSelectionAudit(t, st, store.AuditCoordinatorRejected, store.OutcomeRejected, a.Identity.ID, requestID)
	assertAuditDetail(t, row, store.AuditDetail{Reason: string(CodeCoordinatorUnsupported)})
}

// TestSelectAcceptsUnverifiedCoordinator proves an unverified device is
// selectable: activation is what identifies it, so the selection commits while
// its support stays unverified (FR-003, R5).
func TestSelectAcceptsUnverifiedCoordinator(t *testing.T) {
	h, st := newTestHub(t)
	a := testDevice("ttyUSB0", "aaa", "1-2.3")
	mustSync(t, h, a)

	const requestID = "req-select-unverified"
	mustSelect(t, h, a.Identity.ID, requestID)

	snap := h.Snapshot()
	if snap.ActiveID != a.Identity.ID {
		t.Errorf("the snapshot has active coordinator %q, want %s", snap.ActiveID, a.Identity.ID)
	}
	active := findCoordinator(t, snap, a.Identity.ID)
	if !active.Selectable {
		t.Errorf("the active coordinator is not selectable, want it selectable")
	}
	if active.Support != coordinator.SupportUnverified {
		t.Errorf("the active coordinator reports support %q, want %q", active.Support, coordinator.SupportUnverified)
	}

	assertSelectionAudit(t, st, store.AuditCoordinatorSelected, store.OutcomeSucceeded, a.Identity.ID, requestID)
}

// TestRecordVerdictDetachesSelection proves the unverified-to-unsupported verdict
// is final and detaches an existing selection: nothing stays active, the reason
// is reported, and the device is no longer selectable (FR-003).
func TestRecordVerdictDetachesSelection(t *testing.T) {
	h, _ := newTestHub(t)
	a := testDevice("ttyUSB0", "aaa", "1-2.3")
	mustSync(t, h, a)
	mustSelect(t, h, a.Identity.ID, "req-select-before-detach")

	mustRecordVerdict(t, h, a.Identity.ID, Verdict{Supported: false, Reason: coordinator.ReasonNotZWaveController})

	snap := h.Snapshot()
	if got := snap.ActiveID; got != "" {
		t.Errorf("the snapshot has active coordinator %q after the device was reported unsupported, want the selection detached", got)
	}
	detached := findCoordinator(t, snap, a.Identity.ID)
	if detached.Support != coordinator.SupportUnsupported {
		t.Errorf("the detached coordinator reports support %q, want %q", detached.Support, coordinator.SupportUnsupported)
	}
	if detached.SupportReason != coordinator.ReasonNotZWaveController {
		t.Errorf("the detached coordinator reports reason %q, want %q", detached.SupportReason, coordinator.ReasonNotZWaveController)
	}
	if detached.Selectable {
		t.Errorf("the detached coordinator is still selectable, want it not selectable")
	}
}

// TestSelectSwitchesWithNoSecureNodes proves the FR-003 gate only applies while
// securely included devices exist: with none, a replacement that has never
// reported a home ID is accepted, and the ready report still records the
// provisioning home ID.
func TestSelectSwitchesWithNoSecureNodes(t *testing.T) {
	h, _ := newTestHub(t)
	a := testDevice("ttyUSB0", "aaa", "1-2.3")
	b := testDevice("ttyUSB1", "bbb", "1-2.4")
	mustSync(t, h, a, b)

	mustSelect(t, h, a.Identity.ID, "req-select-a")
	mustSetConnection(t, h, a.Identity.ID, ConnectionReady, &Network{HomeID: "0xdeadbeef", NodeCount: 2, SecureNodeCount: 0})

	if got := h.Snapshot().HomeID; got != "0xdeadbeef" {
		t.Fatalf("the snapshot reports home id %q after a ready report, want 0xdeadbeef", got)
	}

	mustSelect(t, h, b.Identity.ID, "req-select-b")
	if got := h.Snapshot().ActiveID; got != b.Identity.ID {
		t.Errorf("the snapshot has active coordinator %q after the switch, want %s", got, b.Identity.ID)
	}
}

// TestSelectAcceptsSameHomeID proves the gate admits a replacement that reports
// the stored provisioning home ID while secure nodes exist (FR-003,
// data-model.md §9.2).
func TestSelectAcceptsSameHomeID(t *testing.T) {
	h, _ := newTestHub(t)
	a := testDevice("ttyUSB0", "aaa", "1-2.3")
	b := testDevice("ttyUSB1", "bbb", "1-2.4")
	mustSync(t, h, a, b)

	mustSelect(t, h, a.Identity.ID, "req-select-a")
	mustSetConnection(t, h, a.Identity.ID, ConnectionReady, &Network{HomeID: "0xdeadbeef", NodeCount: 4, SecureNodeCount: 2})
	mustSetConnection(t, h, b.Identity.ID, ConnectionConnecting, &Network{HomeID: "0xdeadbeef"})

	mustSelect(t, h, b.Identity.ID, "req-select-b")
	if got := h.Snapshot().ActiveID; got != b.Identity.ID {
		t.Errorf("the snapshot has active coordinator %q after the switch, want %s", got, b.Identity.ID)
	}
}

// TestSelectRejectsNetworkMismatch proves the gate refuses a replacement that
// reports a different or absent home ID while secure nodes exist: the refusal
// names both identifiers, the selection stays where it was, and the refusal is
// audited with the same detail (FR-003, data-model.md §9.2).
func TestSelectRejectsNetworkMismatch(t *testing.T) {
	tests := []struct {
		name       string
		network    *Network
		wantActual string
	}{
		{name: "different home id", network: &Network{HomeID: "0xcafebabe"}, wantActual: "0xcafebabe"},
		{name: "no network reported", network: nil},
		{name: "empty home id", network: &Network{HomeID: ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, st := newTestHub(t)
			a := testDevice("ttyUSB0", "aaa", "1-2.3")
			b := testDevice("ttyUSB1", "bbb", "1-2.4")
			mustSync(t, h, a, b)

			mustSelect(t, h, a.Identity.ID, "req-select-a")
			mustSetConnection(t, h, a.Identity.ID, ConnectionReady, &Network{HomeID: "0xdeadbeef", NodeCount: 4, SecureNodeCount: 2})
			mustSetConnection(t, h, b.Identity.ID, ConnectionConnecting, tt.network)

			const requestID = "req-select-mismatch"
			_, err := h.Select(context.Background(), testSelection(b.Identity.ID, requestID))

			var selErr *SelectionError
			if !errors.As(err, &selErr) {
				t.Fatalf("Select returned %v, want a *SelectionError", err)
			}
			if selErr.Code != CodeCoordinatorNetworkMismatch {
				t.Errorf("the refusal code is %q, want %q", selErr.Code, CodeCoordinatorNetworkMismatch)
			}
			if selErr.ExpectedHomeID != "0xdeadbeef" {
				t.Errorf("the refusal expects home id %q, want 0xdeadbeef", selErr.ExpectedHomeID)
			}
			if selErr.ActualHomeID != tt.wantActual {
				t.Errorf("the refusal reports actual home id %q, want %q", selErr.ActualHomeID, tt.wantActual)
			}
			if got := h.Snapshot().ActiveID; got != a.Identity.ID {
				t.Errorf("the snapshot has active coordinator %q after a refused switch, want %s", got, a.Identity.ID)
			}

			row := assertSelectionAudit(t, st, store.AuditCoordinatorRejected, store.OutcomeRejected, b.Identity.ID, requestID)
			assertAuditDetail(t, row, store.AuditDetail{
				Reason:         string(CodeCoordinatorNetworkMismatch),
				ExpectedHomeID: "0xdeadbeef",
				ActualHomeID:   tt.wantActual,
			})
		})
	}
}

// TestBootRestore proves FR-015/FR-016: the hub re-establishes the committed
// selection and network identifier from the store on every start, without a
// device being attached, and without any operator input. The driver is not
// started until discovery reports the device; until then the API reports
// awaiting_coordinator.
func TestBootRestore(t *testing.T) {
	dir := t.TempDir()

	st1, err := store.Open(context.Background(), store.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	h1, err := New(st1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { defer close(done1); h1.Run(ctx1) }()

	dev := testDevice("ttyUSB0", "abc", "")
	if err := h1.SyncDiscovery(context.Background(), []coordinator.Device{dev}); err != nil {
		t.Fatal(err)
	}
	if _, err := h1.Select(context.Background(), testSelection(dev.Identity.ID, "r1")); err != nil {
		t.Fatal(err)
	}
	if err := h1.Adopt(context.Background(), dev.Identity.ID, Identified{Network: Network{HomeID: "0x00000001", NodeCount: 1}}); err != nil {
		t.Fatal(err)
	}

	cancel1()
	select {
	case <-done1:
	case <-time.After(testTimeout):
		t.Fatal("the hub did not stop")
	}
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: reopen the store and the hub, with no device attached and no API
	// calls. The committed selection and the network identifier must already be
	// there, read back from the database.
	st2, err := store.Open(context.Background(), store.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	h2, err := New(st2, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { defer close(done2); h2.Run(ctx2) }()
	defer func() {
		cancel2()
		select {
		case <-done2:
		case <-time.After(testTimeout):
			t.Fatal("the hub did not stop")
		}
	}()

	snap := h2.Snapshot()
	if snap.ActiveID != dev.Identity.ID {
		t.Fatalf("active id after restart = %q, want %q", snap.ActiveID, dev.Identity.ID)
	}
	if snap.HomeID != "0x00000001" {
		t.Fatalf("home id after restart = %q, want 0x00000001", snap.HomeID)
	}
	active, ok := snap.Coordinator(snap.ActiveID)
	if !ok {
		t.Fatal("the selected coordinator is missing after restart")
	}
	if active.Connection != ConnectionNotAttached {
		t.Fatalf("connection after restart = %q, want not_attached (no driver started yet)", active.Connection)
	}
	if got, ok := snap.Restore(); ok {
		t.Fatalf("Restore returned %q before the device is attached; want empty (awaiting_coordinator)", got.ID)
	}

	// The same identity returns at a different path: it is adopted without a
	// second selection, and the selection stays on the same identity.
	replugged := testDevice("ttyUSB0", "abc", "")
	replugged.Path = "/dev/ttyUSB1"
	replugged.PathKind = coordinator.PathTTY
	if err := h2.SyncDiscovery(context.Background(), []coordinator.Device{replugged}); err != nil {
		t.Fatal(err)
	}
	got, ok := h2.Snapshot().Restore()
	if !ok || got.ID != dev.Identity.ID || got.Path != "/dev/ttyUSB1" {
		t.Fatalf("Restore after re-plug = %+v, %t; want %q at /dev/ttyUSB1", got, ok, dev.Identity.ID)
	}
	if h2.Snapshot().ActiveID != dev.Identity.ID {
		t.Fatalf("the selection changed on re-plug")
	}
}

// TestAdoptionOnReplug proves FR-004/FR-016: a re-plugged coordinator with the
// same identity is adopted at the new path without changing the selection, and an
// unknown device that appears stays available without becoming the selection.
func TestAdoptionOnReplug(t *testing.T) {
	h, _ := newTestHub(t)

	dev := testDevice("ttyUSB0", "abc", "")
	if err := h.SyncDiscovery(context.Background(), []coordinator.Device{dev}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Select(context.Background(), testSelection(dev.Identity.ID, "r1")); err != nil {
		t.Fatal(err)
	}
	if err := h.Adopt(context.Background(), dev.Identity.ID, Identified{Network: Network{HomeID: "0x00000001", NodeCount: 1}}); err != nil {
		t.Fatal(err)
	}

	// An unknown device appears alongside the selected one: the selection stays,
	// and the new device is listed as available.
	stranger := testDevice("ttyUSB1", "zzz", "")
	if err := h.SyncDiscovery(context.Background(), []coordinator.Device{dev, stranger}); err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().ActiveID != dev.Identity.ID {
		t.Fatalf("an unknown device changed the selection to %q", h.Snapshot().ActiveID)
	}
	strangerView, ok := h.Snapshot().Coordinator(stranger.Identity.ID)
	if !ok || !strangerView.Attached || !strangerView.Selectable {
		t.Fatalf("the unknown device is not available: %+v (ok=%t)", strangerView, ok)
	}

	// The same identity returns at a new path: the selection is untouched and
	// the path is refreshed, so the driver can be started against the new node.
	replugged := dev
	replugged.Path = "/dev/ttyUSB1"
	replugged.PathKind = coordinator.PathTTY
	if err := h.SyncDiscovery(context.Background(), []coordinator.Device{replugged, stranger}); err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().ActiveID != dev.Identity.ID {
		t.Fatalf("the selection changed on re-plug")
	}
	view, _ := h.Snapshot().Coordinator(dev.Identity.ID)
	if view.Path != "/dev/ttyUSB1" {
		t.Fatalf("path not refreshed on re-plug: %q", view.Path)
	}
	if view.Attached == false {
		t.Fatal("the re-plugged coordinator is not attached")
	}
}
