package hub

import (
	"context"
	"errors"

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

// Hub is the coordinator domain: the committed selection, the discovery
// overlay and the rules that decide which selection is allowed. Every change
// to the committed state goes through the command queue; discovery and
// identification update the overlay and the durable rows directly, because
// neither is an audited configuration change (data-model.md §7).
type Hub struct {
	store   *store.Store
	emitter *event.Emitter
	worker  *Worker[provisioning]
}

// New returns a hub over st. It does not start the command worker; call Run
// once. A nil emitter disables events, which is what tests use.
func New(st *store.Store, emitter *event.Emitter) (*Hub, error) {
	if st == nil {
		return nil, errors.New("hub: store is nil")
	}
	return &Hub{
		store:   st,
		emitter: emitter,
		worker:  NewWorker[provisioning](st),
	}, nil
}

// Run consumes selection commands until ctx is cancelled. It must be called
// exactly once, from the goroutine that owns the hub's lifetime.
func (h *Hub) Run(ctx context.Context) {
	h.worker.Run(ctx)
}

// Snapshot returns the current view without waiting behind a command. The
// committed part may be empty before the first command commits.
func (h *Hub) Snapshot() Snapshot {
	state, ok := h.worker.Snapshot()
	if !ok {
		return Snapshot{}
	}
	return Snapshot{
		ActiveID:    state.activeID,
		HomeID:      state.homeID,
		KeyRevision: state.keyRevision,
	}
}

// Select commits req through the command queue: the provisioning row and the
// audit row land in one transaction, and the snapshot is published only after
// the commit. It returns a *SelectionError for a refusal, with the state
// unchanged, and a wrapped queue or store error for a real failure.
func (h *Hub) Select(ctx context.Context, req SelectionRequest) (Snapshot, error) {
	// T024 fills in the selection rules.
	return h.Snapshot(), errors.New("hub: coordinator selection is not implemented")
}

// SyncDiscovery merges one discovery poll: new devices are added, seen devices
// get their last path and last-seen time refreshed, and devices that are gone
// stay in the list with Attached false. It reports discovered, removed and
// support-changed events and never changes the selection (FR-004, FR-016).
func (h *Hub) SyncDiscovery(ctx context.Context, devices []coordinator.Device) error {
	return errors.New("hub: discovery sync is not implemented")
}

// SetConnection records what the driver reported for one coordinator. A ready
// report with a home ID also stores it in the provisioning row, which is what
// the FR-003 gate compares a replacement against. It is called by the driver
// adapter, never by a client.
func (h *Hub) SetConnection(ctx context.Context, id string, conn Connection, network *Network) error {
	return errors.New("hub: connection report is not implemented")
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
func (h *Hub) RecordVerdict(ctx context.Context, id string, verdict Verdict) error {
	return errors.New("hub: identification verdict is not implemented")
}
