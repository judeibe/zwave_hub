package driver

import (
	"encoding/json"
	"errors"
	"fmt"
)

// State is what the hub reads from a start_listening or controller.get_state
// payload (contracts/zwave-server-sidecar.md §5). Every field is optional in
// the payload: the mock driver reports only controller.homeId.
type State struct {
	// HomeID is 0x followed by eight lowercase hex digits, or empty while no
	// network is configured.
	HomeID string
	// NodeCount is the number of nodes the driver lists.
	NodeCount int
	// SecureNodeCount counts nodes whose highestSecurityClass is S2 or S0
	// (the FR-014 gate).
	SecureNodeCount int
	// Controller is the controller identity, with nil where the payload did
	// not carry a field.
	Controller Controller
}

// Controller is the Z-Wave module identity shown in the coordinator list.
type Controller struct {
	ManufacturerID  *int
	ProductType     *int
	ProductID       *int
	FirmwareVersion string
	SDKVersion      string
	ChipType        string
}

// Security classes that count as securely included (§5): 0..2 are S2
// Unauthenticated/Authenticated/AccessControl and 7 is S0 Legacy. -1 is None.
func secureClass(class int) bool {
	return (class >= 0 && class <= 2) || class == 7
}

type statePayload struct {
	Driver     json.RawMessage `json:"driver"`
	Controller *struct {
		HomeID          *int64          `json:"homeId"`
		ManufacturerID  *int            `json:"manufacturerId"`
		ProductType     *int            `json:"productType"`
		ProductID       *int            `json:"productId"`
		FirmwareVersion string          `json:"firmwareVersion"`
		SDKVersion      string          `json:"sdkVersion"`
		ChipType        json.RawMessage `json:"zwaveChipType"`
	} `json:"controller"`
	Nodes []struct {
		NodeID               int  `json:"nodeId"`
		HighestSecurityClass *int `json:"highestSecurityClass"`
	} `json:"nodes"`
}

// ParseState maps a state payload to State. A payload without a controller
// object is malformed; one with a controller but no home ID is a stick with no
// network.
func ParseState(raw json.RawMessage) (State, error) {
	var payload statePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return State{}, fmt.Errorf("driver: decode state: %w", err)
	}
	if payload.Controller == nil {
		return State{}, errors.New("driver: state has no controller")
	}
	c := payload.Controller
	state := State{
		NodeCount: len(payload.Nodes),
		Controller: Controller{
			ManufacturerID:  c.ManufacturerID,
			ProductType:     c.ProductType,
			ProductID:       c.ProductID,
			FirmwareVersion: c.FirmwareVersion,
			SDKVersion:      c.SDKVersion,
			ChipType:        chipTypeText(c.ChipType),
		},
	}
	if c.HomeID != nil && *c.HomeID != 0 {
		state.HomeID = FormatHomeID(*c.HomeID)
	}
	for _, node := range payload.Nodes {
		if node.HighestSecurityClass != nil && secureClass(*node.HighestSecurityClass) {
			state.SecureNodeCount++
		}
	}
	return state, nil
}

// FormatHomeID renders a home ID as 0x plus eight lowercase hex digits. The
// driver reports it as an unsigned 32-bit number that may arrive negative when
// a signed bitwise operation produced it, so only the low 32 bits count.
func FormatHomeID(id int64) string {
	return fmt.Sprintf("0x%08x", uint32(id))
}

// chipTypeText renders zwaveChipType, which is an object like
// {"type":7,"version":0} on real sticks and absent below schema 47.
func chipTypeText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var chip struct {
		Type    int `json:"type"`
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &chip); err != nil {
		return ""
	}
	return fmt.Sprintf("%d.%d", chip.Type, chip.Version)
}
