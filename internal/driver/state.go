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

// The connection states of data-model.md §8.1, spelled as text because Report
// carries the state as text: this package does not import the hub, whose own
// Connection type holds the same five values. Reports and the bridge use these
// so the two sides of the boundary cannot drift on a spelling.
const (
	// ConnectionNotAttached means no device with the selected identity is
	// present.
	ConnectionNotAttached = "not_attached"
	// ConnectionConnecting means a child is starting for the device.
	ConnectionConnecting = "connecting"
	// ConnectionReady means start_listening answered and the network is known.
	ConnectionReady = "ready"
	// ConnectionRetrying means the last attempt failed and another is
	// scheduled.
	ConnectionRetrying = "retrying"
	// ConnectionFaulted means retrying paused until the device set changes.
	ConnectionFaulted = "faulted"
)

// Situation is one reconcile observation: the hub's committed selection, what
// discovery reports for it, and what the supervisor is running. It is the input
// of Decide and holds no live handle, so it can be built and compared in a test.
type Situation struct {
	// Selected is the active coordinator's identity, empty when nothing is
	// selected.
	Selected string
	// Attached reports that a device with that identity is present, and Usable
	// that the hub may run it: supported, or unverified and about to be
	// identified. Path is where it is attached now.
	Attached bool
	Usable   bool
	Path     string
	// Connection is the connection state the hub last held for the selection.
	Connection string
	// Running is the identity the supervisor holds, empty when it holds none,
	// and RunningPath the path it was started with.
	Running     string
	RunningPath string
	// DeviceSetChanged reports that the settled attached-device set differs
	// from the one a paused supervisor last saw.
	DeviceSetChanged bool
}

// Action is what one reconcile step must do.
type Action int

const (
	// ActionLeave means the child already matches the hub: nothing runs when
	// nothing should, and the running target is still the selected one.
	ActionLeave Action = iota
	// ActionStart spawns a child for the selection, replacing any other child.
	ActionStart
	// ActionResume restarts the supervision loop of a paused target, which is
	// what a settled device-set change asks for.
	ActionResume
	// ActionStop ends the child, because no usable device is selected now.
	ActionStop
)

// String names the action for a test failure message.
func (a Action) String() string {
	switch a {
	case ActionStart:
		return "start"
	case ActionResume:
		return "resume"
	case ActionStop:
		return "stop"
	default:
		return "leave"
	}
}

// Decide derives one reconcile step (data-model.md §9.3). It is a pure function
// of its input, so a settled situation always yields ActionLeave: a repeated
// poll cannot restart a healthy driver, and the status settles instead of
// oscillating (R14 §7).
//
// A selection that is not attached, or is unsupported, runs nothing: the driver
// is started only for a device that is there, which is what keeps a hub with no
// hardware serving (SC-008). The same identity returning — at the same path or,
// because the selection is bound to identity and not to a path, at a new one —
// starts it again with the selection untouched (FR-004, FR-016).
func Decide(in Situation) Action {
	wantRun := in.Selected != "" && in.Attached && in.Usable
	switch {
	case !wantRun:
		if in.Running != "" {
			return ActionStop
		}
		return ActionLeave
	case in.Running != in.Selected || in.RunningPath != in.Path:
		return ActionStart
	case in.Connection == ConnectionFaulted && in.DeviceSetChanged:
		return ActionResume
	}
	return ActionLeave
}

// Detached returns the connection state a coordinator whose device is not
// attached reports, and whether that differs from the state it has now
// (data-model.md §9.3: ready → not_attached on unplug). The selection is never
// touched: a device that leaves leaves the selection behind, and the same
// device is adopted again when it returns (FR-004, FR-016).
func Detached(current string) (string, bool) {
	return ConnectionNotAttached, current != ConnectionNotAttached
}
