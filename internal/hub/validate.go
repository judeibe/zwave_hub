package hub

import (
	"bytes"
	"encoding/hex"
	"slices"
	"strings"
)

// The six security classes, spelled exactly as data-model.md §4 fixes them. A
// class name is a wire value: a client that sends one of these names gets the
// class it asked for, and any other spelling is a different, unknown class.
const (
	SecurityClassS2AccessControl   = "S2_AccessControl"
	SecurityClassS2Authenticated   = "S2_Authenticated"
	SecurityClassS2Unauthenticated = "S2_Unauthenticated"
	SecurityClassS0Legacy          = "S0_Legacy"
	SecurityClassS2AuthenticatedLR = "S2_Authenticated_LR"
	SecurityClassS2AccessControlLR = "S2_AccessControl_LR"
)

// ClassicClasses returns the four classic classes in canonical order. The
// classic set is what readiness for secure inclusion is judged on (FR-007),
// which is why its order is fixed here rather than left to a caller.
//
// The slice is fresh, so a caller may sort or append to it without disturbing
// the order every other caller depends on.
func ClassicClasses() []string {
	return []string{
		SecurityClassS2AccessControl,
		SecurityClassS2Authenticated,
		SecurityClassS2Unauthenticated,
		SecurityClassS0Legacy,
	}
}

// LongRangeClasses returns the two Long Range classes in canonical order, again
// as a fresh slice. They exist only once an operator provisions them, so they
// are kept apart from the classic set (FR-008).
func LongRangeClasses() []string {
	return []string{
		SecurityClassS2AuthenticatedLR,
		SecurityClassS2AccessControlLR,
	}
}

// AllClasses returns all six classes in canonical order: the classic four
// first, then the Long Range pair. Deterministic order is what lets a refusal
// name the same class every time, whatever order a map is ranged in.
func AllClasses() []string {
	// The classic slice has no spare capacity, so the append allocates and the
	// result shares nothing with either accessor.
	return append(ClassicClasses(), LongRangeClasses()...)
}

// The refusal codes a key submission can carry. They are the `code` values the
// API maps to problem responses (FR-011).
const (
	// CodeInvalidKeyMaterial means a value was not 32 hexadecimal characters,
	// or the submission named a class the hub does not know.
	CodeInvalidKeyMaterial = "invalid_key_material"
	// CodeDuplicateKeyMaterial means two classes of one set were given the
	// same value, which zwave-js refuses to build a driver from (R14 §9).
	CodeDuplicateKeyMaterial = "duplicate_key_material"
)

// ValidationError is a refused key submission. It carries a code and class
// names only: the values it rejected are key material, and FR-013 forbids a key
// value from reaching a response, a log record or an error message.
type ValidationError struct {
	// Code is CodeInvalidKeyMaterial or CodeDuplicateKeyMaterial.
	Code string
	// Classes names the offending class, or both classes of a duplicate, in
	// canonical order. It is nil when the submission named no class at all.
	Classes []string
}

// Error makes ValidationError an error. The text repeats nothing from the
// submission beyond the class names, so it is safe to surface (FR-013).
func (e *ValidationError) Error() string {
	if len(e.Classes) == 0 {
		return "hub: key submission refused: " + e.Code
	}
	return "hub: key submission refused: " + e.Code + " (" + strings.Join(e.Classes, ", ") + ")"
}

// ValidateKeys checks one submission in full and decodes it. It is atomic: any
// violation returns (nil, nil, *ValidationError) and changes nothing, so the
// configuration already stored stays in effect whole (FR-011, SC-005).
//
// A value must be exactly 32 hexadecimal characters, which decode to the 16
// bytes every class uses. Case does not matter; a prefix, a separator or any
// other length is refused. The map may name any of the six classes and need not
// be complete, because provisioning is per class and the classes omitted keep
// what they hold. An empty submission is refused rather than accepted as a
// no-op.
//
// Values are judged per set (data-model.md §3): a repeat inside the classic four
// or inside the Long Range pair is refused, because zwave-js will not construct
// a driver whose keys within one set repeat. A value shared across the two sets
// is legal and returned as a warning naming both classes.
func ValidateKeys(supplied map[string]string) (map[string][]byte, []string, error) {
	if len(supplied) == 0 {
		return nil, nil, &ValidationError{Code: CodeInvalidKeyMaterial}
	}

	// Known classes are judged in canonical order and unknown names after
	// them, so a submission carrying several violations always reports the
	// same one instead of whichever class the map happened to range first.
	keys := make(map[string][]byte, len(supplied))
	for _, class := range AllClasses() {
		value, ok := supplied[class]
		if !ok {
			continue
		}
		decoded, ok := decodeKeyMaterial(value)
		if !ok {
			return nil, nil, &ValidationError{Code: CodeInvalidKeyMaterial, Classes: []string{class}}
		}
		keys[class] = decoded
	}

	if class, ok := firstUnknownClass(supplied); ok {
		return nil, nil, &ValidationError{Code: CodeInvalidKeyMaterial, Classes: []string{class}}
	}

	if pair := firstDuplicate(ClassicClasses(), keys); pair != nil {
		return nil, nil, &ValidationError{Code: CodeDuplicateKeyMaterial, Classes: pair}
	}
	if pair := firstDuplicate(LongRangeClasses(), keys); pair != nil {
		return nil, nil, &ValidationError{Code: CodeDuplicateKeyMaterial, Classes: pair}
	}

	return keys, crossSetWarnings(keys), nil
}

// decodeKeyMaterial decodes one submitted value, or reports that it is not 32
// hexadecimal characters. The length check comes first because hex refuses a
// 31- or 33-character value only after it has been told to decode it; either
// way the value is refused whole, never padded or truncated.
func decodeKeyMaterial(value string) ([]byte, bool) {
	// FR-011 fixes a key at 32 hex characters, which is the 16 bytes of
	// data-model.md §3, so the character count is derived from the byte count
	// rather than written twice.
	const keyLength = 16
	if len(value) != 2*keyLength {
		return nil, false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

// firstUnknownClass returns the lexicographically first class name the hub does
// not know, if any. Lexicographic order is what keeps a submission naming
// several unknown classes deterministic, and the empty name is unknown like any
// other: it is a client's mistake, not an absent entry.
func firstUnknownClass(supplied map[string]string) (string, bool) {
	var unknown []string
	for class := range supplied {
		if !knownClass(class) {
			unknown = append(unknown, class)
		}
	}
	if len(unknown) == 0 {
		return "", false
	}
	slices.Sort(unknown)
	return unknown[0], true
}

// knownClass reports whether class is one of the six names the API exchanges.
// The comparison is exact, so a wrong-case spelling such as s2_authenticated is
// an unknown class rather than a synonym of S2_Authenticated.
func knownClass(class string) bool {
	switch class {
	case SecurityClassS2AccessControl,
		SecurityClassS2Authenticated,
		SecurityClassS2Unauthenticated,
		SecurityClassS0Legacy,
		SecurityClassS2AuthenticatedLR,
		SecurityClassS2AccessControlLR:
		return true
	default:
		return false
	}
}

// firstDuplicate returns the first pair of classes in set that hold the same
// value, both named in canonical order, or nil when every value in the set is
// distinct. The inner loop starts after the outer index, so the pair reported
// is the earliest one in canonical order and never depends on map iteration.
func firstDuplicate(set []string, keys map[string][]byte) []string {
	for i := range len(set) {
		first, ok := keys[set[i]]
		if !ok {
			continue
		}
		for j := i + 1; j < len(set); j++ {
			second, ok := keys[set[j]]
			if !ok {
				continue
			}
			// Decoded bytes are compared, so an upper- and a lowercase
			// spelling of one value collide exactly as two identical
			// spellings do.
			if bytes.Equal(first, second) {
				return []string{set[i], set[j]}
			}
		}
	}
	return nil
}

// crossSetWarnings returns one warning per value that a classic class and a
// Long Range class share, in canonical order. Sharing across the sets is
// permitted — the driver only refuses repeats within one set — but it is often
// not what an operator meant, so it is surfaced as non-blocking information
// (R14 §9). The result is nil when there is nothing to report, which a caller
// can tell from an empty list.
func crossSetWarnings(keys map[string][]byte) []string {
	var warnings []string
	longRange := LongRangeClasses()
	for _, classic := range ClassicClasses() {
		classicKey, ok := keys[classic]
		if !ok {
			continue
		}
		for _, lr := range longRange {
			lrKey, ok := keys[lr]
			if !ok {
				continue
			}
			if bytes.Equal(classicKey, lrKey) {
				warnings = append(warnings, classic+" and "+lr+" share the same key value")
			}
		}
	}
	return warnings
}
