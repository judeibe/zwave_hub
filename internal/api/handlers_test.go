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
	st, err := store.Open(context.Background(), store.Options{DataDir: t.TempDir()})
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
	if status["state"] != "ready" || status["home_id"] != "0x00000001" {
		t.Fatalf("status = %v", status)
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
