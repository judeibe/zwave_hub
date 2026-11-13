package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/hub"
	"github.com/judeibe/zwave_hub/internal/store"
)

// TestEveryOperationWritesExactlyOneAuditRow is the FR-019 / SC-007 completeness
// check across the whole API: each mutating operation and each refused one
// leaves exactly one audit row of the expected action, outcome and actor, and a
// read leaves none. The confirmation tokens, the issued ones and the refused
// ones alike, appear on neither the trail nor the log stream.
func TestEveryOperationWritesExactlyOneAuditRow(t *testing.T) {
	f := newRedactionFixture(t)
	ctx := context.Background()

	dev := device("complete")
	if err := f.hub.SyncDiscovery(ctx, []coordinator.Device{dev}); err != nil {
		t.Fatalf("sync discovery: %v", err)
	}
	driver := &resetTestDriver{}
	f.hub.SetNetworkDriver(driver)

	count := func() int {
		t.Helper()
		page, err := f.store.ListAudit(ctx, store.AuditQuery{Limit: 500})
		if err != nil {
			t.Fatalf("list the audit trail: %v", err)
		}
		return len(page.Items)
	}
	latest := func() store.AuditEntry {
		t.Helper()
		page, err := f.store.ListAudit(ctx, store.AuditQuery{Limit: 1})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("read the newest audit row: %v (%d rows)", err, len(page.Items))
		}
		return page.Items[0]
	}

	var tokens []string
	var presented []string
	step := func(what, method, path, body string, wantStatus int, wantAction store.AuditAction, wantOutcome store.AuditOutcome) {
		t.Helper()
		before := count()
		rec := f.do(method, path, body)
		if rec.Code != wantStatus {
			t.Fatalf("%s: status %d, want %d: %s", what, rec.Code, wantStatus, rec.Body)
		}
		if got := count() - before; got != 1 {
			t.Fatalf("%s wrote %d audit rows, want exactly 1", what, got)
		}
		row := latest()
		if row.Action != wantAction || row.Outcome != wantOutcome {
			t.Errorf("%s audit row = %s/%s, want %s/%s", what, row.Action, row.Outcome, wantAction, wantOutcome)
		}
		if row.ActorAddr == "" || row.RequestID == "" {
			t.Errorf("%s audit row lacks an actor address or request id: %+v", what, row)
		}
		if token, _ := decode(t, rec)["token"].(string); token != "" {
			tokens = append(tokens, token)
		}
	}
	silent := func(what, method, path string) {
		t.Helper()
		before := count()
		if rec := f.do(method, path, ""); rec.Code >= 500 {
			t.Fatalf("%s: status %d", what, rec.Code)
		}
		if got := count() - before; got != 0 {
			t.Errorf("%s wrote %d audit rows, want none: reads are not audited", what, got)
		}
	}

	silent("GET /api/v1/keys", "GET", "/api/v1/keys")
	silent("GET /api/v1/network", "GET", "/api/v1/network")
	silent("GET /api/v1/coordinators", "GET", "/api/v1/coordinators")

	step("selecting an unknown coordinator", "PUT", "/api/v1/active-coordinator",
		`{"coordinator_id":"usb:dead:beef:none"}`, http.StatusNotFound, store.AuditCoordinatorRejected, store.OutcomeRejected)
	step("selecting a coordinator", "PUT", "/api/v1/active-coordinator",
		`{"coordinator_id":`+mustJSON(t, dev.Identity.ID)+`}`, http.StatusOK, store.AuditCoordinatorSelected, store.OutcomeSucceeded)

	if err := f.hub.Adopt(ctx, dev.Identity.ID, hub.Identified{Network: hub.Network{HomeID: "0x00000001"}}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	// Adopting a stick with no network generates the classic set; the rows that
	// produced are not part of the operations counted here.

	step("submitting a malformed key", "PUT", "/api/v1/keys",
		`{"mode":"merge","keys":{"S0_Legacy":"not-hex"}}`, http.StatusUnprocessableEntity, store.AuditKeysRejected, store.OutcomeRejected)
	step("submitting keys", "PUT", "/api/v1/keys",
		keysTestSubmissionBody(t, "merge", map[string]string{hub.SecurityClassS0Legacy: "ffeeddccbbaa99887766554433221100"}),
		http.StatusOK, store.AuditKeysReplaced, store.OutcomeSucceeded)
	step("generating keys", "POST", "/api/v1/keys/generate",
		`{"classes":["S2_Authenticated_LR"]}`, http.StatusOK, store.AuditKeysGenerated, store.OutcomeSucceeded)

	step("requesting a key export", "POST", "/api/v1/keys/export", "",
		http.StatusAccepted, store.AuditKeysExportRequested, store.OutcomeSucceeded)
	exportToken := tokens[len(tokens)-1]
	presented = append(presented, "not-a-real-export-token")
	step("exporting with an unknown token", "POST", "/api/v1/keys/export",
		`{"confirm_token":"not-a-real-export-token"}`, http.StatusNotFound, store.AuditKeysExportRejected, store.OutcomeRejected)
	step("exporting with the issued token", "POST", "/api/v1/keys/export",
		`{"confirm_token":`+mustJSON(t, exportToken)+`}`, http.StatusOK, store.AuditKeysExported, store.OutcomeSucceeded)
	step("replaying the export token", "POST", "/api/v1/keys/export",
		`{"confirm_token":`+mustJSON(t, exportToken)+`}`, http.StatusGone, store.AuditKeysExportRejected, store.OutcomeRejected)

	// Securely included devices put every configured class in use.
	if err := f.hub.Adopt(ctx, dev.Identity.ID, hub.Identified{Network: resetTestSecureNetwork}); err != nil {
		t.Fatalf("adopt with secure nodes: %v", err)
	}
	step("replacing a key in use", "PUT", "/api/v1/keys",
		keysTestSubmissionBody(t, "merge", map[string]string{hub.SecurityClassS0Legacy: "00112233445566778899aabbccddeeff"}),
		http.StatusConflict, store.AuditKeysRejected, store.OutcomeRejected)

	step("requesting a reset", "POST", "/api/v1/network/reset", "",
		http.StatusAccepted, store.AuditNetworkResetRequested, store.OutcomeSucceeded)
	resetToken := tokens[len(tokens)-1]
	presented = append(presented, "not-a-real-reset-token")
	step("resetting with an unknown token", "POST", "/api/v1/network/reset",
		`{"confirm_token":"not-a-real-reset-token"}`, http.StatusNotFound, store.AuditNetworkResetRejected, store.OutcomeRejected)
	step("resetting with the issued token", "POST", "/api/v1/network/reset",
		`{"confirm_token":`+mustJSON(t, resetToken)+`}`, http.StatusOK, store.AuditNetworkReset, store.OutcomeSucceeded)
	step("replaying the reset token", "POST", "/api/v1/network/reset",
		`{"confirm_token":`+mustJSON(t, resetToken)+`}`, http.StatusGone, store.AuditNetworkResetRejected, store.OutcomeRejected)

	if driver.resets != 1 {
		t.Errorf("the driver was asked to reset %d times, want once", driver.resets)
	}

	// Neither the tokens the hub issued nor those clients presented may be
	// recoverable from the trail or the log stream.
	secrets := append(append([]string{}, tokens...), presented...)
	page, err := f.store.ListAudit(ctx, store.AuditQuery{Limit: 500})
	if err != nil {
		t.Fatalf("list the audit trail: %v", err)
	}
	logs := f.logs.String()
	if logs == "" {
		t.Fatal("the emitter wrote no record, so the log scan proves nothing")
	}
	for _, secret := range secrets {
		if strings.Contains(logs, secret) {
			t.Errorf("the log stream carries a confirmation token (%d characters)", len(secret))
		}
		for _, entry := range page.Items {
			if strings.Contains(auditRowText(entry), secret) {
				t.Errorf("audit row %s carries a confirmation token (%d characters)", entry.Action, len(secret))
			}
		}
	}
}
