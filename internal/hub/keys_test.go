package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/judeibe/zwave_hub/internal/store"
)

// testKeyValue returns the 32 hexadecimal characters a submitted value has to
// be (FR-011), derived from seed so every case has a stable value. Distinctness
// is what matters: ValidateKeys refuses two classes of one set that share a
// value, and the seeds keep them apart. It is a hash of a test seed, never
// anything that could be mistaken for a real key.
func testKeyValue(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:testKeyLength])
}

// mustConfigureKey submits one supplied value for class and fails the test if
// the hub refuses it. Every case here configures its keys this way while the
// network holds no secure devices, which is the state FR-014 lets a change
// through; the ready report that makes the class in use comes afterwards.
func mustConfigureKey(t *testing.T, h *Hub, class, value, requestID string) {
	t.Helper()
	if _, err := h.SubmitKeys(context.Background(), KeySubmission{
		Keys:      map[string]string{class: value},
		RequestID: requestID,
		ActorAddr: testActorAddr,
	}); err != nil {
		t.Fatalf("configure %s while the network holds no secure devices: %v", class, err)
	}
}

// inUseHub returns a hub whose coordinator is ready on a network the driver
// reports as holding secureNodeCount securely included devices, with each class
// in configured already holding a supplied key.
//
// The keys go in before the ready report. A ready report that arrives with
// nothing configured and no nodes is the FR-008 first-activation path, and the
// set it generates would sit on top of the case under test.
func inUseHub(t *testing.T, secureNodeCount int, configured ...string) (*Hub, *store.Store) {
	t.Helper()
	h, st := newTestHub(t)

	dev := testDevice("ttyUSB0", "aabbcc", "usb-1")
	mustSync(t, h, dev)
	mustSelect(t, h, dev.Identity.ID, "req-select")

	for i, class := range configured {
		mustConfigureKey(t, h, class, testKeyValue("configured-"+class), fmt.Sprintf("req-configure-%d", i))
	}

	mustSetConnection(t, h, dev.Identity.ID, ConnectionReady, &Network{
		HomeID: "0xdeadbeef",
		// One non-secure node besides the secure ones, so the report is never
		// the empty network the first-activation trigger looks for.
		NodeCount:       secureNodeCount + 1,
		SecureNodeCount: secureNodeCount,
	})
	return h, st
}

// TestInUseRefusesConfiguredClass proves the FR-014 gate is per class: with
// S0_Legacy configured and two securely included devices on the network, the
// merge that replaces S0_Legacy is refused as a *KeyInUseError naming that class
// and the count, and no other class is implicated.
func TestInUseRefusesConfiguredClass(t *testing.T) {
	h, _ := inUseHub(t, 2, SecurityClassS0Legacy)

	_, err := h.SubmitKeys(context.Background(), KeySubmission{
		Keys:      map[string]string{SecurityClassS0Legacy: testKeyValue("replacement")},
		RequestID: "req-in-use-merge",
		ActorAddr: testActorAddr,
	})

	var inUse *KeyInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("SubmitKeys returned %v, want a *KeyInUseError while 2 secure nodes are protected by S0_Legacy", err)
	}
	if want := []string{SecurityClassS0Legacy}; !slices.Equal(inUse.Classes, want) {
		t.Errorf("the refusal names classes %v, want %v", inUse.Classes, want)
	}
	if inUse.SecureNodeCount != 2 {
		t.Errorf("the refusal reports %d secure nodes, want 2", inUse.SecureNodeCount)
	}
}

// TestInUseMergeRefusesOnlyConfiguredClasses proves a class with no stored key
// is not in use: a merge that names a configured class and an unconfigured one
// is refused for the configured class alone, because supplying matching keys for
// a network the hub has no keys for is the FR-018 path, not a replacement.
func TestInUseMergeRefusesOnlyConfiguredClasses(t *testing.T) {
	h, _ := inUseHub(t, 2, SecurityClassS0Legacy)

	_, err := h.SubmitKeys(context.Background(), KeySubmission{
		Mode: KeyModeMerge,
		Keys: map[string]string{
			SecurityClassS0Legacy:        testKeyValue("merge-configured"),
			SecurityClassS2AccessControl: testKeyValue("merge-unconfigured"),
		},
		RequestID: "req-in-use-merge-mixed",
		ActorAddr: testActorAddr,
	})

	var inUse *KeyInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("SubmitKeys returned %v, want a *KeyInUseError", err)
	}
	if want := []string{SecurityClassS0Legacy}; !slices.Equal(inUse.Classes, want) {
		t.Errorf("the refusal names classes %v, want %v: %s holds no key, so nothing is in use for it",
			inUse.Classes, want, SecurityClassS2AccessControl)
	}
}

// TestInUseReplaceRefusesEveryConfiguredClass proves a replace puts every
// configured class in use, not only the ones it names: a replace writes the
// classes it lists and deletes the configured classes it omits, so both are a
// replacement of a key protecting an included device (FR-009, FR-014).
func TestInUseReplaceRefusesEveryConfiguredClass(t *testing.T) {
	// S2_Authenticated and S0_Legacy are configured; the submission names all
	// four classic classes, so each is being written.
	h, _ := inUseHub(t, 2, SecurityClassS2Authenticated, SecurityClassS0Legacy)

	keys := make(map[string]string, len(ClassicClasses()))
	for _, class := range ClassicClasses() {
		keys[class] = testKeyValue("replace-" + class)
	}
	_, err := h.SubmitKeys(context.Background(), KeySubmission{
		Mode:      KeyModeReplace,
		Keys:      keys,
		RequestID: "req-in-use-replace",
		ActorAddr: testActorAddr,
	})

	var inUse *KeyInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("SubmitKeys returned %v, want a *KeyInUseError", err)
	}
	want := []string{SecurityClassS2Authenticated, SecurityClassS0Legacy}
	if !slices.Equal(inUse.Classes, want) {
		t.Errorf("the refusal names classes %v, want %v: a replace replaces every configured class", inUse.Classes, want)
	}
	if inUse.SecureNodeCount != 2 {
		t.Errorf("the refusal reports %d secure nodes, want 2", inUse.SecureNodeCount)
	}
}

// TestInUseGenerationRefusesConfiguredClass proves regeneration is gated like a
// submission: a configured class cannot be drawn again while included devices
// depend on it. A request that names no class defaults to the classic four, and
// the refusal still names only the configured class among them (FR-010,
// FR-014).
func TestInUseGenerationRefusesConfiguredClass(t *testing.T) {
	cases := []struct {
		name    string
		classes []string
	}{
		{name: "named class", classes: []string{SecurityClassS0Legacy}},
		{name: "default classic set", classes: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := inUseHub(t, 2, SecurityClassS0Legacy)

			_, err := h.GenerateKeys(context.Background(), KeyGeneration{
				Classes:   tc.classes,
				RequestID: "req-in-use-generate",
				ActorAddr: testActorAddr,
			})

			var inUse *KeyInUseError
			if !errors.As(err, &inUse) {
				t.Fatalf("GenerateKeys(%v) returned %v, want a *KeyInUseError", tc.classes, err)
			}
			want := []string{SecurityClassS0Legacy}
			if !slices.Equal(inUse.Classes, want) {
				t.Errorf("the refusal names classes %v, want %v: S0_Legacy is the only requested class that is configured",
					inUse.Classes, want)
			}
			if inUse.SecureNodeCount != 2 {
				t.Errorf("the refusal reports %d secure nodes, want 2", inUse.SecureNodeCount)
			}
		})
	}
}

// TestInUseAllowsUnconfiguredClass proves the gate is about a key being in use,
// not about the network simply having devices: a class with no stored row may
// still be supplied while included devices exist, because that is how matching
// keys are added to a network the hub has no keys for (FR-018).
func TestInUseAllowsUnconfiguredClass(t *testing.T) {
	h, st := inUseHub(t, 2, SecurityClassS0Legacy)
	ctx := context.Background()

	before, err := h.Keys(ctx)
	if err != nil {
		t.Fatalf("read the key configuration: %v", err)
	}

	class := SecurityClassS2AccessControl
	if _, err := h.SubmitKeys(ctx, KeySubmission{
		Keys:      map[string]string{class: testKeyValue("fresh-" + class)},
		RequestID: "req-in-use-fresh-class",
		ActorAddr: testActorAddr,
	}); err != nil {
		t.Fatalf("supply %s while it holds no key and 2 secure nodes exist: %v", class, err)
	}

	row, err := st.GetKey(ctx, class)
	if err != nil {
		t.Fatalf("read the stored row for %s after the accepted submission: %v", class, err)
	}
	if row.Source != store.KeySourceSupplied {
		t.Errorf("the new row's source is %q, want %q", row.Source, store.KeySourceSupplied)
	}
	if len(row.Blob) == 0 {
		t.Errorf("the new row for %s holds no blob", class)
	}

	after, err := h.Keys(ctx)
	if err != nil {
		t.Fatalf("read the key configuration: %v", err)
	}
	if after.Revision != before.Revision+1 {
		t.Errorf("the key revision is %d after the accepted submission, want %d", after.Revision, before.Revision+1)
	}
}

// TestInUseWritesNothing proves the refusal is a no-op: the configured row, the
// key revision and the applied revision are exactly what they were, and the only
// trace is one keys.rejected audit row carrying the reason and the refused
// classes (FR-014, FR-019).
func TestInUseWritesNothing(t *testing.T) {
	h, st := inUseHub(t, 2, SecurityClassS0Legacy)
	ctx := context.Background()

	class := SecurityClassS0Legacy
	beforeRow, err := st.GetKey(ctx, class)
	if err != nil {
		t.Fatalf("read the stored row for %s: %v", class, err)
	}
	before, err := h.Keys(ctx)
	if err != nil {
		t.Fatalf("read the key configuration: %v", err)
	}
	// Record the committed revision as applied, so "the applied revision does
	// not move" compares a value instead of nil against nil.
	applied := before.Revision
	if err := st.SetAppliedRevision(ctx, &applied); err != nil {
		t.Fatalf("record the applied revision: %v", err)
	}
	rowsBefore := len(auditRows(t, st))

	const requestID = "req-in-use-writes-nothing"
	_, err = h.SubmitKeys(ctx, KeySubmission{
		Keys:      map[string]string{class: testKeyValue("refused-write")},
		RequestID: requestID,
		ActorAddr: testActorAddr,
	})
	var inUse *KeyInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("SubmitKeys returned %v, want a *KeyInUseError", err)
	}

	after, err := h.Keys(ctx)
	if err != nil {
		t.Fatalf("read the key configuration: %v", err)
	}
	if after.Revision != before.Revision {
		t.Errorf("the key revision moved from %d to %d: a refusal writes nothing", before.Revision, after.Revision)
	}
	if after.AppliedRevision == nil {
		t.Errorf("the refusal cleared the applied revision, want it left at %d", applied)
	} else if *after.AppliedRevision != applied {
		t.Errorf("the applied revision moved to %d, want %d", *after.AppliedRevision, applied)
	}

	afterRow, err := st.GetKey(ctx, class)
	if err != nil {
		t.Fatalf("read the stored row for %s after the refusal: %v", class, err)
	}
	if !bytes.Equal(afterRow.Blob, beforeRow.Blob) {
		t.Errorf("the stored blob for %s changed: a refusal writes nothing", class)
	}
	if afterRow.Source != beforeRow.Source {
		t.Errorf("the stored source for %s changed from %q to %q", class, beforeRow.Source, afterRow.Source)
	}
	if !afterRow.ChangedAt.Equal(beforeRow.ChangedAt) {
		t.Errorf("the stored change time for %s moved from %s to %s: the row was rewritten",
			class, beforeRow.ChangedAt, afterRow.ChangedAt)
	}

	keysAfter, err := st.ListKeys(ctx)
	if err != nil {
		t.Fatalf("list the stored keys after the refusal: %v", err)
	}
	if len(keysAfter) != 1 {
		t.Errorf("the key set holds %d classes after the refusal, want only the configured one: %v", len(keysAfter), keysAfter)
	}

	rowsAfter := auditRows(t, st)
	if got := len(rowsAfter) - rowsBefore; got != 1 {
		t.Fatalf("the refused submission wrote %d audit rows, want exactly 1", got)
	}
	row := rowsAfter[0]
	if row.Action != store.AuditKeysRejected {
		t.Errorf("the audit row is %s, want %s", row.Action, store.AuditKeysRejected)
	}
	if row.Outcome != store.OutcomeRejected {
		t.Errorf("the audit row has outcome %s, want %s", row.Outcome, store.OutcomeRejected)
	}
	if row.Detail.Reason != CodeKeyInUse {
		t.Errorf("the audit row records reason %q, want %q", row.Detail.Reason, CodeKeyInUse)
	}
	if want := []string{class}; !slices.Equal(row.Detail.Classes, want) {
		t.Errorf("the audit row names classes %v, want %v", row.Detail.Classes, want)
	}
	if row.RequestID != requestID {
		t.Errorf("the audit row carries request %q, want %q", row.RequestID, requestID)
	}
	if row.ActorAddr != testActorAddr {
		t.Errorf("the audit row records actor %q, want %q", row.ActorAddr, testActorAddr)
	}
}

// TestInUseAllowsWithoutSecureNodes proves the gate is judged on included
// devices, not on keys being configured: with zero securely included devices the
// same replacement is accepted, the revision moves and the audit row records a
// replacement rather than a refusal (FR-014).
func TestInUseAllowsWithoutSecureNodes(t *testing.T) {
	h, st := inUseHub(t, 0, SecurityClassS0Legacy)
	ctx := context.Background()

	class := SecurityClassS0Legacy
	beforeRow, err := st.GetKey(ctx, class)
	if err != nil {
		t.Fatalf("read the stored row for %s: %v", class, err)
	}
	before, err := h.Keys(ctx)
	if err != nil {
		t.Fatalf("read the key configuration: %v", err)
	}
	rowsBefore := len(auditRows(t, st))

	if _, err := h.SubmitKeys(ctx, KeySubmission{
		Keys:      map[string]string{class: testKeyValue("no-secure-replacement")},
		RequestID: "req-in-use-no-secure-nodes",
		ActorAddr: testActorAddr,
	}); err != nil {
		t.Fatalf("replace %s while no secure node exists: %v", class, err)
	}

	after, err := h.Keys(ctx)
	if err != nil {
		t.Fatalf("read the key configuration: %v", err)
	}
	if after.Revision != before.Revision+1 {
		t.Errorf("the key revision is %d after the accepted replacement, want %d", after.Revision, before.Revision+1)
	}

	afterRow, err := st.GetKey(ctx, class)
	if err != nil {
		t.Fatalf("read the stored row for %s after the replacement: %v", class, err)
	}
	if bytes.Equal(afterRow.Blob, beforeRow.Blob) {
		t.Errorf("the stored blob for %s is unchanged, want the accepted replacement written", class)
	}

	rows := auditRows(t, st)
	if got := len(rows) - rowsBefore; got != 1 {
		t.Fatalf("the accepted submission wrote %d audit rows, want exactly 1", got)
	}
	if rows[0].Action != store.AuditKeysReplaced {
		t.Errorf("the newest audit row is %s, want %s", rows[0].Action, store.AuditKeysReplaced)
	}
}

// TestInUseErrorNamesResetPath proves the message is written for the operator who
// has to get out of the refusal: it names a reset and the endpoint that starts
// one, because FR-014 makes an explicit reset the only supported path.
func TestInUseErrorNamesResetPath(t *testing.T) {
	h, _ := inUseHub(t, 2, SecurityClassS0Legacy)

	_, err := h.SubmitKeys(context.Background(), KeySubmission{
		Keys:      map[string]string{SecurityClassS0Legacy: testKeyValue("message")},
		RequestID: "req-in-use-message",
		ActorAddr: testActorAddr,
	})

	var inUse *KeyInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("SubmitKeys returned %v, want a *KeyInUseError", err)
	}
	message := inUse.Error()
	if !strings.Contains(message, "reset") {
		t.Errorf("the refusal message %q does not name a reset: FR-014 makes the reset the only supported path", message)
	}
	if !strings.Contains(message, "POST /api/v1/network/reset") {
		t.Errorf("the refusal message %q does not name POST /api/v1/network/reset", message)
	}
}

// TestInvalidKeyWorkIsAudited proves FR-019 attributes refused key work: a
// submission or a generation the validation refuses writes exactly one
// keys.rejected row naming the code and the classes, the stored set is unchanged
// and the row carries no part of the submitted value (FR-013, FR-019, SC-009).
func TestInvalidKeyWorkIsAudited(t *testing.T) {
	ctx := context.Background()
	const invalidValue = "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	const unknownClass = "S9_Bogus"
	cases := []struct {
		name      string
		refuse    func(*Hub) error
		classes   []string
		forbidden string
	}{
		{
			name: "submitted value",
			refuse: func(h *Hub) error {
				_, err := h.SubmitKeys(ctx, KeySubmission{
					Keys:      map[string]string{SecurityClassS2AccessControl: invalidValue},
					RequestID: "req-invalid-value",
					ActorAddr: testActorAddr,
				})
				return err
			},
			classes:   []string{SecurityClassS2AccessControl},
			forbidden: invalidValue,
		},
		{
			name: "generated unknown class",
			refuse: func(h *Hub) error {
				_, err := h.GenerateKeys(ctx, KeyGeneration{
					Classes:   []string{unknownClass},
					RequestID: "req-unknown-class",
					ActorAddr: testActorAddr,
				})
				return err
			},
			classes: []string{unknownClass},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, st := newTestHub(t)
			mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("kept"), "req-configure")
			before, err := h.Keys(ctx)
			if err != nil {
				t.Fatalf("read the key configuration: %v", err)
			}
			rowsBefore := len(auditRows(t, st))

			err = tc.refuse(h)
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("the refused work returned %v, want a *ValidationError", err)
			}

			after, err := h.Keys(ctx)
			if err != nil {
				t.Fatalf("read the key configuration after the refusal: %v", err)
			}
			if after.Revision != before.Revision {
				t.Errorf("the key revision moved from %d to %d: a refusal writes nothing", before.Revision, after.Revision)
			}
			if _, err := st.GetKey(ctx, SecurityClassS2AccessControl); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("S2_AccessControl is stored after the refusal, want no row (err %v)", err)
			}

			rows := auditRows(t, st)
			if got := len(rows) - rowsBefore; got != 1 {
				t.Fatalf("the refused work wrote %d audit rows, want exactly 1", got)
			}
			row := rows[0]
			if row.Action != store.AuditKeysRejected || row.Outcome != store.OutcomeRejected {
				t.Errorf("the audit row is %s/%s, want %s/%s",
					row.Action, row.Outcome, store.AuditKeysRejected, store.OutcomeRejected)
			}
			if row.Detail.Reason != CodeInvalidKeyMaterial {
				t.Errorf("the audit row records reason %q, want %q", row.Detail.Reason, CodeInvalidKeyMaterial)
			}
			if !slices.Equal(row.Detail.Classes, tc.classes) {
				t.Errorf("the audit row names classes %v, want %v", row.Detail.Classes, tc.classes)
			}
			if row.ActorAddr != testActorAddr {
				t.Errorf("the audit row records actor %q, want %q", row.ActorAddr, testActorAddr)
			}
			if row.RequestID == "" {
				t.Error("the audit row carries no request id")
			}
			detail, err := json.Marshal(row.Detail)
			if err != nil {
				t.Fatalf("marshal the audit detail: %v", err)
			}
			if tc.forbidden != "" && strings.Contains(string(detail), tc.forbidden) {
				t.Errorf("the audit detail %s carries the refused value: a row never holds key material", detail)
			}
		})
	}
}
