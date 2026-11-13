package hub

import (
	"bytes"
	"slices"
	"testing"
)

// testKeyLength is the size of a security key before encryption: 16 bytes, 32
// hex characters (data-model.md §3).
const testKeyLength = 16

// testClassicOrder and testLongRangeOrder are the canonical class orders the
// API documents: the classic four first, then the Long Range pair
// (data-model.md §4).
var (
	testClassicOrder = []string{
		SecurityClassS2AccessControl,
		SecurityClassS2Authenticated,
		SecurityClassS2Unauthenticated,
		SecurityClassS0Legacy,
	}
	testLongRangeOrder = []string{
		SecurityClassS2AuthenticatedLR,
		SecurityClassS2AccessControlLR,
	}
)

// testZeroDraw and testOnesDraw are the two draws the generator rejects and
// redraws (R15).
var (
	testZeroDraw = make([]byte, testKeyLength)
	testOnesDraw = bytes.Repeat([]byte{0xFF}, testKeyLength)
)

// testKeyPattern is a valid, recognisable 16-byte key: 0x00 through 0x0f. It
// begins with a zero byte, so a generator that rejects any leading zero —
// rather than only an all-zero value — fails on it.
var testKeyPattern = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
}

// testAllBytesAre reports whether every byte of value is b. An empty value
// reports false, so a truncated draw is not mistaken for a rejected one.
func testAllBytesAre(value []byte, b byte) bool {
	if len(value) == 0 {
		return false
	}
	for _, got := range value {
		if got != b {
			return false
		}
	}
	return true
}

// testDrawReader returns a reader holding one rejected 16-byte draw followed by
// testKeyPattern, the shape the redraw tests hand to generateFrom.
func testDrawReader(rejected []byte) *bytes.Reader {
	draw := make([]byte, 0, len(rejected)+len(testKeyPattern))
	draw = append(draw, rejected...)
	draw = append(draw, testKeyPattern...)
	return bytes.NewReader(draw)
}

// TestGenerateClassesDefaultsToClassic proves an empty or nil request resolves
// to the classic four in canonical order, never a Long Range class: first
// activation provisions the classic set alone (FR-008, R15).
func TestGenerateClassesDefaultsToClassic(t *testing.T) {
	requests := []struct {
		name    string
		request []string
	}{
		{name: "nil", request: nil},
		{name: "empty", request: []string{}},
	}

	for _, tc := range requests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GenerateClasses(tc.request)
			if err != nil {
				t.Fatalf("GenerateClasses(%v) returned %v, want the classic four", tc.request, err)
			}
			if !slices.Equal(got, testClassicOrder) {
				t.Errorf("GenerateClasses(%v) returned %v, want %v", tc.request, got, testClassicOrder)
			}
			for _, class := range got {
				if slices.Contains(testLongRangeOrder, class) {
					t.Errorf("GenerateClasses(%v) returned the Long Range class %s: Long Range is never generated implicitly (FR-008)", tc.request, class)
				}
			}
		})
	}
}

// TestGenerateClassesKeepsCanonicalOrder proves a mixed, out-of-order request
// comes back in canonical order with the classic classes first, so neither the
// request order nor its gaps reach the driver's option object (FR-010).
func TestGenerateClassesKeepsCanonicalOrder(t *testing.T) {
	request := []string{SecurityClassS0Legacy, SecurityClassS2AuthenticatedLR, SecurityClassS2AccessControl}
	want := []string{SecurityClassS2AccessControl, SecurityClassS0Legacy, SecurityClassS2AuthenticatedLR}

	got, err := GenerateClasses(request)
	if err != nil {
		t.Fatalf("GenerateClasses(%v) returned %v", request, err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("GenerateClasses(%v) returned %v, want %v: the classic classes come first, each set in canonical order", request, got, want)
	}
}

// TestGenerateClassesDeduplicatesAndRejectsUnknown proves a repeated name is
// resolved once and an unknown name is refused with no classes at all: the API
// can only accept the names the driver's option object carries (FR-010).
func TestGenerateClassesDeduplicatesAndRejectsUnknown(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		request := []string{SecurityClassS2Authenticated, SecurityClassS2Authenticated, SecurityClassS0Legacy}
		want := []string{SecurityClassS2Authenticated, SecurityClassS0Legacy}

		got, err := GenerateClasses(request)
		if err != nil {
			t.Fatalf("GenerateClasses(%v) returned %v", request, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("GenerateClasses(%v) returned %v, want %v: a repeated name is resolved once", request, got, want)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		request := []string{SecurityClassS2Authenticated, "S2_Bogus"}

		got, err := GenerateClasses(request)
		if err == nil {
			t.Fatalf("GenerateClasses(%v) returned %v and no error, want the unknown class refused", request, got)
		}
		if len(got) != 0 {
			t.Errorf("a refused request returned %v, want no classes", got)
		}
	})
}

// TestGenerateKeysProducesUsablePairwiseDistinctValues proves the generated
// material is usable: one 16-byte value per classic class, none an all-zero or
// all-0xFF draw, and no two sharing a value within the classic set
// (data-model.md §4, R15).
func TestGenerateKeysProducesUsablePairwiseDistinctValues(t *testing.T) {
	classes, err := GenerateClasses(nil)
	if err != nil {
		t.Fatalf("resolve the default class list: %v", err)
	}

	keys, err := GenerateKeys(classes)
	if err != nil {
		t.Fatalf("GenerateKeys(%v) returned %v", classes, err)
	}
	if len(keys) != len(testClassicOrder) {
		t.Fatalf("GenerateKeys returned %d keys, want %d", len(keys), len(testClassicOrder))
	}

	for _, class := range testClassicOrder {
		value, ok := keys[class]
		if !ok {
			t.Errorf("GenerateKeys returned no key for %s", class)
			continue
		}
		if len(value) != testKeyLength {
			t.Errorf("the key for %s is %d bytes, want %d", class, len(value), testKeyLength)
		}
		if testAllBytesAre(value, 0x00) || testAllBytesAre(value, 0xFF) {
			t.Errorf("the key for %s is %x: a rejected draw must be redrawn, not returned (R15)", class, value)
		}
	}

	for i, left := range testClassicOrder {
		leftValue, hasLeft := keys[left]
		if !hasLeft {
			continue
		}
		for _, right := range testClassicOrder[i+1:] {
			rightValue, hasRight := keys[right]
			if !hasRight {
				continue
			}
			if bytes.Equal(leftValue, rightValue) {
				t.Errorf("the keys for %s and %s are both %x: no two classes within the classic set may share a value",
					left, right, leftValue)
			}
		}
	}

	for _, class := range testLongRangeOrder {
		if _, ok := keys[class]; ok {
			t.Errorf("GenerateKeys generated the Long Range class %s for a classic request: Long Range is opt-in (FR-008)", class)
		}
	}
}

// TestGenerateKeysLongRangeNeedsNaming proves Long Range material is generated
// only when the request names it, and then for that class alone (FR-008,
// FR-010).
func TestGenerateKeysLongRangeNeedsNaming(t *testing.T) {
	keys, err := GenerateKeys([]string{SecurityClassS2AuthenticatedLR})
	if err != nil {
		t.Fatalf("GenerateKeys returned %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("GenerateKeys returned %d keys, want the one named class: %v", len(keys), keys)
	}
	value, ok := keys[SecurityClassS2AuthenticatedLR]
	if !ok {
		t.Fatalf("GenerateKeys returned no key for %s: %v", SecurityClassS2AuthenticatedLR, keys)
	}
	if len(value) != testKeyLength {
		t.Errorf("the key for %s is %d bytes, want %d", SecurityClassS2AuthenticatedLR, len(value), testKeyLength)
	}
	if testAllBytesAre(value, 0x00) || testAllBytesAre(value, 0xFF) {
		t.Errorf("the key for %s is %x: a rejected draw must be redrawn, not returned (R15)", SecurityClassS2AuthenticatedLR, value)
	}
}

// TestGenerateKeysRequiresResolvedClasses proves an unresolved class list is
// refused: GenerateKeys takes the output of GenerateClasses, so an empty list
// is a caller mistake, not a request for the defaults (FR-008).
func TestGenerateKeysRequiresResolvedClasses(t *testing.T) {
	cases := []struct {
		name    string
		classes []string
	}{
		{name: "nil", classes: nil},
		{name: "empty", classes: []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := GenerateKeys(tc.classes)
			if err == nil {
				t.Fatalf("GenerateKeys(%v) returned %d keys and no error, want an error: a resolved class list is required", tc.classes, len(keys))
			}
			if len(keys) != 0 {
				t.Errorf("a refused request returned %v, want no keys", keys)
			}
		})
	}
}

// TestGenerateFromRedrawsRejectedDraw proves the generator never hands back a
// value it rejected: an all-zero or all-0xFF draw is discarded and a fresh one
// taken from the same reader (R15).
func TestGenerateFromRedrawsRejectedDraw(t *testing.T) {
	cases := []struct {
		name     string
		rejected []byte
	}{
		{name: "all zero", rejected: testZeroDraw},
		{name: "all 0xFF", rejected: testOnesDraw},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testDrawReader(tc.rejected)

			keys, err := generateFrom(r, []string{SecurityClassS2Authenticated})
			if err != nil {
				t.Fatalf("generateFrom returned %v after a 16-byte %s draw", err, tc.name)
			}
			got, ok := keys[SecurityClassS2Authenticated]
			if !ok {
				t.Fatalf("generateFrom returned no key for %s: %v", SecurityClassS2Authenticated, keys)
			}
			if !bytes.Equal(got, testKeyPattern) {
				t.Errorf("generateFrom returned %x, want the redrawn %x: the rejected draw must not be returned", got, testKeyPattern)
			}
			if r.Len() != 0 {
				t.Errorf("generateFrom left %d bytes unread, want none: it must consume both the rejected draw and the redraw", r.Len())
			}
		})
	}
}

// TestGenerateFromFailsWithoutPartialMap proves an exhausted reader is an error
// with nothing returned: a key the hub could not draw must never reach the store
// as an unusable value (R15).
func TestGenerateFromFailsWithoutPartialMap(t *testing.T) {
	t.Run("only rejected draws", func(t *testing.T) {
		// Sixteen zero bytes and nothing else: the redraw meets the end of the
		// reader.
		r := bytes.NewReader(testZeroDraw)

		keys, err := generateFrom(r, []string{SecurityClassS2Authenticated})
		if err == nil {
			t.Fatalf("generateFrom returned %v and no error after a single rejected draw", keys)
		}
		if len(keys) != 0 {
			t.Errorf("a failed generation returned %v, want no keys", keys)
		}
	})

	t.Run("second class exhausted", func(t *testing.T) {
		// One valid draw for the first class, then nothing for the second.
		r := bytes.NewReader(testKeyPattern)

		keys, err := generateFrom(r, []string{SecurityClassS2Authenticated, SecurityClassS2AccessControl})
		if err == nil {
			t.Fatalf("generateFrom returned %v and no error with the reader exhausted", keys)
		}
		if len(keys) != 0 {
			t.Errorf("a failed generation returned the partial map %v, want none: an incomplete set is unusable", keys)
		}
	})
}

// TestShouldGenerateOnActivation proves the first-activation trigger fires only
// in the FR-008 state: a ready coordinator, no key rows, no committed revision
// and a controller holding no nodes. A stick that already carries a network
// must never have fresh keys invented for it (FR-008, FR-018, R15).
func TestShouldGenerateOnActivation(t *testing.T) {
	ready := Activation{
		Connection:  ConnectionReady,
		KeyRevision: 0,
		Configured:  0,
		NodeCount:   0,
	}

	cases := []struct {
		name       string
		activation Activation
		want       bool
	}{
		{name: "ready with nothing configured", activation: ready, want: true},
		{name: "one node on the controller", activation: Activation{Connection: ConnectionReady, NodeCount: 1}, want: false},
		{name: "not attached", activation: Activation{Connection: ConnectionNotAttached}, want: false},
		{name: "connecting", activation: Activation{Connection: ConnectionConnecting}, want: false},
		{name: "retrying", activation: Activation{Connection: ConnectionRetrying}, want: false},
		{name: "faulted", activation: Activation{Connection: ConnectionFaulted}, want: false},
		{name: "a revision is committed", activation: Activation{Connection: ConnectionReady, KeyRevision: 1}, want: false},
		{name: "keys are configured", activation: Activation{Connection: ConnectionReady, Configured: 1}, want: false},
		{name: "zero activation", activation: Activation{}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldGenerateOnActivation(tc.activation); got != tc.want {
				t.Errorf("ShouldGenerateOnActivation(%+v) = %t, want %t", tc.activation, got, tc.want)
			}
		})
	}

	// Automatic generation is the classic four only: the Long Range classes
	// exist once an operator asks for them (FR-008, FR-010).
	classes, err := GenerateClasses(nil)
	if err != nil {
		t.Fatalf("resolve the automatic class list: %v", err)
	}
	for _, class := range testLongRangeOrder {
		if slices.Contains(classes, class) {
			t.Errorf("the automatic class list holds the Long Range class %s, want the classic four only", class)
		}
	}
}
