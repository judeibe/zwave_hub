package hub

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/judeibe/zwave_hub/internal/event"
	"github.com/judeibe/zwave_hub/internal/store"
)

// KeyExportRequest is one confirmed export. The identity fields are recorded in
// the audit trail unverified, as every audit row is (FR-019).
type KeyExportRequest struct {
	// RequestID correlates the audit rows with the event stream and the
	// response.
	RequestID string
	// ActorAddr is the requesting client's address, port stripped.
	ActorAddr string
	// ActorLabel is the self-declared label, at most 64 characters.
	ActorLabel string
}

// KeyExportChallenge is the first step of the two-step export: a token handed to
// its requester exactly once and the instant it stops being redeemable. Nothing
// has been exported when it is returned (contracts/openapi.yaml exportKeys,
// FR-013).
type KeyExportChallenge struct {
	// Token is the single-use secret. It authorizes one export from the address
	// it was issued to, and is never logged or audited.
	Token string
	// ExpiresAt is when the token stops being redeemable.
	ExpiresAt time.Time
}

// KeyExportConfirmationError reports an export refused at its confirmation step.
// The two cases FR-013 tells apart are a token that was issued for this
// operation and address and has since expired or been consumed (Unusable), and
// one that is unknown or bound to another operation or client. Err is the
// store's sentinel, so errors.Is(err, store.ErrConfirmationInvalid) still holds.
type KeyExportConfirmationError struct {
	// Unusable is true for an expired or already used token.
	Unusable bool
	// Err is the cause, which never names the token.
	Err error
}

// Error states the refusal without naming the token.
func (e *KeyExportConfirmationError) Error() string {
	if e.Unusable {
		return "hub: the export confirmation has expired or was already used"
	}
	return "hub: the export confirmation is not valid for this client and operation"
}

// Unwrap exposes the store's sentinel to errors.Is.
func (e *KeyExportConfirmationError) Unwrap() error { return e.Err }

// KeyExport is the confirmed export of FR-013: the plaintext values of every
// configured class with the network they belong to. It is the one value that
// carries key material out of the hub, and only toward the response handler,
// which wipes it once the body is encoded.
type KeyExport struct {
	// HomeID is the network on record, empty when the hub has none.
	HomeID string
	// ExportedAt is when the values were opened.
	ExportedAt time.Time
	// Keys maps a security class to its raw 16 bytes.
	Keys map[string][]byte
}

// Wipe zeroes every exported value once the response has been encoded, so the
// plaintext lives only as long as building the body takes (FR-013).
func (e *KeyExport) Wipe() {
	wipeKeyValues(e.Keys)
}

// RequestKeyExport issues the token a two-step export starts with (FR-013). The
// step performs nothing but the issue: the token row, the keys.export_requested
// audit row and the event. The values are opened only when the token comes back
// through ConfirmKeyExport.
//
// A store that cannot authenticate a stored row is refused before a token is
// minted, because the confirmed step could never export it: the operator has to
// recover the set first (R14 §2). An export is not gated by
// ZWAVE_HUB_DESTRUCTIVE_OPS; only the network reset is (INC-4).
func (h *Hub) RequestKeyExport(ctx context.Context, req KeyExportRequest) (KeyExportChallenge, error) {
	if err := h.refuseCorruptStore(ctx); err != nil {
		return KeyExportChallenge{}, err
	}
	issued, err := h.store.IssueConfirmation(ctx, store.ConfirmationKeysExport, req.ActorAddr, time.Time{})
	if err != nil {
		return KeyExportChallenge{}, fmt.Errorf("hub: issue the export confirmation: %w", err)
	}
	// The token is issued before the row that records it: a live token with no
	// row expires unused, while a row for a token that was never issued would
	// claim an operation nobody can redeem.
	if err := h.recordKeyExportRequest(ctx, req); err != nil {
		return KeyExportChallenge{}, err
	}
	h.emitKeyExport(ctx, req, &event.KeysExportRequested{
		Operation: string(store.ConfirmationKeysExport),
		ExpiresAt: issued.ExpiresAt,
	})
	return KeyExportChallenge{Token: issued.Token, ExpiresAt: issued.ExpiresAt}, nil
}

// ConfirmKeyExport redeems the token and opens the stored keys (FR-013). The
// token is consumed before any value is opened, so one token can never authorize
// two exports. A refused token exports nothing, is audited as
// keys.export_rejected, and comes back as a *KeyExportConfirmationError naming
// whether it was unusable (expired or used) or invalid (unknown or another
// client's).
//
// A stored blob that fails authentication refuses the whole export rather than
// handing back a partial set (R14 §2). The refusal happens after the token is
// spent, because authorization comes before the read.
func (h *Hub) ConfirmKeyExport(ctx context.Context, req KeyExportRequest, token string) (KeyExport, error) {
	err := h.store.ConsumeConfirmation(ctx, token, store.ConfirmationKeysExport, req.ActorAddr, time.Time{})
	if err != nil {
		if !errors.Is(err, store.ErrConfirmationInvalid) {
			return KeyExport{}, fmt.Errorf("hub: redeem the export confirmation: %w", err)
		}
		refusal := &KeyExportConfirmationError{Unusable: errors.Is(err, store.ErrConfirmationUnusable), Err: err}
		if recordErr := h.recordKeyExportRejected(ctx, req, refusal.Unusable); recordErr != nil {
			return KeyExport{}, recordErr
		}
		return KeyExport{}, refusal
	}

	export, err := h.openKeyExport(ctx)
	if err != nil {
		return KeyExport{}, err
	}
	classes := classesOf(export.Keys)
	if err := h.recordKeyExported(ctx, req, classes); err != nil {
		export.Wipe()
		return KeyExport{}, err
	}
	h.emitKeyExport(ctx, req, &event.KeysExported{Classes: classes, HomeID: export.HomeID})
	return export, nil
}

// openKeyExport opens every stored key, together with the home ID on record, in
// one transaction so the pair is consistent. One blob that fails authentication
// makes the whole read a *KeyStoreCorruptError: the caller is handed either the
// complete set or no set at all.
func (h *Hub) openKeyExport(ctx context.Context) (KeyExport, error) {
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
		return KeyExport{}, fmt.Errorf("hub: read the key set: %w", err)
	}

	values := make(map[string][]byte, len(keys))
	var corrupt []string
	for _, stored := range keys {
		value, err := h.store.OpenKey(stored.SecurityClass, stored.Blob)
		if err != nil {
			corrupt = append(corrupt, stored.SecurityClass)
			continue
		}
		values[stored.SecurityClass] = value
	}
	h.reportKeyStore(ctx, corrupt)
	if len(corrupt) > 0 {
		wipeKeyValues(values)
		return KeyExport{}, &KeyStoreCorruptError{Classes: canonicalClasses(corrupt)}
	}
	return KeyExport{HomeID: row.HomeID, ExportedAt: time.Now().UTC(), Keys: values}, nil
}

// recordKeyExportRequest writes the keys.export_requested row of an issued token
// (data-model.md §7). Nothing else changes, so the command carries only its row.
func (h *Hub) recordKeyExportRequest(ctx context.Context, req KeyExportRequest) error {
	_, err := h.worker.Submit(ctx, Command[provisioning]{
		Audit: store.AuditEntry{
			Action:        store.AuditKeysExportRequested,
			Outcome:       store.OutcomeSucceeded,
			ActorAddr:     req.ActorAddr,
			ActorLabel:    req.ActorLabel,
			RequestID:     req.RequestID,
			CoordinatorID: h.committedState().activeID,
		},
		Apply: func(ctx context.Context, tx *store.Tx) (provisioning, error) {
			return committedIn(ctx, tx)
		},
	})
	if err != nil {
		return fmt.Errorf("hub: record the export request: %w", err)
	}
	return nil
}

// recordKeyExported audits a completed export (FR-019). The row names the
// classes that were exported and never a value: the trail records that the
// export happened, not what it contained.
func (h *Hub) recordKeyExported(ctx context.Context, req KeyExportRequest, classes []string) error {
	_, err := h.worker.Submit(ctx, Command[provisioning]{
		Audit: store.AuditEntry{
			Action:        store.AuditKeysExported,
			Outcome:       store.OutcomeSucceeded,
			ActorAddr:     req.ActorAddr,
			ActorLabel:    req.ActorLabel,
			RequestID:     req.RequestID,
			CoordinatorID: h.committedState().activeID,
			Detail:        store.AuditDetail{Classes: classes},
		},
		Apply: func(ctx context.Context, tx *store.Tx) (provisioning, error) {
			return committedIn(ctx, tx)
		},
	})
	if err != nil {
		return fmt.Errorf("hub: record the completed export: %w", err)
	}
	return nil
}

// recordKeyExportRejected audits a token the export refused (FR-019). The row
// carries the problem code the client received and never the token or its hash.
func (h *Hub) recordKeyExportRejected(ctx context.Context, req KeyExportRequest, unusable bool) error {
	auditReason, eventReason := exportRejection(unusable)
	_, err := h.worker.Submit(ctx, Command[provisioning]{
		Audit: store.AuditEntry{
			Action:        store.AuditKeysExportRejected,
			Outcome:       store.OutcomeRejected,
			ActorAddr:     req.ActorAddr,
			ActorLabel:    req.ActorLabel,
			RequestID:     req.RequestID,
			CoordinatorID: h.committedState().activeID,
			Detail:        store.AuditDetail{Reason: auditReason},
		},
		Apply: func(ctx context.Context, tx *store.Tx) (provisioning, error) {
			return committedIn(ctx, tx)
		},
	})
	if err != nil {
		return fmt.Errorf("hub: record the refused export: %w", err)
	}
	h.emitKeyExport(ctx, req, &event.KeysExportRejected{Reason: eventReason})
	return nil
}

// exportRejection names one refused token twice: the audit row carries the
// problem code the client received, while the event carries the closed reason
// set of contracts/events.md §3.5. The store cannot tell an expired token from a
// consumed one, so both read as expired.
func exportRejection(unusable bool) (auditReason, eventReason string) {
	if unusable {
		return "confirmation_expired", "expired"
	}
	return "confirmation_invalid", "invalid"
}

// emitKeyExport reports one export event (contracts/events.md §3.5). The events
// carry the classes, the network identifier and the reason, never a value.
func (h *Hub) emitKeyExport(ctx context.Context, req KeyExportRequest, ev event.Event) {
	if h.emitter == nil {
		return
	}
	h.emitter.Emit(ctx, ev, event.Correlation{
		RequestID:     req.RequestID,
		ActorAddr:     req.ActorAddr,
		ActorLabel:    req.ActorLabel,
		CoordinatorID: h.committedState().activeID,
	})
}
