package api

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/driver"
	"github.com/judeibe/zwave_hub/internal/hub"
)

// readyDeadline is the SC-001 bound: select to ready within 30 seconds.
const readyDeadline = 30 * time.Second

// mockSidecarDir finds the repository's sidecar and skips when npm ci has not
// run or node is missing.
func mockSidecarDir(t *testing.T) string {
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

// startMockSupervisor builds a mock-driver supervisor whose reports adopt the
// coordinator into h, as cmd/zwave-hub's bridge does. It is stopped before the
// fixture's cleanup closes the store, because cleanups run last-in first-out.
func startMockSupervisor(t *testing.T, h *hub.Hub) *driver.Supervisor {
	t.Helper()
	sup, err := driver.NewSupervisor(driver.Options{
		SidecarDir: mockSidecarDir(t),
		Mock:       true,
		RunDir:     t.TempDir(),
		CacheDir:   t.TempDir(),
		Report: func(ctx context.Context, r driver.Report) {
			if r.Connection != "ready" || r.State == nil {
				return
			}
			_ = h.Adopt(ctx, r.CoordinatorID, hub.Identified{Network: hub.Network{
				HomeID:          r.State.HomeID,
				NodeCount:       r.State.NodeCount,
				SecureNodeCount: r.State.SecureNodeCount,
			}})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sup.Stop)
	return sup
}

func TestServeWithNoHardware(t *testing.T) {
	f := newHandlerFixture(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := f.do("GET", path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d: %s", path, rec.Code, rec.Body)
		}
	}

	list := f.do("GET", "/api/v1/coordinators", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", list.Code, list.Body)
	}
	body := decode(t, list)
	if items, ok := body["items"].([]any); !ok || len(items) != 0 {
		t.Errorf("items = %#v, want an empty array", body["items"])
	}
	if body["active_coordinator_id"] != nil {
		t.Errorf("active_coordinator_id = %v, want null", body["active_coordinator_id"])
	}

	network := f.do("GET", "/api/v1/network", "")
	if network.Code != http.StatusOK {
		t.Fatalf("network status %d: %s", network.Code, network.Body)
	}
	if state := decode(t, network)["state"]; state != "unprovisioned" {
		t.Errorf("network state = %v, want unprovisioned", state)
	}

	active := f.do("GET", "/api/v1/active-coordinator", "")
	if active.Code != http.StatusNotFound {
		t.Fatalf("active status %d: %s", active.Code, active.Body)
	}
	if ct := active.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("active content type = %q", ct)
	}
	if code := decode(t, active)["code"]; code != "no_active_coordinator" {
		t.Errorf("active code = %v, want no_active_coordinator", code)
	}
}

func TestSelectMockCoordinatorReachesReady(t *testing.T) {
	mockSidecarDir(t)
	f := newHandlerFixture(t)
	sup := startMockSupervisor(t, f.hub)

	dev := device("mock")
	if err := f.hub.SyncDiscovery(context.Background(), []coordinator.Device{dev}); err != nil {
		t.Fatal(err)
	}
	id := dev.Identity.ID

	started := time.Now()
	put := f.do("PUT", "/api/v1/active-coordinator", `{"coordinator_id":"`+id+`"}`)
	if put.Code != http.StatusOK {
		t.Fatalf("select status %d: %s", put.Code, put.Body)
	}
	sup.Start(context.Background(), driver.Target{CoordinatorID: id, SerialPath: dev.Path})

	var active map[string]any
	for {
		rec := f.do("GET", "/api/v1/active-coordinator", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("active status %d: %s", rec.Code, rec.Body)
		}
		active = decode(t, rec)
		if active["connection"] == "ready" {
			break
		}
		if time.Since(started) > readyDeadline {
			t.Fatalf("coordinator not ready within %v; connection = %v", readyDeadline, active["connection"])
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("select to ready took %v", time.Since(started))

	if active["id"] != id {
		t.Errorf("active id = %v, want %s", active["id"], id)
	}
	network, _ := active["network"].(map[string]any)
	if network["home_id"] != "0x00000001" {
		t.Errorf("active network = %v, want home_id 0x00000001", active["network"])
	}

	status := decode(t, f.do("GET", "/api/v1/network", ""))
	// The coordinator is ready, but no keys are configured, so the derived
	// state is degraded until the classic set exists (data-model.md §8.2).
	if status["state"] != "degraded" {
		t.Errorf("network state = %v, want degraded", status["state"])
	}
	if status["ready_for_secure_inclusion"] != false {
		t.Errorf("ready_for_secure_inclusion = %v, want false without keys", status["ready_for_secure_inclusion"])
	}
	if status["home_id"] == nil || status["home_id"] != "0x00000001" {
		t.Errorf("network home_id = %v, want 0x00000001", status["home_id"])
	}

	if home := f.hub.Snapshot().HomeID; home == "" {
		t.Error("provisioning home_id is empty once the coordinator is ready")
	}
}

func TestSelectUnknownCoordinatorIsRejected(t *testing.T) {
	f := newHandlerFixture(t)

	put := f.do("PUT", "/api/v1/active-coordinator", `{"coordinator_id":"usb:ffff:ffff:none"}`)
	if put.Code != http.StatusNotFound {
		t.Fatalf("select status %d: %s", put.Code, put.Body)
	}
	if code := decode(t, put)["code"]; code != "coordinator_not_found" {
		t.Errorf("code = %v, want coordinator_not_found", code)
	}

	audit := f.do("GET", "/api/v1/audit?action=coordinator.rejected", "")
	if audit.Code != http.StatusOK {
		t.Fatalf("audit status %d: %s", audit.Code, audit.Body)
	}
	items, _ := decode(t, audit)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("audit items = %v, want one coordinator.rejected row", items)
	}
	entry, _ := items[0].(map[string]any)
	if entry["action"] != "coordinator.rejected" || entry["outcome"] != "rejected" ||
		entry["coordinator_id"] != "usb:ffff:ffff:none" {
		t.Errorf("audit entry = %v", entry)
	}
}
