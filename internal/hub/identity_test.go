package hub

import (
	"context"
	"testing"
	"time"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/store"
)

func TestAdoptMovesToSupportedReady(t *testing.T) {
	h, st := newTestHub(t)
	dev := testDevice("ttyUSB0", "aaa", "1-2.3")
	mustSync(t, h, dev)
	mustSelect(t, h, dev.Identity.ID, "req-adopt")

	retry := time.Now().Add(time.Second)
	if err := h.SetDriverStatus(context.Background(), dev.Identity.ID, ConnectionRetrying, &retry, &LastError{Code: "driver_start_failed", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := findCoordinator(t, h.Snapshot(), dev.Identity.ID); got.NextRetryAt == nil || got.LastError == nil {
		t.Fatalf("retrying status not recorded: %+v", got)
	}

	manufacturer := 134
	err := h.Adopt(context.Background(), dev.Identity.ID, Identified{
		Controller: store.ControllerIdentity{FirmwareVersion: "7.21", ManufacturerID: &manufacturer, ChipType: "7.0"},
		Network:    Network{HomeID: "0xdeadbeef", NodeCount: 2, SecureNodeCount: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	snap := h.Snapshot()
	got := findCoordinator(t, snap, dev.Identity.ID)
	if got.Support != coordinator.SupportSupported || got.Connection != ConnectionReady {
		t.Fatalf("support=%s connection=%s", got.Support, got.Connection)
	}
	if got.Controller == nil || got.Controller.FirmwareVersion != "7.21" || *got.Controller.ManufacturerID != 134 {
		t.Fatalf("controller = %+v", got.Controller)
	}
	if got.NextRetryAt != nil || got.LastError != nil {
		t.Fatalf("status not cleared: %+v", got)
	}
	if snap.HomeID != "0xdeadbeef" {
		t.Fatalf("provisioning home id = %q", snap.HomeID)
	}
	row, err := st.GetCoordinator(context.Background(), dev.Identity.ID)
	if err != nil || row.FirmwareVersion != "7.21" || row.Support != "supported" {
		t.Fatalf("row = %+v err=%v", row, err)
	}
}

func TestRejectDetachesAndRecordsReason(t *testing.T) {
	h, _ := newTestHub(t)
	dev := testDevice("ttyUSB0", "aaa", "1-2.3")
	mustSync(t, h, dev)
	mustSelect(t, h, dev.Identity.ID, "req-reject")

	if err := h.Reject(context.Background(), dev.Identity.ID, coordinator.ReasonNotZWaveController); err != nil {
		t.Fatal(err)
	}
	snap := h.Snapshot()
	got := findCoordinator(t, snap, dev.Identity.ID)
	if snap.ActiveID != "" || got.Support != coordinator.SupportUnsupported || got.SupportReason != coordinator.ReasonNotZWaveController {
		t.Fatalf("snap.ActiveID=%q coordinator=%+v", snap.ActiveID, got)
	}
}
