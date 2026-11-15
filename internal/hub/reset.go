package hub

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/judeibe/zwave_hub/internal/event"
	"github.com/judeibe/zwave_hub/internal/store"
)

const (
	// ResetTimeout bounds one confirmed reset from the driver's acceptance
	// to the confirmation that the controller carries no network any more
	// (contracts/zwave-server-sidecar.md §6). The caller's context bounds it
	// as well, so a caller that gives up ends the reset early.
	ResetTimeout = 60 * time.Second

	// ResetPollInterval is how often the confirmation reads the controller's
	// state while it waits (contracts/zwave-server-sidecar.md §6).
	ResetPollInterval = 500 * time.Millisecond
)

// NetworkDriver is the part of the driver a reset needs. It is an interface so
// the hub does not import the driver package, and so a test can drive a reset
// without a sidecar (constitution III).
type NetworkDriver interface {
	// HardReset asks the driver to wipe the controller's network
	// configuration. A nil error means the request was accepted, never that
	// the reset happened: the server answers the result before it performs
	// the reset, and it never forwards the completion (contracts/
	// zwave-server-sidecar.md §6).
	HardReset(ctx context.Context) error
	// ControllerState reads the controller's current network state.
	ControllerState(ctx context.Context) (Network, error)
}

// ResetRequest is one confirmed reset. The identity fields are recorded in
// the audit trail unverified, as every audit row is (FR-019).
type ResetRequest struct {
	// RequestID correlates the audit rows with the event stream and the
	// response.
	RequestID string
	// ActorAddr is the requesting client's address, port stripped.
	ActorAddr string
	// ActorLabel is the self-declared label, at most 64 characters.
	ActorLabel string
}

// ResetState is the network state a completed reset reports.
type ResetState string

// ResetStateUnprovisioned is the only state a completed reset reports
// (contracts/openapi.yaml ResetResult).
const ResetStateUnprovisioned ResetState = "unprovisioned"

// ResetResult is what a completed reset reports: the state the hub is in
// afterwards and what the reset discarded. It holds no key material.
type ResetResult struct {
	// State is ResetStateUnprovisioned: the hub has no keys and no network
	// on record (FR-017).
	State ResetState
	// ResetAt is when the reset completed.
	ResetAt time.Time
	// CoordinatorID is the coordinator that was selected; it stays selected
	// afterwards, because a reset clears membership and keys, not the choice
	// of stick (FR-017).
	CoordinatorID string
	// HomeIDBefore is the network identifier the reset cleared, empty when
	// the coordinator carried none.
	HomeIDBefore string
	// KeysCleared reports that every key row was deleted.
	KeysCleared bool
	// NetworkMembershipCleared reports that the coordinator no longer
	// carries a network.
	NetworkMembershipCleared bool
}

// The failures a reset reports.
var (
	// ErrNoActiveCoordinator reports a reset with nothing selected: there is
	// no coordinator whose network could be reset.
	ErrNoActiveCoordinator = errors.New("hub: no coordinator is selected")
	// ErrNetworkDriverUnavailable reports a reset while the hub has no
	// driver to run it, so the reset cannot even be started.
	ErrNetworkDriverUnavailable = errors.New("hub: no network driver is attached")
	// ErrFirmwareUpdateInProgress reports a reset the driver refused because
	// it is updating its own firmware, which is the one cause the contract
	// names separately (contracts/zwave-server-sidecar.md §6). The driver
	// adapter recognises the driver's refusal code and wraps it in this, so
	// the hub classifies it without importing the driver package.
	ErrFirmwareUpdateInProgress = errors.New("hub: a firmware update is in progress")
)

// ResetDriverFailure is a reset the driver did not confirm. The stored state
// is untouched: the reset may still land on the stick after the deadline
// (contracts/zwave-server-sidecar.md §6). Reason is one of the
// network.reset_failed reasons of contracts/events.md §3.6.
type ResetDriverFailure struct {
	// Reason is timeout, driver_error or firmware_update_in_progress.
	Reason string
	// Err is the cause.
	Err error
}

// Error names the reason and the cause. The cause is a driver error, which
// never carries key material.
func (e *ResetDriverFailure) Error() string {
	return "hub: network reset did not complete (" + e.Reason + "): " + e.Err.Error()
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *ResetDriverFailure) Unwrap() error { return e.Err }

// SetNetworkDriver binds the driver a reset runs through. It is a setter
// rather than a constructor argument because the driver's report callback
// needs the hub, so the two cannot be built in one expression; the binding
// happens once, at start-up, before the API serves (constitution III).
func (h *Hub) SetNetworkDriver(d NetworkDriver) {
	h.driverMu.Lock()
	defer h.driverMu.Unlock()
	h.driver = d
}

// networkDriver returns the bound driver, or nil when none was bound.
func (h *Hub) networkDriver() NetworkDriver {
	h.driverMu.RLock()
	defer h.driverMu.RUnlock()
	return h.driver
}

// Reset performs the confirmed network reset (FR-017). The driver work
// happens outside the command queue and only the clearing is a command, so a
// key submission that arrives while the driver is working legitimately wins
// (FR-021): holding the store's one connection for the reset's 60 s would stall
// every other command behind it.
//
// The order is the contract's (contracts/zwave-server-sidecar.md §6). The hard
// reset is sent, and its result only means "accepted"; completion is confirmed
// by polling the controller until it reports no home ID and no nodes. Only then
// are the key rows deleted and the home ID and both revisions cleared, in one
// command that carries the network.reset row. Until that confirmation the
// stored state is untouched: a reset that does not confirm reports
// network.reset_failed, records the attempt and returns a *ResetDriverFailure,
// because the reset may still have landed on the stick.
//
// The selection is kept. A reset clears what the stick knows, not which stick
// the operator chose (FR-017).
func (h *Hub) Reset(ctx context.Context, req ResetRequest) (ResetResult, error) {
	state := h.committedState()
	if state.activeID == "" {
		return ResetResult{}, ErrNoActiveCoordinator
	}
	driver := h.networkDriver()
	if driver == nil {
		return ResetResult{}, ErrNetworkDriverUnavailable
	}

	coordinatorID := state.activeID
	homeIDBefore := h.homeIDOnRecord(coordinatorID)
	began := time.Now()
	h.emitReset(ctx, req, &event.NetworkResetStarted{HomeIDBefore: homeIDBefore}, coordinatorID)

	if err := driver.HardReset(ctx); err != nil {
		return ResetResult{}, h.failReset(ctx, req, coordinatorID, resetFailureReason(err), err)
	}
	if err := h.confirmReset(ctx, driver); err != nil {
		return ResetResult{}, h.failReset(ctx, req, coordinatorID, resetFailureReason(err), err)
	}

	result, err := h.clearNetwork(ctx, req, coordinatorID, homeIDBefore)
	if err != nil {
		return ResetResult{}, err
	}
	h.emitReset(ctx, req, &event.NetworkResetCompleted{
		HomeIDBefore: homeIDBefore,
		DurationMS:   time.Since(began).Milliseconds(),
	}, coordinatorID)
	return result, nil
}

// confirmReset waits for the driver to report that the controller carries no
// network any more. The first poll is immediate, because a fast reset is
// already done by the time the result frame arrives, and the caller's context
// bounds the wait as well as ResetTimeout does.
func (h *Hub) confirmReset(ctx context.Context, driver NetworkDriver) error {
	ctx, cancel := context.WithTimeout(ctx, ResetTimeout)
	defer cancel()
	ticker := time.NewTicker(ResetPollInterval)
	defer ticker.Stop()
	for {
		network, err := driver.ControllerState(ctx)
		if err != nil {
			return err
		}
		if resetConfirmed(network) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// resetConfirmed reports the post-condition a reset waits for: no home ID and
// no nodes (contracts/zwave-server-sidecar.md §6).
func resetConfirmed(network Network) bool {
	return network.HomeID == "" && network.NodeCount == 0
}

// resetFailureReason maps a driver error to the closed reason set of
// network.reset_failed (contracts/events.md §3.6). A caller that gave up and a
// deadline both read as a timeout: to the driver the two are the same silence.
func resetFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrFirmwareUpdateInProgress):
		return "firmware_update_in_progress"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return "driver_error"
	}
}

// failReset records and reports a reset the driver did not confirm, then returns
// the failure. The audit row is written under a context that ignores the
// caller's cancellation: a reset that ran out of time is exactly the attempt the
// trail has to hold, and the caller's deadline is what made it run out. The
// stored state is deliberately left alone — the reset may still land on the
// stick, and the operator is told to re-read the network before retrying.
func (h *Hub) failReset(ctx context.Context, req ResetRequest, coordinatorID, reason string, cause error) error {
	_, err := h.worker.Submit(context.WithoutCancel(ctx), Command[provisioning]{
		Audit: store.AuditEntry{
			Action:        store.AuditNetworkResetFailed,
			Outcome:       store.OutcomeFailed,
			ActorAddr:     req.ActorAddr,
			ActorLabel:    req.ActorLabel,
			RequestID:     req.RequestID,
			CoordinatorID: coordinatorID,
			Detail:        store.AuditDetail{Reason: reason},
		},
		// A failed reset changes nothing: the command carries only its row.
		Apply: func(ctx context.Context, tx *store.Tx) (provisioning, error) {
			return committedIn(ctx, tx)
		},
	})
	if err != nil {
		return fmt.Errorf("hub: record the failed network reset: %w", err)
	}
	h.emitReset(ctx, req, &event.NetworkResetFailed{Reason: reason}, coordinatorID)
	return &ResetDriverFailure{Reason: reason, Err: cause}
}

// clearNetwork is the reset's one state-changing command: every key row, the
// home ID, the key revision and the applied revision go in one transaction with
// the network.reset row, so a reader never sees half a reset (FR-017,
// data-model.md §9.1). Like the failure row it runs under a context that ignores
// the caller's cancellation: the controller has already been wiped, so leaving
// the keys behind would be the one outcome worse than finishing.
func (h *Hub) clearNetwork(ctx context.Context, req ResetRequest, coordinatorID, homeIDBefore string) (ResetResult, error) {
	cleared := true
	var homeIDCleared string
	_, err := h.worker.Submit(context.WithoutCancel(ctx), Command[provisioning]{
		Audit: store.AuditEntry{
			Action:        store.AuditNetworkReset,
			Outcome:       store.OutcomeSucceeded,
			ActorAddr:     req.ActorAddr,
			ActorLabel:    req.ActorLabel,
			RequestID:     req.RequestID,
			CoordinatorID: coordinatorID,
			Detail:        store.AuditDetail{Cleared: &cleared},
		},
		Apply: func(ctx context.Context, tx *store.Tx) (provisioning, error) {
			current, err := committedIn(ctx, tx)
			if err != nil {
				return provisioning{}, err
			}
			// The row is the truth, not the last published snapshot: a
			// submission that committed while the driver was resetting is
			// cleared with the rest, and its revision is what is reported.
			homeIDCleared = current.homeID
			if err := tx.DeleteAllKeys(ctx); err != nil {
				return provisioning{}, err
			}
			if err := tx.SetHomeID(ctx, ""); err != nil {
				return provisioning{}, err
			}
			if err := tx.SetKeyRevision(ctx, 0); err != nil {
				return provisioning{}, err
			}
			if err := tx.SetAppliedRevision(ctx, nil); err != nil {
				return provisioning{}, err
			}
			current.homeID, current.keyRevision = "", 0
			return current, nil
		},
	})
	if err != nil {
		return ResetResult{}, fmt.Errorf("hub: clear the network state: %w", err)
	}

	// The overlay holds the last network the driver reported. A reset removes
	// it, or readiness would keep reporting a network the controller no longer
	// carries for as long as the child stays up.
	h.mu.Lock()
	if known, ok := h.known[coordinatorID]; ok {
		known.overlay.network = nil
	}
	h.mu.Unlock()

	if homeIDBefore == "" {
		homeIDBefore = homeIDCleared
	}
	return ResetResult{
		State:                    ResetStateUnprovisioned,
		ResetAt:                  time.Now(),
		CoordinatorID:            coordinatorID,
		HomeIDBefore:             homeIDBefore,
		KeysCleared:              true,
		NetworkMembershipCleared: true,
	}, nil
}

// homeIDOnRecord is the network identifier a reset reports it cleared: the one
// the hub committed, or the one the selected coordinator last reported when the
// hub has none on record yet.
func (h *Hub) homeIDOnRecord(id string) string {
	if state := h.committedState(); state.homeID != "" {
		return state.homeID
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if known, ok := h.known[id]; ok && known.overlay.network != nil {
		return known.overlay.network.HomeID
	}
	return ""
}

// emitReset reports one reset event (contracts/events.md §3.6). The events carry
// the network identifiers and the reason, never key material.
func (h *Hub) emitReset(ctx context.Context, req ResetRequest, ev event.Event, coordinatorID string) {
	if h.emitter == nil {
		return
	}
	h.emitter.Emit(ctx, ev, event.Correlation{
		RequestID:     req.RequestID,
		ActorAddr:     req.ActorAddr,
		ActorLabel:    req.ActorLabel,
		CoordinatorID: coordinatorID,
	})
}
