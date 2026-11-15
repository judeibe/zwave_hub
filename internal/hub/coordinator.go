package hub

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/event"
	"github.com/judeibe/zwave_hub/internal/store"
)

// Connection is a coordinator's connection state (FR-005, data-model.md §8.1).
type Connection string

const (
	// ConnectionNotAttached means no device with the selected identity is
	// present.
	ConnectionNotAttached Connection = "not_attached"
	// ConnectionConnecting means the driver is starting for the device.
	ConnectionConnecting Connection = "connecting"
	// ConnectionReady means the driver is listening and the network is known.
	ConnectionReady Connection = "ready"
	// ConnectionRetrying means the last start failed and backoff is running.
	ConnectionRetrying Connection = "retrying"
	// ConnectionFaulted means retrying has paused until the device set changes.
	ConnectionFaulted Connection = "faulted"
)

// Network is the membership the driver reported for one coordinator. The hub
// retains the last report whatever the connection state, because the FR-003
// gate compares a replacement's home ID before it is selected; the API presents
// it only while that coordinator's connection is ready (contracts/openapi.yaml
// Coordinator.network).
type Network struct {
	// HomeID is the network identifier as 0x followed by eight hex digits.
	HomeID string
	// NodeCount and SecureNodeCount are what start_listening reported; the
	// secure count is what gates a coordinator switch (FR-003).
	NodeCount       int
	SecureNodeCount int
}

// Coordinator is the hub's view of one device: its persisted identity and
// verdict, merged with the discovery overlay and what the driver last
// reported. A device that has been unplugged stays in the list with
// Attached false so its identity, verdict and selection survive (FR-004).
type Coordinator struct {
	// ID is the derived identity; it is never supplied by a client.
	ID           string
	VID          string
	PID          string
	Serial       string
	IdentityKind coordinator.IdentityKind
	Manufacturer string
	Product      string
	// Support and SupportReason are the durable verdict (data-model.md §2).
	// While that verdict is unverified, SupportReason falls back to the cause
	// discovery found this poll (not_passed_through, permission_denied,
	// ambiguous_identity), because otherwise a device the operator cannot use
	// would report no cause at all.
	Support       coordinator.Support
	SupportReason coordinator.Reason
	// LastPath is the durable last known path, kept across unplugging.
	LastPath string

	// Attached, Path, PathKind, Connection, Selectable and Network are
	// transient: they come from the current discovery poll and the driver.
	Attached   bool
	Path       string
	PathKind   coordinator.PathKind
	Connection Connection
	Selectable bool
	Network    *Network

	// Controller is the Z-Wave module identity the driver reported; nil until
	// identification has run. NextRetryAt and LastError describe the latest
	// failed start and are cleared by a ready report.
	Controller  *store.ControllerIdentity
	NextRetryAt *time.Time
	LastError   *LastError
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// LastError is the cause of the latest failed driver start. Message is a fixed
// sentence, never child output.
type LastError struct {
	Code    string
	Message string
	At      time.Time
}

// Snapshot is an immutable view of the coordinator domain. Workers publish it
// as a value, so readers may keep it; nothing in it is mutated after publish.
type Snapshot struct {
	// Coordinators is every known device, sorted by ID, including the ones
	// that are currently unplugged.
	Coordinators []Coordinator
	// ActiveID is the selected coordinator's identity, or empty when nothing
	// is selected. A selection whose device is later reported unsupported is
	// detached: the identity stays persisted for the audit trail but no
	// coordinator is active.
	ActiveID string
	// HomeID is the network identifier recorded in the provisioning row.
	HomeID string
	// KeyRevision is the committed key revision the gate of FR-003 consults.
	KeyRevision int
}

// Coordinator returns the named coordinator of the snapshot and whether the
// snapshot holds it. It is the one lookup of a coordinator by identity: the
// hub, the API and the driver bridge all ask the same question.
func (s Snapshot) Coordinator(id string) (Coordinator, bool) {
	for _, c := range s.Coordinators {
		if c.ID == id {
			return c, true
		}
	}
	return Coordinator{}, false
}

// Restore returns the coordinator the driver should run for this snapshot: the
// committed selection, matched against the devices discovery has reported. It is
// empty when nothing is selected or when the selected device is not attached,
// which is the awaiting_coordinator the API reports (FR-015, FR-016).
//
// The selection is bound to the identity, never to a path, so a device that
// returns — at the same path or at another one — is the same coordinator here
// (FR-004). Nothing in this method changes the selection.
func (s Snapshot) Restore() (Coordinator, bool) {
	active, ok := s.Coordinator(s.ActiveID)
	if !ok || !active.Attached || active.Support == coordinator.SupportUnsupported {
		return Coordinator{}, false
	}
	return active, true
}

// SelectionCode is the machine-readable cause of a refused selection. The
// values are the `code` of the coordinator.rejected event
// (contracts/events.md §3.3) plus coordinator_not_found; the API maps them to
// problem codes.
type SelectionCode string

const (
	// CodeCoordinatorNotFound means no device with that identity is known.
	CodeCoordinatorNotFound SelectionCode = "coordinator_not_found"
	// CodeCoordinatorUnsupported means the device is not a coordinator.
	CodeCoordinatorUnsupported SelectionCode = "coordinator_unsupported"
	// CodeCoordinatorNetworkMismatch means the replacement reports a different
	// home ID than the stored one while secure nodes exist (FR-003).
	CodeCoordinatorNetworkMismatch SelectionCode = "coordinator_network_mismatch"
	// CodeAmbiguousIdentity means another attached device claims the identity.
	CodeAmbiguousIdentity SelectionCode = "ambiguous_identity"
)

// SelectionError is a refused selection. The refusal itself committed — its
// audit row is durable and the state is unchanged — so Select returns it as an
// error for the API to render, not as a failed command.
type SelectionError struct {
	// Code is the refusal cause.
	Code SelectionCode
	// ExpectedHomeID and ActualHomeID are set for a network mismatch, so the
	// audit row and the problem response can name both.
	ExpectedHomeID string
	ActualHomeID   string
}

// Error makes SelectionError an error.
func (e *SelectionError) Error() string {
	return "hub: coordinator selection refused: " + string(e.Code)
}

// SelectionRequest is one operator selection. RequestID, ActorAddr and
// ActorLabel are recorded in the audit trail unverified (FR-019); the hub does
// not authenticate them.
type SelectionRequest struct {
	// CoordinatorID is the identity the client selected.
	CoordinatorID string
	// RequestID correlates the audit row with the event and the response.
	RequestID string
	// ActorAddr is the requesting client's address, port stripped.
	ActorAddr string
	// ActorLabel is the self-declared label, at most 64 characters.
	ActorLabel string
}

// provisioning is the committed state the command queue owns. It is not
// exported: readers see Snapshot, which merges it with the transient overlay.
type provisioning struct {
	activeID    string
	homeID      string
	keyRevision int
}

// knownCoordinator is one coordinator as the hub holds it: the durable row plus
// the transient facts discovery and the driver have reported since it was read.
type knownCoordinator struct {
	row     store.CoordinatorRow
	overlay overlay
}

// overlay is what the current poll and the driver know about one coordinator.
// None of it is persisted: identity, support and the selection are the durable
// state (data-model.md §2).
type overlay struct {
	attached   bool
	path       string
	pathKind   coordinator.PathKind
	ambiguous  bool
	reason     coordinator.Reason
	connection Connection
	network    *Network

	nextRetryAt *time.Time
	lastError   *LastError
}

// snapshot builds the published view of one coordinator.
func (k *knownCoordinator) snapshot() Coordinator {
	support := coordinator.Support(k.row.Support)
	reason := coordinator.Reason(k.row.SupportReason)
	if support == coordinator.SupportUnverified && k.overlay.reason != "" {
		reason = k.overlay.reason
	}
	connection := k.overlay.connection
	if connection == "" {
		connection = ConnectionNotAttached
	}
	var network *Network
	if k.overlay.network != nil {
		held := *k.overlay.network
		network = &held
	}
	return Coordinator{
		ID:            k.row.ID,
		VID:           k.row.VID,
		PID:           k.row.PID,
		Serial:        k.row.Serial,
		IdentityKind:  coordinator.IdentityKind(k.row.IdentityKind),
		Manufacturer:  k.row.Manufacturer,
		Product:       k.row.Product,
		Support:       support,
		SupportReason: reason,
		LastPath:      k.row.LastPath,
		Attached:      k.overlay.attached,
		Path:          k.overlay.path,
		PathKind:      k.overlay.pathKind,
		Connection:    connection,
		// The rule of coordinator.Selectable, applied to the merged view: an
		// unverified device is selectable because activation identifies it.
		Selectable: k.overlay.attached && !k.overlay.ambiguous &&
			(support == coordinator.SupportSupported || support == coordinator.SupportUnverified),
		Network: network,

		Controller:  controllerOf(k.row),
		NextRetryAt: k.overlay.nextRetryAt,
		LastError:   k.overlay.lastError,
		FirstSeenAt: k.row.FirstSeenAt,
		LastSeenAt:  k.row.LastSeenAt,
	}
}

// Hub is the coordinator domain: the committed selection, the discovery
// overlay and the rules that decide which selection is allowed. Every change
// to the committed state goes through the command queue; discovery and
// identification write the overlay and the coordinator rows directly, because
// neither is an audited configuration change (data-model.md §7) — except for
// the selection a failed identification detaches, which is committed state and
// therefore commits through the queue with the verdict.
type Hub struct {
	store   *store.Store
	emitter *event.Emitter
	worker  *Worker[provisioning]

	// mu guards known. It is never held across a store call, so a command
	// reading the overlay inside its transaction never waits on a discovery
	// poll writing the read model.
	mu    sync.RWMutex
	known map[string]*knownCoordinator
}

// selectionAttempts bounds how often Select rebuilds its decision when a
// concurrent change moved the state it decided on. A real race needs one
// retry; three leaves room for a second driver report arriving meanwhile.
const selectionAttempts = 3

// errSelectionMoved aborts a selection command whose decision no longer holds
// against the state it commits into. It rolls the command back unanswered,
// audit row included, and Select decides again from the new state.
var errSelectionMoved = errors.New("hub: coordinator selection state moved")

// New returns a hub over st, seeded with the coordinators and the committed
// selection the store already holds, so a restart lists what it knew before.
// It does not start the command worker; call Run once. A nil emitter disables
// events, which is what tests use.
func New(st *store.Store, emitter *event.Emitter) (*Hub, error) {
	if st == nil {
		return nil, errors.New("hub: store is nil")
	}
	// New has no context because its signature is fixed by its callers; the
	// two reads are short and happen before Run.
	ctx := context.Background()
	rows, err := st.ListCoordinators(ctx)
	if err != nil {
		return nil, err
	}
	provisioningRow, err := st.GetProvisioning(ctx)
	if err != nil {
		return nil, err
	}

	h := &Hub{
		store:   st,
		emitter: emitter,
		worker:  NewWorker[provisioning](st),
		known:   make(map[string]*knownCoordinator, len(rows)),
	}
	for _, row := range rows {
		h.known[row.ID] = &knownCoordinator{row: row}
	}
	h.worker.publish(provisioning{
		activeID:    provisioningRow.ActiveCoordinatorID,
		homeID:      provisioningRow.HomeID,
		keyRevision: provisioningRow.KeyRevision,
	})
	return h, nil
}

// Run consumes selection commands until ctx is cancelled. It must be called
// exactly once, from the goroutine that owns the hub's lifetime.
func (h *Hub) Run(ctx context.Context) {
	h.worker.Run(ctx)
}

// Snapshot returns the current view without waiting behind a command. The
// committed part is empty until a selection is made and no coordinator row
// exists; the coordinator list is the seeded rows merged with the overlay, so
// an unplugged device stays in it with Attached false.
func (h *Hub) Snapshot() Snapshot {
	state, _ := h.worker.Snapshot()
	coordinators := make([]Coordinator, 0, len(h.known))
	h.mu.RLock()
	for _, k := range h.known {
		coordinators = append(coordinators, k.snapshot())
	}
	h.mu.RUnlock()
	slices.SortFunc(coordinators, func(a, b Coordinator) int { return strings.Compare(a.ID, b.ID) })
	return Snapshot{
		Coordinators: coordinators,
		ActiveID:     state.activeID,
		HomeID:       state.homeID,
		KeyRevision:  state.keyRevision,
	}
}

// committedState is the last published provisioning state. The worker is the
// only writer, so a value read here is a committed one.
func (h *Hub) committedState() provisioning {
	state, _ := h.worker.Snapshot()
	return state
}

// Select commits req through the command queue: the provisioning row and the
// audit row land in one transaction, and the snapshot is published only after
// the commit. It returns a *SelectionError for a refusal, with the state
// unchanged, and a wrapped queue or store error for a real failure.
//
// The decision is built twice: once to know which audit action the command
// carries, and again inside the transaction against the state it commits into.
// Agreement is what makes the audit row and the state change consistent; a
// disagreement rolls the command back and Select decides again (FR-003, FR-021).
func (h *Hub) Select(ctx context.Context, req SelectionRequest) (Snapshot, error) {
	for attempt := 0; attempt < selectionAttempts; attempt++ {
		refusal := h.decide(req.CoordinatorID, h.committedState())
		audit := selectionAudit(req, refusal)

		var previous string
		_, err := h.worker.Submit(ctx, Command[provisioning]{
			Audit: audit,
			Apply: func(ctx context.Context, tx *store.Tx) (provisioning, error) {
				current, err := committedIn(ctx, tx)
				if err != nil {
					return provisioning{}, err
				}
				if again := h.decide(req.CoordinatorID, current); !sameRefusal(refusal, again) {
					return provisioning{}, errSelectionMoved
				}
				previous = current.activeID
				if refusal != nil || current.activeID == req.CoordinatorID {
					return current, nil
				}
				if err := tx.SetActiveCoordinator(ctx, req.CoordinatorID); err != nil {
					return provisioning{}, err
				}
				current.activeID = req.CoordinatorID
				return current, nil
			},
		})
		switch {
		case errors.Is(err, errSelectionMoved):
			continue
		case err != nil:
			return h.Snapshot(), err
		}

		h.emitSelection(ctx, req, refusal, previous)
		if refusal != nil {
			return h.Snapshot(), refusal
		}
		return h.Snapshot(), nil
	}
	return h.Snapshot(), errors.New("hub: coordinator selection kept changing while committing")
}

// decide returns the refusal a selection of id meets against state, or nil when
// it is allowed. The order matters: a device that is already active is
// re-selectable even while unplugged (FR-004), so the idempotent case is
// checked before attachment; and an unsupported device is refused even if it
// were somehow still selected (FR-003).
func (h *Hub) decide(id string, state provisioning) *SelectionError {
	h.mu.RLock()
	target, known := h.known[id]
	var (
		attached, ambiguous bool
		support             string
		actualHomeID        string
		secureNodeCount     int
	)
	if known {
		attached = target.overlay.attached
		ambiguous = target.overlay.ambiguous
		support = target.row.Support
		if target.overlay.network != nil {
			actualHomeID = target.overlay.network.HomeID
		}
	}
	if active, ok := h.known[state.activeID]; ok && active.overlay.network != nil {
		secureNodeCount = active.overlay.network.SecureNodeCount
	}
	h.mu.RUnlock()

	switch {
	case !known:
		return &SelectionError{Code: CodeCoordinatorNotFound}
	case ambiguous:
		return &SelectionError{Code: CodeAmbiguousIdentity}
	case support == string(coordinator.SupportUnsupported):
		return &SelectionError{Code: CodeCoordinatorUnsupported}
	case state.activeID == id:
		return nil
	case !attached:
		return &SelectionError{Code: CodeCoordinatorNotFound}
	}

	// FR-003: a switch is gated while the network holds securely included
	// devices or committed keys exist. The first selection is not, because the
	// hub learns its network from the coordinator it adopts (data-model.md §9.2).
	gated := state.activeID != "" && (secureNodeCount > 0 || state.keyRevision > 0)
	if gated && actualHomeID != state.homeID {
		return &SelectionError{
			Code:           CodeCoordinatorNetworkMismatch,
			ExpectedHomeID: state.homeID,
			ActualHomeID:   actualHomeID,
		}
	}
	return nil
}

// sameRefusal reports whether two decisions agree. SelectionError holds only
// comparable fields, so the comparison covers the code and both home IDs.
func sameRefusal(a, b *SelectionError) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// committedIn reads the provisioning row on tx and converts it to the state the
// queue publishes. Reading it through the transaction, rather than trusting the
// last published value, is what makes the decision and the commit one step.
func committedIn(ctx context.Context, tx *store.Tx) (provisioning, error) {
	row, err := tx.GetProvisioning(ctx)
	if err != nil {
		return provisioning{}, err
	}
	return provisioning{
		activeID:    row.ActiveCoordinatorID,
		homeID:      row.HomeID,
		keyRevision: row.KeyRevision,
	}, nil
}

// selectionAudit builds the trail row the command writes: a committed selection
// for an accepted request, a refusal carrying its cause for a refused one. A
// mismatch names both identifiers so the operator can act without digging
// (data-model.md §7).
func selectionAudit(req SelectionRequest, refusal *SelectionError) store.AuditEntry {
	entry := store.AuditEntry{
		Action:        store.AuditCoordinatorSelected,
		Outcome:       store.OutcomeSucceeded,
		ActorAddr:     req.ActorAddr,
		ActorLabel:    req.ActorLabel,
		RequestID:     req.RequestID,
		CoordinatorID: req.CoordinatorID,
	}
	if refusal == nil {
		return entry
	}
	entry.Action = store.AuditCoordinatorRejected
	entry.Outcome = store.OutcomeRejected
	entry.Detail = store.AuditDetail{
		Reason:         string(refusal.Code),
		ExpectedHomeID: refusal.ExpectedHomeID,
		ActualHomeID:   refusal.ActualHomeID,
	}
	return entry
}

// emitSelection reports a committed selection or a committed refusal on the
// event stream. A not-found request is not in the coordinator.rejected
// catalogue (contracts/events.md §3.3), so it is audited but not emitted.
func (h *Hub) emitSelection(ctx context.Context, req SelectionRequest, refusal *SelectionError, previous string) {
	if h.emitter == nil {
		return
	}
	correlation := event.Correlation{
		RequestID:     req.RequestID,
		ActorAddr:     req.ActorAddr,
		ActorLabel:    req.ActorLabel,
		CoordinatorID: req.CoordinatorID,
	}
	if refusal != nil {
		if refusal.Code == CodeCoordinatorNotFound {
			return
		}
		h.emitter.Emit(ctx, &event.CoordinatorRejected{Code: string(refusal.Code)}, correlation)
		return
	}
	var previousID *string
	if previous != "" {
		held := previous
		previousID = &held
	}
	h.emitter.Emit(ctx, &event.CoordinatorSelected{
		PreviousCoordinatorID: previousID,
		Reason:                "operator",
	}, correlation)
}

// SyncDiscovery merges one discovery poll: new devices are added, seen devices
// get their last path and last-seen time refreshed, and devices that are gone
// stay in the list with Attached false. It reports discovered and removed
// events and never changes the selection (FR-004, FR-016).
func (h *Hub) SyncDiscovery(ctx context.Context, devices []coordinator.Device) error {
	now := time.Now()
	records := make([]store.DiscoveredCoordinator, len(devices))
	for i, device := range devices {
		vid, pid := identityParts(device.Identity.ID)
		records[i] = store.DiscoveredCoordinator{
			ID:           device.Identity.ID,
			VID:          vid,
			PID:          pid,
			Serial:       device.Identity.Serial,
			IdentityKind: string(device.Identity.Kind),
			Manufacturer: device.Manufacturer,
			Product:      device.Product,
			Path:         device.Path,
			SeenAt:       now,
		}
	}

	// The rows and the read model are refreshed in one transaction, so a poll
	// never leaves half a device set behind.
	rows := make([]store.CoordinatorRow, len(records))
	fresh := make([]bool, len(records))
	err := h.store.WithTx(ctx, func(tx *store.Tx) error {
		for i := range records {
			isNew, err := tx.UpsertCoordinator(ctx, records[i])
			if err != nil {
				return err
			}
			fresh[i] = isNew
			row, err := tx.GetCoordinator(ctx, records[i].ID)
			if err != nil {
				return err
			}
			rows[i] = row
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("hub: record discovered coordinators: %w", err)
	}

	var (
		discovered []store.DiscoveredCoordinator
		removed    []string
	)
	h.mu.Lock()
	present := make(map[string]struct{}, len(devices))
	for i, device := range devices {
		id := device.Identity.ID
		present[id] = struct{}{}

		known, ok := h.known[id]
		if !ok {
			known = &knownCoordinator{row: rows[i]}
			h.known[id] = known
		} else {
			// The row this poll read can predate a verdict that committed
			// while the transaction ran; the verdict is the newer fact and
			// RecordVerdict owns it, so only the descriptive fields are taken.
			support, reason := known.row.Support, known.row.SupportReason
			known.row = rows[i]
			known.row.Support, known.row.SupportReason = support, reason
		}
		wasAttached := known.overlay.attached
		known.overlay.attached = device.Attached
		known.overlay.ambiguous = device.Ambiguous
		known.overlay.reason = device.Reason
		if device.Attached {
			known.overlay.path = device.Path
			known.overlay.pathKind = device.PathKind
		} else {
			known.overlay.path = ""
			known.overlay.pathKind = ""
		}
		if fresh[i] {
			discovered = append(discovered, records[i])
		}
		if wasAttached && !device.Attached {
			removed = append(removed, known.row.LastPath)
		}
	}
	// A device absent from the poll is unplugged, not forgotten: the row and
	// the selection stay, and only the transient attachment is cleared.
	for id, known := range h.known {
		if _, ok := present[id]; ok || !known.overlay.attached {
			continue
		}
		known.overlay.attached = false
		known.overlay.path = ""
		known.overlay.pathKind = ""
		removed = append(removed, known.row.LastPath)
	}
	h.mu.Unlock()

	if h.emitter == nil {
		return nil
	}
	for _, record := range discovered {
		h.emitter.Emit(ctx, &event.CoordinatorDiscovered{
			IdentityKind: record.IdentityKind,
			VID:          record.VID,
			PID:          record.PID,
			SerialPath:   record.Path,
		}, event.Correlation{CoordinatorID: record.ID})
	}
	for _, lastPath := range removed {
		h.emitter.Emit(ctx, &event.CoordinatorRemoved{LastPath: lastPath}, event.Correlation{})
	}
	return nil
}

// identityParts recovers the vendor and product ids from a resolved identity,
// so the row records exactly the hex the id is bound to. A resolved identity is
// always usb:<vid>:<pid>:<rest>; anything else yields empty parts, which the
// store refuses.
func identityParts(id string) (vid, pid string) {
	parts := strings.SplitN(id, ":", 4)
	if len(parts) != 4 || parts[0] != "usb" {
		return "", ""
	}
	return parts[1], parts[2]
}

// SetConnection records what the driver reported for one coordinator. A ready
// report of the active coordinator with a home ID also stores it in the
// provisioning row, which is what the FR-003 gate compares a replacement
// against, and runs the first-activation key generation when the controller
// reports an empty network (FR-008). The last network stays on record whatever
// the connection state, so the gate can judge a replacement before it is
// selected. It is called by the driver adapter, never by a client.
func (h *Hub) SetConnection(ctx context.Context, id string, conn Connection, network *Network) error {
	switch conn {
	case ConnectionNotAttached, ConnectionConnecting, ConnectionReady, ConnectionRetrying, ConnectionFaulted:
	default:
		return fmt.Errorf("hub: %q is not a coordinator connection state", conn)
	}
	var held *Network
	if network != nil {
		copied := *network
		held = &copied
	}

	h.mu.Lock()
	target, ok := h.known[id]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("hub: record a connection for unknown coordinator %s: %w", id, store.ErrNotFound)
	}
	target.overlay.connection = conn
	if held != nil {
		target.overlay.network = held
	}
	h.mu.Unlock()

	if conn != ConnectionReady || held == nil || held.HomeID == "" {
		return nil
	}
	err := h.worker.mutate(ctx, func(ctx context.Context, tx *store.Tx, current provisioning) (provisioning, error) {
		if current.activeID != id || current.homeID == held.HomeID {
			return current, nil
		}
		if err := tx.SetHomeID(ctx, held.HomeID); err != nil {
			return current, err
		}
		current.homeID = held.HomeID
		return current, nil
	})
	if err != nil {
		return fmt.Errorf("hub: record the network of %s: %w", id, err)
	}
	if err := h.generateOnActivation(ctx, id, held.NodeCount); err != nil {
		return fmt.Errorf("hub: generate the first-activation keys of %s: %w", id, err)
	}
	return nil
}

// Verdict is the driver's identification result for one coordinator. A false
// Supported makes the verdict final: support moves unverified → unsupported
// and never back (data-model.md §2).
type Verdict struct {
	// Supported reports that the device answered the Serial API as a Z-Wave
	// controller.
	Supported bool
	// Reason is the FR-006 cause when Supported is false.
	Reason coordinator.Reason
}

// RecordVerdict moves a coordinator from unverified to supported or
// unsupported. A supported verdict is never downgraded by a later failed open;
// an unsupported verdict on the active coordinator detaches the selection and
// reports the cause (FR-003). It is called by the driver adapter, never by a
// client.
//
// The verdict and the detach land in one transaction: identification is not an
// audited configuration change (data-model.md §7), but clearing the selection
// is committed state, so it goes through the queue with the verdict rather than
// beside it.
func (h *Hub) RecordVerdict(ctx context.Context, id string, verdict Verdict) error {
	if verdict.Supported && verdict.Reason != "" {
		return errors.New("hub: a supported verdict carries no reason")
	}
	if !verdict.Supported && verdict.Reason == "" {
		return errors.New("hub: an unsupported verdict needs a reason")
	}
	target := string(coordinator.SupportUnsupported)
	if verdict.Supported {
		target = string(coordinator.SupportSupported)
	}

	var (
		from, to string
		changed  bool
	)
	err := h.worker.mutate(ctx, func(ctx context.Context, tx *store.Tx, current provisioning) (provisioning, error) {
		row, err := tx.GetCoordinator(ctx, id)
		if err != nil {
			return current, fmt.Errorf("hub: read coordinator %s: %w", id, err)
		}
		from = row.Support
		stored, moved, err := tx.SetSupport(ctx, id, target, string(verdict.Reason))
		if err != nil {
			return current, err
		}
		to, changed = stored, moved
		if stored == string(coordinator.SupportUnsupported) && current.activeID == id {
			if err := tx.SetActiveCoordinator(ctx, ""); err != nil {
				return current, err
			}
			current.activeID = ""
		}
		return current, nil
	})
	if err != nil {
		return fmt.Errorf("hub: record the verdict for %s: %w", id, err)
	}

	if !changed {
		return nil
	}
	h.mu.Lock()
	if known, ok := h.known[id]; ok {
		known.row.Support = to
		known.row.SupportReason = string(verdict.Reason)
	}
	h.mu.Unlock()

	if h.emitter != nil {
		h.emitter.Emit(ctx, &event.CoordinatorSupportChanged{
			From:   from,
			To:     to,
			Reason: string(verdict.Reason),
		}, event.Correlation{CoordinatorID: id})
	}
	return nil
}
