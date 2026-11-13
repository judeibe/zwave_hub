package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/judeibe/zwave_hub/internal/store"
)

// resetTestHub starts a hub in the state a reset begins from: one discovered and
// selected coordinator, two configured classic classes, a recorded home id and an
// applied revision. It returns the hub, the store, the selected coordinator's id
// and the home id on record.
func resetTestHub(t *testing.T) (*Hub, *store.Store, string, string) {
	t.Helper()
	h, st := newTestHub(t)

	dev := testDevice("ttyUSB0", "reset", "1-2.3")
	mustSync(t, h, dev)
	mustSelect(t, h, dev.Identity.ID, "req-select-before-reset")

	cfg, err := h.SubmitKeys(context.Background(), KeySubmission{
		Keys: map[string]string{
			SecurityClassS0Legacy:        resetKeyValue("reset-s0"),
			SecurityClassS2Authenticated: resetKeyValue("reset-s2"),
		},
		RequestID: "req-keys-before-reset",
		ActorAddr: testActorAddr,
	})
	if err != nil {
		t.Fatalf("submit the keys the reset must clear: %v", err)
	}
	if cfg.Revision == 0 {
		t.Fatalf("the key submission committed revision 0, so there is nothing for a reset to clear")
	}

	ctx := context.Background()
	const homeID = "0xdeadbeef"
	if err := st.SetHomeID(ctx, homeID); err != nil {
		t.Fatalf("record the home id the reset must clear: %v", err)
	}
	applied := cfg.Revision
	if err := st.SetAppliedRevision(ctx, &applied); err != nil {
		t.Fatalf("record the applied revision the reset must clear: %v", err)
	}
	return h, st, dev.Identity.ID, homeID
}

// resetKeyValue returns a 32-character lowercase hex value that is distinct per
// seed: ValidateKeys refuses two classes of one set holding the same value, so
// seeding a set needs one of these per class (data-model.md §3).
func resetKeyValue(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}

// fakeResetDriver stands in for the sidecar. HardReset only reports that the
// request was accepted; ControllerState is what the confirmation polls, so the
// network it holds decides whether the reset can ever complete
// (contracts/zwave-server-sidecar.md §6).
type fakeResetDriver struct {
	// network is the controller state every poll sees.
	network Network
	// resets counts the hard-reset requests the hub made.
	resets int
}

// HardReset records the request and accepts it. A nil error means only that the
// driver took the request, never that the network is gone.
func (d *fakeResetDriver) HardReset(_ context.Context) error {
	d.resets++
	return nil
}

// ControllerState reports the state the test posed.
func (d *fakeResetDriver) ControllerState(_ context.Context) (Network, error) {
	return d.network, nil
}

// TestResetConfirmedClearsState proves a confirmed reset leaves the hub
// unprovisioned: every key row is gone, the home id and both revisions are
// cleared, and the completion is audited (FR-017, data-model.md §9.1).
func TestResetConfirmedClearsState(t *testing.T) {
	h, st, id, _ := resetTestHub(t)
	// The controller reports no home id and no nodes, which is what confirms a
	// completed reset (contracts/zwave-server-sidecar.md §6).
	driver := &fakeResetDriver{network: Network{}}
	h.SetNetworkDriver(driver)

	const requestID = "req-reset-confirmed"
	result, err := h.Reset(context.Background(), ResetRequest{
		RequestID: requestID,
		ActorAddr: testActorAddr,
	})
	if err != nil {
		t.Fatalf("Reset returned %v, want a completed reset", err)
	}

	// The driver is asked first: its answer only starts the confirmation, so a
	// hub that never asked it cannot have completed a reset.
	if driver.resets != 1 {
		t.Errorf("the hub asked the driver to hard-reset %d times, want once", driver.resets)
	}
	if result.State != ResetStateUnprovisioned {
		t.Errorf("the reset reports state %q, want %q", result.State, ResetStateUnprovisioned)
	}
	if !result.KeysCleared {
		t.Errorf("the reset reports KeysCleared false, want the key rows cleared")
	}
	if !result.NetworkMembershipCleared {
		t.Errorf("the reset reports NetworkMembershipCleared false, want the network membership cleared")
	}
	if result.CoordinatorID != id {
		t.Errorf("the reset reports coordinator %q, want the selected %s", result.CoordinatorID, id)
	}

	// The store is what an operator observes after a restart: no key in any
	// class, no home id, revision zero and no applied revision (FR-017).
	ctx := context.Background()
	keys, err := st.ListKeys(ctx)
	if err != nil {
		t.Fatalf("list the key rows: %v", err)
	}
	if len(keys) != 0 {
		names := make([]string, 0, len(keys))
		for _, k := range keys {
			names = append(names, k.SecurityClass)
		}
		t.Errorf("the store holds %d key rows after a confirmed reset (%v), want none", len(keys), names)
	}

	prov, err := st.GetProvisioning(ctx)
	if err != nil {
		t.Fatalf("read the provisioning row: %v", err)
	}
	if prov.HomeID != "" {
		t.Errorf("the provisioning row records home id %q after a confirmed reset, want none", prov.HomeID)
	}
	if prov.KeyRevision != 0 {
		t.Errorf("the provisioning row has key revision %d after a confirmed reset, want 0", prov.KeyRevision)
	}
	if prov.AppliedRevision != nil {
		t.Errorf("the provisioning row has applied revision %d after a confirmed reset, want NULL", *prov.AppliedRevision)
	}

	// A reset clears keys and network membership, not the choice of stick: the
	// coordinator stays selected (FR-017).
	if prov.ActiveCoordinatorID != id {
		t.Errorf("the provisioning row has active coordinator %q after a confirmed reset, want the selected %s", prov.ActiveCoordinatorID, id)
	}
	if got := h.Snapshot().ActiveID; got != id {
		t.Errorf("the snapshot has active coordinator %q after a confirmed reset, want the selected %s", got, id)
	}

	// Clearing is a configuration change, so its completion is on the trail
	// (FR-019, data-model.md §7).
	rows := auditRows(t, st)
	if len(rows) == 0 {
		t.Fatal("the audit trail is empty, want a network.reset row")
	}
	row := rows[0]
	if row.Action != store.AuditNetworkReset {
		t.Errorf("the newest audit row is %s, want %s", row.Action, store.AuditNetworkReset)
	}
	if row.Outcome != store.OutcomeSucceeded {
		t.Errorf("the newest audit row has outcome %s, want %s", row.Outcome, store.OutcomeSucceeded)
	}
	if row.RequestID != requestID {
		t.Errorf("the newest audit row carries request %q, want %q", row.RequestID, requestID)
	}
	if row.ActorAddr != testActorAddr {
		t.Errorf("the newest audit row records actor %q, want %q", row.ActorAddr, testActorAddr)
	}
}

// TestResetUnconfirmedLeavesState proves a reset the driver never confirms
// changes nothing: the deadline is reported as a timeout, every stored value
// stays as it was, and the failure is audited. The reset may still land on the
// stick after the deadline, so the hub must not pretend it did (FR-017,
// contracts/zwave-server-sidecar.md §6).
func TestResetUnconfirmedLeavesState(t *testing.T) {
	h, st, id, homeID := resetTestHub(t)
	// The controller keeps its network, so the confirmation polls until the
	// deadline and never sees the empty state a completed reset leaves.
	driver := &fakeResetDriver{network: Network{HomeID: homeID, NodeCount: 4}}
	h.SetNetworkDriver(driver)

	ctx := context.Background()
	before, err := st.GetProvisioning(ctx)
	if err != nil {
		t.Fatalf("read the provisioning row before the reset: %v", err)
	}
	if before.HomeID != homeID || before.KeyRevision == 0 || before.AppliedRevision == nil {
		t.Fatalf("the reset test state is not seeded: %+v", before)
	}
	beforeKeys, err := st.ListKeys(ctx)
	if err != nil {
		t.Fatalf("list the key rows before the reset: %v", err)
	}

	// A short deadline bounds the wait: the reset ends when the caller gives up
	// rather than running for the full ResetTimeout.
	resetCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	const requestID = "req-reset-timeout"
	_, err = h.Reset(resetCtx, ResetRequest{
		RequestID: requestID,
		ActorAddr: testActorAddr,
	})

	var failure *ResetDriverFailure
	if !errors.As(err, &failure) {
		t.Fatalf("Reset returned %v, want a *ResetDriverFailure", err)
	}
	if failure.Reason != "timeout" {
		t.Errorf("the failure reason is %q, want timeout", failure.Reason)
	}
	if driver.resets != 1 {
		t.Errorf("the hub asked the driver to hard-reset %d times, want once", driver.resets)
	}

	// Nothing is cleared: the confirmation never arrived, so the stored state
	// still describes the network the controller carries.
	after, err := st.GetProvisioning(ctx)
	if err != nil {
		t.Fatalf("read the provisioning row after the failed reset: %v", err)
	}
	if after.HomeID != before.HomeID {
		t.Errorf("the provisioning row records home id %q after a failed reset, want the unchanged %q", after.HomeID, before.HomeID)
	}
	if after.KeyRevision != before.KeyRevision {
		t.Errorf("the provisioning row has key revision %d after a failed reset, want the unchanged %d", after.KeyRevision, before.KeyRevision)
	}
	if after.AppliedRevision == nil {
		t.Errorf("the provisioning row lost its applied revision after a failed reset, want the unchanged %d", *before.AppliedRevision)
	} else if *after.AppliedRevision != *before.AppliedRevision {
		t.Errorf("the provisioning row has applied revision %d after a failed reset, want the unchanged %d", *after.AppliedRevision, *before.AppliedRevision)
	}
	if after.ActiveCoordinatorID != id {
		t.Errorf("the provisioning row has active coordinator %q after a failed reset, want the unchanged %s", after.ActiveCoordinatorID, id)
	}
	if got := h.Snapshot().ActiveID; got != id {
		t.Errorf("the snapshot has active coordinator %q after a failed reset, want the unchanged %s", got, id)
	}

	afterKeys, err := st.ListKeys(ctx)
	if err != nil {
		t.Fatalf("list the key rows after the failed reset: %v", err)
	}
	if len(afterKeys) != len(beforeKeys) {
		t.Fatalf("the store holds %d key rows after a failed reset, want the unchanged %d", len(afterKeys), len(beforeKeys))
	}
	for i := range beforeKeys {
		if afterKeys[i].SecurityClass != beforeKeys[i].SecurityClass {
			t.Errorf("key row %d is %s after a failed reset, want the unchanged %s", i, afterKeys[i].SecurityClass, beforeKeys[i].SecurityClass)
		}
	}

	// A reset the driver did not finish is still an audited attempt (FR-019,
	// contracts/events.md §3.6).
	rows := auditRows(t, st)
	if len(rows) == 0 {
		t.Fatal("the audit trail is empty, want a network.reset_failed row")
	}
	row := rows[0]
	if row.Action != store.AuditNetworkResetFailed {
		t.Errorf("the newest audit row is %s, want %s", row.Action, store.AuditNetworkResetFailed)
	}
	if row.Outcome != store.OutcomeFailed {
		t.Errorf("the newest audit row has outcome %s, want %s", row.Outcome, store.OutcomeFailed)
	}
	if row.RequestID != requestID {
		t.Errorf("the newest audit row carries request %q, want %q", row.RequestID, requestID)
	}
	if row.ActorAddr != testActorAddr {
		t.Errorf("the newest audit row records actor %q, want %q", row.ActorAddr, testActorAddr)
	}
}

// TestResetWithoutSelectionRefused proves a reset with nothing selected is
// refused before anything runs: the hub reports ErrNoActiveCoordinator and
// writes neither state nor an audit row (FR-017).
func TestResetWithoutSelectionRefused(t *testing.T) {
	h, st := newTestHub(t)
	// A driver that would confirm is attached, so only the missing selection
	// can be the reason for the refusal.
	driver := &fakeResetDriver{network: Network{}}
	h.SetNetworkDriver(driver)

	ctx := context.Background()
	before, err := st.GetProvisioning(ctx)
	if err != nil {
		t.Fatalf("read the provisioning row before the reset: %v", err)
	}
	if before.ActiveCoordinatorID != "" {
		t.Fatalf("the reset test state has active coordinator %q, want none", before.ActiveCoordinatorID)
	}
	beforeKeys, err := st.ListKeys(ctx)
	if err != nil {
		t.Fatalf("list the key rows before the reset: %v", err)
	}
	beforeRows := len(auditRows(t, st))

	_, err = h.Reset(ctx, ResetRequest{
		RequestID: "req-reset-no-selection",
		ActorAddr: testActorAddr,
	})
	if !errors.Is(err, ErrNoActiveCoordinator) {
		t.Fatalf("Reset returned %v, want ErrNoActiveCoordinator", err)
	}
	// Nothing was reset on the hardware either: there is no coordinator whose
	// network the driver could wipe.
	if driver.resets != 0 {
		t.Errorf("the hub asked the driver to hard-reset %d times with no coordinator selected, want none", driver.resets)
	}

	// A refusal before the reset runs writes nothing at all, which is what
	// keeps the trail honest about work the hub declined (FR-019).
	if got := len(auditRows(t, st)); got != beforeRows {
		t.Errorf("the audit trail grew from %d to %d rows after a refused reset, want it unchanged", beforeRows, got)
	}
	after, err := st.GetProvisioning(ctx)
	if err != nil {
		t.Fatalf("read the provisioning row after the refused reset: %v", err)
	}
	if after.ActiveCoordinatorID != before.ActiveCoordinatorID {
		t.Errorf("the provisioning row has active coordinator %q after a refused reset, want the unchanged %q", after.ActiveCoordinatorID, before.ActiveCoordinatorID)
	}
	if after.HomeID != before.HomeID {
		t.Errorf("the provisioning row records home id %q after a refused reset, want the unchanged %q", after.HomeID, before.HomeID)
	}
	if after.KeyRevision != before.KeyRevision {
		t.Errorf("the provisioning row has key revision %d after a refused reset, want the unchanged %d", after.KeyRevision, before.KeyRevision)
	}
	switch {
	case after.AppliedRevision == nil && before.AppliedRevision != nil:
		t.Errorf("the provisioning row lost its applied revision after a refused reset, want the unchanged %d", *before.AppliedRevision)
	case after.AppliedRevision != nil && before.AppliedRevision == nil:
		t.Errorf("the provisioning row gained applied revision %d after a refused reset, want none", *after.AppliedRevision)
	case after.AppliedRevision != nil && *after.AppliedRevision != *before.AppliedRevision:
		t.Errorf("the provisioning row has applied revision %d after a refused reset, want the unchanged %d", *after.AppliedRevision, *before.AppliedRevision)
	}

	afterKeys, err := st.ListKeys(ctx)
	if err != nil {
		t.Fatalf("list the key rows after the refused reset: %v", err)
	}
	if len(afterKeys) != len(beforeKeys) {
		t.Errorf("the store holds %d key rows after a refused reset, want the unchanged %d", len(afterKeys), len(beforeKeys))
	}
}
