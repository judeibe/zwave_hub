package hub

import (
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The fixtures are distinct 32-hex values, one per class, so a complete key set
// carries nothing the duplicate rule has to catch.
const (
	testKeyA = "000102030405060708090a0b0c0d0e0f"
	testKeyB = "101112131415161718191a1b1c1d1e1f"
	testKeyC = "202122232425262728292a2b2c2d2e2f"
	testKeyD = "303132333435363738393a3b3c3d3e3f"
	testKeyE = "404142434445464748494a4b4c4d4e4f"
	testKeyF = "505152535455565758595a5b5c5d5e5f"
)

// assertRefusedSubmission proves a submission was refused atomically with code:
// no keys, no warnings, and a *ValidationError carrying that code, whatever the
// violation (FR-011, SC-005).
func assertRefusedSubmission(t *testing.T, keys map[string][]byte, warnings []string, err error, code string) *ValidationError {
	t.Helper()
	if err == nil {
		t.Fatalf("ValidateKeys accepted the submission, want a %s refusal", code)
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("ValidateKeys returned %v, want a *ValidationError", err)
	}
	if valErr.Code != code {
		t.Errorf("the refusal code is %q, want %q", valErr.Code, code)
	}
	if keys != nil {
		t.Errorf("a refused submission returned keys, want nil: the whole submission is rejected")
	}
	if warnings != nil {
		t.Errorf("a refused submission returned warnings %v, want nil: the whole submission is rejected", warnings)
	}
	return valErr
}

// assertRefusalNamesClasses proves the refusal blames exactly want, in order.
func assertRefusalNamesClasses(t *testing.T, valErr *ValidationError, want []string) {
	t.Helper()
	if !reflect.DeepEqual(valErr.Classes, want) {
		t.Errorf("the refusal names classes %v, want %v", valErr.Classes, want)
	}
}

// assertRefusalText proves the message names its code and every blamed class,
// and echoes no submitted value: key material never appears in a response
// (FR-011, FR-013, SC-004).
func assertRefusalText(t *testing.T, err error, code string, classes, secrets []string) {
	t.Helper()
	text := err.Error()
	if !strings.Contains(text, code) {
		t.Errorf("the refusal text %q does not name the code %s", text, code)
	}
	for _, class := range classes {
		if class == "" {
			continue
		}
		if !strings.Contains(text, class) {
			t.Errorf("the refusal text %q does not name the class %s", text, class)
		}
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(text, secret) {
			t.Errorf("the refusal text %q echoes the submitted value %s", text, secret)
		}
	}
}

// TestValidateClassSets pins the class names and their canonical order: every
// refusal above reports classes in that order, and data-model.md §4 fixes the
// names the API exchanges (FR-007).
func TestValidateClassSets(t *testing.T) {
	names := map[string]string{
		SecurityClassS2AccessControl:   "S2_AccessControl",
		SecurityClassS2Authenticated:   "S2_Authenticated",
		SecurityClassS2Unauthenticated: "S2_Unauthenticated",
		SecurityClassS0Legacy:          "S0_Legacy",
		SecurityClassS2AuthenticatedLR: "S2_Authenticated_LR",
		SecurityClassS2AccessControlLR: "S2_AccessControl_LR",
	}
	for got, want := range names {
		if got != want {
			t.Errorf("a security class constant reads %q, want %q", got, want)
		}
	}

	wantClassic := []string{
		SecurityClassS2AccessControl,
		SecurityClassS2Authenticated,
		SecurityClassS2Unauthenticated,
		SecurityClassS0Legacy,
	}
	if got := ClassicClasses(); !reflect.DeepEqual(got, wantClassic) {
		t.Errorf("ClassicClasses() = %v, want %v", got, wantClassic)
	}

	wantLongRange := []string{
		SecurityClassS2AuthenticatedLR,
		SecurityClassS2AccessControlLR,
	}
	if got := LongRangeClasses(); !reflect.DeepEqual(got, wantLongRange) {
		t.Errorf("LongRangeClasses() = %v, want %v", got, wantLongRange)
	}

	wantAll := append(append([]string(nil), wantClassic...), wantLongRange...)
	if got := AllClasses(); !reflect.DeepEqual(got, wantAll) {
		t.Errorf("AllClasses() = %v, want %v", got, wantAll)
	}
}

// TestValidateKeysAcceptsValidMaterial proves the format rule is exactly 32
// hexadecimal characters, case-insensitively, and that what comes back is the
// 16 bytes those characters encode (FR-011).
func TestValidateKeysAcceptsValidMaterial(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "lowercase", value: testKeyA},
		{name: "uppercase", value: strings.ToUpper(testKeyA)},
		{name: "mixed case", value: "0f1E2d3C4b5A69788796a5B4c3D2e1F0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, decodeErr := hex.DecodeString(tt.value)
			if decodeErr != nil {
				t.Fatalf("the fixture %q is not hex: %v", tt.value, decodeErr)
			}
			if len(want) != 16 {
				t.Fatalf("the fixture %q decodes to %d bytes, want 16", tt.value, len(want))
			}

			keys, warnings, err := ValidateKeys(map[string]string{SecurityClassS2Authenticated: tt.value})
			if err != nil {
				t.Fatalf("ValidateKeys refused %s material: %v", tt.name, err)
			}
			got, ok := keys[SecurityClassS2Authenticated]
			if !ok {
				t.Fatalf("keys holds no entry for %s: %v", SecurityClassS2Authenticated, keys)
			}
			if len(got) != 16 {
				t.Errorf("%s decodes to %d bytes, want 16", tt.value, len(got))
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s decodes to %x, want %x", tt.value, got, want)
			}
			if len(warnings) != 0 {
				t.Errorf("a single distinct key returned warnings %v, want none", warnings)
			}
		})
	}
}

// TestValidateKeysRejectsInvalidMaterial proves every malformed submission is
// refused whole with invalid_key_material, naming exactly the offending class
// and echoing no value (FR-011, FR-013).
func TestValidateKeysRejectsInvalidMaterial(t *testing.T) {
	tests := []struct {
		name     string
		supplied map[string]string
		offender string
	}{
		{
			name:     "31 characters",
			supplied: map[string]string{SecurityClassS2Authenticated: testKeyA[:31]},
			offender: SecurityClassS2Authenticated,
		},
		{
			name:     "33 characters",
			supplied: map[string]string{SecurityClassS0Legacy: testKeyA + "0"},
			offender: SecurityClassS0Legacy,
		},
		{
			name:     "non-hex character",
			supplied: map[string]string{SecurityClassS2Unauthenticated: testKeyA[:31] + "g"},
			offender: SecurityClassS2Unauthenticated,
		},
		{
			name:     "0x prefix",
			supplied: map[string]string{SecurityClassS2AccessControl: "0x" + testKeyA[:30]},
			offender: SecurityClassS2AccessControl,
		},
		{
			name:     "embedded whitespace",
			supplied: map[string]string{SecurityClassS2Unauthenticated: testKeyA[:16] + " " + testKeyA[17:]},
			offender: SecurityClassS2Unauthenticated,
		},
		{
			name:     "unknown class in the wrong case",
			supplied: map[string]string{"s2_authenticated": testKeyB},
			offender: "s2_authenticated",
		},
		{
			name:     "unknown class",
			supplied: map[string]string{"S3_Imaginary": testKeyB},
			offender: "S3_Imaginary",
		},
		{
			name:     "empty class name",
			supplied: map[string]string{"": testKeyB},
			offender: "",
		},
		{
			name: "one bad value among valid ones",
			supplied: map[string]string{
				SecurityClassS2AccessControl: testKeyA,
				SecurityClassS2Authenticated: "not-a-key",
				SecurityClassS0Legacy:        testKeyC,
			},
			offender: SecurityClassS2Authenticated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys, warnings, err := ValidateKeys(tt.supplied)
			valErr := assertRefusedSubmission(t, keys, warnings, err, CodeInvalidKeyMaterial)
			assertRefusalNamesClasses(t, valErr, []string{tt.offender})

			secrets := make([]string, 0, len(tt.supplied))
			for _, value := range tt.supplied {
				secrets = append(secrets, value)
			}
			assertRefusalText(t, err, CodeInvalidKeyMaterial, []string{tt.offender}, secrets)
		})
	}
}

// TestValidateKeysRejectsEmptySubmission proves a submission with no classes is
// refused rather than accepted as a no-op, whether it arrives empty or absent.
func TestValidateKeysRejectsEmptySubmission(t *testing.T) {
	tests := []struct {
		name     string
		supplied map[string]string
	}{
		{name: "empty map", supplied: map[string]string{}},
		{name: "absent map", supplied: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys, warnings, err := ValidateKeys(tt.supplied)
			valErr := assertRefusedSubmission(t, keys, warnings, err, CodeInvalidKeyMaterial)
			if len(valErr.Classes) != 0 {
				t.Errorf("an empty submission blames classes %v, want none", valErr.Classes)
			}
			assertRefusalText(t, err, CodeInvalidKeyMaterial, nil, nil)
		})
	}
}

// TestValidateKeysRejectsDuplicateWithinClassicSet proves a value repeated
// within the classic set is refused, naming both classes in canonical order no
// matter how the map is built or iterated: zwave-js refuses a driver whose
// classic keys repeat (FR-011, R14 §9).
func TestValidateKeysRejectsDuplicateWithinClassicSet(t *testing.T) {
	want := []string{SecurityClassS2AccessControl, SecurityClassS0Legacy}
	orders := [][]string{
		{SecurityClassS2AccessControl, SecurityClassS0Legacy},
		{SecurityClassS0Legacy, SecurityClassS2AccessControl},
	}
	// Go randomises map iteration per range, so repeated calls prove the
	// reported order comes from the canonical class order, not the iteration.
	const runs = 4

	for _, order := range orders {
		t.Run(strings.Join(order, "+"), func(t *testing.T) {
			for range runs {
				supplied := make(map[string]string, len(order))
				for _, class := range order {
					supplied[class] = testKeyA
				}
				keys, warnings, err := ValidateKeys(supplied)
				valErr := assertRefusedSubmission(t, keys, warnings, err, CodeDuplicateKeyMaterial)
				assertRefusalNamesClasses(t, valErr, want)
				assertRefusalText(t, err, CodeDuplicateKeyMaterial, want, []string{testKeyA})
			}
		})
	}
}

// TestValidateKeysRejectsDuplicateWithinLongRangeSet proves the same rule holds
// inside the Long Range set, again naming both classes in canonical order
// (FR-011, R14 §9).
func TestValidateKeysRejectsDuplicateWithinLongRangeSet(t *testing.T) {
	want := []string{SecurityClassS2AuthenticatedLR, SecurityClassS2AccessControlLR}
	orders := [][]string{
		{SecurityClassS2AuthenticatedLR, SecurityClassS2AccessControlLR},
		{SecurityClassS2AccessControlLR, SecurityClassS2AuthenticatedLR},
	}
	const runs = 4

	for _, order := range orders {
		t.Run(strings.Join(order, "+"), func(t *testing.T) {
			for range runs {
				supplied := make(map[string]string, len(order))
				for _, class := range order {
					supplied[class] = testKeyB
				}
				keys, warnings, err := ValidateKeys(supplied)
				valErr := assertRefusedSubmission(t, keys, warnings, err, CodeDuplicateKeyMaterial)
				assertRefusalNamesClasses(t, valErr, want)
				assertRefusalText(t, err, CodeDuplicateKeyMaterial, want, []string{testKeyB})
			}
		})
	}
}

// TestValidateKeysAcceptsCrossSetDuplicateWithWarning proves the two sets are
// judged separately: a value shared between a classic class and its Long Range
// counterpart is accepted, both keys are returned, and exactly one non-blocking
// warning names the pair (R14 §9).
func TestValidateKeysAcceptsCrossSetDuplicateWithWarning(t *testing.T) {
	tests := []struct {
		name    string
		classic string
		lr      string
	}{
		{name: "S2 Authenticated", classic: SecurityClassS2Authenticated, lr: SecurityClassS2AuthenticatedLR},
		{name: "S2 Access Control", classic: SecurityClassS2AccessControl, lr: SecurityClassS2AccessControlLR},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, decodeErr := hex.DecodeString(testKeyB)
			if decodeErr != nil {
				t.Fatalf("the fixture %q is not hex: %v", testKeyB, decodeErr)
			}

			supplied := map[string]string{tt.classic: testKeyB, tt.lr: testKeyB}
			keys, warnings, err := ValidateKeys(supplied)
			if err != nil {
				t.Fatalf("ValidateKeys refused a cross-set duplicate: %v", err)
			}
			if len(keys) != 2 {
				t.Fatalf("keys holds %d entries, want 2: %v", len(keys), keys)
			}
			for _, class := range []string{tt.classic, tt.lr} {
				got, ok := keys[class]
				if !ok {
					t.Fatalf("keys holds no entry for %s: %v", class, keys)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s decodes to %x, want %x", class, got, want)
				}
			}

			if len(warnings) != 1 {
				t.Fatalf("a cross-set duplicate returned %d warnings, want exactly one: %v", len(warnings), warnings)
			}
			wantWarning := tt.classic + " and " + tt.lr + " share the same key value"
			if warnings[0] != wantWarning {
				t.Errorf("the warning reads %q, want %q", warnings[0], wantWarning)
			}
		})
	}
}

// TestValidateKeysAcceptsOperatorSuppliedAllZero proves an all-zero value is
// accepted when an operator supplies it: FR-011 limits refusal to length,
// format and duplicates, and the entropy rule belongs to the generator alone
// (R15 discarded values).
func TestValidateKeysAcceptsOperatorSuppliedAllZero(t *testing.T) {
	const allZero = "00000000000000000000000000000000"

	keys, warnings, err := ValidateKeys(map[string]string{SecurityClassS2AccessControl: allZero})
	if err != nil {
		t.Fatalf("ValidateKeys refused an operator-supplied all-zero key: %v", err)
	}
	got, ok := keys[SecurityClassS2AccessControl]
	if !ok {
		t.Fatalf("keys holds no entry for %s: %v", SecurityClassS2AccessControl, keys)
	}
	if len(got) != 16 {
		t.Errorf("the all-zero key decodes to %d bytes, want 16", len(got))
	}
	for i, b := range got {
		if b != 0 {
			t.Errorf("byte %d of the all-zero key is %#x, want 0x00: %x", i, b, got)
		}
	}
	if len(warnings) != 0 {
		t.Errorf("a single all-zero key returned warnings %v, want none", warnings)
	}
}

// TestValidateErrorNamesCodeAndClasses proves the refusal text is actionable
// and carries no key material: it names the code and every class it blames
// (FR-011, FR-013).
func TestValidateErrorNamesCodeAndClasses(t *testing.T) {
	tests := []struct {
		name   string
		err    *ValidationError
		expect []string
	}{
		{
			name:   "invalid material",
			err:    &ValidationError{Code: CodeInvalidKeyMaterial, Classes: []string{SecurityClassS2Authenticated}},
			expect: []string{CodeInvalidKeyMaterial, SecurityClassS2Authenticated},
		},
		{
			name:   "duplicate material",
			err:    &ValidationError{Code: CodeDuplicateKeyMaterial, Classes: []string{SecurityClassS2AccessControl, SecurityClassS0Legacy}},
			expect: []string{CodeDuplicateKeyMaterial, SecurityClassS2AccessControl, SecurityClassS0Legacy},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := tt.err.Error()
			if text == "" {
				t.Fatal("ValidationError.Error() returned an empty string")
			}
			for _, want := range tt.expect {
				if !strings.Contains(text, want) {
					t.Errorf("ValidationError.Error() = %q, want it to mention %q", text, want)
				}
			}
		})
	}
}

// TestValidateKeysAcceptsAllSixClasses proves a complete distinct key set is
// accepted whole: six entries, each the 16 bytes its value encodes, and no
// warnings to report (FR-007, data-model.md §4).
func TestValidateKeysAcceptsAllSixClasses(t *testing.T) {
	classes := []string{
		SecurityClassS2AccessControl,
		SecurityClassS2Authenticated,
		SecurityClassS2Unauthenticated,
		SecurityClassS0Legacy,
		SecurityClassS2AuthenticatedLR,
		SecurityClassS2AccessControlLR,
	}
	values := []string{testKeyA, testKeyB, testKeyC, testKeyD, testKeyE, testKeyF}

	supplied := make(map[string]string, len(classes))
	for i, class := range classes {
		supplied[class] = values[i]
	}

	keys, warnings, err := ValidateKeys(supplied)
	if err != nil {
		t.Fatalf("ValidateKeys refused a complete distinct key set: %v", err)
	}
	if len(keys) != len(classes) {
		t.Fatalf("keys holds %d classes, want %d: %v", len(keys), len(classes), keys)
	}
	for i, class := range classes {
		want, decodeErr := hex.DecodeString(values[i])
		if decodeErr != nil {
			t.Fatalf("the fixture %q is not hex: %v", values[i], decodeErr)
		}
		got, ok := keys[class]
		if !ok {
			t.Fatalf("keys holds no entry for %s", class)
		}
		if len(got) != 16 {
			t.Errorf("%s decodes to %d bytes, want 16", class, len(got))
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s decodes to %x, want %x", class, got, want)
		}
	}
	if len(warnings) != 0 {
		t.Errorf("a complete distinct key set returned warnings %v, want none", warnings)
	}
}
