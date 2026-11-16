package hub

import (
	"context"
	"fmt"
	"time"

	"github.com/judeibe/zwave_hub/internal/event"
	"github.com/judeibe/zwave_hub/internal/store"
)

// KeyMaterial is the opened key set the driver renders into its config file
// (FR-012). It is the one place plaintext keys leave the hub, and only toward
// the sidecar's config file: no response, event or audit row can carry it. The
// receiver wipes the values once they are written.
type KeyMaterial struct {
	// Revision is the committed revision these values belong to.
	Revision int
	// Classic and LongRange map a class to its raw 16 bytes.
	Classic   map[string][]byte
	LongRange map[string][]byte
}

// KeyMaterial opens every stored key, together with the revision they were
// committed under, in one transaction so the pair is consistent. A key that
// fails authentication is a *KeyStoreCorruptError: the child is not started
// from a partial set.
func (h *Hub) KeyMaterial(ctx context.Context) (KeyMaterial, error) {
	var (
		keys []store.StoredKey
		row  store.Provisioning
	)
	err := h.store.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		if keys, err = tx.ListKeys(ctx); err != nil {
			return err
		}
		row, err = tx.GetProvisioning(ctx)
		return err
	})
	if err != nil {
		return KeyMaterial{}, fmt.Errorf("hub: read the key set: %w", err)
	}

	material := KeyMaterial{Revision: row.KeyRevision, Classic: map[string][]byte{}, LongRange: map[string][]byte{}}
	var corrupt []string
	for _, stored := range keys {
		value, err := h.store.OpenKey(stored.SecurityClass, stored.Blob)
		if err != nil {
			corrupt = append(corrupt, stored.SecurityClass)
			continue
		}
		if isLongRange(stored.SecurityClass) {
			material.LongRange[stored.SecurityClass] = value
		} else {
			material.Classic[stored.SecurityClass] = value
		}
	}
	h.reportKeyStore(ctx, corrupt)
	if len(corrupt) > 0 {
		wipeKeyValues(material.Classic)
		wipeKeyValues(material.LongRange)
		return KeyMaterial{}, &KeyStoreCorruptError{Classes: canonicalClasses(corrupt)}
	}
	return material, nil
}

func isLongRange(class string) bool {
	return class == SecurityClassS2AuthenticatedLR || class == SecurityClassS2AccessControlLR
}

// KeyApplier restarts the child so it picks up the committed keys. It is an
// interface so the hub does not import the driver package (constitution III).
// Apply returns at once: the outcome arrives through NoteKeysApplied or
// NoteKeysApplyFailed when the child's start succeeds or fails.
type KeyApplier interface {
	Apply(ctx context.Context)
}

// SetKeyApplier binds the applier a committed key change calls. It is set once
// at start-up, before the API serves, like SetNetworkDriver.
func (h *Hub) SetKeyApplier(a KeyApplier) {
	h.driverMu.Lock()
	defer h.driverMu.Unlock()
	h.applier = a
}

// applyCommitted asks the driver to restart with the committed keys. Nothing to
// do without an applier or without a selected coordinator: the revision stays
// stored and the next start renders it (R14 §1, §3).
func (h *Hub) applyCommitted(ctx context.Context) {
	h.driverMu.RLock()
	applier := h.applier
	h.driverMu.RUnlock()
	if applier == nil || h.committedState().activeID == "" {
		return
	}
	applier.Apply(context.WithoutCancel(ctx))
}

// CommittedKeyRevision is the key revision of the last committed key change.
func (h *Hub) CommittedKeyRevision() int {
	return h.committedState().keyRevision
}

// NoteKeysApplied records that the child came back ready carrying revision
// (FR-012, FR-015): applied_revision moves to it and the controller's home ID
// is persisted on the same path, as one transaction. A revision of zero is a
// child that ran without keys and records nothing. started is when the apply
// began, for the event's duration.
//
// The recorded revision is the one the child was started with, not the latest
// committed one: a submission that landed while the child was starting leaves
// the revisions apart, and the next start applies it.
func (h *Hub) NoteKeysApplied(ctx context.Context, id string, revision int, homeID string, started time.Time) error {
	if revision <= 0 {
		return nil
	}
	state := h.committedState()
	if state.activeID != id {
		return nil
	}
	err := h.worker.mutate(ctx, func(ctx context.Context, tx *store.Tx, current provisioning) (provisioning, error) {
		if current.activeID != id {
			return current, nil
		}
		applied := revision
		if err := tx.SetAppliedRevision(ctx, &applied); err != nil {
			return current, err
		}
		if homeID != "" && current.homeID != homeID {
			if err := tx.SetHomeID(ctx, homeID); err != nil {
				return current, err
			}
			current.homeID = homeID
		}
		return current, nil
	})
	if err != nil {
		return fmt.Errorf("hub: record the applied key revision: %w", err)
	}
	h.mu.Lock()
	if known, ok := h.known[id]; ok {
		known.overlay.applyFailedRevision = 0
	}
	h.mu.Unlock()
	if h.emitter != nil {
		h.emitter.Emit(ctx, &event.KeysApplied{
			Revision:        h.committedState().keyRevision,
			AppliedRevision: revision,
			DurationMS:      time.Since(started).Milliseconds(),
		}, event.Correlation{CoordinatorID: id, HomeID: homeID})
	}
	return nil
}

// NoteKeysApplyFailed records that the child could not be brought up with the
// committed revision (FR-015): the revision in effect stays what it was, the
// failure is audited as keys.apply_failed and emitted, and the reason goes to
// the API as driver_restart_failed through the overlay's last error.
//
// One failure is recorded per revision: the supervisor retries with backoff and
// each attempt reports, but the operator needs one attributable row, not one
// per retry.
func (h *Hub) NoteKeysApplyFailed(ctx context.Context, id, reason string) error {
	state := h.committedState()
	if state.activeID != id || state.keyRevision == 0 {
		return nil
	}
	h.mu.Lock()
	if known, ok := h.known[id]; ok {
		if known.overlay.applyFailedRevision == state.keyRevision {
			h.mu.Unlock()
			return nil
		}
		known.overlay.applyFailedRevision = state.keyRevision
	}
	h.mu.Unlock()
	row, err := h.store.GetProvisioning(ctx)
	if err != nil {
		return fmt.Errorf("hub: read the applied key revision: %w", err)
	}
	if revisionInEffect(row.KeyRevision, row.AppliedRevision) {
		return nil
	}
	appliedNow := 0
	if row.AppliedRevision != nil {
		appliedNow = *row.AppliedRevision
	}
	_, err = h.worker.Submit(ctx, Command[provisioning]{
		Audit: store.AuditEntry{
			Action:        store.AuditKeysApplyFailed,
			Outcome:       store.OutcomeFailed,
			ActorAddr:     activationActorAddr,
			RequestID:     activationRequestID,
			CoordinatorID: id,
			Detail:        store.AuditDetail{Reason: reason},
		},
		Apply: func(ctx context.Context, tx *store.Tx) (provisioning, error) {
			return committedIn(ctx, tx)
		},
	})
	if err != nil {
		return fmt.Errorf("hub: record the failed key apply: %w", err)
	}
	if h.emitter != nil {
		h.emitter.Emit(ctx, &event.KeysApplyFailed{
			Revision: row.KeyRevision, AppliedRevision: appliedNow, Reason: reason,
		}, event.Correlation{CoordinatorID: id})
	}
	return nil
}
