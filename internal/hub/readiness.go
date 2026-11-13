package hub

import "slices"

// KeyStoreState is the health of the encrypted key store (data-model.md §3).
// It is judged as a whole: one blob that fails authentication makes the set
// corrupt, never just one class (R14 §2).
type KeyStoreState string

const (
	// KeyStoreOK means every stored blob authenticated.
	KeyStoreOK KeyStoreState = "ok"
	// KeyStoreCorrupt means at least one stored blob failed authentication.
	KeyStoreCorrupt KeyStoreState = "corrupt"
)

// BlockingCode is one reason the network is not ready for secure inclusion.
// The set and its order are fixed by data-model.md §8.3 and
// contracts/openapi.yaml; a client switches on the code.
type BlockingCode string

// The eight blocking reasons, in the order DeriveReadiness emits them.
const (
	// BlockingKeyStoreCorrupt means a stored key cannot be authenticated.
	BlockingKeyStoreCorrupt BlockingCode = "key_store_corrupt"
	// BlockingNoActiveCoordinator means no coordinator has been selected.
	BlockingNoActiveCoordinator BlockingCode = "no_active_coordinator"
	// BlockingCoordinatorNotAttached means the selected stick is unplugged or
	// its driver has not started.
	BlockingCoordinatorNotAttached BlockingCode = "coordinator_not_attached"
	// BlockingCoordinatorFaulted means the start path has paused.
	BlockingCoordinatorFaulted BlockingCode = "coordinator_faulted"
	// BlockingNetworkMismatch means the coordinator reports a network other than
	// the hub's.
	BlockingNetworkMismatch BlockingCode = "network_mismatch"
	// BlockingExistingNetworkDetected means the coordinator carries nodes while
	// no key revision has ever been committed (FR-018).
	BlockingExistingNetworkDetected BlockingCode = "existing_network_detected"
	// BlockingKeysMissing means a classic class has no key configured.
	BlockingKeysMissing BlockingCode = "keys_missing"
	// BlockingKeysNotInEffect means the stored revision is not the one running.
	BlockingKeysNotInEffect BlockingCode = "keys_not_in_effect"
)

// BlockingReason is one emitted reason, carrying the values a client needs to
// act on it. Only the fields the code calls for are set; Message is always a
// fixed sentence so a caller with no detail fields still has something to show.
type BlockingReason struct {
	Code    BlockingCode
	Message string

	// Classes names the security classes the reason concerns (key store
	// corruption, missing keys). It never carries a value.
	Classes []string
	// Reason is the fault cause for coordinator_faulted (FR-006).
	Reason string
	// LastPath is the by-id path of the selected stick for
	// coordinator_not_attached, so a client can name what to plug back in.
	LastPath string
	// HomeID and NodeCount describe the network found for
	// existing_network_detected (FR-018).
	HomeID    string
	NodeCount int
	// ExpectedHomeID is the hub's committed network and ActualHomeID the one
	// the coordinator reported, for network_mismatch (FR-003).
	ExpectedHomeID string
	ActualHomeID   string
	// KeyRevision and AppliedRevision are the two revisions of
	// keys_not_in_effect; AppliedRevision is nil when nothing was ever applied.
	KeyRevision     int
	AppliedRevision *int
}

// ReadinessInput is everything the readiness predicate reads: the key store's
// health, the selected coordinator and its connection, the network both sides
// believe they are on, and the committed key state. Every field is a committed
// value or a driver report, never a client request.
type ReadinessInput struct {
	// KeyStore is the health of the stored keys.
	KeyStore KeyStoreState
	// CorruptClasses names the classes that failed authentication when
	// KeyStore is KeyStoreCorrupt.
	CorruptClasses []string

	// ActiveID is the selected coordinator, empty when none is.
	ActiveID string
	// Attached reports that a device with the selected identity is present,
	// and Connection is what its driver last reported.
	Attached   bool
	Connection Connection
	// LastPath is the selected coordinator's last by-id path.
	LastPath string
	// FaultReason is the FR-006 cause when Connection is ConnectionFaulted.
	FaultReason string

	// HomeID is the network the hub committed, and ReportedHomeID the one the
	// coordinator last reported.
	HomeID         string
	ReportedHomeID string
	// NodeCount is the coordinator's node count.
	NodeCount int

	// KeyRevision is the committed revision and AppliedRevision the one the
	// driver is running, nil when none has been applied.
	KeyRevision     int
	AppliedRevision *int
	// Configured lists the security classes that have a stored key.
	Configured []string
}

// Readiness is the result of the derivation: ready exactly when no reason was
// emitted, and the reasons in their fixed order otherwise.
type Readiness struct {
	Ready           bool
	BlockingReasons []BlockingReason
}

// DeriveReadiness decides whether the network can accept a securely included
// device and, when it cannot, why (data-model.md §8.3, SC-002). The reasons are
// independent — several can hold at once — and are emitted in the fixed order
// so the first one is the one to act on.
//
// Readiness needs all of: a healthy key store, a selected coordinator that is
// attached and ready, one network both sides agree on, no pre-existing network
// the hub has no keys for, the four classic classes configured, and the stored
// revision in effect.
func DeriveReadiness(in ReadinessInput) Readiness {
	reasons := make([]BlockingReason, 0, 8)

	if in.KeyStore == KeyStoreCorrupt {
		reasons = append(reasons, BlockingReason{
			Code:    BlockingKeyStoreCorrupt,
			Message: "The stored keys cannot be authenticated; recover the key store before including any device.",
			Classes: slices.Clone(in.CorruptClasses),
		})
	}

	if in.ActiveID == "" {
		reasons = append(reasons, BlockingReason{
			Code:    BlockingNoActiveCoordinator,
			Message: "No coordinator is selected.",
		})
	} else {
		switch {
		case !in.Attached:
			reasons = append(reasons, BlockingReason{
				Code:     BlockingCoordinatorNotAttached,
				Message:  "The selected coordinator is not attached.",
				LastPath: in.LastPath,
			})
		case in.Connection == ConnectionFaulted:
			reasons = append(reasons, BlockingReason{
				Code:    BlockingCoordinatorFaulted,
				Message: "The coordinator's start path is faulted.",
				Reason:  in.FaultReason,
			})
		case in.Connection != ConnectionReady:
			reasons = append(reasons, BlockingReason{
				Code:     BlockingCoordinatorNotAttached,
				Message:  "The selected coordinator is not running yet.",
				LastPath: in.LastPath,
			})
		}
	}

	if in.HomeID != "" && in.ReportedHomeID != "" && in.HomeID != in.ReportedHomeID {
		reasons = append(reasons, BlockingReason{
			Code:           BlockingNetworkMismatch,
			Message:        "The coordinator reports a different network than the hub's.",
			ExpectedHomeID: in.HomeID,
			ActualHomeID:   in.ReportedHomeID,
		})
	}

	// A stick is reported, never adopted, when it carries nodes while no key
	// revision has ever been committed: generating keys for it would mean
	// nothing to its devices, and adopting it would hide the other network
	// instead of reporting it (FR-018, R15).
	homeID := in.HomeID
	if homeID == "" {
		homeID = in.ReportedHomeID
	}
	if in.KeyRevision == 0 && in.NodeCount > 0 && homeID != "" {
		reasons = append(reasons, BlockingReason{
			Code:      BlockingExistingNetworkDetected,
			Message:   "The coordinator carries an existing network the hub has no keys for.",
			HomeID:    homeID,
			NodeCount: in.NodeCount,
		})
	}

	if missing := MissingClassicClasses(in.Configured); len(missing) > 0 {
		reasons = append(reasons, BlockingReason{
			Code:    BlockingKeysMissing,
			Message: "The classic key set is incomplete.",
			Classes: missing,
		})
	}

	if !keysInEffect(in.KeyRevision, in.AppliedRevision) {
		reasons = append(reasons, BlockingReason{
			Code:            BlockingKeysNotInEffect,
			Message:         "The stored key revision is not running yet.",
			KeyRevision:     in.KeyRevision,
			AppliedRevision: in.AppliedRevision,
		})
	}

	return Readiness{Ready: len(reasons) == 0, BlockingReasons: reasons}
}

// keysInEffect reports whether the stored revision is the one the driver is
// running. A nil applied revision is in effect only when nothing has ever been
// committed: there is no stored change waiting to be applied (data-model.md §5).
func keysInEffect(revision int, applied *int) bool {
	if applied == nil {
		return revision == 0
	}
	return *applied == revision
}

// ClassicComplete reports whether configured holds every classic class,
// repeats allowed. The classic four are the base key set readiness is judged on;
// the Long Range pair never stands in for them (FR-007, FR-008, SC-002).
func ClassicComplete(configured []string) bool {
	return len(MissingClassicClasses(configured)) == 0
}

// MissingClassicClasses returns the classic classes configured lacks, in
// canonical order, or nil when the set is complete.
func MissingClassicClasses(configured []string) []string {
	var missing []string
	for _, class := range ClassicClasses() {
		if !slices.Contains(configured, class) {
			missing = append(missing, class)
		}
	}
	return missing
}
