package driver

import (
	"encoding/json"
	"testing"
)

func TestParseStateMock(t *testing.T) {
	state, err := ParseState(json.RawMessage(`{"controller":{"homeId":1},"nodes":[],"driver":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if state.HomeID != "0x00000001" || state.NodeCount != 0 || state.SecureNodeCount != 0 {
		t.Fatalf("state = %+v", state)
	}
}

func TestParseStateSecureCounts(t *testing.T) {
	raw := `{"controller":{"homeId":3735928559,"firmwareVersion":"7.21","manufacturerId":134,"zwaveChipType":{"type":7,"version":0}},
	"nodes":[{"nodeId":1,"highestSecurityClass":2},{"nodeId":2,"highestSecurityClass":7},
	{"nodeId":3,"highestSecurityClass":-1},{"nodeId":4},{"nodeId":5,"highestSecurityClass":0}]}`
	state, err := ParseState(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if state.HomeID != "0xdeadbeef" {
		t.Fatalf("home id = %s", state.HomeID)
	}
	if state.NodeCount != 5 || state.SecureNodeCount != 3 {
		t.Fatalf("counts = %d/%d", state.NodeCount, state.SecureNodeCount)
	}
	if state.Controller.ChipType != "7.0" || state.Controller.FirmwareVersion != "7.21" || *state.Controller.ManufacturerID != 134 {
		t.Fatalf("controller = %+v", state.Controller)
	}
}

func TestParseStateNoNetworkAndMalformed(t *testing.T) {
	state, err := ParseState(json.RawMessage(`{"controller":{},"nodes":[]}`))
	if err != nil || state.HomeID != "" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if _, err := ParseState(json.RawMessage(`{"nodes":[]}`)); err == nil {
		t.Fatal("payload without controller accepted")
	}
}

func TestFormatHomeIDNegative(t *testing.T) {
	if got := FormatHomeID(-1); got != "0xffffffff" {
		t.Fatalf("got %s", got)
	}
}
