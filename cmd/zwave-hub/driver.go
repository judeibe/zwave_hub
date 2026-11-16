package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/driver"
	"github.com/judeibe/zwave_hub/internal/hub"
	"github.com/judeibe/zwave_hub/internal/store"
)

// driverBridge connects the hub's selection to the supervisor: it runs the
// sidecar for the active coordinator while that device is attached, and turns
// the supervisor's reports into hub state. It lives here, not in either
// package, because neither may import the other.
//
// The reconcile rules are not here either: driver.Decide derives them
// (data-model.md §9.3), so the transitions that a restart, an unplug and a
// re-plug produce are one tested function instead of branches spread over two
// packages.
type driverBridge struct {
	hub *hub.Hub
	sup *driver.Supervisor

	mu      sync.Mutex
	running string
	path    string
	// devices is the attached-device signature the faulted pause resumes on.
	signature string
	// wake carries a settled discovery change to the reconciler.
	wake chan struct{}
}

func newDriverBridge(h *hub.Hub) *driverBridge {
	return &driverBridge{hub: h, wake: make(chan struct{}, 1)}
}

// attach gives the bridge its supervisor; the supervisor's Report callback is
// bridge.report, so the two are wired in this order.
func (b *driverBridge) attach(sup *driver.Supervisor) { b.sup = sup }

// deviceSetChanged wakes the reconciler: a settled discovery change is what
// resumes a paused driver (R14 §7), so waiting for the next tick would add up
// to a tick of avoidable delay to every re-plug.
func (b *driverBridge) deviceSetChanged() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// reconcile starts, restarts or stops the driver so it matches the hub: one
// child for the active coordinator while it is attached, none otherwise. A
// faulted driver resumes only when the attached-device set changes.
func (b *driverBridge) reconcile(ctx context.Context) {
	snap := b.hub.Snapshot()
	active, selected := snap.Coordinator(snap.ActiveID)

	b.mu.Lock()
	defer b.mu.Unlock()

	signature := attachedSignature(snap)
	action := driver.Decide(driver.Situation{
		Selected:         snap.ActiveID,
		Attached:         active.Attached,
		Usable:           selected && active.Support != coordinator.SupportUnsupported,
		Path:             active.Path,
		Connection:       string(active.Connection),
		Running:          b.running,
		RunningPath:      b.path,
		DeviceSetChanged: signature != b.signature,
	})

	switch action {
	case driver.ActionStart:
		b.sup.Start(context.WithoutCancel(ctx), driver.Target{
			CoordinatorID: snap.ActiveID,
			SerialPath:    active.Path,
		})
		b.running, b.path = snap.ActiveID, active.Path
	case driver.ActionResume:
		// The device set moved, so the same target may work now (R14 §8). The
		// supervisor keeps the target it was given.
		b.sup.Resume(context.WithoutCancel(ctx))
	case driver.ActionStop:
		stopped := b.running
		b.sup.Stop()
		b.running, b.path = "", ""
		if stopped != "" {
			_ = b.hub.SetConnection(ctx, stopped, hub.ConnectionNotAttached, nil)
		}
	case driver.ActionLeave:
	}

	// A selection whose device is not present is reported not_attached, whether
	// it was ready before the device left or never started at all
	// (data-model.md §9.3). The selection itself stays.
	if selected {
		if state, changed := driver.Detached(string(active.Connection)); changed && !active.Attached {
			_ = b.hub.SetConnection(ctx, active.ID, hub.Connection(state), nil)
		}
	}
	b.signature = signature
}

// Apply makes the bridge the hub's key applier: a committed key change restarts
// only the child, which re-renders its config from the database (FR-012). The
// supervisor ignores it when no child is being supervised.
func (b *driverBridge) Apply(ctx context.Context) {
	if b.sup != nil {
		b.sup.Apply(ctx)
	}
}

// keys is the supervisor's key source: the opened set of the committed
// revision. A store that cannot authenticate is an error, so the child is never
// started from a partial set.
func (b *driverBridge) keys(ctx context.Context) (driver.KeySet, error) {
	material, err := b.hub.KeyMaterial(ctx)
	if err != nil {
		return driver.KeySet{}, err
	}
	return driver.KeySet{Revision: material.Revision, Keys: material.Classic, LongRange: material.LongRange}, nil
}

func (b *driverBridge) stop() {
	if b.sup != nil {
		b.sup.Stop()
	}
}

// HardReset and ControllerState make the bridge the hub's network driver: the
// reset orchestration in internal/hub asks the bridge, the bridge asks the
// supervisor, and the supervisor asks the child that is running now. Neither
// package imports the other, so the adapter lives here.
func (b *driverBridge) HardReset(ctx context.Context) error {
	if b.sup == nil {
		return driver.ErrClosed
	}
	err := b.sup.HardReset(ctx)
	if driver.IsFirmwareUpdateBusy(err) {
		// The hub's reason set names this cause separately, and it must not
		// import the driver package to recognise the driver's error code.
		return fmt.Errorf("%w: %v", hub.ErrFirmwareUpdateInProgress, err)
	}
	return err
}

// ControllerState reports the live driver's network. The reset reads it until
// the controller carries no home ID and no nodes.
func (b *driverBridge) ControllerState(ctx context.Context) (hub.Network, error) {
	if b.sup == nil {
		return hub.Network{}, driver.ErrClosed
	}
	state, err := b.sup.ControllerState(ctx)
	if err != nil {
		return hub.Network{}, err
	}
	return hub.Network{
		HomeID:          state.HomeID,
		NodeCount:       state.NodeCount,
		SecureNodeCount: state.SecureNodeCount,
	}, nil
}

// report maps a supervisor report to hub state.
func (b *driverBridge) report(ctx context.Context, r driver.Report) {
	switch r.Connection {
	case driver.ConnectionReady:
		if r.State == nil {
			return
		}
		started := time.Now()
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
		// The child is up with the revision it was started with: record it, and
		// the controller's home ID with it (FR-012, FR-015).
		_ = b.hub.NoteKeysApplied(ctx, r.CoordinatorID, r.KeyRevision, r.State.HomeID, started)
	case driver.ConnectionConnecting:
		_ = b.hub.SetDriverStatus(ctx, r.CoordinatorID, hub.ConnectionConnecting, nil, nil)
	case driver.ConnectionRetrying, driver.ConnectionFaulted:
		var last *hub.LastError
		if r.LastError != nil {
			last = &hub.LastError{Code: r.LastError.Code, Message: r.LastError.Message, At: r.LastError.At}
		}
		conn := hub.ConnectionRetrying
		if r.Connection == driver.ConnectionFaulted {
			conn = hub.ConnectionFaulted
		}
		_ = b.hub.SetDriverStatus(ctx, r.CoordinatorID, conn, r.NextRetryAt, last)
		if last != nil {
			_ = b.hub.NoteKeysApplyFailed(ctx, r.CoordinatorID, last.Code)
		}
	}
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
// selection or a discovery change is picked up within one tick, or at once when
// discovery settles and wakes the bridge.
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
		case <-b.wake:
		}
	}
}
