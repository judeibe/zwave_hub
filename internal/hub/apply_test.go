package hub

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/judeibe/zwave_hub/internal/event"
	"github.com/judeibe/zwave_hub/internal/store"
)

// eventsHub starts a hub whose event stream is written to the returned buffer,
// so a test can assert what the apply path reported.
func eventsHub(t *testing.T) (*Hub, *store.Store, *bytes.Buffer) {
	t.Helper()
	buf := new(bytes.Buffer)
	h, st := newTestHubWith(t, event.New(buf, slog.LevelInfo))
	return h, st, buf
}

// selectedHub returns a hub with one selected coordinator and no keys.
func selectedHub(t *testing.T) (*Hub, *store.Store, *bytes.Buffer, string) {
	t.Helper()
	h, st, buf := eventsHub(t)
	dev := testDevice("ttyUSB0", "aabbcc", "usb-1")
	mustSync(t, h, dev)
	mustSelect(t, h, dev.Identity.ID, "req-select")
	return h, st, buf, dev.Identity.ID
}

// appliedRevision returns *row.AppliedRevision, or zero when nothing was ever
// applied.
func appliedRevision(t *testing.T, st *store.Store) int {
	t.Helper()
	row, err := st.GetProvisioning(context.Background())
	if err != nil {
		t.Fatalf("read the provisioning row: %v", err)
	}
	if row.AppliedRevision == nil {
		return 0
	}
	return *row.AppliedRevision
}

// auditRowsWithAction returns the audit rows carrying action, newest first.
func auditRowsWithAction(t *testing.T, st *store.Store, action store.AuditAction) []store.AuditEntry {
	t.Helper()
	var found []store.AuditEntry
	for _, row := range auditRows(t, st) {
		if row.Action == action {
			found = append(found, row)
		}
	}
	return found
}

// TestNoteKeysAppliedRecordsRevisionAndHomeID proves the success half of
// FR-012: a ready report carrying the revision the child started with moves
// applied_revision to it, persists the controller's home ID on the same path,
// makes the configuration in effect, and emits keys.applied.
func TestNoteKeysAppliedRecordsRevisionAndHomeID(t *testing.T) {
	h, st, events, id := selectedHub(t)
	mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("s0"), "req-submit")

	if err := h.NoteKeysApplied(context.Background(), id, 1, "0x01020304", time.Now()); err != nil {
		t.Fatalf("note the applied revision: %v", err)
	}
	if got := appliedRevision(t, st); got != 1 {
		t.Errorf("applied revision = %d, want 1", got)
	}
	row, err := st.GetProvisioning(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if row.HomeID != "0x01020304" {
		t.Errorf("home ID = %q, want the controller's 0x01020304", row.HomeID)
	}

	cfg, err := h.Keys(context.Background())
	if err != nil {
		t.Fatalf("read the key configuration: %v", err)
	}
	if !cfg.InEffect {
		t.Errorf("revision %d is not in effect after the ready report", cfg.Revision)
	}
	if cfg.Apply != nil {
		t.Errorf("apply = %+v on an in-effect configuration, want none", cfg.Apply)
	}
	if !strings.Contains(events.String(), `"event":"keys.applied"`) {
		t.Error("the event stream has no keys.applied record")
	}
}

// TestNoteKeysAppliedKeepsTheStartedRevision proves the revision recorded is the
// one the child started with, not the latest committed one: a submission that
// landed while the child was starting bumps the stored revision, the ready
// report still records the older one, and the configuration stays pending with
// the newer revision.
func TestNoteKeysAppliedKeepsTheStartedRevision(t *testing.T) {
	h, st, _, id := selectedHub(t)
	mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("first"), "req-first")
	mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("second"), "req-second")

	// The child that came up was started with revision 1.
	if err := h.NoteKeysApplied(context.Background(), id, 1, "0x00000001", time.Now()); err != nil {
		t.Fatalf("note the applied revision: %v", err)
	}
	if got := appliedRevision(t, st); got != 1 {
		t.Errorf("applied revision = %d, want the started revision 1", got)
	}

	cfg, err := h.Keys(context.Background())
	if err != nil {
		t.Fatalf("read the key configuration: %v", err)
	}
	if cfg.Revision != 2 {
		t.Fatalf("revision = %d, want 2", cfg.Revision)
	}
	if cfg.InEffect {
		t.Error("revision 2 is in effect, but the child was started with revision 1")
	}
	if cfg.Apply == nil {
		t.Fatal("no apply reason on a stored revision that is not running")
	}
	if cfg.Apply.Reason != ApplyCoordinatorUnavailable {
		t.Errorf("apply reason = %q, want %q while the coordinator is not running",
			cfg.Apply.Reason, ApplyCoordinatorUnavailable)
	}
}

// TestNoteKeysApplyFailedAuditsOncePerRevision proves the failure half of
// FR-015: the failure is recorded as one keys.apply_failed row per revision
// under the hub's own identity, the retries that follow add nothing, the
// configuration reports driver_restart_failed, and a successful apply lets the
// next revision's failure be recorded again.
func TestNoteKeysApplyFailedAuditsOncePerRevision(t *testing.T) {
	h, st, events, id := selectedHub(t)
	mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("first"), "req-first")

	if err := h.NoteKeysApplyFailed(context.Background(), id, "driver_start_failed"); err != nil {
		t.Fatalf("note the failed apply: %v", err)
	}
	rows := auditRowsWithAction(t, st, store.AuditKeysApplyFailed)
	if len(rows) != 1 {
		t.Fatalf("%d keys.apply_failed rows after one failure, want 1", len(rows))
	}
	row := rows[0]
	if row.Outcome != store.OutcomeFailed {
		t.Errorf("outcome = %q, want %q", row.Outcome, store.OutcomeFailed)
	}
	if row.Detail.Reason != "driver_start_failed" {
		t.Errorf("reason = %q, want the failure code", row.Detail.Reason)
	}
	if row.ActorAddr != activationActorAddr || row.RequestID != activationRequestID {
		t.Errorf("actor = %q/%q, want the hub's activation identity", row.ActorAddr, row.RequestID)
	}
	if row.CoordinatorID != id {
		t.Errorf("coordinator = %q, want %q", row.CoordinatorID, id)
	}

	// The supervisor retries with backoff and reports each attempt; the
	// operator gets one attributable row, not one per retry.
	if err := h.NoteKeysApplyFailed(context.Background(), id, "driver_start_failed"); err != nil {
		t.Fatalf("note the repeat failure: %v", err)
	}
	if got := len(auditRowsWithAction(t, st, store.AuditKeysApplyFailed)); got != 1 {
		t.Errorf("%d keys.apply_failed rows after the retry reported again, want 1", got)
	}

	cfg, err := h.Keys(context.Background())
	if err != nil {
		t.Fatalf("read the key configuration: %v", err)
	}
	if cfg.Apply == nil || cfg.Apply.Reason != ApplyDriverRestartFailed {
		t.Fatalf("apply = %+v, want reason %q", cfg.Apply, ApplyDriverRestartFailed)
	}
	if !strings.Contains(events.String(), `"event":"keys.apply_failed"`) {
		t.Error("the event stream has no keys.apply_failed record")
	}

	// A successful start clears the marker with the applied revision, so the
	// next revision failing is a new fact and is recorded.
	if err := h.NoteKeysApplied(context.Background(), id, 1, "0x00000001", time.Now()); err != nil {
		t.Fatalf("note the applied revision: %v", err)
	}
	mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("second"), "req-second")
	if err := h.NoteKeysApplyFailed(context.Background(), id, "driver_start_failed"); err != nil {
		t.Fatalf("note the failure of revision 2: %v", err)
	}
	if got := len(auditRowsWithAction(t, st, store.AuditKeysApplyFailed)); got != 2 {
		t.Errorf("%d keys.apply_failed rows after revision 2 failed, want 2", got)
	}
}

// TestNoteKeysAppliedIgnoresAnotherCoordinator proves a report for a coordinator
// that is not the active one records nothing: the revision belongs to the
// selected coordinator's network, and another device's report must not move it.
func TestNoteKeysAppliedIgnoresAnotherCoordinator(t *testing.T) {
	h, st, _, id := selectedHub(t)
	mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("s0"), "req-submit")

	other := testDevice("ttyUSB1", "ddeeff", "usb-2")
	mustSync(t, h, other)
	if err := h.NoteKeysApplied(context.Background(), other.Identity.ID, 1, "0x00000002", time.Now()); err != nil {
		t.Fatalf("note an inactive coordinator's apply: %v", err)
	}
	if got := appliedRevision(t, st); got != 0 {
		t.Errorf("applied revision = %d after an inactive coordinator reported, want 0", got)
	}
	if err := h.NoteKeysApplyFailed(context.Background(), other.Identity.ID, "driver_start_failed"); err != nil {
		t.Fatalf("note an inactive coordinator's failure: %v", err)
	}
	if got := len(auditRowsWithAction(t, st, store.AuditKeysApplyFailed)); got != 0 {
		t.Errorf("%d keys.apply_failed rows for an inactive coordinator, want 0", got)
	}
	if err := h.NoteKeysApplyFailed(context.Background(), id, "driver_start_failed"); err != nil {
		t.Fatal(err)
	}
	if got := len(auditRowsWithAction(t, st, store.AuditKeysApplyFailed)); got != 1 {
		t.Errorf("%d keys.apply_failed rows for the active coordinator, want 1", got)
	}
}
