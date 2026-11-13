package event

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"
)

var hexRun = regexp.MustCompile(`[0-9a-fA-F]{32}`)

func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line is not one JSON object: %v\n%s", err, line)
		}
		out = append(out, record)
	}
	return out
}

func sampleEvents() []Event {
	code := "invalid_request"
	exit := 1
	signal := "SIGKILL"
	retry := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	prev := "usb:10c4:ea60:00a1b2c3"
	return []Event{
		&HubStarted{Version: "1.0.0", GoVersion: "go1.27.4", DBSchema: 1, Bind: "0.0.0.0:8080", DataDir: "/data", DestructiveOps: "enabled"},
		&HubStopping{Reason: "signal", UptimeS: 10},
		&HubConfigInvalid{Field: "bind", Reason: "must be a host:port address"},
		&StoreMigrated{FromVersion: 0, ToVersion: 1, DurationMS: 4},
		&StoreIntegrityFailed{Detail: "integrity_check_failed"},
		&APIRequest{Method: "GET", Route: "/api/v1/keys", Status: 400, DurationMS: 2, Code: &code},
		&APIPanic{Route: "/api/v1/keys", PanicType: "runtime.Error"},
		&CoordinatorDiscovered{IdentityKind: "serial", VID: "10c4", PID: "ea60", SerialPath: "/dev/serial/by-id/usb-x"},
		&CoordinatorRemoved{LastPath: "/dev/serial/by-id/usb-x"},
		&CoordinatorSupportChanged{From: "unverified", To: "supported", Reason: ""},
		&CoordinatorSelected{PreviousCoordinatorID: &prev, Reason: "operator"},
		&CoordinatorRejected{Code: "coordinator_unsupported"},
		&DriverStarting{Attempt: 1, SerialPath: "/dev/serial/by-id/usb-x", SchemaVersion: 52},
		&DriverReady{SchemaVersion: 52, StartMS: 3412, NodeCount: 7, SecureNodeCount: 5},
		&DriverFailed{Attempt: 2, Reason: "exited", ExitCode: &exit, Signal: &signal, DurationMS: 100, NextRetryAt: &retry},
		&DriverStopped{Reason: "requested"},
		&DriverRestartPaused{ConsecutiveFailures: 5, Since: retry},
		&DriverLog{Severity: slog.LevelInfo, Line: "driver started"},
		&KeysStored{Revision: 2, Classes: []string{"S0_Legacy"}, Source: "supplied"},
		&KeysApplied{Revision: 2, AppliedRevision: 2, DurationMS: 9},
		&KeysApplyFailed{Revision: 2, AppliedRevision: 1, Reason: "driver_restart_failed"},
		&KeysInUseRejected{Classes: []string{"S0_Legacy"}, SecureNodeCount: 3},
		&KeysCorrupt{Classes: []string{"S0_Legacy"}, DetectedBy: "aead"},
		&KeysExportRequested{Operation: "keys.export", ExpiresAt: retry},
		&KeysExported{Classes: []string{"S0_Legacy"}, HomeID: "0xdeadbeef"},
		&KeysExportRejected{Reason: "expired"},
		&NetworkStateChanged{From: "unprovisioned", To: "ready", Reason: "keys_applied"},
		&NetworkExistingDetected{NodeCount: 4},
		&NetworkResetRequested{ExpiresAt: retry},
		&NetworkResetStarted{HomeIDBefore: "0xdeadbeef"},
		&NetworkResetCompleted{HomeIDBefore: "0xdeadbeef", DurationMS: 800},
		&NetworkResetFailed{Reason: "timeout"},
	}
}

func TestEveryEventCarriesTheEnvelope(t *testing.T) {
	var buf bytes.Buffer
	emitter := New(&buf, slog.LevelDebug)
	events := sampleEvents()
	for _, ev := range events {
		emitter.Emit(context.Background(), ev, Correlation{})
	}
	records := lines(t, &buf)
	if len(records) != len(events) {
		t.Fatalf("emitted %d records for %d events", len(records), len(events))
	}
	for i, record := range records {
		name := events[i].Name()
		if record["event"] != name {
			t.Errorf("record %d: event = %v, want %s", i, record["event"], name)
		}
		for _, key := range []string{"ts", "level", "schema", "event", "msg"} {
			if _, ok := record[key]; !ok {
				t.Errorf("%s: envelope field %q missing", name, key)
			}
		}
		if record["schema"] != float64(1) {
			t.Errorf("%s: schema = %v, want 1", name, record["schema"])
		}
		if ts, _ := record["ts"].(string); !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`).MatchString(ts) {
			t.Errorf("%s: ts %q is not RFC 3339 UTC with milliseconds", name, ts)
		}
	}
}

func TestEventFieldsMatchTheCatalogue(t *testing.T) {
	want := map[string][]string{
		"hub.started":               {"version", "go_version", "db_schema", "bind", "data_dir", "destructive_ops"},
		"hub.stopping":              {"reason", "uptime_s"},
		"hub.config_invalid":        {"field", "reason"},
		"store.migrated":            {"from_version", "to_version", "duration_ms"},
		"store.integrity_failed":    {"detail"},
		"api.request":               {"method", "route", "status", "duration_ms", "code"},
		"api.panic":                 {"route", "panic_type"},
		"coordinator.discovered":    {"identity_kind", "vid", "pid", "serial_path"},
		"coordinator.removed":       {"last_path"},
		"coordinator.selected":      {"previous_coordinator_id", "reason"},
		"coordinator.rejected":      {"code"},
		"driver.starting":           {"attempt", "serial_path", "schema_version"},
		"driver.ready":              {"schema_version", "start_ms", "node_count", "secure_node_count"},
		"driver.failed":             {"attempt", "reason", "exit_code", "signal", "duration_ms", "next_retry_at"},
		"driver.stopped":            {"reason"},
		"driver.restart_paused":     {"consecutive_failures", "since"},
		"driver.log":                {"line"},
		"keys.stored":               {"revision", "classes", "source"},
		"keys.applied":              {"revision", "applied_revision", "duration_ms"},
		"keys.apply_failed":         {"revision", "applied_revision", "reason"},
		"keys.in_use_rejected":      {"classes", "secure_node_count"},
		"keys.corrupt":              {"classes", "detected_by"},
		"keys.export_requested":     {"operation", "expires_at"},
		"keys.exported":             {"classes", "home_id"},
		"keys.export_rejected":      {"reason"},
		"network.state_changed":     {"from", "to", "reason"},
		"network.existing_detected": {"node_count"},
		"network.reset_requested":   {"expires_at"},
		"network.reset_started":     {"home_id_before"},
		"network.reset_completed":   {"home_id_before", "duration_ms"},
		"network.reset_failed":      {"reason"},
	}
	envelope := map[string]bool{"ts": true, "level": true, "schema": true, "event": true, "msg": true}
	var buf bytes.Buffer
	emitter := New(&buf, slog.LevelDebug)
	for _, ev := range sampleEvents() {
		buf.Reset()
		emitter.Emit(context.Background(), ev, Correlation{})
		record := lines(t, &buf)[0]
		fields, ok := want[ev.Name()]
		if !ok {
			continue
		}
		for _, key := range fields {
			if _, present := record[key]; !present {
				t.Errorf("%s: field %q missing from %v", ev.Name(), key, record)
			}
		}
		for key := range record {
			if envelope[key] {
				continue
			}
			found := false
			for _, f := range fields {
				if f == key {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: unexpected field %q", ev.Name(), key)
			}
		}
	}
}

func TestOptionalFieldsAreOmittedWhenNil(t *testing.T) {
	var buf bytes.Buffer
	emitter := New(&buf, slog.LevelInfo)
	emitter.Emit(context.Background(), &APIRequest{Method: "GET", Route: "/healthz", Status: 200, DurationMS: 1}, Correlation{})
	emitter.Emit(context.Background(), &DriverFailed{Attempt: 1, Reason: "spawn_failed", DurationMS: 5}, Correlation{})
	emitter.Emit(context.Background(), &CoordinatorSelected{Reason: "operator"}, Correlation{})
	records := lines(t, &buf)
	for _, key := range []string{"code"} {
		if _, ok := records[0][key]; ok {
			t.Errorf("api.request carries %q for a 200", key)
		}
	}
	for _, key := range []string{"exit_code", "signal", "next_retry_at"} {
		if _, ok := records[1][key]; ok {
			t.Errorf("driver.failed carries unset %q", key)
		}
	}
	if _, ok := records[2]["previous_coordinator_id"]; ok {
		t.Error("coordinator.selected carries an unset previous_coordinator_id")
	}
}

func TestCorrelationFieldsAppearOnlyWhenSet(t *testing.T) {
	var buf bytes.Buffer
	emitter := New(&buf, slog.LevelInfo)
	emitter.Emit(context.Background(), &HubStopping{Reason: "signal"}, Correlation{})
	emitter.Emit(context.Background(), &HubStopping{Reason: "signal"}, Correlation{
		RequestID: "r-1", ActorAddr: "192.0.2.1", ActorLabel: "ops\x00team", CoordinatorID: "usb:10c4:ea60:aa", HomeID: "0xdeadbeef",
	})
	records := lines(t, &buf)
	for _, key := range []string{"request_id", "actor_addr", "actor_label", "coordinator_id", "home_id"} {
		if _, ok := records[0][key]; ok {
			t.Errorf("empty correlation emitted %q", key)
		}
		if _, ok := records[1][key]; !ok {
			t.Errorf("correlation field %q missing", key)
		}
	}
	if records[1]["actor_label"] != "opsteam" {
		t.Errorf("actor_label = %q, want control characters stripped", records[1]["actor_label"])
	}
}

func TestHomeIDAppearsOnceOnKeysExported(t *testing.T) {
	var buf bytes.Buffer
	emitter := New(&buf, slog.LevelInfo)
	emitter.Emit(context.Background(), &KeysExported{Classes: []string{"S0_Legacy"}, HomeID: "0xdeadbeef"}, Correlation{HomeID: "0xdeadbeef"})
	if n := strings.Count(buf.String(), `"home_id"`); n != 1 {
		t.Errorf("home_id appears %d times, want 1: %s", n, buf.String())
	}
}

func TestActorLabelIsCappedAt64(t *testing.T) {
	var buf bytes.Buffer
	emitter := New(&buf, slog.LevelInfo)
	emitter.Emit(context.Background(), &HubStopping{Reason: "signal"}, Correlation{ActorLabel: strings.Repeat("x", 200)})
	if got := lines(t, &buf)[0]["actor_label"].(string); len(got) != 64 {
		t.Errorf("actor_label length = %d, want 64", len(got))
	}
}

func TestLevelFloorDropsLowerEvents(t *testing.T) {
	var buf bytes.Buffer
	emitter := New(&buf, slog.LevelWarn)
	emitter.Emit(context.Background(), &HubStopping{Reason: "signal"}, Correlation{})
	if buf.Len() != 0 {
		t.Errorf("an info event passed a warn floor: %s", buf.String())
	}
	emitter.Emit(context.Background(), &KeysExportRejected{Reason: "invalid"}, Correlation{})
	if buf.Len() == 0 {
		t.Error("a warn event was dropped at a warn floor")
	}
}

func TestDriverLogSecurityLinesAreDropped(t *testing.T) {
	var buf bytes.Buffer
	emitter := New(&buf, slog.LevelDebug)
	for _, line := range []string{
		"S0 key: 00112233445566778899aabbccddeeff",
		"Security details: enabled",
		"network key loaded",
		"Authorization: Bearer abc",
		"DSK 12345-12345-12345-12345-12345-12345-12345-12345",
	} {
		emitter.Emit(context.Background(), &DriverLog{Severity: slog.LevelInfo, Line: line}, Correlation{})
	}
	if buf.Len() != 0 {
		t.Errorf("a security-details line was written: %s", buf.String())
	}
	emitter.Emit(context.Background(), &DriverLog{Severity: slog.LevelWarn, Line: "serial port opened"}, Correlation{})
	records := lines(t, &buf)
	if len(records) != 1 || records[0]["level"] != "warn" {
		t.Errorf("an ordinary driver line was not written at the child's level: %v", records)
	}
}

func TestDriverLogLineIsCappedAt4KiB(t *testing.T) {
	var buf bytes.Buffer
	emitter := New(&buf, slog.LevelDebug)
	emitter.Emit(context.Background(), &DriverLog{Severity: slog.LevelInfo, Line: strings.Repeat("é", 5000)}, Correlation{})
	got := lines(t, &buf)[0]["line"].(string)
	if len(got) > 4096 {
		t.Errorf("line is %d bytes, want at most 4096", len(got))
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Error("the cap split a multi-byte character")
	}
}

func TestHexRunsAreRedactedWhateverTheField(t *testing.T) {
	var buf bytes.Buffer
	emitter := New(&buf, slog.LevelDebug)
	secret := "00112233445566778899aabbccddeeff"
	emitter.Emit(context.Background(), &HubConfigInvalid{Field: "bind", Reason: "bad " + secret}, Correlation{})
	emitter.Emit(context.Background(), &KeysStored{Revision: 1, Classes: []string{"S0_Legacy", secret}, Source: "supplied"}, Correlation{})
	if hexRun.MatchString(buf.String()) {
		t.Errorf("a 32-hex run reached the stream: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "[redacted]") {
		t.Error("the value was removed rather than replaced")
	}
}

func TestUnlistedAttributesCannotReachTheStream(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{})
	_ = handler
	allow := allowList()
	for _, forbidden := range []string{
		"key", "keys", "master_key", "confirm_token", "token", "token_hash", "dsk",
		"authorization", "cookie", "headers", "body", "stack", "panic", "config", "key_blob", "nonce",
	} {
		if allow[forbidden] {
			t.Errorf("the allow-list admits %q, which is on the never-send list", forbidden)
		}
	}
}

// TestNoKeyMaterial is the scanner named by `make lint` and quickstart.md §4:
// every event constructor is rendered and no 32-hex run may appear (SC-004).
func TestNoKeyMaterial(t *testing.T) {
	var buf bytes.Buffer
	emitter := New(&buf, slog.LevelDebug)
	secret := strings.Repeat("ab", 16)
	corr := Correlation{RequestID: "req-" + secret, ActorAddr: secret, ActorLabel: secret, CoordinatorID: secret, HomeID: secret}
	for _, ev := range sampleEvents() {
		emitter.Emit(context.Background(), ev, corr)
	}
	if loc := hexRun.FindString(buf.String()); loc != "" {
		t.Fatalf("a 32-hex run reached the event stream: %q", loc)
	}
}
