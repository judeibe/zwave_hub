package hub

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/judeibe/zwave_hub/internal/event"
	"github.com/judeibe/zwave_hub/internal/store"
)

// KeyMode is how a submission treats the classes it does not name (FR-009).
type KeyMode string

const (
	// KeyModeMerge changes only the classes the submission names and leaves
	// every other configured class alone. It is the default.
	KeyModeMerge KeyMode = "merge"
	// KeyModeReplace writes the classes the submission names and removes every
	// configured class it does not name. It must name all four classic classes:
	// a replace that dropped one would take the base set readiness is judged on
	// away as a side effect of a change meant to repair it (FR-007, R14 §2).
	KeyModeReplace KeyMode = "replace"
)

// KeySubmission is one operator key submission (FR-009). Keys maps a security
// class to 32 hexadecimal characters; Mode is empty for merge. The audit
// identity is recorded unverified, as every audit row is (FR-019).
type KeySubmission struct {
	Mode       KeyMode
	Keys       map[string]string
	RequestID  string
	ActorAddr  string
	ActorLabel string
}

// KeyGeneration is one generation request (FR-010). An empty Classes means the
// four classic classes, because first activation provisions those alone
// (FR-008); the Long Range pair is generated only when it is named.
type KeyGeneration struct {
	Classes    []string
	RequestID  string
	ActorAddr  string
	ActorLabel string
}

// KeyClass is one security class's metadata in a KeyConfiguration. It carries
// no value, only whether the class is configured and when it last changed
// (FR-013).
type KeyClass struct {
	Class      string
	Configured bool
	Source     store.KeySource
	CreatedAt  time.Time
	ChangedAt  time.Time
}

// KeyConfiguration is the metadata view of the stored key set: the committed
// revision, whether that revision is the one running, per-class metadata and
// any non-blocking advisories. No field carries key material (FR-013).
type KeyConfiguration struct {
	Revision        int
	AppliedRevision *int
	InEffect        bool
	ClassicComplete bool
	Classes         []KeyClass
	Warnings        []string
	// Apply names why a committed revision is not running. It is nil when the
	// revision is in effect or when nothing is committed yet: there is no
	// stored change to explain (contracts/openapi.yaml KeyConfiguration.apply).
	Apply *KeyApply
}

// KeyApplyReason is why a stored key revision is not in effect. The set is
// closed by contracts/openapi.yaml.
type KeyApplyReason string

const (
	// ApplyNoCoordinator means no coordinator is selected, so nothing can run
	// the keys.
	ApplyNoCoordinator KeyApplyReason = "no_coordinator"
	// ApplyCoordinatorUnavailable means a coordinator is selected but is not
	// running, so the stored revision will be applied when it starts.
	ApplyCoordinatorUnavailable KeyApplyReason = "coordinator_unavailable"
	// ApplyCoordinatorFaulted means the start path has paused; the stored
	// revision waits for the device set to change or the hub to restart.
	ApplyCoordinatorFaulted KeyApplyReason = "coordinator_faulted"
	// ApplyDriverRestartFailed means the last apply attempt failed; the driver
	// reports the cause and the next successful start applies the revision
	// (FR-015). The driver adapter produces this reason.
	ApplyDriverRestartFailed KeyApplyReason = "driver_restart_failed"
)

// KeyApply explains a stored revision that is not running (R14 §1). It holds no
// key material.
type KeyApply struct {
	Reason  KeyApplyReason
	Message string
}

// KeyStoreCorruptError reports that one or more stored keys failed AES-GCM
// authentication, so the set cannot be read. A corrupted set is never
// regenerated over: new values would strand every securely included device.
// The three recovery paths of R14 §2 are in the message, because whoever reads
// the refusal is the one who has to choose between them.
type KeyStoreCorruptError struct {
	// Classes are the classes whose stored blob failed authentication, in
	// canonical order.
	Classes []string
}

// Error names the failed classes and the three recovery paths. It never
// contains key material (FR-013), only class names.
func (e *KeyStoreCorruptError) Error() string {
	detail := "the stored keys failed authentication"
	if len(e.Classes) > 0 {
		detail = "the stored keys for " + strings.Join(e.Classes, ", ") + " failed authentication"
	}
	return "hub: " + detail + ": restore the database from backup, re-supply the original keys, " +
		"or reset the network and re-include the devices"
}

// CodeKeyInUse is the refusal code of a key change the network's securely
// included devices make unsafe (FR-014). It is the `code` of the problem
// response and the `reason` of the audit row the refusal writes.
const CodeKeyInUse = "key_in_use"

// KeyInUseError is a refused key change: one or more of the classes it would
// replace already hold a key while the network holds securely included
// devices. Replacing such a key strands every device that was included with
// it, so the only supported path is a confirmed network reset (FR-017).
//
// The refusal writes nothing: it is a command that succeeds with outcome
// rejected and leaves the stored key set exactly as it was (FR-014, FR-019).
// It carries class names and a count only — never key material (FR-013).
type KeyInUseError struct {
	// Classes are the configured classes the change would have replaced, in
	// canonical order. A class that was never configured is not among them:
	// nothing is in use for it.
	Classes []string
	// SecureNodeCount is how many securely included devices the coordinator
	// reported.
	SecureNodeCount int
}

// Error names the classes, the count and the reset path, and nothing else:
// the text may be shown to an operator, so it carries no key material.
func (e *KeyInUseError) Error() string {
	detail := "hub: key change refused (" + CodeKeyInUse + ")"
	if len(e.Classes) > 0 {
		detail += ": the classes " + strings.Join(e.Classes, ", ") + " are in use"
	}
	if e.SecureNodeCount > 0 {
		detail += " by " + strconv.Itoa(e.SecureNodeCount) + " securely included devices"
	}
	return detail + ": reset the network (POST /api/v1/network/reset) before replacing them"
}

// The audit identity of the automatic first-activation generation. No client
// requested it (FR-008), but every key change is audited (FR-019) and the row
// has to say where the change came from; "hub" is the hub itself, not a client
// address.
const (
	activationActorAddr = "hub"
	activationRequestID = "activation"
)

// keyRequest is the audit identity common to a submission and a generation,
// so both commits and both events describe the same caller.
type keyRequest struct {
	requestID  string
	actorAddr  string
	actorLabel string
}

// keyRequest returns the audit identity of the submission.
func (r KeySubmission) keyRequest() keyRequest {
	return keyRequest{requestID: r.RequestID, actorAddr: r.ActorAddr, actorLabel: r.ActorLabel}
}

// keyRequest returns the audit identity of the generation.
func (r KeyGeneration) keyRequest() keyRequest {
	return keyRequest{requestID: r.RequestID, actorAddr: r.ActorAddr, actorLabel: r.ActorLabel}
}

// SubmitKeys validates and commits one submission: merge changes the classes it
// names, replace also removes the configured classes it does not name. The
// rows, the revision bump and the audit row land in one transaction, and the
// sealed values are the only thing written — the plaintext is wiped as soon as
// the sealing returns.
//
// A submission that would replace a key protecting an included device is
// refused before anything is written (FR-014). A class the hub holds no key for
// is not in use: supplying the matching keys of a network the hub has no keys
// for is the FR-018 path, not a replacement.
//
// Nothing here looks at the coordinator's connection state. A submission while
// the coordinator is faulted or absent is accepted and stored, and the response
// says through Apply that it is not running yet (R14 §1, §3).
func (h *Hub) SubmitKeys(ctx context.Context, req KeySubmission) (KeyConfiguration, error) {
	mode := req.Mode
	if mode == "" {
		mode = KeyModeMerge
	}
	switch mode {
	case KeyModeMerge, KeyModeReplace:
	default:
		return KeyConfiguration{}, fmt.Errorf("hub: %q is not a key submission mode", req.Mode)
	}

	decoded, warnings, err := ValidateKeys(req.Keys)
	if err != nil {
		return KeyConfiguration{}, err
	}
	defer wipeKeyValues(decoded)

	classes := classesOf(decoded)
	if mode == KeyModeReplace {
		if missing := MissingClassicClasses(classes); len(missing) > 0 {
			// The closed problem set of contracts/openapi.yaml has one 422 code
			// for a submission that cannot be stored as written; the missing
			// classes name what has to be added.
			return KeyConfiguration{}, &ValidationError{Code: CodeInvalidKeyMaterial, Classes: missing}
		}
	}
	reqID := req.keyRequest()
	if err := h.refuseInUse(ctx, reqID, classes, mode == KeyModeReplace); err != nil {
		return KeyConfiguration{}, err
	}
	blobs, err := h.sealKeys(classes, decoded)
	if err != nil {
		return KeyConfiguration{}, err
	}

	state, err := h.commit(ctx, reqID, store.AuditKeysReplaced, keyChange{
		classes: classes,
		blobs:   blobs,
		source:  store.KeySourceSupplied,
		replace: mode == KeyModeReplace,
	})
	if err != nil {
		return KeyConfiguration{}, fmt.Errorf("hub: submit keys: %w", err)
	}
	h.emitKeys(ctx, reqID, state.keyRevision, classes, store.KeySourceSupplied)
	h.applyCommitted(ctx)

	return h.keyConfiguration(ctx, warnings)
}

// GenerateKeys draws and commits new values for the requested classes,
// defaulting to the four classic classes. Generation over a store that cannot
// authenticate a stored row is refused: the operator has to recover the set
// first, because replacing it would strand every secure device (R14 §2).
//
// Like a submission, a generation that would replace a key protecting an
// included device is refused (FR-014, FR-021).
func (h *Hub) GenerateKeys(ctx context.Context, req KeyGeneration) (KeyConfiguration, error) {
	classes, err := GenerateClasses(req.Classes)
	if err != nil {
		return KeyConfiguration{}, err
	}
	if err := h.refuseCorruptStore(ctx); err != nil {
		return KeyConfiguration{}, err
	}
	reqID := req.keyRequest()
	if err := h.refuseInUse(ctx, reqID, classes, false); err != nil {
		return KeyConfiguration{}, err
	}
	values, err := GenerateKeys(classes)
	if err != nil {
		return KeyConfiguration{}, err
	}
	defer wipeKeyValues(values)
	blobs, err := h.sealKeys(classes, values)
	if err != nil {
		return KeyConfiguration{}, err
	}

	state, err := h.commit(ctx, reqID, store.AuditKeysGenerated, keyChange{
		classes: classes,
		blobs:   blobs,
		source:  store.KeySourceGenerated,
	})
	if err != nil {
		return KeyConfiguration{}, fmt.Errorf("hub: generate keys: %w", err)
	}
	h.emitKeys(ctx, reqID, state.keyRevision, classes, store.KeySourceGenerated)
	h.applyCommitted(ctx)

	return h.keyConfiguration(ctx, nil)
}

// refuseInUse applies the FR-014 gate: a change may not replace a key that
// protects an included device, because the device was included with that value
// and would be stranded. The refusal is a command that succeeds with outcome
// rejected, so it leaves the key set, the revisions and the applied revision
// exactly as they were and leaves one keys.rejected row naming the reason and
// the classes (FR-019). It returns a *KeyInUseError for the refusal, whose
// message names the reset, and a wrapped error only when the audit row itself
// could not be written.
//
// The classes it refuses are the configured ones among those the change would
// write: the named classes for a merge or a generation, and every configured
// class for a replace, since a replace deletes the configured classes it does
// not name. A class with no stored key is never refused — nothing is in use for
// it, and supplying it is how a network the hub has no keys for gets its keys
// (FR-018).
func (h *Hub) refuseInUse(ctx context.Context, req keyRequest, classes []string, replace bool) error {
	secureNodeCount := h.secureNodeCount()
	if secureNodeCount == 0 {
		return nil
	}
	stored, err := h.store.ListKeys(ctx)
	if err != nil {
		return fmt.Errorf("hub: read the configured keys: %w", err)
	}
	configured := make([]string, 0, len(stored))
	for _, key := range stored {
		configured = append(configured, key.SecurityClass)
	}
	refused := make([]string, 0, len(configured))
	for _, class := range configured {
		if replace || slices.Contains(classes, class) {
			refused = append(refused, class)
		}
	}
	refused = canonicalClasses(refused)
	if len(refused) == 0 {
		return nil
	}

	refusal := &KeyInUseError{Classes: refused, SecureNodeCount: secureNodeCount}
	_, err = h.worker.Submit(ctx, Command[provisioning]{
		Audit: store.AuditEntry{
			Action:     store.AuditKeysRejected,
			Outcome:    store.OutcomeRejected,
			ActorAddr:  req.actorAddr,
			ActorLabel: req.actorLabel,
			RequestID:  req.requestID,
			Detail: store.AuditDetail{
				Reason:  CodeKeyInUse,
				Classes: slices.Clone(refused),
			},
		},
		// A refusal writes nothing: the command carries only its audit row.
		Apply: func(ctx context.Context, tx *store.Tx) (provisioning, error) {
			return committedIn(ctx, tx)
		},
	})
	if err != nil {
		return fmt.Errorf("hub: record the refused key change: %w", err)
	}

	if h.emitter != nil {
		h.emitter.Emit(ctx, &event.KeysInUseRejected{
			Classes:         refused,
			SecureNodeCount: secureNodeCount,
		}, event.Correlation{
			RequestID:  req.requestID,
			ActorAddr:  req.actorAddr,
			ActorLabel: req.actorLabel,
		})
	}
	return refusal
}

// secureNodeCount is how many securely included devices the selected
// coordinator last reported. It is read from the overlay rather than from a
// command, because the driver's report is what carries it and a submission does
// not wait for the queue to tell it. A coordinator that has not reported yet, or
// a snapshot with no network, counts zero: nothing can be protected by a key
// the hub has not seen in use.
func (h *Hub) secureNodeCount() int {
	state := h.committedState()
	h.mu.RLock()
	defer h.mu.RUnlock()
	if known, ok := h.known[state.activeID]; ok && known.overlay.network != nil {
		return known.overlay.network.SecureNodeCount
	}
	return 0
}

// Keys returns the metadata view of the stored key set (FR-013). A row that
// fails authentication makes the whole read a refusal rather than a partial
// answer: a corrupt class is treated as corruption of the set (R14 §2).
func (h *Hub) Keys(ctx context.Context) (KeyConfiguration, error) {
	if err := h.refuseCorruptStore(ctx); err != nil {
		return KeyConfiguration{}, err
	}
	return h.keyConfiguration(ctx, nil)
}

// refuseCorruptStore checks every stored blob and returns a
// *KeyStoreCorruptError naming the classes that fail. It never returns key
// material, and an empty store is never corrupt: there is nothing to open.
func (h *Hub) refuseCorruptStore(ctx context.Context) error {
	corrupt, err := h.store.CheckKeys(ctx)
	if err != nil {
		return fmt.Errorf("hub: check the key store: %w", err)
	}
	if len(corrupt) == 0 {
		return nil
	}
	return &KeyStoreCorruptError{Classes: canonicalClasses(corrupt)}
}

// errActivationMoved aborts an automatic generation whose preconditions no
// longer hold, so two readiness reports that race cannot generate two sets. It
// rolls the command back, audit row included, and the trigger reports success
// with nothing written.
var errActivationMoved = errors.New("hub: first-activation state moved")

// generateOnActivation runs the FR-008 first-activation trigger for one ready
// report of the active coordinator. Keys are generated only when nothing is
// configured, no revision has ever been committed, and the controller reports
// zero nodes: a stick that already carries nodes keeps its network untouched
// (FR-018), and a corrupt or partial set is never regenerated over.
//
// The decision is taken twice. The preconditions are checked before anything is
// drawn, so an ordinary ready report costs one read; they are checked again
// inside the transaction, because that is the only place the check and the
// write are one step.
func (h *Hub) generateOnActivation(ctx context.Context, id string, nodeCount int) error {
	state := h.committedState()
	if state.activeID != id {
		return nil
	}
	stored, err := h.store.ListKeys(ctx)
	if err != nil {
		return fmt.Errorf("hub: read the configured keys: %w", err)
	}
	if !ShouldGenerateOnActivation(Activation{
		Connection:  ConnectionReady,
		KeyRevision: state.keyRevision,
		Configured:  len(stored),
		NodeCount:   nodeCount,
	}) {
		return nil
	}

	classes := ClassicClasses()
	values, err := GenerateKeys(classes)
	if err != nil {
		return err
	}
	defer wipeKeyValues(values)
	blobs, err := h.sealKeys(classes, values)
	if err != nil {
		return err
	}

	req := keyRequest{requestID: activationRequestID, actorAddr: activationActorAddr}
	committed, err := h.commit(ctx, req, store.AuditKeysGenerated, keyChange{
		classes: classes,
		blobs:   blobs,
		source:  store.KeySourceGenerated,
		check: func(ctx context.Context, tx *store.Tx, current provisioning) error {
			if current.activeID != id || current.keyRevision != 0 {
				return errActivationMoved
			}
			existing, err := tx.ListKeys(ctx)
			if err != nil {
				return err
			}
			if len(existing) != 0 {
				return errActivationMoved
			}
			return nil
		},
	})
	if errors.Is(err, errActivationMoved) {
		return nil
	}
	if err != nil {
		return err
	}
	h.emitKeys(ctx, req, committed.keyRevision, classes, store.KeySourceGenerated)
	h.applyCommitted(ctx)
	return nil
}

// keyChange is one accepted key change ready to commit: the classes in
// canonical order with their sealed blobs, the source that goes on every row,
// and how to treat configured classes the change does not name.
type keyChange struct {
	classes []string
	blobs   map[string][]byte
	source  store.KeySource
	replace bool
	// check, when set, runs on the transaction before anything is written and
	// may refuse the change. It is how the automatic trigger re-validates the
	// state it decided on.
	check func(ctx context.Context, tx *store.Tx, current provisioning) error
}

// commit writes change and its audit row in one command. The revision is read
// on the transaction, not from the last published snapshot, so the bump always
// lands on the revision it follows; it is bumped once per accepted change even
// when every value repeats (data-model.md §5). The sealed blobs are written;
// nothing here can read them back.
func (h *Hub) commit(ctx context.Context, req keyRequest, action store.AuditAction, change keyChange) (provisioning, error) {
	audit := store.AuditEntry{
		Action:     action,
		Outcome:    store.OutcomeSucceeded,
		ActorAddr:  req.actorAddr,
		ActorLabel: req.actorLabel,
		RequestID:  req.requestID,
		Detail: store.AuditDetail{
			Classes: slices.Clone(change.classes),
			Source:  string(change.source),
		},
	}
	return h.worker.Submit(ctx, Command[provisioning]{
		Audit: audit,
		Apply: func(ctx context.Context, tx *store.Tx) (provisioning, error) {
			current, err := committedIn(ctx, tx)
			if err != nil {
				return provisioning{}, err
			}
			if change.check != nil {
				if err := change.check(ctx, tx, current); err != nil {
					return provisioning{}, err
				}
			}
			if change.replace {
				existing, err := tx.ListKeys(ctx)
				if err != nil {
					return provisioning{}, err
				}
				for _, stored := range existing {
					if slices.Contains(change.classes, stored.SecurityClass) {
						continue
					}
					if err := tx.DeleteKey(ctx, stored.SecurityClass); err != nil {
						return provisioning{}, err
					}
				}
			}
			for _, class := range change.classes {
				if err := tx.PutKey(ctx, class, change.blobs[class], change.source, time.Time{}); err != nil {
					return provisioning{}, err
				}
			}
			revision := current.keyRevision + 1
			if err := tx.SetKeyRevision(ctx, revision); err != nil {
				return provisioning{}, err
			}
			current.keyRevision = revision
			return current, nil
		},
	})
}

// sealKeys seals one plaintext value per class. The caller wipes the plaintext
// values once this returns; the blobs are what reaches the store (R4).
func (h *Hub) sealKeys(classes []string, values map[string][]byte) (map[string][]byte, error) {
	blobs := make(map[string][]byte, len(classes))
	for _, class := range classes {
		blob, err := h.store.SealKey(class, values[class])
		if err != nil {
			return nil, fmt.Errorf("hub: seal the key for %s: %w", class, err)
		}
		blobs[class] = blob
	}
	return blobs, nil
}

// emitKeys reports a committed key change on the event stream
// (contracts/events.md §3.5). The event carries the classes and the source,
// never a value.
func (h *Hub) emitKeys(ctx context.Context, req keyRequest, revision int, classes []string, source store.KeySource) {
	if h.emitter == nil {
		return
	}
	h.emitter.Emit(ctx, &event.KeysStored{
		Revision: revision,
		Classes:  classes,
		Source:   string(source),
	}, event.Correlation{
		RequestID:  req.requestID,
		ActorAddr:  req.actorAddr,
		ActorLabel: req.actorLabel,
	})
}

// keyConfiguration reads the committed key metadata and the provisioning row in
// one transaction, so the revision and the class list describe the same commit,
// then adds the derived fields. It must not be called from inside a command:
// the command already holds the store's connection.
func (h *Hub) keyConfiguration(ctx context.Context, warnings []string) (KeyConfiguration, error) {
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
		return KeyConfiguration{}, fmt.Errorf("hub: read the key configuration: %w", err)
	}

	cfg := buildKeyConfiguration(row, keys, warnings)
	cfg.Apply = h.pendingApply(cfg, row.ActiveCoordinatorID)
	return cfg, nil
}

// buildKeyConfiguration shapes the metadata view: all six classes are always
// listed, configured or not, so a client can render the set without knowing the
// catalogue (contracts/openapi.yaml KeyConfiguration).
func buildKeyConfiguration(row store.Provisioning, keys []store.StoredKey, warnings []string) KeyConfiguration {
	configured := make(map[string]store.StoredKey, len(keys))
	names := make([]string, 0, len(keys))
	for _, stored := range keys {
		configured[stored.SecurityClass] = stored
		names = append(names, stored.SecurityClass)
	}

	cfg := KeyConfiguration{
		Revision:        row.KeyRevision,
		AppliedRevision: row.AppliedRevision,
		InEffect:        revisionInEffect(row.KeyRevision, row.AppliedRevision),
		ClassicComplete: len(MissingClassicClasses(names)) == 0,
		Classes:         make([]KeyClass, 0, len(AllClasses())),
		Warnings:        warnings,
	}
	for _, class := range AllClasses() {
		entry := KeyClass{Class: class}
		if stored, ok := configured[class]; ok {
			entry.Configured = true
			entry.Source = stored.Source
			entry.CreatedAt = stored.CreatedAt
			entry.ChangedAt = stored.ChangedAt
		}
		cfg.Classes = append(cfg.Classes, entry)
	}
	return cfg
}

// pendingApply names why a committed revision is not running. Nothing can be
// pending when the revision is in effect or when no revision was ever
// committed; otherwise the selected coordinator's connection decides between
// the four reasons (contracts/openapi.yaml KeyConfiguration.apply).
func (h *Hub) pendingApply(cfg KeyConfiguration, activeID string) *KeyApply {
	if cfg.InEffect || cfg.Revision == 0 {
		return nil
	}
	if activeID == "" {
		return &KeyApply{
			Reason:  ApplyNoCoordinator,
			Message: "No coordinator is selected, so the stored keys are not in use.",
		}
	}

	h.mu.RLock()
	known, ok := h.known[activeID]
	connection := ConnectionNotAttached
	applyFailed := false
	if ok && known.overlay.connection != "" {
		connection = known.overlay.connection
	}
	if ok {
		applyFailed = known.overlay.applyFailedRevision == cfg.Revision
	}
	h.mu.RUnlock()

	if applyFailed && connection != ConnectionFaulted {
		return &KeyApply{
			Reason:  ApplyDriverRestartFailed,
			Message: "The driver could not be restarted with the stored keys; the next successful start applies them.",
		}
	}

	if connection == ConnectionFaulted {
		return &KeyApply{
			Reason:  ApplyCoordinatorFaulted,
			Message: "The coordinator is faulted, so the stored keys are applied when it recovers.",
		}
	}
	return &KeyApply{
		Reason:  ApplyCoordinatorUnavailable,
		Message: "The coordinator is not running, so the stored keys are applied when it starts.",
	}
}

// revisionInEffect reports whether the committed revision is the one the driver
// is running (data-model.md §5): a revision is in effect only when it has been
// applied and is not zero.
func revisionInEffect(revision int, applied *int) bool {
	return revision > 0 && applied != nil && *applied == revision
}

// canonicalClasses returns the names in found in canonical order, dropping
// repeats and unknown names, so a refusal names classes the same way every
// time.
func canonicalClasses(found []string) []string {
	if len(found) == 0 {
		return nil
	}
	ordered := make([]string, 0, len(found))
	for _, class := range AllClasses() {
		if slices.Contains(found, class) {
			ordered = append(ordered, class)
		}
	}
	return ordered
}

// classesOf returns the classes of a decoded submission in canonical order.
// Every name is known by then: ValidateKeys refused the rest.
func classesOf(keys map[string][]byte) []string {
	found := make([]string, 0, len(keys))
	for class := range keys {
		found = append(found, class)
	}
	return canonicalClasses(found)
}

// wipeKeyValues zeroes decoded or drawn key material once it has been sealed,
// so a plaintext value lives only as long as sealing it takes (FR-013).
func wipeKeyValues(values map[string][]byte) {
	for _, value := range values {
		for i := range value {
			value[i] = 0
		}
	}
}
