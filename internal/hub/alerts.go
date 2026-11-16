package hub

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/judeibe/zwave_hub/internal/event"
)

// The two alert triggers of contracts/events.md §5 that are derived from state
// rather than from an operation: network.state_changed feeds
// coordinator_unreachable, and keys.corrupt is key_store_corrupt. Neither
// belongs to one command, so both are detected where the state is derived and
// by Monitor, which derives it on a timer so the alerts do not depend on a
// client happening to read GET /network.

// stateUnknown is the `from` of the first observation after start-up. A hub
// that boots already faulted must still emit a transition into faulted, or the
// coordinator_unreachable rule would never see it.
const stateUnknown = "unknown"

// stateReasonReady is the `reason` of a transition into ready, which has no
// blocking reason to name.
const stateReasonReady = "ready"

// alertWatch remembers what has already been reported, so a steady state is
// not re-announced every time it is derived.
type alertWatch struct {
	mu sync.Mutex
	// state is the last emitted network state, empty before the first
	// observation.
	state NetworkState
	// corrupt is the last reported corrupt class set, joined; empty means the
	// store was last seen healthy.
	corrupt string
}

// observeNetwork emits network.state_changed when the derived state differs
// from the last one observed. The mutex is held across the emit so two
// concurrent derivations cannot publish their transitions out of order.
func (h *Hub) observeNetwork(ctx context.Context, status NetworkStatus) {
	if h.emitter == nil {
		return
	}
	h.alerts.mu.Lock()
	defer h.alerts.mu.Unlock()

	from := string(h.alerts.state)
	if h.alerts.state == status.State {
		return
	}
	if from == "" {
		from = stateUnknown
	}
	h.alerts.state = status.State
	h.emitter.Emit(ctx, &event.NetworkStateChanged{
		From:   from,
		To:     string(status.State),
		Reason: stateReason(status),
	}, event.Correlation{CoordinatorID: status.ActiveID, HomeID: status.HomeID})
}

// stateReason names why the network is in its state: the first blocking reason,
// which is the one to act on first (data-model.md §8.3), or ready.
func stateReason(status NetworkStatus) string {
	if len(status.Readiness.BlockingReasons) == 0 {
		return stateReasonReady
	}
	return string(status.Readiness.BlockingReasons[0].Code)
}

// reportKeyStore emits keys.corrupt when the set of classes failing
// authentication is new, and forgets it when none fail, so a corruption that is
// repaired and then recurs alerts again. Re-reporting the same set on every
// read would turn a one-occurrence alert into a flood.
//
// found is the complete result of a check over every stored blob. detectedBy is
// aead, because an authentication failure is how every corrupt row here is
// found; a damaged database file is refused at open (store.integrity_failed).
func (h *Hub) reportKeyStore(ctx context.Context, found []string) {
	if h.emitter == nil {
		return
	}
	classes := canonicalClasses(found)
	joined := strings.Join(classes, ",")

	h.alerts.mu.Lock()
	defer h.alerts.mu.Unlock()
	if joined == h.alerts.corrupt {
		return
	}
	h.alerts.corrupt = joined
	if len(classes) == 0 {
		return
	}
	h.emitter.Emit(ctx, &event.KeysCorrupt{
		Classes:    slices.Clone(classes),
		DetectedBy: "aead",
	}, event.Correlation{CoordinatorID: h.committedState().activeID})
}

// Monitor derives the network status every interval until ctx ends, which is
// what emits network.state_changed and keys.corrupt without any client asking.
// It runs one derivation at once, so a hub that starts faulted or with a
// corrupt key store alerts at start-up instead of one interval later.
func (h *Hub) Monitor(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		// A failed derivation is a store failure the caller of NetworkStatus
		// would see; here there is nobody to tell, and the next tick retries.
		_, _ = h.NetworkStatus(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
