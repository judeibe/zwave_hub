package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/event"
	"github.com/judeibe/zwave_hub/internal/hub"
	"github.com/judeibe/zwave_hub/internal/store"
)

// redactionLog is the emitter's sink. The hub writes its events from the command
// worker while the test goroutine reads the buffer, so both sides hold the mutex.
type redactionLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends one record. It has the io.Writer shape event.New takes.
func (l *redactionLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// String returns every record written so far.
func (l *redactionLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// redactionFixture is the handlerFixture with a live emitter. newHandlerFixture
// builds its hub with a nil emitter, so nothing the hub logs reaches a record and
// the log surface of SC-004 could not be scanned through it.
type redactionFixture struct {
	handlerFixture
	logs *redactionLog
}

// newRedactionFixture builds a store, a hub whose events land in a buffer, and
// the full router, all of it torn down by the test's cleanup.
func newRedactionFixture(t *testing.T) *redactionFixture {
	t.Helper()
	// The master key is the fixed 32 bytes the other fixtures use: the rows are
	// sealed and opened inside one process, and supplying it keeps Open from
	// writing a master.key file per fixture.
	st, err := store.Open(context.Background(), store.Options{
		DataDir:   t.TempDir(),
		MasterKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	logs := &redactionLog{}
	emitter := event.New(logs, slog.LevelInfo)
	h, err := hub.New(st, emitter)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = st.Close()
	})

	// The router carries the same emitter as the hub, so the access log is part
	// of the scanned surface rather than a blind spot.
	rt := NewRouter(Options{Emitter: emitter})
	NewCoordinators(h).Mount(rt)
	NewNetwork(h).Mount(rt)
	NewKeys(h).Mount(rt)
	NewConfirmations(h, true).Mount(rt)
	NewAudit(st).Mount(rt)
	return &redactionFixture{
		handlerFixture: handlerFixture{hub: h, store: st, router: rt},
		logs:           logs,
	}
}

// keyShapedRun is the shape of a Z-Wave key, and of any hash of one: 32 or more
// hexadecimal characters with no break. Present and the event redactor use the
// same threshold, so a body that must not carry a key is exactly a body this
// matches. It is compiled per call because the package keeps no state.
func keyShapedRun() *regexp.Regexp {
	return regexp.MustCompile(`[0-9a-fA-F]{32,}`)
}

// assertNoKeyShapedRun fails when text carries a run of 32 or more hex
// characters. what names the surface; the failure never quotes the run, because
// the run is the key it was looking for.
func assertNoKeyShapedRun(t *testing.T, what, text string) {
	t.Helper()
	if keyShapedRun().MatchString(text) {
		t.Errorf("%s: carries a run of 32 or more hex characters", what)
	}
}

// assertNoKeyMaterialHeaders fails when a response header carries a key-shaped
// run: a value in a header leaks exactly as one in the body does.
func assertNoKeyMaterialHeaders(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	pattern := keyShapedRun()
	for name, values := range rec.Header() {
		for _, value := range values {
			if pattern.MatchString(value) {
				t.Errorf("%s: header %s carries a run of 32 or more hex characters", what, name)
			}
		}
	}
}

// assertNoKeyMaterial scans one response whole: every header and the body.
func assertNoKeyMaterial(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	assertNoKeyMaterialHeaders(t, what, rec)
	assertNoKeyShapedRun(t, what, rec.Body.String())
}

// assertValuesAbsent fails when text carries one of the submitted values. It
// names the surface and the class, never the value.
func assertValuesAbsent(t *testing.T, what, text string, values map[string]string) {
	t.Helper()
	for class, value := range values {
		if strings.Contains(text, value) {
			t.Errorf("%s: the submitted value for %s reached it", what, class)
		}
	}
}

// assertChallengeHasNoKeyMaterial scans a two-step challenge body. The body
// carries the confirmation token by design, and a token is a secret of another
// kind rather than key material, so the token is removed before the key-shaped
// scan; the submitted values are still checked against the whole body.
func assertChallengeHasNoKeyMaterial(t *testing.T, what string, rec *httptest.ResponseRecorder, values map[string]string) {
	t.Helper()
	token, _ := decode(t, rec)["token"].(string)
	assertNoKeyMaterialHeaders(t, what, rec)
	assertNoKeyShapedRun(t, what, strings.ReplaceAll(rec.Body.String(), token, ""))
	assertValuesAbsent(t, what, rec.Body.String(), values)
}

// auditRowText joins every field of one audit row, Detail included, into the
// text the scan reads, so a value that reaches any allow-listed member is
// scanned with the rest of the row.
func auditRowText(entry store.AuditEntry) string {
	fields := []string{
		string(entry.Action), entry.ActorAddr, entry.ActorLabel, entry.RequestID,
		entry.CoordinatorID, entry.SecurityClass, string(entry.Outcome),
		entry.Detail.Source, entry.Detail.Reason,
		entry.Detail.ExpectedHomeID, entry.Detail.ActualHomeID,
	}
	fields = append(fields, entry.Detail.Classes...)
	return strings.Join(fields, "\n")
}

// mustJSON renders a fixture body, failing the test rather than the request when
// the fixture itself cannot be encoded.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode the fixture body: %v", err)
	}
	return string(encoded)
}

// TestNoKeyMaterialNeverLeavesTheAPI is SC-004 across every operation the API
// offers: the submitted values reach the confirmed export body of FR-013 and no
// other surface — not a success body, a problem body, a log record or an audit
// row.
func TestNoKeyMaterialNeverLeavesTheAPI(t *testing.T) {
	f := newRedactionFixture(t)
	ctx := context.Background()

	dev := device("redact")
	if err := f.hub.SyncDiscovery(ctx, []coordinator.Device{dev}); err != nil {
		t.Fatalf("sync discovery: %v", err)
	}

	// Four distinct values: a repeat inside the classic set is refused as
	// duplicate_key_material, so a shared value would never reach the export.
	submitted := map[string]string{
		"S2_AccessControl":   "000102030405060708090a0b0c0d0e0f",
		"S2_Authenticated":   "101112131415161718191a1b1c1d1e1f",
		"S2_Unauthenticated": "202122232425262728292a2b2c2d2e2f",
		"S0_Legacy":          "303132333435363738393a3b3c3d3e3f",
	}

	// call sends one request, fails unless it answers want, and scans what came
	// back. The status is checked first, so a problem body is never mistaken for
	// the operation's own body.
	call := func(what, method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		rec := f.do(method, path, body)
		if rec.Code != want {
			t.Fatalf("%s: status %d, want %d", what, rec.Code, want)
		}
		assertNoKeyMaterial(t, what, rec)
		assertValuesAbsent(t, what, rec.Body.String(), submitted)
		return rec
	}

	call("GET /api/v1/keys on an empty hub", "GET", "/api/v1/keys", "", http.StatusOK)
	call("GET /api/v1/network on an empty hub", "GET", "/api/v1/network", "", http.StatusOK)
	call("GET /api/v1/coordinators on an empty hub", "GET", "/api/v1/coordinators", "", http.StatusOK)
	call("GET /api/v1/active-coordinator with nothing selected", "GET", "/api/v1/active-coordinator", "", http.StatusNotFound)

	call("PUT /api/v1/active-coordinator", "PUT", "/api/v1/active-coordinator",
		`{"coordinator_id":`+mustJSON(t, dev.Identity.ID)+`}`, http.StatusOK)

	// A ready report of a stick with no network generates the classic set
	// (FR-008). The submission below replaces it, because no secure node is on
	// the network yet, so the FR-014 gate does not apply.
	if err := f.hub.Adopt(ctx, dev.Identity.ID, hub.Identified{
		Network: hub.Network{HomeID: "0x00000001"},
	}); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	call("GET /api/v1/active-coordinator once ready", "GET", "/api/v1/active-coordinator", "", http.StatusOK)
	call("GET /api/v1/coordinators once known", "GET", "/api/v1/coordinators", "", http.StatusOK)
	call("GET /api/v1/network with keys configured", "GET", "/api/v1/network", "", http.StatusOK)

	call("PUT /api/v1/keys", "PUT", "/api/v1/keys",
		`{"mode":"merge","keys":`+mustJSON(t, submitted)+`}`, http.StatusOK)

	// The metadata view has to report the submission landed, otherwise the
	// export below could pass by returning values the hub invented.
	configured := decode(t, call("GET /api/v1/keys after the submission", "GET", "/api/v1/keys", "", http.StatusOK))
	classes, _ := configured["classes"].([]any)
	supplied := 0
	for _, entry := range classes {
		class, _ := entry.(map[string]any)
		name, _ := class["class"].(string)
		if submitted[name] != "" && class["configured"] == true {
			supplied++
		}
	}
	if supplied != len(submitted) {
		t.Errorf("GET /api/v1/keys reports %d of the %d submitted classes configured", supplied, len(submitted))
	}

	call("POST /api/v1/keys/generate", "POST", "/api/v1/keys/generate",
		`{"classes":["S2_Authenticated_LR"]}`, http.StatusOK)

	// The driver now reports three securely included devices, so replacing a key
	// would strand them and the FR-014 gate refuses the change. The refusal names
	// the classes and the count, never the value that was sent.
	if err := f.hub.Adopt(ctx, dev.Identity.ID, hub.Identified{
		Network: hub.Network{HomeID: "0x00000001", NodeCount: 3, SecureNodeCount: 3},
	}); err != nil {
		t.Fatalf("adopt with secure nodes: %v", err)
	}
	const replacementValue = "f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff"
	inUse := call("PUT /api/v1/keys replacing a key in use", "PUT", "/api/v1/keys",
		`{"mode":"merge","keys":{"S0_Legacy":`+mustJSON(t, replacementValue)+`}}`, http.StatusConflict)
	assertValuesAbsent(t, "PUT /api/v1/keys replacing a key in use", inUse.Body.String(),
		map[string]string{"S0_Legacy": replacementValue})
	if code := decode(t, inUse)["code"]; code != string(CodeKeyInUse) {
		t.Errorf("PUT /api/v1/keys replacing a key in use: code %v, want %s", code, CodeKeyInUse)
	}

	// Step one of the export hands out a token and no value. It is not sent
	// through call, because the token is the one member the strict scan would
	// read as key-shaped if it happened to be.
	challengeRec := f.do("POST", "/api/v1/keys/export", "")
	if challengeRec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/v1/keys/export step one: status %d, want %d", challengeRec.Code, http.StatusAccepted)
	}
	assertChallengeHasNoKeyMaterial(t, "POST /api/v1/keys/export step one", challengeRec, submitted)
	token, _ := decode(t, challengeRec)["token"].(string)
	if token == "" {
		t.Fatal("POST /api/v1/keys/export step one answered no token")
	}

	// Step two is the one place a value may appear (FR-013).
	exportRec := f.do("POST", "/api/v1/keys/export", `{"confirm_token":`+mustJSON(t, token)+`}`)
	if exportRec.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/keys/export step two: status %d, want %d", exportRec.Code, http.StatusOK)
	}
	keys, _ := decode(t, exportRec)["keys"].(map[string]any)
	for class, value := range submitted {
		if got, _ := keys[class].(string); !strings.EqualFold(got, value) {
			t.Errorf("confirmed export: the value for %s is absent or is not the submitted one", class)
		}
	}

	// The token is spent, so the replay is 410 and carries no value either.
	replay := call("POST /api/v1/keys/export step two replayed", "POST", "/api/v1/keys/export",
		`{"confirm_token":`+mustJSON(t, token)+`}`, http.StatusGone)
	if code := decode(t, replay)["code"]; code != string(CodeConfirmationExpired) {
		t.Errorf("POST /api/v1/keys/export replayed: code %v, want %s", code, CodeConfirmationExpired)
	}

	audited := decode(t, call("GET /api/v1/audit", "GET", "/api/v1/audit", "", http.StatusOK))
	if items, _ := audited["items"].([]any); len(items) == 0 {
		t.Fatal("GET /api/v1/audit returned no row, so its scan proves nothing")
	}

	// The same trail read straight from the store, so the scan covers the
	// durable row and not only its rendering.
	page, err := f.store.ListAudit(ctx, store.AuditQuery{Limit: 200})
	if err != nil {
		t.Fatalf("list the audit trail: %v", err)
	}
	var exportedRow, rejectedRow bool
	for _, entry := range page.Items {
		switch entry.Action {
		case store.AuditKeysExported:
			exportedRow = true
		case store.AuditKeysExportRejected:
			rejectedRow = true
		}
		surface := "audit row " + string(entry.Action)
		text := auditRowText(entry)
		assertNoKeyShapedRun(t, surface, text)
		assertValuesAbsent(t, surface, text, submitted)
	}
	if !exportedRow || !rejectedRow {
		t.Errorf("the audit trail holds keys.exported %t and keys.export_rejected %t, want both", exportedRow, rejectedRow)
	}

	// The log stream last, because every operation above wrote to it.
	logs := f.logs.String()
	if logs == "" {
		t.Fatal("the emitter wrote no record, so the log scan proves nothing")
	}
	// Prove the scan has reach over both writers: the hub's key events and the
	// router's access log.
	for _, record := range []string{"keys.stored", "keys.exported", "api.request"} {
		if !strings.Contains(logs, record) {
			t.Errorf("the log holds no %s record, so its scan is not proven", record)
		}
	}
	assertNoKeyShapedRun(t, "the log stream", logs)
	assertValuesAbsent(t, "the log stream", logs, submitted)
}

// TestNoKeyMaterialInProblemBodies is the error half of SC-004: a value the API
// refused is never echoed, whether it was key-shaped or not, and the two-step
// token problems carry no value either.
func TestNoKeyMaterialInProblemBodies(t *testing.T) {
	f := newHandlerFixture(t)

	// Three shapes of a refused value: too short to be a key, too long, and
	// valid but repeated across two classes (R14 §9).
	const (
		shortValue = "00112233445566778899aabbccddeef"
		longValue  = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
		validValue = "00112233445566778899aabbccddeeff"
	)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   Code
		values map[string]string
	}{
		{
			name: "submission with a value too short", method: "PUT", path: "/api/v1/keys",
			body:   `{"mode":"merge","keys":{"S0_Legacy":` + mustJSON(t, shortValue) + `}}`,
			status: http.StatusUnprocessableEntity, code: CodeInvalidKeyMaterial,
			values: map[string]string{"S0_Legacy": shortValue},
		},
		{
			name: "submission with a hash-shaped value", method: "PUT", path: "/api/v1/keys",
			body:   `{"mode":"merge","keys":{"S0_Legacy":` + mustJSON(t, longValue) + `}}`,
			status: http.StatusUnprocessableEntity, code: CodeInvalidKeyMaterial,
			values: map[string]string{"S0_Legacy": longValue},
		},
		{
			name: "submission repeating one value across two classes", method: "PUT", path: "/api/v1/keys",
			body: `{"mode":"merge","keys":{"S2_AccessControl":` + mustJSON(t, validValue) +
				`,"S2_Authenticated":` + mustJSON(t, validValue) + `}}`,
			status: http.StatusUnprocessableEntity, code: CodeDuplicateKeyMaterial,
			values: map[string]string{"S2_AccessControl": validValue},
		},
		{
			name: "submission naming an unknown class", method: "PUT", path: "/api/v1/keys",
			body:   `{"mode":"merge","keys":{"S9_Other":` + mustJSON(t, validValue) + `}}`,
			status: http.StatusUnprocessableEntity, code: CodeInvalidKeyMaterial,
			values: map[string]string{"S9_Other": validValue},
		},
		{
			name: "submission with an unknown member", method: "PUT", path: "/api/v1/keys",
			body:   `{"mode":"merge","keys":{},"extra":1}`,
			status: http.StatusBadRequest, code: CodeInvalidRequest,
		},
		{
			name: "generation with an unknown member", method: "POST", path: "/api/v1/keys/generate",
			body:   `{"classes":["S0_Legacy"],"extra":1}`,
			status: http.StatusBadRequest, code: CodeInvalidRequest,
		},
		{
			name: "export with an unknown token", method: "POST", path: "/api/v1/keys/export",
			body:   `{"confirm_token":"never-issued"}`,
			status: http.StatusNotFound, code: CodeConfirmationInvalid,
			values: map[string]string{"confirm_token": "never-issued"},
		},
		{
			name: "export with an unknown member", method: "POST", path: "/api/v1/keys/export",
			body:   `{"confirm_token":"x","extra":1}`,
			status: http.StatusBadRequest, code: CodeInvalidRequest,
		},
		{
			name: "reset with an unknown member", method: "POST", path: "/api/v1/network/reset",
			body:   `{"confirm_token":"x","extra":1}`,
			status: http.StatusBadRequest, code: CodeInvalidRequest,
		},
		{
			name: "selection carrying key-shaped text", method: "PUT", path: "/api/v1/active-coordinator",
			body:   `{"coordinator_id":"usb:ffff:ffff:none","key":` + mustJSON(t, validValue) + `}`,
			status: http.StatusBadRequest, code: CodeInvalidRequest,
			values: map[string]string{"key": validValue},
		},
		{
			name: "selection of an unknown coordinator", method: "PUT", path: "/api/v1/active-coordinator",
			body:   `{"coordinator_id":"usb:ffff:ffff:none"}`,
			status: http.StatusNotFound, code: CodeCoordinatorNotFound,
		},
		{
			name: "read of an unknown coordinator", method: "GET", path: "/api/v1/coordinators/usb:ffff:ffff:none",
			status: http.StatusNotFound, code: CodeCoordinatorNotFound,
		},
		{
			name: "active coordinator with nothing selected", method: "GET", path: "/api/v1/active-coordinator",
			status: http.StatusNotFound, code: CodeNoActiveCoordinator,
		},
		{
			name: "audit with an out-of-range limit", method: "GET", path: "/api/v1/audit?limit=0",
			status: http.StatusBadRequest, code: CodeInvalidRequest,
		},
		{
			name: "unmatched route", method: "GET", path: "/api/v1/nothing-here",
			status: http.StatusNotFound, code: CodeInvalidRequest,
		},
		{
			name: "method not allowed", method: "POST", path: "/api/v1/keys",
			status: http.StatusMethodNotAllowed, code: CodeInvalidRequest,
		},
	}

	for _, tc := range cases {
		rec := f.do(tc.method, tc.path, tc.body)
		if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.status)
			continue
		}
		if code := decode(t, rec)["code"]; code != string(tc.code) {
			t.Errorf("%s: code %v, want %s", tc.name, code, tc.code)
		}
		assertNoKeyMaterial(t, tc.name, rec)
		assertValuesAbsent(t, tc.name, rec.Body.String(), tc.values)
	}

	// A token that was redeemed is spent: the 410 body must not carry the keys
	// the confirmation exported, and the export body of the first redemption is
	// the only place those values may appear.
	challengeRec := f.do("POST", "/api/v1/keys/export", "")
	if challengeRec.Code != http.StatusAccepted {
		t.Fatalf("export step one: status %d, want %d", challengeRec.Code, http.StatusAccepted)
	}
	token, _ := decode(t, challengeRec)["token"].(string)
	if token == "" {
		t.Fatal("export step one answered no token")
	}
	confirm := `{"confirm_token":` + mustJSON(t, token) + `}`
	if rec := f.do("POST", "/api/v1/keys/export", confirm); rec.Code != http.StatusOK {
		t.Fatalf("export step two: status %d, want %d", rec.Code, http.StatusOK)
	}
	replay := f.do("POST", "/api/v1/keys/export", confirm)
	if replay.Code != http.StatusGone {
		t.Fatalf("export step two replayed: status %d, want %d", replay.Code, http.StatusGone)
	}
	if code := decode(t, replay)["code"]; code != string(CodeConfirmationExpired) {
		t.Errorf("export replayed: code %v, want %s", code, CodeConfirmationExpired)
	}
	assertNoKeyMaterial(t, "export replayed", replay)
	assertValuesAbsent(t, "export replayed", replay.Body.String(), map[string]string{"confirm_token": token})
}
