package main

import (
	"context"
	"sync"
	"time"

	"github.com/judeibe/zwave_hub/internal/driver"
	"github.com/judeibe/zwave_hub/internal/hub"
	"github.com/judeibe/zwave_hub/internal/store"
)

// driverBridge connects the hub's selection to the supervisor: it runs the
// sidecar for the active coordinator while that device is attached, and turns
// the supervisor's reports into hub state. It lives here, not in either
// package, because neither may import the other.
type driverBridge struct {
	hub *hub.Hub
	sup *driver.Supervisor

	mu      sync.Mutex
	running string
	path    string
	// devices is the attached-device signature the faulted pause resumes on.
	signature string
}

func newDriverBridge(h *hub.Hub) *driverBridge {
	return &driverBridge{hub: h}
}

// attach gives the bridge its supervisor; the supervisor's Report callback is
// bridge.report, so the two are wired in this order.
func (b *driverBridge) attach(sup *driver.Supervisor) { b.sup = sup }

// reconcile starts, restarts or stops the driver so it matches the hub: one
// child for the active coordinator while it is attached, none otherwise. A
// faulted driver resumes only when the attached-device set changes.
func (b *driverBridge) reconcile(ctx context.Context) {
	snap := b.hub.Snapshot()
	b.mu.Lock()
	defer b.mu.Unlock()

	active, ok := coordinatorByID(snap, snap.ActiveID)
	signature := attachedSignature(snap)
	wantRun := ok && active.Attached && active.Support != "unsupported"

	switch {
	case !wantRun:
		if b.running != "" {
			b.sup.Stop()
			b.running, b.path = "", ""
			_ = b.hub.SetConnection(ctx, active.ID, hub.ConnectionNotAttached, nil)
		}
		if ok && !active.Attached && active.Connection != hub.ConnectionNotAttached {
			_ = b.hub.SetConnection(ctx, active.ID, hub.ConnectionNotAttached, nil)
		}
	case b.running != active.ID || b.path != active.Path:
		b.sup.Start(context.WithoutCancel(ctx), driver.Target{CoordinatorID: active.ID, SerialPath: active.Path})
		b.running, b.path = active.ID, active.Path
	case active.Connection == hub.ConnectionFaulted && signature != b.signature:
		b.sup.Start(context.WithoutCancel(ctx), driver.Target{CoordinatorID: active.ID, SerialPath: active.Path})
	}
	b.signature = signature
}

func (b *driverBridge) stop() {
	if b.sup != nil {
		b.sup.Stop()
	}
}

// report maps a supervisor report to hub state.
func (b *driverBridge) report(ctx context.Context, r driver.Report) {
	switch r.Connection {
	case "ready":
		if r.State == nil {
			return
		}
		_ = b.hub.Adopt(ctx, r.CoordinatorID, hub.Identified{
			Controller: store.ControllerIdentity{
				FirmwareVersion: r.State.Controller.FirmwareVersion,
				SDKVersion:      r.State.Controller.SDKVersion,
				ManufacturerID:  r.State.Controller.ManufacturerID,
				ProductType:     r.State.Controller.ProductType,
				ProductID:       r.State.Controller.ProductID,
				ChipType:        r.State.Controller.ChipType,
			},
			Network: hub.Network{
				HomeID:          r.State.HomeID,
				NodeCount:       r.State.NodeCount,
				SecureNodeCount: r.State.SecureNodeCount,
			},
		})
	case "connecting":
		_ = b.hub.SetDriverStatus(ctx, r.CoordinatorID, hub.ConnectionConnecting, nil, nil)
	case "retrying", "faulted":
		var last *hub.LastError
		if r.LastError != nil {
			last = &hub.LastError{Code: r.LastError.Code, Message: r.LastError.Message, At: r.LastError.At}
		}
		conn := hub.ConnectionRetrying
		if r.Connection == "faulted" {
			conn = hub.ConnectionFaulted
		}
		_ = b.hub.SetDriverStatus(ctx, r.CoordinatorID, conn, r.NextRetryAt, last)
	}
}

func coordinatorByID(snap hub.Snapshot, id string) (hub.Coordinator, bool) {
	for _, c := range snap.Coordinators {
		if c.ID == id {
			return c, true
		}
	}
	return hub.Coordinator{}, false
}

// attachedSignature identifies the set of attached devices and their paths, so
// a change resumes a faulted driver (contracts/zwave-server-sidecar.md §7).
func attachedSignature(snap hub.Snapshot) string {
	var sig string
	for _, c := range snap.Coordinators {
		if c.Attached {
			sig += c.ID + "@" + c.Path + ";"
		}
	}
	return sig
}

// reconcileInterval is how often the bridge compares the hub with the driver. A
// selection or a discovery change is picked up within one tick.
const reconcileInterval = 250 * time.Millisecond

func (b *driverBridge) run(ctx context.Context) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		b.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
