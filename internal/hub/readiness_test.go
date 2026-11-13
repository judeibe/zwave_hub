package hub

import (
	"slices"
	"testing"
)

// testLastPath is the by-id path the ready input records: the stable name a
// client can act on when the selected stick is unplugged.
const testLastPath = "/dev/serial/by-id/usb-10c4_ea60_00a1b2c3-if00-port0"

// classicClassesInOrder is the canonical classic order (data-model.md §4). It is
// spelled out here rather than taken from ClassicClasses so a wrong canonical
// order fails a test instead of agreeing with itself.
func classicClassesInOrder() []string {
	return []string{"S2_AccessControl", "S2_Authenticated", "S2_Unauthenticated", "S0_Legacy"}
}

// testIntPointer returns a pointer to v, for the nullable revision fields.
func testIntPointer(v int) *int { return &v }

// readyInput is a fully provisioned network, the baseline every blocking-reason
// case mutates: a healthy key store, an attached coordinator whose connection is
// ready on a known network, all four classic classes configured and the stored
// revision in effect (data-model.md §8.3, SC-002).
func readyInput() ReadinessInput {
	return ReadinessInput{
		KeyStore:        KeyStoreOK,
		ActiveID:        "usb:10c4:ea60:00a1b2c3",
		Connection:      ConnectionReady,
		Attached:        true,
		LastPath:        testLastPath,
		HomeID:          "0xdeadbeef",
		ReportedHomeID:  "0xdeadbeef",
		NodeCount:       7,
		KeyRevision:     2,
		AppliedRevision: testIntPointer(2),
		Configured:      ClassicClasses(),
	}
}

// mustDeriveReadiness derives readiness and checks the invariants every state
// must hold: a fixed sentence on every reason, so a client always has something
// to show, and Ready exactly when no reason was emitted (data-model.md §8.3).
func mustDeriveReadiness(t *testing.T, in ReadinessInput) Readiness {
	t.Helper()
	got := DeriveReadiness(in)
	if got.Ready != (len(got.BlockingReasons) == 0) {
		t.Errorf("Ready is %t with blocking reasons %v: readiness is true exactly when no reason is emitted",
			got.Ready, blockingCodes(got.BlockingReasons))
	}
	for _, reason := range got.BlockingReasons {
		if reason.Message == "" {
			t.Errorf("reason %s carries an empty message, want a fixed sentence", reason.Code)
		}
	}
	return got
}

// blockingCodes lists the emitted reason codes in order.
func blockingCodes(reasons []BlockingReason) []BlockingCode {
	codes := make([]BlockingCode, len(reasons))
	for i, reason := range reasons {
		codes[i] = reason.Code
	}
	return codes
}

// assertOnlyReason proves exactly one reason was emitted and that it is want,
// returning it for the caller's field checks.
func assertOnlyReason(t *testing.T, got Readiness, want BlockingCode) BlockingReason {
	t.Helper()
	if got.Ready {
		t.Errorf("readiness is true with %s blocking, want false", want)
	}
	if len(got.BlockingReasons) != 1 {
		t.Fatalf("got blocking reasons %v, want exactly [%s]", blockingCodes(got.BlockingReasons), want)
	}
	reason := got.BlockingReasons[0]
	if reason.Code != want {
		t.Fatalf("got reason %s, want %s", reason.Code, want)
	}
	return reason
}

// assertClasses proves reason carries exactly want in that order.
func assertClasses(t *testing.T, reason BlockingReason, want ...string) {
	t.Helper()
	if !slices.Equal(reason.Classes, want) {
		t.Errorf("%s carries classes %v, want %v in that order", reason.Code, reason.Classes, want)
	}
}

// TestReadyInputIsReady proves the baseline every other case mutates is ready:
// a healthy store, a ready connection on a known network, all classic classes
// configured and the stored revision applied (FR-008, SC-002).
func TestReadyInputIsReady(t *testing.T) {
	in := readyInput()
	if !ClassicComplete(in.Configured) {
		t.Fatalf("the ready input configures %v, which is not the complete classic set", in.Configured)
	}

	got := mustDeriveReadiness(t, in)
	if !got.Ready {
		t.Fatalf("readiness is false, want true: %v", blockingCodes(got.BlockingReasons))
	}
	if len(got.BlockingReasons) != 0 {
		t.Errorf("got blocking reasons %v, want none", blockingCodes(got.BlockingReasons))
	}
}

// TestDeriveReadinessBlockingReasons proves each of the eight reasons of
// data-model.md §8.3 is produced on its own by the one condition that causes it,
// and carries the fields a client needs to act on it.
func TestDeriveReadinessBlockingReasons(t *testing.T) {
	tests := []struct {
		code   BlockingCode
		mutate func(*ReadinessInput)
		verify func(*testing.T, BlockingReason)
	}{
		{
			code: BlockingKeyStoreCorrupt,
			mutate: func(in *ReadinessInput) {
				in.KeyStore = KeyStoreCorrupt
				in.CorruptClasses = []string{"S2_Authenticated", "S0_Legacy"}
			},
			verify: func(t *testing.T, reason BlockingReason) {
				assertClasses(t, reason, "S2_Authenticated", "S0_Legacy")
			},
		},
		{
			code:   BlockingNoActiveCoordinator,
			mutate: func(in *ReadinessInput) { in.ActiveID = "" },
			verify: func(t *testing.T, reason BlockingReason) {
				if reason.LastPath != "" {
					t.Errorf("no_active_coordinator carries last path %q, want none: nothing is selected", reason.LastPath)
				}
			},
		},
		{
			code: BlockingCoordinatorNotAttached,
			mutate: func(in *ReadinessInput) {
				in.Attached = false
				in.Connection = ConnectionNotAttached
			},
			verify: func(t *testing.T, reason BlockingReason) {
				if reason.LastPath != testLastPath {
					t.Errorf("coordinator_not_attached carries last path %q, want %q", reason.LastPath, testLastPath)
				}
			},
		},
		{
			code: BlockingCoordinatorFaulted,
			mutate: func(in *ReadinessInput) {
				in.Connection = ConnectionFaulted
				in.FaultReason = "permission_denied"
			},
			verify: func(t *testing.T, reason BlockingReason) {
				if got, want := reason.Reason, "permission_denied"; got != want {
					t.Errorf("coordinator_faulted carries reason %q, want %q", got, want)
				}
			},
		},
		{
			code:   BlockingNetworkMismatch,
			mutate: func(in *ReadinessInput) { in.ReportedHomeID = "0xfeedface" },
			verify: func(t *testing.T, reason BlockingReason) {
				if got, want := reason.ExpectedHomeID, "0xdeadbeef"; got != want {
					t.Errorf("network_mismatch expects home ID %q, want %q", got, want)
				}
				if got, want := reason.ActualHomeID, "0xfeedface"; got != want {
					t.Errorf("network_mismatch reports home ID %q, want %q", got, want)
				}
			},
		},
		{
			code: BlockingExistingNetworkDetected,
			mutate: func(in *ReadinessInput) {
				in.KeyRevision = 0
				// The driver started with the empty revision, so only the
				// unknown network is left blocking.
				in.AppliedRevision = testIntPointer(0)
				in.NodeCount = 3
			},
			verify: func(t *testing.T, reason BlockingReason) {
				if got, want := reason.HomeID, "0xdeadbeef"; got != want {
					t.Errorf("existing_network_detected reports home ID %q, want %q", got, want)
				}
				if got, want := reason.NodeCount, 3; got != want {
					t.Errorf("existing_network_detected reports %d nodes, want %d", got, want)
				}
			},
		},
		{
			code:   BlockingKeysMissing,
			mutate: func(in *ReadinessInput) { in.Configured = []string{"S2_AccessControl"} },
			verify: func(t *testing.T, reason BlockingReason) {
				assertClasses(t, reason, "S2_Authenticated", "S2_Unauthenticated", "S0_Legacy")
			},
		},
		{
			code:   BlockingKeysNotInEffect,
			mutate: func(in *ReadinessInput) { in.AppliedRevision = nil },
			verify: func(t *testing.T, reason BlockingReason) {
				if got, want := reason.KeyRevision, 2; got != want {
					t.Errorf("keys_not_in_effect reports revision %d, want %d", got, want)
				}
				if reason.AppliedRevision != nil {
					t.Errorf("keys_not_in_effect reports applied revision %d, want none: nothing was applied", *reason.AppliedRevision)
				}
			},
		},
	}

	covered := make(map[BlockingCode]bool, len(tests))
	for _, tt := range tests {
		covered[tt.code] = true
		t.Run(string(tt.code), func(t *testing.T) {
			in := readyInput()
			tt.mutate(&in)

			got := mustDeriveReadiness(t, in)
			reason := assertOnlyReason(t, got, tt.code)
			tt.verify(t, reason)
		})
	}

	// Every code of data-model.md §8.3 has a case, so a renamed or dropped code
	// fails here rather than passing silently.
	for _, code := range []BlockingCode{
		BlockingKeyStoreCorrupt,
		BlockingNoActiveCoordinator,
		BlockingCoordinatorNotAttached,
		BlockingCoordinatorFaulted,
		BlockingNetworkMismatch,
		BlockingExistingNetworkDetected,
		BlockingKeysMissing,
		BlockingKeysNotInEffect,
	} {
		if !covered[code] {
			t.Errorf("no case covers %s", code)
		}
	}
}

// TestDeriveReadinessReasonOrder proves several conditions at once are reported
// in the fixed order of data-model.md §8.3, so a client can act on the first
// reason without scanning the list.
func TestDeriveReadinessReasonOrder(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReadinessInput)
		want   []BlockingCode
	}{
		{
			name: "corrupt store and nothing selected on an unknown network",
			mutate: func(in *ReadinessInput) {
				in.KeyStore = KeyStoreCorrupt
				in.CorruptClasses = []string{"S2_Authenticated"}
				in.ActiveID = ""
				in.KeyRevision = 0
				in.NodeCount = 3
				in.Configured = []string{"S2_AccessControl"}
				// The driver still runs revision 2 while the stored revision
				// is 0, so the revision is not in effect.
				in.AppliedRevision = testIntPointer(2)
			},
			want: []BlockingCode{
				BlockingKeyStoreCorrupt,
				BlockingNoActiveCoordinator,
				BlockingExistingNetworkDetected,
				BlockingKeysMissing,
				BlockingKeysNotInEffect,
			},
		},
		{
			name: "detached stick that belongs to another network",
			mutate: func(in *ReadinessInput) {
				in.Attached = false
				in.Connection = ConnectionNotAttached
				in.ReportedHomeID = "0xfeedface"
				in.KeyRevision = 0
				in.NodeCount = 3
				in.Configured = []string{"S2_AccessControl"}
				in.AppliedRevision = testIntPointer(2)
			},
			want: []BlockingCode{
				BlockingCoordinatorNotAttached,
				BlockingNetworkMismatch,
				BlockingExistingNetworkDetected,
				BlockingKeysMissing,
				BlockingKeysNotInEffect,
			},
		},
		{
			name: "faulted stick on another network with the new keys unapplied",
			mutate: func(in *ReadinessInput) {
				in.Connection = ConnectionFaulted
				in.FaultReason = "permission_denied"
				in.ReportedHomeID = "0xfeedface"
				in.KeyRevision = 1
				in.AppliedRevision = nil
			},
			want: []BlockingCode{
				BlockingCoordinatorFaulted,
				BlockingNetworkMismatch,
				BlockingKeysNotInEffect,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := readyInput()
			tt.mutate(&in)

			got := mustDeriveReadiness(t, in)
			if got.Ready {
				t.Errorf("readiness is true, want false: %v", tt.want)
			}
			if codes := blockingCodes(got.BlockingReasons); !slices.Equal(codes, tt.want) {
				t.Errorf("got reasons %v, want %v in that order (data-model.md §8.3)", codes, tt.want)
			}
		})
	}
}

// TestDeriveReadinessBoundaries proves the edges of the predicate and of
// keys_not_in_effect and existing_network_detected: a missing or stale applied
// revision blocks readiness, one absent classic class is named exactly, and a
// network with no nodes is not an existing network (FR-018).
func TestDeriveReadinessBoundaries(t *testing.T) {
	t.Run("applied revision never recorded", func(t *testing.T) {
		in := readyInput()
		in.AppliedRevision = nil

		got := mustDeriveReadiness(t, in)
		reason := assertOnlyReason(t, got, BlockingKeysNotInEffect)
		if got, want := reason.KeyRevision, 2; got != want {
			t.Errorf("keys_not_in_effect reports revision %d, want %d", got, want)
		}
		if reason.AppliedRevision != nil {
			t.Errorf("keys_not_in_effect reports applied revision %d, want none", *reason.AppliedRevision)
		}
	})

	t.Run("applied revision behind the stored one", func(t *testing.T) {
		in := readyInput()
		in.AppliedRevision = testIntPointer(3)

		got := mustDeriveReadiness(t, in)
		reason := assertOnlyReason(t, got, BlockingKeysNotInEffect)
		if got, want := reason.KeyRevision, 2; got != want {
			t.Errorf("keys_not_in_effect reports revision %d, want %d", got, want)
		}
		if reason.AppliedRevision == nil {
			t.Fatalf("keys_not_in_effect reports no applied revision, want 3")
		}
		if got, want := *reason.AppliedRevision, 3; got != want {
			t.Errorf("keys_not_in_effect reports applied revision %d, want %d", got, want)
		}
	})

	t.Run("one classic class not configured", func(t *testing.T) {
		in := readyInput()
		in.Configured = []string{"S2_AccessControl", "S2_Authenticated", "S2_Unauthenticated"}

		got := mustDeriveReadiness(t, in)
		reason := assertOnlyReason(t, got, BlockingKeysMissing)
		assertClasses(t, reason, "S0_Legacy")
	})

	t.Run("a network with no nodes is not an existing network", func(t *testing.T) {
		in := readyInput()
		in.KeyRevision = 0
		in.AppliedRevision = testIntPointer(0)
		in.NodeCount = 0

		got := mustDeriveReadiness(t, in)
		if codes := blockingCodes(got.BlockingReasons); len(codes) != 0 {
			t.Fatalf("got blocking reasons %v, want none: a network with no nodes is not an existing one", codes)
		}
		if !got.Ready {
			t.Error("readiness is false, want true: every condition of the predicate holds")
		}
	})
}

// TestClassicComplete proves readiness is judged on the four classic classes
// alone: all four make the set complete, each single omission leaves it
// incomplete, and duplicates, Long Range classes and an empty list never count
// toward the classic four (FR-008, SC-002).
func TestClassicComplete(t *testing.T) {
	if got, want := ClassicClasses(), classicClassesInOrder(); !slices.Equal(got, want) {
		t.Fatalf("ClassicClasses() is %v, want the canonical four %v", got, want)
	}

	tests := []struct {
		name       string
		configured []string
		want       bool
		wantMiss   []string
	}{
		{
			name:       "all four",
			configured: ClassicClasses(),
			want:       true,
		},
		{
			name: "all four with duplicates",
			configured: []string{
				"S2_AccessControl", "S2_AccessControl",
				"S2_Authenticated", "S2_Authenticated",
				"S2_Unauthenticated",
				"S0_Legacy", "S0_Legacy",
			},
			want: true,
		},
		{
			name:       "all four beside the Long Range classes",
			configured: append(append([]string{}, ClassicClasses()...), "S2_Authenticated_LR", "S2_AccessControl_LR"),
			want:       true,
		},
		{
			name:       "nothing configured",
			configured: nil,
			want:       false,
			wantMiss:   classicClassesInOrder(),
		},
		{
			name:       "Long Range only",
			configured: []string{"S2_AccessControl_LR", "S2_Authenticated_LR"},
			want:       false,
			wantMiss:   classicClassesInOrder(),
		},
		{
			name:       "only one classic class",
			configured: []string{"S2_AccessControl"},
			want:       false,
			wantMiss:   []string{"S2_Authenticated", "S2_Unauthenticated", "S0_Legacy"},
		},
		{
			name:       "one classic class duplicated",
			configured: []string{"S2_AccessControl", "S2_AccessControl", "S2_Authenticated"},
			want:       false,
			wantMiss:   []string{"S2_Unauthenticated", "S0_Legacy"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassicComplete(tt.configured); got != tt.want {
				t.Errorf("ClassicComplete(%v) = %t, want %t", tt.configured, got, tt.want)
			}

			missing := MissingClassicClasses(tt.configured)
			if !slices.Equal(missing, tt.wantMiss) {
				t.Errorf("MissingClassicClasses(%v) = %v, want %v in canonical order", tt.configured, missing, tt.wantMiss)
			}
			if complete := ClassicComplete(tt.configured); complete != (len(missing) == 0) {
				t.Errorf("ClassicComplete is %t with %d missing classes: completeness is exactly an empty missing list", complete, len(missing))
			}
		})
	}
}

// TestMissingClassicClassesEachOmission proves each of the four classic classes
// is required on its own: omitting exactly one names exactly that class, and a
// Long Range class with the same base never stands in for it (FR-008).
func TestMissingClassicClassesEachOmission(t *testing.T) {
	canonical := classicClassesInOrder()
	for i, class := range canonical {
		t.Run("without "+class, func(t *testing.T) {
			configured := make([]string, 0, len(canonical))
			configured = append(configured, canonical[:i]...)
			configured = append(configured, canonical[i+1:]...)
			// The Long Range counterpart must not satisfy the classic class.
			configured = append(configured, class+"_LR")

			if ClassicComplete(configured) {
				t.Errorf("ClassicComplete(%v) = true, want false with %s absent", configured, class)
			}
			missing := MissingClassicClasses(configured)
			if want := []string{class}; !slices.Equal(missing, want) {
				t.Errorf("MissingClassicClasses(%v) = %v, want exactly %v", configured, missing, want)
			}
		})
	}
}
