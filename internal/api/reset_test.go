package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/hub"
	"github.com/judeibe/zwave_hub/internal/store"
)

// This file drives quickstart.md §4 scenario 10 through the HTTP surface: a key
// change refused while secure devices exist, a two-step reset that changes
// nothing until it is confirmed, and the unprovisioned state it leaves behind
// (FR-014, FR-017, FR-019).

// resetTestDriver stands in for the sidecar the reset runs through. HardReset
// only reports that the request was accepted; ControllerState is the network
// the confirmation polls, so an empty one is what a completed reset leaves
// (contracts/zwave-server-sidecar.md §6).
type resetTestDriver struct {
	network hub.Network
	resets  int
}

func (d *resetTestDriver) HardReset(context.Context) error {
	d.resets++
	return nil
}

func (d *resetTestDriver) ControllerState(context.Context) (hub.Network, error) {
	return d.network, nil
}

// resetTestSecureNetwork is a network of one non-secure node and three securely
// included ones, which is what makes every configured class in use.
var resetTestSecureNetwork = hub.Network{HomeID: "0x0badcafe", NodeCount: 4, SecureNodeCount: 3}

// resetTestKeys is a classic set whose classes hold distinct values, because a
// set with a repeated value is refused before the gate is reached.
var resetTestKeys = map[string]string{
	hub.SecurityClassS0Legacy:          "ffeeddccbbaa99887766554433221100",
	hub.SecurityClassS2Unauthenticated: "00112233445566778899aabbccddeeff",
	hub.SecurityClassS2Authenticated:   "0f1e2d3c4b5a69788796a5b4c3d2e1f0",
	hub.SecurityClassS2AccessControl:   "a1b2c3d4e5f60718293a4b5c6d7e8f90",
}

// resetTestProvision selects a coordinator, configures the classic keys and then
// has the driver report a network holding secure devices, which is the order
// that leaves the keys in use: configured first, reported second, as a hub
// that was provisioned before the devices were included would be.
func resetTestProvision(t *testing.T, f *handlerFixture) (coordinatorID string, driver *resetTestDriver) {
	t.Helper()
	ctx := context.Background()
	dev := device("reset01")
	if err := f.hub.SyncDiscovery(ctx, []coordinator.Device{dev}); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if rec := f.do("PUT", "/api/v1/active-coordinator", `{"coordinator_id":"`+dev.Identity.ID+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("select status %d: %s", rec.Code, rec.Body)
	}
	if rec := f.do("PUT", "/api/v1/keys", keysTestSubmissionBody(t, "", resetTestKeys)); rec.Code != http.StatusOK {
		t.Fatalf("submit status %d: %s", rec.Code, rec.Body)
	}
	if err := f.hub.Adopt(ctx, dev.Identity.ID, hub.Identified{Network: resetTestSecureNetwork}); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	// The controller reports no network once the reset has run.
	driver = &resetTestDriver{}
	f.hub.SetNetworkDriver(driver)
	return dev.Identity.ID, driver
}

// resetTestAuditActions lists the actions on the trail, oldest first, so a test
// can assert an order.
func resetTestAuditActions(t *testing.T, f *handlerFixture) []store.AuditEntry {
	t.Helper()
	page, err := f.store.ListAudit(context.Background(), store.AuditQuery{Limit: 200})
	if err != nil {
		t.Fatalf("list the audit trail: %v", err)
	}
	entries := make([]store.AuditEntry, len(page.Items))
	for i, entry := range page.Items {
		entries[len(page.Items)-1-i] = entry
	}
	return entries
}

// TestResetRefusesKeyChangeWhileSecureNodesExist proves the gate half of
// scenario 10: replacing or regenerating a class in use is 409 key_in_use, names
// the reset path and the classes, writes nothing and leaves a rejected audit row
// (FR-014, FR-019).
func TestResetRefusesKeyChangeWhileSecureNodesExist(t *testing.T) {
	f := newHandlerFixture(t)
	resetTestProvision(t, f)
	before := keysTestStoredKeys(t, context.Background(), f)
	revision := keysTestIntMember(t, "keys before the refusal", decode(t, f.do("GET", "/api/v1/keys", "")), "revision")

	const replacement = "f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff"
	replace := f.do("PUT", "/api/v1/keys", keysTestSubmissionBody(t, "", map[string]string{hub.SecurityClassS0Legacy: replacement}))
	if replace.Code != http.StatusConflict {
		t.Fatalf("replacing a key in use: status %d, want 409: %s", replace.Code, replace.Body)
	}
	body := decode(t, replace)
	if body["code"] != string(CodeKeyInUse) {
		t.Errorf("replacing a key in use: code %v, want %s", body["code"], CodeKeyInUse)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "/api/v1/network/reset") {
		t.Errorf("replacing a key in use: detail %q does not name the reset path", detail)
	}
	if strings.Contains(strings.ToLower(replace.Body.String()), replacement) {
		t.Error("the refusal body carries the value that was sent")
	}

	generate := f.do("POST", "/api/v1/keys/generate", keysTestGenerationBody(t, []string{hub.SecurityClassS0Legacy}))
	if generate.Code != http.StatusConflict || decode(t, generate)["code"] != string(CodeKeyInUse) {
		t.Errorf("regenerating a key in use: status %d body %s, want 409 key_in_use", generate.Code, generate.Body)
	}

	keysTestAssertSameStoredKeys(t, "after both refusals", before, keysTestStoredKeys(t, context.Background(), f))
	if got := keysTestIntMember(t, "keys after the refusals", decode(t, f.do("GET", "/api/v1/keys", "")), "revision"); got != revision {
		t.Errorf("the key revision moved from %d to %d across two refusals", revision, got)
	}

	rejected := 0
	for _, entry := range resetTestAuditActions(t, f) {
		if entry.Action != store.AuditKeysRejected {
			continue
		}
		rejected++
		if entry.Outcome != store.OutcomeRejected {
			t.Errorf("a keys.rejected row has outcome %s, want %s", entry.Outcome, store.OutcomeRejected)
		}
	}
	if rejected != 2 {
		t.Errorf("the trail holds %d keys.rejected rows, want 2 (one per refusal)", rejected)
	}
}

// TestResetNeedsTheTwoStepConfirmationAndLeavesUnprovisioned proves the reset
// half of scenario 10: the first step changes nothing, the second returns the
// hub to unprovisioned with the coordinator still selected, the token is single
// use, and readiness is false afterwards (FR-017, FR-013).
func TestResetNeedsTheTwoStepConfirmationAndLeavesUnprovisioned(t *testing.T) {
	f := newHandlerFixture(t)
	coordinatorID, driver := resetTestProvision(t, f)

	before := decode(t, f.do("GET", "/api/v1/network", ""))
	if before["state"] == "unprovisioned" || before["home_id"] != resetTestSecureNetwork.HomeID {
		t.Fatalf("the network before the reset = %v, want it provisioned on %s", before, resetTestSecureNetwork.HomeID)
	}

	// Step one: a token, and nothing performed.
	challenge := f.do("POST", "/api/v1/network/reset", "")
	if challenge.Code != http.StatusAccepted {
		t.Fatalf("reset step one status %d, want 202: %s", challenge.Code, challenge.Body)
	}
	challengeBody := decode(t, challenge)
	token, _ := challengeBody["token"].(string)
	if token == "" || challengeBody["operation"] != "network.reset" {
		t.Fatalf("reset step one body = %v, want a token for network.reset", challengeBody)
	}
	if driver.resets != 0 {
		t.Fatalf("step one asked the driver to reset %d times, want none", driver.resets)
	}
	afterStepOne := decode(t, f.do("GET", "/api/v1/network", ""))
	if afterStepOne["state"] != before["state"] || afterStepOne["home_id"] != before["home_id"] {
		t.Errorf("step one changed the network from %v to %v", before, afterStepOne)
	}

	// A token nobody issued is 404 and does not spend the real one.
	if rec := f.do("POST", "/api/v1/network/reset", `{"confirm_token":"not-a-real-token"}`); rec.Code != http.StatusNotFound {
		t.Errorf("reset with an unknown token status %d, want 404", rec.Code)
	} else if code := decode(t, rec)["code"]; code != string(CodeConfirmationInvalid) {
		t.Errorf("reset with an unknown token code %v, want %s", code, CodeConfirmationInvalid)
	}
	if driver.resets != 0 {
		t.Fatalf("a bad token asked the driver to reset %d times, want none", driver.resets)
	}

	// Step two: the reset.
	done := f.do("POST", "/api/v1/network/reset", `{"confirm_token":"`+token+`"}`)
	if done.Code != http.StatusOK {
		t.Fatalf("reset step two status %d, want 200: %s", done.Code, done.Body)
	}
	result := decode(t, done)
	if result["state"] != "unprovisioned" || result["coordinator_id"] != coordinatorID || result["home_id_before"] != resetTestSecureNetwork.HomeID {
		t.Errorf("reset result = %v, want unprovisioned on %s having cleared %s", result, coordinatorID, resetTestSecureNetwork.HomeID)
	}
	if cleared, _ := result["cleared"].(map[string]any); cleared["keys"] != true || cleared["network_membership"] != true {
		t.Errorf("reset cleared = %v, want keys and network_membership", result["cleared"])
	}
	if driver.resets != 1 {
		t.Errorf("the driver was asked to reset %d times, want once", driver.resets)
	}

	// Observable post-conditions (FR-017).
	after := decode(t, f.do("GET", "/api/v1/network", ""))
	if after["state"] != "unprovisioned" || after["ready_for_secure_inclusion"] != false {
		t.Errorf("the network after the reset = %v, want unprovisioned and not ready", after)
	}
	if after["home_id"] != nil {
		t.Errorf("home_id after the reset = %v, want null", after["home_id"])
	}
	if after["active_coordinator_id"] != coordinatorID {
		t.Errorf("active_coordinator_id after the reset = %v, want the selected %s", after["active_coordinator_id"], coordinatorID)
	}
	keys, _ := after["keys"].(map[string]any)
	if classes, _ := keys["configured_classes"].([]any); len(classes) != 0 {
		t.Errorf("configured classes after the reset = %v, want none", classes)
	}
	if revision := keysTestIntMember(t, "network keys after the reset", keys, "revision"); revision != 0 {
		t.Errorf("key revision after the reset = %d, want 0", revision)
	}
	if rec := f.do("GET", "/api/v1/active-coordinator", ""); rec.Code != http.StatusOK {
		t.Errorf("active coordinator after the reset status %d, want 200 (selection is kept)", rec.Code)
	}
	if got := keysTestStoredKeys(t, context.Background(), f); len(got) != 0 {
		t.Errorf("the store holds %d key rows after the reset, want none", len(got))
	}

	// The token is single use.
	replay := f.do("POST", "/api/v1/network/reset", `{"confirm_token":"`+token+`"}`)
	if replay.Code != http.StatusGone {
		t.Errorf("reset replay status %d, want 410", replay.Code)
	} else if code := decode(t, replay)["code"]; code != string(CodeConfirmationExpired) {
		t.Errorf("reset replay code %v, want %s", code, CodeConfirmationExpired)
	}
	if driver.resets != 1 {
		t.Errorf("a replayed token asked the driver to reset again: %d resets, want 1", driver.resets)
	}

	// With the network gone, nothing is in use any more, so the keys the gate
	// refused a moment ago can now be supplied (FR-018).
	if rec := f.do("PUT", "/api/v1/keys", keysTestSubmissionBody(t, "", map[string]string{hub.SecurityClassS0Legacy: resetTestKeys[hub.SecurityClassS0Legacy]})); rec.Code != http.StatusOK {
		t.Errorf("supplying a key after the reset status %d, want 200: %s", rec.Code, rec.Body)
	}

	// The audit trail holds the request, the refused presentations and the reset,
	// in that order, and none of it carries the token.
	var actions []store.AuditAction
	for _, entry := range resetTestAuditActions(t, f) {
		switch entry.Action {
		case store.AuditNetworkResetRequested, store.AuditNetworkResetRejected, store.AuditNetworkReset:
			actions = append(actions, entry.Action)
			if strings.Contains(entry.ActorAddr+entry.ActorLabel+entry.RequestID, token) {
				t.Errorf("audit row %s carries the confirmation token", entry.Action)
			}
		}
	}
	want := []store.AuditAction{
		store.AuditNetworkResetRequested,
		store.AuditNetworkResetRejected,
		store.AuditNetworkReset,
		store.AuditNetworkResetRejected,
	}
	if len(actions) != len(want) {
		t.Fatalf("reset audit actions = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Errorf("reset audit action %d = %s, want %s (all: %v)", i, actions[i], want[i], actions)
		}
	}
}

// TestResetIsRefusedWhenDestructiveOpsAreDisabled proves an operator can close
// the reset without a code change: both steps answer 503
// destructive_ops_disabled and nothing is requested, reset or audited.
func TestResetIsRefusedWhenDestructiveOpsAreDisabled(t *testing.T) {
	f := newHandlerFixture(t)
	_, driver := resetTestProvision(t, f)

	closed := NewRouter(Options{})
	NewConfirmations(f.hub, false).Mount(closed)
	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/network/reset", strings.NewReader(body))
		req.RemoteAddr = "192.0.2.10:5555"
		rec := httptest.NewRecorder()
		closed.ServeHTTP(rec, req)
		return rec
	}
	auditBefore := len(resetTestAuditActions(t, f))

	for name, body := range map[string]string{
		"step one": "",
		"step two": `{"confirm_token":"anything"}`,
	} {
		rec := do(body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with destructive operations disabled: status %d, want 503", name, rec.Code)
			continue
		}
		if code := decode(t, rec)["code"]; code != string(CodeDestructiveOpsDisabled) {
			t.Errorf("%s with destructive operations disabled: code %v, want %s", name, code, CodeDestructiveOpsDisabled)
		}
	}
	if driver.resets != 0 {
		t.Errorf("a disabled reset asked the driver to reset %d times, want none", driver.resets)
	}
	if got := len(resetTestAuditActions(t, f)); got != auditBefore {
		t.Errorf("a disabled reset wrote %d audit rows, want none", got-auditBefore)
	}
	if status := decode(t, f.do("GET", "/api/v1/network", "")); status["home_id"] != resetTestSecureNetwork.HomeID {
		t.Errorf("the network after a disabled reset = %v, want it untouched", status)
	}
}
