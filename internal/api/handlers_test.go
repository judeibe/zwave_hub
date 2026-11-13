package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/hub"
	"github.com/judeibe/zwave_hub/internal/store"
)

type handlerFixture struct {
	hub    *hub.Hub
	store  *store.Store
	router *Router
}

func newHandlerFixture(t *testing.T) *handlerFixture {
	t.Helper()
	// Key provisioning needs a master key; a real hub gets it from
	// ZWAVE_HUB_MASTER_KEY or <DataDir>/master.key (R4), and tests supply a
	// fixed 32-byte one rather than creating a file per fixture.
	st, err := store.Open(context.Background(), store.Options{
		DataDir:   t.TempDir(),
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
	t.Cleanup(func() {
		cancel()
		<-done
		_ = st.Close()
	})
	rt := NewRouter(Options{})
	NewCoordinators(h).Mount(rt)
	NewNetwork(h).Mount(rt)
	NewKeys(h).Mount(rt)
	NewConfirmations(h, true).Mount(rt)
	NewAudit(st).Mount(rt)
	return &handlerFixture{hub: h, store: st, router: rt}
}

func (f *handlerFixture) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:5555"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func device(serial string) coordinator.Device {
	return coordinator.Device{
		TTY:        "ttyUSB0",
		Attributes: coordinator.Attributes{VID: "10c4", PID: "ea60", Serial: serial},
		Identity:   coordinator.Identity{ID: "usb:10c4:ea60:" + serial, Kind: coordinator.IdentitySerial, Serial: serial},
		Path:       "/dev/serial/by-id/usb-" + serial,
		PathKind:   coordinator.PathByID,
		Attached:   true,
		Accessible: true,
		Support:    coordinator.SupportUnverified,
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestEmptyHubListsNothingAndReportsUnprovisioned(t *testing.T) {
	f := newHandlerFixture(t)
	list := f.do("GET", "/api/v1/coordinators", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status %d", list.Code)
	}
	body := decode(t, list)
	if items, ok := body["items"].([]any); !ok || len(items) != 0 {
		t.Fatalf("items = %v", body["items"])
	}
	if body["active_coordinator_id"] != nil {
		t.Fatalf("active = %v", body["active_coordinator_id"])
	}
	if got := f.do("GET", "/api/v1/active-coordinator", ""); got.Code != http.StatusNotFound {
		t.Fatalf("active status %d", got.Code)
	}
	network := decode(t, f.do("GET", "/api/v1/network", ""))
	if network["state"] != "unprovisioned" || network["ready_for_secure_inclusion"] != false {
		t.Fatalf("network = %v", network)
	}
}

func TestSelectReadyAndNetworkStatus(t *testing.T) {
	f := newHandlerFixture(t)
	dev := device("aaa")
	if err := f.hub.SyncDiscovery(context.Background(), []coordinator.Device{dev}); err != nil {
		t.Fatal(err)
	}
	put := f.do("PUT", "/api/v1/active-coordinator", `{"coordinator_id":"`+dev.Identity.ID+`"}`)
	if put.Code != http.StatusOK {
		t.Fatalf("select status %d: %s", put.Code, put.Body)
	}
	if err := f.hub.Adopt(context.Background(), dev.Identity.ID, hub.Identified{
		Network: hub.Network{HomeID: "0x00000001"},
	}); err != nil {
		t.Fatal(err)
	}
	active := decode(t, f.do("GET", "/api/v1/active-coordinator", ""))
	if active["connection"] != "ready" || active["support"] != "supported" {
		t.Fatalf("active = %v", active)
	}
	if net, _ := active["network"].(map[string]any); net["home_id"] != "0x00000001" {
		t.Fatalf("network = %v", active["network"])
	}
	status := decode(t, f.do("GET", "/api/v1/network", ""))
	// A ready coordinator with no keys is degraded, not ready: readiness needs
	// the classic set configured and in effect (data-model.md §8.2, §8.3).
	if status["state"] != "degraded" || status["home_id"] != "0x00000001" {
		t.Fatalf("status = %v", status)
	}
}

// blockingCodes returns the blocking reason codes of one network body.
func blockingCodes(status map[string]any) []string {
	reasons, _ := status["blocking_reasons"].([]any)
	codes := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if body, ok := reason.(map[string]any); ok {
			code, _ := body["code"].(string)
			codes = append(codes, code)
		}
	}
	return codes
}

// TestNetworkReadyOnceKeysAreInEffect proves the readiness half of T051 end to
// end: the first ready report of a zero-node coordinator generates the classic
// set (FR-008), the network is degraded with keys_not_in_effect until the driver
// reports the revision running, and from then on it reports ready for secure
// inclusion with no blocking reason and keys.in_effect true.
func TestNetworkReadyOnceKeysAreInEffect(t *testing.T) {
	f := newHandlerFixture(t)
	ctx := context.Background()
	dev := device("aaa")
	if err := f.hub.SyncDiscovery(ctx, []coordinator.Device{dev}); err != nil {
		t.Fatal(err)
	}
	if rec := f.do("PUT", "/api/v1/active-coordinator", `{"coordinator_id":"`+dev.Identity.ID+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("select status %d: %s", rec.Code, rec.Body)
	}
	if err := f.hub.Adopt(ctx, dev.Identity.ID, hub.Identified{Network: hub.Network{HomeID: "0x00000001"}}); err != nil {
		t.Fatal(err)
	}

	revision := f.hub.CommittedKeyRevision()
	if revision != 1 {
		t.Fatalf("first activation committed revision %d, want the automatically generated classic set", revision)
	}
	degraded := decode(t, f.do("GET", "/api/v1/network", ""))
	if degraded["state"] != "degraded" || degraded["ready_for_secure_inclusion"] != false {
		t.Fatalf("stored but not applied: state %v ready %v", degraded["state"], degraded["ready_for_secure_inclusion"])
	}
	if codes := blockingCodes(degraded); len(codes) != 1 || codes[0] != "keys_not_in_effect" {
		t.Fatalf("stored but not applied: blocking reasons %v, want keys_not_in_effect", codes)
	}

	// The driver's ready report carrying the revision it started with is what
	// makes the set in effect; the fixture has no supervisor, so the report is
	// made directly, as the bridge would make it.
	if err := f.hub.NoteKeysApplied(ctx, dev.Identity.ID, revision, "0x00000001", time.Now()); err != nil {
		t.Fatal(err)
	}

	status := decode(t, f.do("GET", "/api/v1/network", ""))
	if status["state"] != "ready" || status["ready_for_secure_inclusion"] != true {
		t.Fatalf("applied: state %v ready %v", status["state"], status["ready_for_secure_inclusion"])
	}
	if codes := blockingCodes(status); len(codes) != 0 {
		t.Fatalf("applied: blocking reasons %v, want none", codes)
	}
	keys, _ := status["keys"].(map[string]any)
	if keys["in_effect"] != true || keys["classic_complete"] != true || keys["applied_revision"] != keys["revision"] {
		t.Fatalf("applied: keys %v", keys)
	}
	if configured, _ := keys["configured_classes"].([]any); len(configured) != 4 {
		t.Fatalf("applied: configured_classes %v, want the four classic classes", keys["configured_classes"])
	}
}

func TestSelectProblems(t *testing.T) {
	f := newHandlerFixture(t)
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"unknown id", `{"coordinator_id":"usb:ffff:ffff:none"}`, http.StatusNotFound, "coordinator_not_found"},
		{"missing id", `{}`, http.StatusBadRequest, "invalid_request"},
		{"unknown member", `{"coordinator_id":"x","extra":1}`, http.StatusBadRequest, "invalid_request"},
		{"not json", `nope`, http.StatusBadRequest, "invalid_request"},
	}
	for _, tc := range cases {
		rec := f.do("PUT", "/api/v1/active-coordinator", tc.body)
		if rec.Code != tc.status || decode(t, rec)["code"] != tc.code {
			t.Errorf("%s: status %d body %s", tc.name, rec.Code, rec.Body)
		}
	}
	if rec := f.do("GET", "/api/v1/coordinators/usb:ffff:ffff:none", ""); rec.Code != http.StatusNotFound {
		t.Errorf("get unknown: %d", rec.Code)
	}
}

func TestAuditPagination(t *testing.T) {
	f := newHandlerFixture(t)
	dev := device("aaa")
	if err := f.hub.SyncDiscovery(context.Background(), []coordinator.Device{dev}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f.do("PUT", "/api/v1/active-coordinator", `{"coordinator_id":"`+dev.Identity.ID+`"}`)
	}
	first := decode(t, f.do("GET", "/api/v1/audit?limit=2", ""))
	items := first["items"].([]any)
	if len(items) != 2 || first["next_before"] == nil {
		t.Fatalf("first page = %v", first)
	}
	entry := items[0].(map[string]any)
	if entry["action"] != "coordinator.selected" || entry["actor_addr"] == "" {
		t.Fatalf("entry = %v", entry)
	}
	cursor := int(first["next_before"].(float64))
	second := decode(t, f.do("GET", "/api/v1/audit?limit=2&before="+itoa(cursor), ""))
	if len(second["items"].([]any)) != 1 || second["next_before"] != nil {
		t.Fatalf("second page = %v", second)
	}
	if rec := f.do("GET", "/api/v1/audit?limit=0", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 status %d", rec.Code)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestPresentCoordinatorHidesNetworkUnlessReady(t *testing.T) {
	c := hub.Coordinator{ID: "usb:10c4:ea60:a", Connection: hub.ConnectionRetrying,
		Network: &hub.Network{HomeID: "0x00000001"}, LastSeenAt: time.Now()}
	if presentCoordinator(c).Network != nil {
		t.Fatal("network shown while retrying")
	}
	c.Connection = hub.ConnectionReady
	if presentCoordinator(c).Network == nil {
		t.Fatal("network hidden while ready")
	}
}

// TestNewBodiesPassPresent runs the coordinator, network and audit bodies
// through Present with every optional member populated, so a member that
// Present refuses (a never-send name, a []byte, a 32-hex run) fails here and
// not at the first request that fills it (SC-004).
func TestNewBodiesPassPresent(t *testing.T) {
	manufacturer := 134
	retry := time.Now()
	coordinatorFull := hub.Coordinator{
		ID: "usb:10c4:ea60:aaa", VID: "10c4", PID: "ea60", Serial: "aaa",
		Manufacturer: "Silicon Labs", Product: "CP2102N", Path: "/dev/serial/by-id/x",
		PathKind: coordinator.PathByID, Attached: true, Selectable: true,
		Connection: hub.ConnectionReady, Support: coordinator.SupportSupported,
		Controller:  &store.ControllerIdentity{FirmwareVersion: "7.21", SDKVersion: "7.21.1", ManufacturerID: &manufacturer, ChipType: "7.0"},
		NextRetryAt: &retry, LastError: &hub.LastError{Code: "driver_start_failed", Message: "x", At: retry},
		Network: &hub.Network{HomeID: "0xdeadbeef", NodeCount: 3, SecureNodeCount: 2},
	}
	snap := hub.Snapshot{Coordinators: []hub.Coordinator{coordinatorFull}, ActiveID: coordinatorFull.ID, HomeID: "0xdeadbeef"}
	applied := 2
	networkStatus := hub.NetworkStatus{
		State: hub.NetworkFaulted,
		Readiness: hub.Readiness{BlockingReasons: []hub.BlockingReason{
			{Code: hub.BlockingKeyStoreCorrupt, Message: "x", Classes: []string{"S0_Legacy"}},
			{Code: hub.BlockingNoActiveCoordinator, Message: "x"},
			{Code: hub.BlockingCoordinatorNotAttached, Message: "x", LastPath: "/dev/serial/by-id/x"},
			{Code: hub.BlockingCoordinatorFaulted, Message: "x", Reason: "driver_start_failed"},
			{Code: hub.BlockingNetworkMismatch, Message: "x", ExpectedHomeID: "0x00000001", ActualHomeID: "0x00000002"},
			{Code: hub.BlockingExistingNetworkDetected, Message: "x", HomeID: "0xdeadbeef", NodeCount: 3},
			{Code: hub.BlockingKeysMissing, Message: "x", Classes: []string{"S2_AccessControl"}},
			{Code: hub.BlockingKeysNotInEffect, Message: "x", KeyRevision: 2, AppliedRevision: &applied},
		}},
		Keys: hub.KeyConfiguration{
			Revision: 2, AppliedRevision: &applied, InEffect: true, ClassicComplete: true,
			Classes: []hub.KeyClass{{Class: "S0_Legacy", Configured: true, Source: store.KeySourceSupplied}},
		},
		KeyStore:        hub.KeyStoreCorrupt,
		CorruptClasses:  []string{"S0_Legacy"},
		ActiveID:        coordinatorFull.ID,
		HomeID:          "0xdeadbeef",
		NodeCount:       3,
		SecureNodeCount: 2,
		Connection:      hub.ConnectionReady,
		LastError:       &hub.LastError{Code: "driver_start_failed", Message: "x", At: retry},
	}
	bodies := map[string]any{
		"coordinator": presentCoordinator(coordinatorFull),
		"list":        presentCoordinatorList(snap),
		"network":     buildNetworkStatus(networkStatus),
		"reset":       presentResetResult(hub.ResetResult{State: hub.ResetStateUnprovisioned, ResetAt: retry, CoordinatorID: coordinatorFull.ID, HomeIDBefore: "0xdeadbeef", KeysCleared: true, NetworkMembershipCleared: true}),
		"challenge":   presentResetChallenge(hub.ResetChallenge{Token: "Kq3-9mZtYbVn0aFwLxPcRdSuHeGjTkNmQrWvXyZa-BC", ExpiresAt: retry}),
		"audit": presentAuditPage(store.AuditPage{Items: []store.AuditEntry{{
			ID: 1, At: retry, Action: store.AuditCoordinatorRejected, Outcome: store.OutcomeRejected,
			ActorAddr: "192.0.2.1", RequestID: "r", CoordinatorID: "usb:10c4:ea60:aaa",
			Detail: store.AuditDetail{Reason: "coordinator_network_mismatch", ExpectedHomeID: "0x00000001", ActualHomeID: "0x00000002"},
		}}}),
		"key configuration": presentKeyConfiguration(hub.KeyConfiguration{
			Revision: 2, AppliedRevision: &applied, InEffect: true, ClassicComplete: true,
			Classes: []hub.KeyClass{
				{Class: "S0_Legacy", Configured: true, Source: store.KeySourceSupplied, CreatedAt: retry, ChangedAt: retry},
				{Class: "S2_AccessControl"},
			},
			Warnings: []string{"a value is shared between a classic and a Long Range class"},
			Apply:    &hub.KeyApply{Reason: hub.ApplyDriverRestartFailed, Message: "The driver could not be restarted."},
		}),
		"key export": presentKeyExport(hub.KeyExport{
			HomeID: "0xdeadbeef", ExportedAt: retry,
			Keys: map[string][]byte{"S0_Legacy": {0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
				0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}},
		}),
	}
	for name, body := range bodies {
		if _, err := Present(body); err != nil {
			t.Errorf("%s body refused by Present: %v", name, err)
		}
	}
}
