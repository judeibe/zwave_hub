package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/judeibe/zwave_hub/internal/hub"
	"github.com/judeibe/zwave_hub/internal/store"
)

// This file drives the key resources of quickstart.md §4 scenarios 4 to 7
// through the HTTP surface. The fixture binds no driver supervisor, so every
// submission is stored but never applied: that is what makes in_effect and the
// apply block observable here, and what a submission without a coordinator
// (scenario 5) relies on.

// keysTestKeyValueShape is the run a Z-Wave key has in every encoding the
// contract uses: 32 hexadecimal characters (FR-013, SC-004).
const keysTestKeyValueShape = `[0-9a-fA-F]{32}`

// keysTestStoredKey is one stored key row in a comparable form. The sealed blob
// is kept as a string only so two reads can be compared; nothing prints it,
// because a failure message must not echo key material.
type keysTestStoredKey struct {
	blob    string
	source  store.KeySource
	created time.Time
	changed time.Time
}

// keysTestSubmissionBody encodes a PUT /api/v1/keys body. An empty mode is
// omitted, which is the contract's default of merge.
func keysTestSubmissionBody(t *testing.T, mode hub.KeyMode, keys map[string]string) string {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Mode string            `json:"mode,omitempty"`
		Keys map[string]string `json:"keys"`
	}{Mode: string(mode), Keys: keys})
	if err != nil {
		t.Fatalf("encode key submission: %v", err)
	}
	return string(encoded)
}

// keysTestGenerationBody encodes a POST /api/v1/keys/generate body naming
// classes, so a test never hand-builds JSON around a class name.
func keysTestGenerationBody(t *testing.T, classes []string) string {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Classes []string `json:"classes"`
	}{Classes: classes})
	if err != nil {
		t.Fatalf("encode key generation: %v", err)
	}
	return string(encoded)
}

// keysTestIntMember reads an integer member of a decoded body. encoding/json
// decodes every number as float64, so a direct wantField against an int would
// never match.
func keysTestIntMember(t *testing.T, what string, body map[string]any, member string) int {
	t.Helper()
	value, ok := body[member].(float64)
	if !ok {
		t.Fatalf("%s: %s = %v, want a JSON number", what, member, body[member])
	}
	return int(value)
}

// keysTestClassEntry returns one class's metadata from a decoded
// KeyConfiguration, and fails when the class is not listed: every class of the
// catalogue is always present, configured or not.
func keysTestClassEntry(t *testing.T, what string, body map[string]any, class string) map[string]any {
	t.Helper()
	entries, ok := body["classes"].([]any)
	if !ok {
		t.Fatalf("%s: classes = %v, want a list", what, body["classes"])
	}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("%s: class entry %v is not an object", what, raw)
		}
		if entry["class"] == class {
			return entry
		}
	}
	t.Fatalf("%s: class %s is not listed, want every class of the catalogue", what, class)
	return nil
}

// keysTestConfiguredClasses lists the classes a decoded KeyConfiguration marks
// configured, sorted so a comparison never depends on the order the body
// happens to list them in.
func keysTestConfiguredClasses(t *testing.T, what string, body map[string]any) []string {
	t.Helper()
	entries, ok := body["classes"].([]any)
	if !ok {
		t.Fatalf("%s: classes = %v, want a list", what, body["classes"])
	}
	configured := make([]string, 0, len(entries))
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("%s: class entry %v is not an object", what, raw)
		}
		if entry["configured"] == true {
			class, _ := entry["class"].(string)
			configured = append(configured, class)
		}
	}
	sort.Strings(configured)
	return configured
}

// keysTestAssertClasses fails unless the metadata lists exactly want as
// configured. Both sides are sorted, so neither order is asserted.
func keysTestAssertClasses(t *testing.T, what string, body map[string]any, want []string) {
	t.Helper()
	got := keysTestConfiguredClasses(t, what, body)
	expected := slices.Clone(want)
	sort.Strings(expected)
	if !slices.Equal(got, expected) {
		t.Errorf("%s: configured classes %v, want %v", what, got, expected)
	}
}

// keysTestApplyBlock returns the apply block of a decoded KeyConfiguration. A
// stored revision that is not running has to say why, so an absent block is
// fatal rather than an empty result to compare against.
func keysTestApplyBlock(t *testing.T, what string, body map[string]any) map[string]any {
	t.Helper()
	apply, ok := body["apply"].(map[string]any)
	if !ok {
		t.Fatalf("%s: apply = %v, want the block explaining a stored revision that is not running", what, body["apply"])
	}
	return apply
}

// keysTestStoredKeys reads every stored key row into a comparable form.
func keysTestStoredKeys(t *testing.T, ctx context.Context, f *handlerFixture) map[string]keysTestStoredKey {
	t.Helper()
	rows, err := f.store.ListKeys(ctx)
	if err != nil {
		t.Fatalf("list stored keys: %v", err)
	}
	stored := make(map[string]keysTestStoredKey, len(rows))
	for _, row := range rows {
		stored[row.SecurityClass] = keysTestStoredKey{
			blob:    string(row.Blob),
			source:  row.Source,
			created: row.CreatedAt,
			changed: row.ChangedAt,
		}
	}
	return stored
}

// keysTestStoredRow returns one class's stored row. A missing row is fatal: the
// comparisons built on it are about values, and an absent row would let them
// pass against a zero value.
func keysTestStoredRow(t *testing.T, what string, stored map[string]keysTestStoredKey, class string) keysTestStoredKey {
	t.Helper()
	row, ok := stored[class]
	if !ok {
		t.Fatalf("%s: %s is not stored, want a row for every configured class", what, class)
	}
	return row
}

// keysTestAssertSameStoredKeys fails when any class's stored row differs between
// two reads. It never prints a blob, because a sealed value is still key
// material the test exists to keep out of the output.
func keysTestAssertSameStoredKeys(t *testing.T, what string, want, got map[string]keysTestStoredKey) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d stored classes, want the unchanged %d", what, len(got), len(want))
	}
	for class, before := range want {
		after, ok := got[class]
		if !ok {
			t.Errorf("%s: %s is no longer stored, want the row unchanged", what, class)
			continue
		}
		if after.blob != before.blob {
			t.Errorf("%s: the sealed value of %s changed, want it unchanged", what, class)
		}
		if after.source != before.source {
			t.Errorf("%s: the source of %s = %q, want the unchanged %q", what, class, after.source, before.source)
		}
		if !after.created.Equal(before.created) {
			t.Errorf("%s: %s created_at moved from %v to %v, want it unchanged", what, class, before.created, after.created)
		}
		if !after.changed.Equal(before.changed) {
			t.Errorf("%s: %s changed_at moved from %v to %v, want it unchanged", what, class, before.changed, after.changed)
		}
	}
}

// keysTestAssertNoHexRun fails when a raw response body carries a run shaped
// like a key. The match is reported by offset and length only: echoing it would
// put key material into the test output the check exists to keep it out of
// (FR-013).
func keysTestAssertNoHexRun(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	loc := regexp.MustCompile(keysTestKeyValueShape).FindIndex(rec.Body.Bytes())
	if loc != nil {
		t.Errorf("%s: the body carries a %d-character run matching %s at offset %d, want metadata only",
			what, loc[1]-loc[0], keysTestKeyValueShape, loc[0])
	}
}

// TestKeysSubmitStoresTheClassAndReturnsMetadata drives quickstart scenario 4's
// submission half (FR-009, FR-013): a valid submission is stored and the answer
// is the metadata view — revision 1, the submitted class configured with its
// source and times, and the other five classes listed as unconfigured.
func TestKeysSubmitStoresTheClassAndReturnsMetadata(t *testing.T) {
	f := newHandlerFixture(t)
	ctx := context.Background()
	const (
		class = hub.SecurityClassS0Legacy
		value = "00112233445566778899aabbccddeeff"
	)

	body := assertBody(t, "submit",
		f.do("PUT", "/api/v1/keys", keysTestSubmissionBody(t, "", map[string]string{class: value})))
	if got := keysTestIntMember(t, "submit", body, "revision"); got != 1 {
		t.Errorf("submit: revision = %d, want 1: the first commit is revision 1", got)
	}
	if body["in_effect"] != false {
		t.Errorf("submit: in_effect = %v, want false while no driver runs the revision", body["in_effect"])
	}

	if entries, _ := body["classes"].([]any); len(entries) != len(hub.AllClasses()) {
		t.Errorf("submit: %d classes listed, want all %d", len(entries), len(hub.AllClasses()))
	}
	entry := keysTestClassEntry(t, "submit", body, class)
	if entry["configured"] != true {
		t.Errorf("submit: %s configured = %v, want true", class, entry["configured"])
	}
	if entry["source"] != string(store.KeySourceSupplied) {
		t.Errorf("submit: %s source = %v, want %q", class, entry["source"], store.KeySourceSupplied)
	}
	for _, member := range []string{"created_at", "changed_at"} {
		if at, _ := entry[member].(string); at == "" {
			t.Errorf("submit: %s %s = %v, want a timestamp on a configured class", class, member, entry[member])
		}
	}

	// Every other class is listed, unconfigured, and carries no source: there is
	// nothing to attribute a value that was never supplied.
	for _, other := range hub.AllClasses() {
		if other == class {
			continue
		}
		otherEntry := keysTestClassEntry(t, "submit", body, other)
		if otherEntry["configured"] != false {
			t.Errorf("submit: %s configured = %v, want false", other, otherEntry["configured"])
		}
		if source, ok := otherEntry["source"]; ok {
			t.Errorf("submit: %s source = %v, want the member absent on an unconfigured class", other, source)
		}
	}

	// Stored, not merely echoed: the submitted class reached the sealed store.
	row := keysTestStoredRow(t, "submit", keysTestStoredKeys(t, ctx, f), class)
	if row.source != store.KeySourceSupplied {
		t.Errorf("submit: stored source of %s = %q, want %q", class, row.source, store.KeySourceSupplied)
	}
	if len(row.blob) == 0 {
		t.Errorf("submit: stored blob of %s is empty, want the sealed value", class)
	}
}

// TestKeysGenerateDefaultsToClassicAndOnlyNamedLongRange drives quickstart
// scenario 4's generation half (FR-008, FR-010): a generation with no body
// produces the four classic classes and neither Long Range class, while a body
// naming S2_Authenticated_LR produces that class alone.
func TestKeysGenerateDefaultsToClassicAndOnlyNamedLongRange(t *testing.T) {
	classic := newHandlerFixture(t)
	classicBody := assertBody(t, "generate with no body",
		classic.do("POST", "/api/v1/keys/generate", ""),
		wantField{"classic_complete", true},
	)
	if got := keysTestIntMember(t, "generate with no body", classicBody, "revision"); got != 1 {
		t.Errorf("generate with no body: revision = %d, want 1", got)
	}
	keysTestAssertClasses(t, "generate with no body", classicBody, hub.ClassicClasses())
	for _, class := range hub.ClassicClasses() {
		entry := keysTestClassEntry(t, "generate with no body", classicBody, class)
		if entry["source"] != string(store.KeySourceGenerated) {
			t.Errorf("generate with no body: %s source = %v, want %q", class, entry["source"], store.KeySourceGenerated)
		}
	}
	for _, class := range hub.LongRangeClasses() {
		entry := keysTestClassEntry(t, "generate with no body", classicBody, class)
		if entry["configured"] != false {
			t.Errorf("generate with no body: %s configured = %v, want false: Long Range is never generated implicitly", class, entry["configured"])
		}
	}

	longRange := newHandlerFixture(t)
	const named = hub.SecurityClassS2AuthenticatedLR
	longRangeBody := assertBody(t, "generate S2_Authenticated_LR",
		longRange.do("POST", "/api/v1/keys/generate", keysTestGenerationBody(t, []string{named})),
		wantField{"classic_complete", false},
	)
	if got := keysTestIntMember(t, "generate S2_Authenticated_LR", longRangeBody, "revision"); got != 1 {
		t.Errorf("generate S2_Authenticated_LR: revision = %d, want 1", got)
	}
	keysTestAssertClasses(t, "generate S2_Authenticated_LR", longRangeBody, []string{named})
	if entry := keysTestClassEntry(t, "generate S2_Authenticated_LR", longRangeBody, named); entry["source"] != string(store.KeySourceGenerated) {
		t.Errorf("generate S2_Authenticated_LR: %s source = %v, want %q", named, entry["source"], store.KeySourceGenerated)
	}
	stored := keysTestStoredKeys(t, context.Background(), longRange)
	if len(stored) != 1 {
		t.Errorf("generate S2_Authenticated_LR: %d stored classes, want only %s", len(stored), named)
	}
	keysTestStoredRow(t, "generate S2_Authenticated_LR", stored, named)
}

// TestKeysInvalidSubmissionIsRefusedWhole drives quickstart scenario 6 (FR-011,
// SC-005): one malformed value refuses the whole submission, the refusal names
// the offending class and never the value, nothing is stored, and the refusal is
// recorded as keys.rejected.
func TestKeysInvalidSubmissionIsRefusedWhole(t *testing.T) {
	f := newHandlerFixture(t)
	ctx := context.Background()
	const (
		configuredClass = hub.SecurityClassS2AccessControl
		configuredValue = "ffeeddccbbaa99887766554433221100"
		submittedClass  = hub.SecurityClassS0Legacy
		submittedValue  = "00112233445566778899aabbccddeeff"
		malformedValue  = "not-a-key"
	)

	// One class is already configured, so "nothing changed" is a statement about
	// a real set rather than about an empty store.
	if rec := f.do("PUT", "/api/v1/keys",
		keysTestSubmissionBody(t, "", map[string]string{configuredClass: configuredValue})); rec.Code != http.StatusOK {
		t.Fatalf("seed status %d, want 200", rec.Code)
	}
	before := keysTestStoredKeys(t, ctx, f)
	beforeBody := decode(t, f.do("GET", "/api/v1/keys", ""))

	rec := f.do("PUT", "/api/v1/keys", keysTestSubmissionBody(t, "", map[string]string{
		submittedClass:                   submittedValue,
		hub.SecurityClassS2Authenticated: malformedValue,
	}))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid submission status %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	problem := decode(t, rec)
	if problem["code"] != hub.CodeInvalidKeyMaterial {
		t.Errorf("invalid submission: code = %v, want %q", problem["code"], hub.CodeInvalidKeyMaterial)
	}
	if problem["class"] != hub.SecurityClassS2Authenticated {
		t.Errorf("invalid submission: class = %v, want %q", problem["class"], hub.SecurityClassS2Authenticated)
	}
	classes, _ := problem["classes"].([]any)
	if len(classes) != 1 || classes[0] != hub.SecurityClassS2Authenticated {
		t.Errorf("invalid submission: classes = %v, want [%q]", classes, hub.SecurityClassS2Authenticated)
	}
	// FR-013: the refusal names the class and echoes neither the malformed value
	// nor the valid one that shared the request.
	for _, value := range []string{submittedValue, malformedValue} {
		if strings.Contains(rec.Body.String(), value) {
			t.Errorf("invalid submission: the problem body echoes a submitted value, want class names only")
		}
	}

	// The valid class of the same request was not stored, and the configured
	// class is untouched: the submission is judged and refused as one set.
	keysTestAssertSameStoredKeys(t, "after the refused submission", before, keysTestStoredKeys(t, ctx, f))
	afterBody := decode(t, f.do("GET", "/api/v1/keys", ""))
	keysTestAssertClasses(t, "after the refused submission", afterBody, []string{configuredClass})
	if got, want := keysTestIntMember(t, "after the refused submission", afterBody, "revision"),
		keysTestIntMember(t, "before the refused submission", beforeBody, "revision"); got != want {
		t.Errorf("after the refused submission: revision = %d, want the unchanged %d", got, want)
	}

	page := decode(t, f.do("GET", "/api/v1/audit?action="+string(store.AuditKeysRejected), ""))
	items, _ := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("keys.rejected audit rows = %d, want exactly the one the refusal wrote", len(items))
	}
	entry, _ := items[0].(map[string]any)
	if entry["action"] != string(store.AuditKeysRejected) || entry["outcome"] != string(store.OutcomeRejected) {
		t.Errorf("audit row = %v, want action %q with outcome %q", entry, store.AuditKeysRejected, store.OutcomeRejected)
	}
	detail, _ := entry["detail"].(map[string]any)
	if detail["reason"] != hub.CodeInvalidKeyMaterial {
		t.Errorf("audit detail reason = %v, want %q", detail["reason"], hub.CodeInvalidKeyMaterial)
	}
	detailClasses, _ := detail["classes"].([]any)
	if len(detailClasses) != 1 || detailClasses[0] != hub.SecurityClassS2Authenticated {
		t.Errorf("audit detail classes = %v, want [%q]", detailClasses, hub.SecurityClassS2Authenticated)
	}
}

// TestKeysMergeReplacesOnlyTheNamedClass drives quickstart scenarios 4 and 5's
// single-class replacement (FR-009): with two classes configured, a merge naming
// one of them replaces that class alone — its changed_at moves while its
// created_at stays — and the class it does not name keeps its sealed value and
// both times.
func TestKeysMergeReplacesOnlyTheNamedClass(t *testing.T) {
	f := newHandlerFixture(t)
	ctx := context.Background()
	const (
		keptClass        = hub.SecurityClassS2Authenticated
		replacedClass    = hub.SecurityClassS0Legacy
		keptValue        = "ffeeddccbbaa99887766554433221100"
		firstValue       = "00112233445566778899aabbccddeeff"
		replacementValue = "0123456789abcdeffedcba9876543210"
	)

	if rec := f.do("PUT", "/api/v1/keys", keysTestSubmissionBody(t, "", map[string]string{
		keptClass: keptValue, replacedClass: firstValue,
	})); rec.Code != http.StatusOK {
		t.Fatalf("configure two classes: status %d, want 200", rec.Code)
	}

	// changed_at is written at millisecond precision, so two submissions inside
	// one millisecond are indistinguishable. Backdating the class the merge will
	// replace puts it an hour in the past — the state of a class configured
	// earlier — so the move is observable without the test depending on how long
	// the requests take.
	seeded := keysTestStoredRow(t, "seed", keysTestStoredKeys(t, ctx, f), replacedClass)
	if err := f.store.WithTx(ctx, func(tx *store.Tx) error {
		return tx.PutKey(ctx, replacedClass, []byte(seeded.blob), store.KeySourceSupplied, time.Now().Add(-time.Hour))
	}); err != nil {
		t.Fatalf("backdate the class the merge replaces: %v", err)
	}
	before := keysTestStoredKeys(t, ctx, f)

	body := assertBody(t, "merge one class", f.do("PUT", "/api/v1/keys",
		keysTestSubmissionBody(t, hub.KeyModeMerge, map[string]string{replacedClass: replacementValue})))
	if got := keysTestIntMember(t, "merge one class", body, "revision"); got != 2 {
		t.Errorf("merge one class: revision = %d, want 2: every accepted change bumps the revision", got)
	}
	keysTestAssertClasses(t, "merge one class", body, []string{keptClass, replacedClass})

	after := keysTestStoredKeys(t, ctx, f)
	keptBefore := keysTestStoredRow(t, "before the merge", before, keptClass)
	keptAfter := keysTestStoredRow(t, "after the merge", after, keptClass)
	if keptAfter.blob != keptBefore.blob {
		t.Errorf("merge one class: the sealed value of %s changed, want it untouched", keptClass)
	}
	if !keptAfter.changed.Equal(keptBefore.changed) {
		t.Errorf("merge one class: %s changed_at moved from %v to %v, want it untouched", keptClass, keptBefore.changed, keptAfter.changed)
	}
	if !keptAfter.created.Equal(keptBefore.created) {
		t.Errorf("merge one class: %s created_at moved from %v to %v, want it untouched", keptClass, keptBefore.created, keptAfter.created)
	}

	replacedBefore := keysTestStoredRow(t, "before the merge", before, replacedClass)
	replacedAfter := keysTestStoredRow(t, "after the merge", after, replacedClass)
	if replacedAfter.blob == replacedBefore.blob {
		t.Errorf("merge one class: the sealed value of %s is unchanged, want the replacement stored", replacedClass)
	}
	if !replacedAfter.changed.After(replacedBefore.changed) {
		t.Errorf("merge one class: %s changed_at = %v, want a time after %v", replacedClass, replacedAfter.changed, replacedBefore.changed)
	}
	if !replacedAfter.created.Equal(replacedBefore.created) {
		t.Errorf("merge one class: %s created_at moved from %v to %v, want the first configuration time kept", replacedClass, replacedBefore.created, replacedAfter.created)
	}
	if replacedAfter.source != store.KeySourceSupplied {
		t.Errorf("merge one class: %s source = %q, want %q", replacedClass, replacedAfter.source, store.KeySourceSupplied)
	}
}

// TestKeysSubmissionWithoutCoordinatorIsStoredAndReportsNoCoordinator drives
// quickstart scenario 4 with no stick selected (R14 §1): the submission is
// accepted and stored, in_effect is false, and the apply block says
// no_coordinator.
func TestKeysSubmissionWithoutCoordinatorIsStoredAndReportsNoCoordinator(t *testing.T) {
	f := newHandlerFixture(t)
	ctx := context.Background()
	const (
		class = hub.SecurityClassS2Unauthenticated
		value = "0123456789abcdeffedcba9876543210"
	)

	// The fixture binds no driver and selects no coordinator. The 404 is the
	// premise of the test, not the behaviour under test.
	if rec := f.do("GET", "/api/v1/active-coordinator", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("active coordinator status %d, want 404: the fixture must have none selected", rec.Code)
	}

	body := assertBody(t, "submit with no coordinator",
		f.do("PUT", "/api/v1/keys", keysTestSubmissionBody(t, "", map[string]string{class: value})),
		wantField{"in_effect", false},
		wantField{"applied_revision", nil},
	)
	if got := keysTestIntMember(t, "submit with no coordinator", body, "revision"); got != 1 {
		t.Errorf("submit with no coordinator: revision = %d, want 1", got)
	}
	apply := keysTestApplyBlock(t, "submit with no coordinator", body)
	if apply["reason"] != string(hub.ApplyNoCoordinator) {
		t.Errorf("submit with no coordinator: apply.reason = %v, want %q", apply["reason"], hub.ApplyNoCoordinator)
	}
	if message, _ := apply["message"].(string); message == "" {
		t.Errorf("submit with no coordinator: apply.message is empty, want the reason explained")
	}

	// Stored, not merely answered: the metadata read and the store both hold the
	// class, so the accepted submission outlives the request.
	keysTestAssertClasses(t, "submit with no coordinator", decode(t, f.do("GET", "/api/v1/keys", "")), []string{class})
	keysTestStoredRow(t, "submit with no coordinator", keysTestStoredKeys(t, ctx, f), class)
}

// TestKeysMetadataReadsCarryNoKeyValues drives quickstart scenario 5 (FR-013,
// SC-004): with all six classes configured, GET /api/v1/keys lists every class,
// and neither that body nor GET /api/v1/network carries a 32-hex run anywhere in
// the raw response.
func TestKeysMetadataReadsCarryNoKeyValues(t *testing.T) {
	f := newHandlerFixture(t)

	generated := assertBody(t, "generate all six",
		f.do("POST", "/api/v1/keys/generate", keysTestGenerationBody(t, hub.AllClasses())),
		wantField{"classic_complete", true},
	)
	keysTestAssertClasses(t, "generate all six", generated, hub.AllClasses())

	keys := f.do("GET", "/api/v1/keys", "")
	if keys.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/keys status %d, want 200", keys.Code)
	}
	body := decode(t, keys)
	if entries, _ := body["classes"].([]any); len(entries) != len(hub.AllClasses()) {
		t.Errorf("GET /api/v1/keys: %d classes listed, want all %d", len(entries), len(hub.AllClasses()))
	}
	keysTestAssertClasses(t, "GET /api/v1/keys", body, hub.AllClasses())
	keysTestAssertNoHexRun(t, "GET /api/v1/keys", keys)

	network := f.do("GET", "/api/v1/network", "")
	if network.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/network status %d, want 200", network.Code)
	}
	networkBody := decode(t, network)
	keysBlock, ok := networkBody["keys"].(map[string]any)
	if !ok {
		t.Fatalf("GET /api/v1/network: keys = %v, want the key metadata block", networkBody["keys"])
	}
	if configured, _ := keysBlock["configured_classes"].([]any); len(configured) != len(hub.AllClasses()) {
		t.Errorf("GET /api/v1/network: configured_classes = %v, want all %d classes", configured, len(hub.AllClasses()))
	}
	keysTestAssertNoHexRun(t, "GET /api/v1/network", network)
}

// TestKeysExportNeedsTheTwoStepConfirmation proves the export contract of
// contracts/openapi.yaml and the playbook's completion section (FR-013, FR-019):
// the first call returns a token and no value, the second returns the values,
// the audit trail records the export without them, and a spent or foreign token
// is refused.
func TestKeysExportNeedsTheTwoStepConfirmation(t *testing.T) {
	f := newHandlerFixture(t)
	ctx := context.Background()
	submitted := map[string]string{
		hub.SecurityClassS2Authenticated: "00112233445566778899aabbccddeeff",
		hub.SecurityClassS0Legacy:        "ffeeddccbbaa99887766554433221100",
	}
	if rec := f.do("PUT", "/api/v1/keys", keysTestSubmissionBody(t, "", submitted)); rec.Code != http.StatusOK {
		t.Fatalf("submit status %d: %s", rec.Code, rec.Body)
	}

	// Step one: 202, a token, and no value anywhere in the body.
	challenge := f.do("POST", "/api/v1/keys/export", "")
	if challenge.Code != http.StatusAccepted {
		t.Fatalf("export step one status %d, want 202", challenge.Code)
	}
	challengeBody := decode(t, challenge)
	token, _ := challengeBody["token"].(string)
	if token == "" {
		t.Fatal("export step one returned no token")
	}
	if challengeBody["operation"] != "keys.export" {
		t.Errorf("export step one operation = %v, want keys.export", challengeBody["operation"])
	}
	if _, has := challengeBody["keys"]; has {
		t.Error("export step one carries a keys member, want none")
	}
	for class, value := range submitted {
		if strings.Contains(strings.ToLower(challenge.Body.String()), value) {
			t.Errorf("export step one body carries the value of %s", class)
		}
	}

	// A token nobody issued is 404, and does not spend the real one.
	if rec := f.do("POST", "/api/v1/keys/export", `{"confirm_token":"not-a-real-token"}`); rec.Code != http.StatusNotFound {
		t.Errorf("export with an unknown token status %d, want 404", rec.Code)
	} else if code := decode(t, rec)["code"]; code != string(CodeConfirmationInvalid) {
		t.Errorf("export with an unknown token code %v, want %s", code, CodeConfirmationInvalid)
	}

	// Step two: the values, exactly as stored.
	exported := f.do("POST", "/api/v1/keys/export", `{"confirm_token":"`+token+`"}`)
	if exported.Code != http.StatusOK {
		t.Fatalf("export step two status %d, want 200", exported.Code)
	}
	body := decode(t, exported)
	keys, _ := body["keys"].(map[string]any)
	for class, value := range submitted {
		if got, _ := keys[class].(string); !strings.EqualFold(got, value) {
			t.Errorf("export step two: the value for %s is absent or differs from the stored one", class)
		}
	}
	if len(keys) != len(submitted) {
		t.Errorf("export step two carries %d classes, want %d: only configured classes are exported", len(keys), len(submitted))
	}
	if body["exported_at"] == nil || body["warning"] == nil {
		t.Errorf("export step two lacks exported_at or warning: %v", body)
	}

	// The token is single use.
	replay := f.do("POST", "/api/v1/keys/export", `{"confirm_token":"`+token+`"}`)
	if replay.Code != http.StatusGone {
		t.Errorf("export replay status %d, want 410", replay.Code)
	} else if code := decode(t, replay)["code"]; code != string(CodeConfirmationExpired) {
		t.Errorf("export replay code %v, want %s", code, CodeConfirmationExpired)
	}

	// The three outcomes are on record and none of them holds a value.
	page, err := f.store.ListAudit(ctx, store.AuditQuery{Limit: 200})
	if err != nil {
		t.Fatalf("list the audit trail: %v", err)
	}
	seen := map[store.AuditAction]int{}
	for _, entry := range page.Items {
		seen[entry.Action]++
		switch entry.Action {
		case store.AuditKeysExportRequested, store.AuditKeysExported, store.AuditKeysExportRejected:
			text := strings.ToLower(entry.ActorAddr + entry.ActorLabel + entry.CoordinatorID + entry.SecurityClass + string(entry.Outcome))
			detail, _ := json.Marshal(entry.Detail)
			text += strings.ToLower(string(detail))
			for class, value := range submitted {
				if strings.Contains(text, value) {
					t.Errorf("audit row %s carries the value of %s", entry.Action, class)
				}
			}
			if regexp.MustCompile(keysTestKeyValueShape).MatchString(text) {
				t.Errorf("audit row %s carries a key-shaped run", entry.Action)
			}
		}
	}
	for _, action := range []store.AuditAction{store.AuditKeysExportRequested, store.AuditKeysExported, store.AuditKeysExportRejected} {
		if seen[action] == 0 {
			t.Errorf("the audit trail holds no %s row", action)
		}
	}
}
