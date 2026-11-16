//go:build contract

package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/judeibe/zwave_hub/internal/driver"
	"github.com/judeibe/zwave_hub/internal/hub"
)

// The real --mock-driver child always reports an empty network, so the states
// below, which decide readiness, cannot be produced by it. Each fixture is a
// recorded start_listening state payload (contracts/zwave-server-sidecar.md §5)
// and the tests feed it through the same two steps the hub takes: ParseState,
// then DeriveReadiness. Nothing here needs node, a sidecar or the network.

// loadFixtureState reads one recorded payload from the fixtures directory and
// parses it as the driver does. A fixture that does not parse is a broken
// fixture, so it fails the test instead of skipping it.
func loadFixtureState(t *testing.T, name string) driver.State {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("fixtures", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if !json.Valid(raw) {
		t.Fatalf("fixture %s is not valid JSON", name)
	}
	state, err := driver.ParseState(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("ParseState(%s): %v", name, err)
	}
	return state
}

// provisionedInput is a hub that holds revision 3 of the key set, runs it, and
// has a ready coordinator on the network the fixture reports. Every case below
// starts from it and changes only what that case is about.
func provisionedInput(state driver.State) hub.ReadinessInput {
	applied := 3
	return hub.ReadinessInput{
		KeyStore:        hub.KeyStoreOK,
		ActiveID:        "usb:10c4:ea60:00a1b2c3",
		Attached:        true,
		Connection:      hub.ConnectionReady,
		LastPath:        "/dev/serial/by-id/usb-10c4_ea60_00a1b2c3-if00-port0",
		HomeID:          state.HomeID,
		ReportedHomeID:  state.HomeID,
		NodeCount:       state.NodeCount,
		KeyRevision:     3,
		AppliedRevision: &applied,
		Configured:      hub.ClassicClasses(),
	}
}

// reasonCodes lists the emitted reason codes in order, for a failure message.
func reasonCodes(reasons []hub.BlockingReason) []hub.BlockingCode {
	codes := make([]hub.BlockingCode, len(reasons))
	for i, reason := range reasons {
		codes[i] = reason.Code
	}
	return codes
}

// findReason returns the reason with the given code.
func findReason(reasons []hub.BlockingReason, code hub.BlockingCode) (hub.BlockingReason, bool) {
	for _, reason := range reasons {
		if reason.Code == code {
			return reason, true
		}
	}
	return hub.BlockingReason{}, false
}

// TestFixtureSecureNodesCountClassesAsTheContractSays proves the §5 rule on a
// recorded payload: S2 (0, 1, 2) and S0 Legacy (7) count as securely included,
// while a node reporting -1 (None) and a node that reports no class do not.
// That count is the FR-014 gate, so a miscount would let an in-use key be
// replaced.
func TestFixtureSecureNodesCountClassesAsTheContractSays(t *testing.T) {
	state := loadFixtureState(t, "secure_nodes.json")

	if state.HomeID != "0xe8d31a4c" {
		t.Errorf("home id = %q, want 0xe8d31a4c", state.HomeID)
	}
	if state.NodeCount != 6 {
		t.Errorf("node count = %d, want 6 (every listed node, secure or not)", state.NodeCount)
	}
	if state.SecureNodeCount != 4 {
		t.Errorf("secure node count = %d, want 4: classes 2, 1, 7 and 0 count, -1 and an absent class do not",
			state.SecureNodeCount)
	}
	if state.Controller.ManufacturerID == nil || *state.Controller.ManufacturerID != 134 {
		t.Errorf("controller manufacturer id = %v, want 134", state.Controller.ManufacturerID)
	}
	if state.Controller.ChipType != "7.0" {
		t.Errorf("controller chip type = %q, want 7.0", state.Controller.ChipType)
	}
}

// TestReadinessSecureNodesBlockedUntilClassicSetComplete proves that a network
// with securely included nodes is not ready while a classic class has no key,
// that the reason names exactly the missing classes in canonical order, and
// that the same state derives ready once the four classic classes are stored
// and their revision is running (FR-007, SC-002). The Long Range pair never
// stands in for a classic class.
func TestReadinessSecureNodesBlockedUntilClassicSetComplete(t *testing.T) {
	state := loadFixtureState(t, "secure_nodes.json")

	cases := []struct {
		name       string
		configured []string
		want       []string
	}{
		{
			name:       "two classic classes missing",
			configured: []string{"S2_AccessControl", "S2_Unauthenticated"},
			want:       []string{"S2_Authenticated", "S0_Legacy"},
		},
		{
			name:       "only S0 Legacy missing",
			configured: []string{"S2_AccessControl", "S2_Authenticated", "S2_Unauthenticated"},
			want:       []string{"S0_Legacy"},
		},
		{
			name:       "long range keys do not stand in for classic ones",
			configured: []string{"S2_Authenticated_LR", "S2_AccessControl_LR", "S2_AccessControl", "S0_Legacy"},
			want:       []string{"S2_Authenticated", "S2_Unauthenticated"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := provisionedInput(state)
			in.Configured = tc.configured

			got := hub.DeriveReadiness(in)
			if got.Ready {
				t.Fatalf("readiness = true with %v missing, want false", tc.want)
			}
			if codes := reasonCodes(got.BlockingReasons); !slices.Equal(codes, []hub.BlockingCode{hub.BlockingKeysMissing}) {
				t.Fatalf("blocking reasons = %v, want exactly [keys_missing]", codes)
			}
			if classes := got.BlockingReasons[0].Classes; !slices.Equal(classes, tc.want) {
				t.Errorf("keys_missing classes = %v, want %v", classes, tc.want)
			}
			if network := hub.DeriveNetworkState(in, got); network != hub.NetworkDegraded {
				t.Errorf("network state = %q, want %q: the coordinator is ready, the keys are not",
					network, hub.NetworkDegraded)
			}
		})
	}

	t.Run("complete classic set with the revision in effect is ready", func(t *testing.T) {
		in := provisionedInput(state)

		got := hub.DeriveReadiness(in)
		if !got.Ready {
			t.Fatalf("readiness = false with blocking reasons %v, want ready", reasonCodes(got.BlockingReasons))
		}
		if len(got.BlockingReasons) != 0 {
			t.Errorf("blocking reasons = %v, want none when ready", reasonCodes(got.BlockingReasons))
		}
		if network := hub.DeriveNetworkState(in, got); network != hub.NetworkReady {
			t.Errorf("network state = %q, want %q", network, hub.NetworkReady)
		}
	})
}

// TestReadinessExistingNetworkIsReportedNotAdopted proves FR-018 on a recorded
// payload: a stick that carries nodes while the hub has never committed a key
// revision is reported as an existing network with its home ID and node count,
// and the state is faulted rather than "nothing is provisioned yet".
func TestReadinessExistingNetworkIsReportedNotAdopted(t *testing.T) {
	state := loadFixtureState(t, "existing_network.json")
	if state.NodeCount == 0 || state.HomeID == "" {
		t.Fatalf("fixture state = %+v, want a network with nodes", state)
	}

	in := hub.ReadinessInput{
		KeyStore:       hub.KeyStoreOK,
		ActiveID:       "usb:10c4:ea60:00a1b2c3",
		Attached:       true,
		Connection:     hub.ConnectionReady,
		ReportedHomeID: state.HomeID,
		NodeCount:      state.NodeCount,
	}

	got := hub.DeriveReadiness(in)
	if got.Ready {
		t.Fatal("readiness = true for a stick carrying an unknown network, want false")
	}
	reason, found := findReason(got.BlockingReasons, hub.BlockingExistingNetworkDetected)
	if !found {
		t.Fatalf("blocking reasons = %v, want existing_network_detected", reasonCodes(got.BlockingReasons))
	}
	if reason.HomeID != "0x2b7e4419" {
		t.Errorf("existing_network_detected home id = %q, want 0x2b7e4419", reason.HomeID)
	}
	if reason.NodeCount != 3 {
		t.Errorf("existing_network_detected node count = %d, want 3", reason.NodeCount)
	}
	if network := hub.DeriveNetworkState(in, got); network != hub.NetworkFaulted {
		t.Errorf("network state = %q, want %q", network, hub.NetworkFaulted)
	}

	// The same stick is not an unknown network once the hub has committed a
	// revision for it: the detection is about keys the hub never held.
	applied := 1
	known := in
	known.HomeID = state.HomeID
	known.KeyRevision = 1
	known.AppliedRevision = &applied
	known.Configured = hub.ClassicClasses()
	after := hub.DeriveReadiness(known)
	if _, found := findReason(after.BlockingReasons, hub.BlockingExistingNetworkDetected); found {
		t.Errorf("blocking reasons = %v after a revision was committed, want no existing_network_detected",
			reasonCodes(after.BlockingReasons))
	}
}

// TestReadinessNetworkMismatchNamesBothHomeIDs proves FR-003 on a recorded
// payload: a coordinator reporting another home ID than the hub's is blocked
// with network_mismatch, and the reason carries both identifiers so the
// operator can see which network the stick belongs to.
func TestReadinessNetworkMismatchNamesBothHomeIDs(t *testing.T) {
	state := loadFixtureState(t, "network_mismatch.json")

	in := provisionedInput(state)
	in.HomeID = "0x2b7e4419"

	got := hub.DeriveReadiness(in)
	if got.Ready {
		t.Fatal("readiness = true for a coordinator on another network, want false")
	}
	if codes := reasonCodes(got.BlockingReasons); !slices.Equal(codes, []hub.BlockingCode{hub.BlockingNetworkMismatch}) {
		t.Fatalf("blocking reasons = %v, want exactly [network_mismatch]", codes)
	}
	reason := got.BlockingReasons[0]
	if reason.ExpectedHomeID != "0x2b7e4419" {
		t.Errorf("network_mismatch expected home id = %q, want the hub's 0x2b7e4419", reason.ExpectedHomeID)
	}
	if reason.ActualHomeID != "0x5f3a9c21" {
		t.Errorf("network_mismatch actual home id = %q, want the coordinator's 0x5f3a9c21", reason.ActualHomeID)
	}
	if network := hub.DeriveNetworkState(in, got); network != hub.NetworkDegraded {
		t.Errorf("network state = %q, want %q", network, hub.NetworkDegraded)
	}
}

// TestFixtureEmptyNetworkIsNotAnExistingNetwork proves that a stick with no
// network at all (homeId 0, no nodes) yields no home ID and no nodes, so
// nothing is treated as an existing network and first activation may proceed
// (FR-008, FR-018).
func TestFixtureEmptyNetworkIsNotAnExistingNetwork(t *testing.T) {
	state := loadFixtureState(t, "empty_network.json")

	if state.HomeID != "" {
		t.Errorf("home id = %q, want empty: homeId 0 is a stick with no network", state.HomeID)
	}
	if state.NodeCount != 0 {
		t.Errorf("node count = %d, want 0", state.NodeCount)
	}
	if state.SecureNodeCount != 0 {
		t.Errorf("secure node count = %d, want 0", state.SecureNodeCount)
	}

	in := hub.ReadinessInput{
		KeyStore:       hub.KeyStoreOK,
		ActiveID:       "usb:10c4:ea60:00a1b2c3",
		Attached:       true,
		Connection:     hub.ConnectionReady,
		ReportedHomeID: state.HomeID,
		NodeCount:      state.NodeCount,
	}
	if hub.DetectExistingNetwork(in) {
		t.Error("DetectExistingNetwork = true for a stick with no network, want false")
	}
	got := hub.DeriveReadiness(in)
	if _, found := findReason(got.BlockingReasons, hub.BlockingExistingNetworkDetected); found {
		t.Errorf("blocking reasons = %v, want no existing_network_detected", reasonCodes(got.BlockingReasons))
	}
	if _, found := findReason(got.BlockingReasons, hub.BlockingNetworkMismatch); found {
		t.Errorf("blocking reasons = %v, want no network_mismatch: there is no reported home id to differ",
			reasonCodes(got.BlockingReasons))
	}
	if network := hub.DeriveNetworkState(in, got); network != hub.NetworkUnprovisioned {
		t.Errorf("network state = %q, want %q: no network on record and no revision committed",
			network, hub.NetworkUnprovisioned)
	}
}
