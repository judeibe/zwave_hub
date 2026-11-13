package coordinator

// IdentityKind says what a coordinator id is bound to (FR-004). A serial
// identity survives any port change; a topology identity survives re-plugging
// into the same port only, which is why the API reports it.
type IdentityKind string

const (
	// IdentitySerial is usb:<vid>:<pid>:<serial>, the normal case.
	IdentitySerial IdentityKind = "serial"
	// IdentityTopology is usb:<vid>:<pid>:port:<topology>, the fallback for a
	// device that reports no usable USB serial.
	IdentityTopology IdentityKind = "topology"
)

// Support is the tri-state verdict of FR-002. Discovery only ever produces
// unverified; the driver's identification makes it final.
type Support string

const (
	// SupportSupported means the driver identified a Z-Wave controller.
	SupportSupported Support = "supported"
	// SupportUnsupported means the device cannot be a coordinator.
	SupportUnsupported Support = "unsupported"
	// SupportUnverified means nothing has identified the device yet.
	SupportUnverified Support = "unverified"
)

// Reason is one support_reason from the closed set of
// contracts/openapi.yaml Coordinator.support_reason (FR-006). The zero value
// means there is no reason to report, which is the supported case.
type Reason string

const (
	// ReasonNotSerialDevice means the node is not a USB serial device.
	ReasonNotSerialDevice Reason = "not_a_serial_device"
	// ReasonNotPassedThrough means sysfs lists the device but no /dev node for
	// it exists in the container (R5).
	ReasonNotPassedThrough Reason = "not_passed_through"
	// ReasonPermissionDenied means the node exists but the hub's user cannot
	// read and write it (R5, R14 §6).
	ReasonPermissionDenied Reason = "permission_denied"
	// ReasonNotZWaveController means the driver opened the device and it did
	// not answer the Serial API.
	ReasonNotZWaveController Reason = "not_a_zwave_controller"
	// ReasonAmbiguousIdentity means another attached device claims the same
	// identity (R14 §11).
	ReasonAmbiguousIdentity Reason = "ambiguous_identity"
	// ReasonIdentificationFailed means identification failed or timed out.
	ReasonIdentificationFailed Reason = "identification_failed"
)

// PathKind says which /dev path a device is reachable at. by_id is the
// /dev/serial/by-id link, which the driver always prefers (R14 §4); tty is the
// raw /dev/tty* node, used only when no link exists.
type PathKind string

const (
	// PathByID is a /dev/serial/by-id link.
	PathByID PathKind = "by_id"
	// PathTTY is a raw /dev/tty* node.
	PathTTY PathKind = "tty"
)

// Attributes are the USB facts an identity is derived from. Serial and
// Topology are the raw sysfs values; Resolve trims surrounding whitespace,
// compares VID and PID case-insensitively and renders them lowercase.
type Attributes struct {
	// VID is the USB vendor id, e.g. "10c4".
	VID string
	// PID is the USB product id, e.g. "ea60".
	PID string
	// Serial is the USB serial string sysfs reports, possibly empty or a
	// placeholder.
	Serial string
	// Topology is the sysfs device path, e.g. "1-2.3".
	Topology string
}

// Identity is the stable hardware identity of one coordinator (FR-004).
type Identity struct {
	// ID is usb:<vid>:<pid>:<serial> or usb:<vid>:<pid>:port:<topology>.
	ID string
	// Kind says whether ID survives a port change.
	Kind IdentityKind
	// Serial is the USB serial ID carries, or empty when the identity is
	// topology-based. It is never a placeholder.
	Serial string
}

// Device is one discovery candidate: a tty node resolved to the USB device
// behind it, with the identity and verdicts derived from that.
type Device struct {
	// TTY is the sysfs tty name, e.g. "ttyUSB0".
	TTY string
	// Attributes are the USB facts the identity comes from.
	Attributes Attributes
	// Manufacturer and Product are the sysfs strings, possibly empty.
	Manufacturer string
	Product      string
	// Path is the node the driver opens, and PathKind says which form it is.
	Path     string
	PathKind PathKind
	// Attached reports that a /dev node for this device exists inside the
	// container. Accessible reports that unix.Access(Path, R_OK|W_OK) passed;
	// it is the no-side-effect permission check of R5, separate from Attached
	// so "not passed through" and "permission denied" stay distinguishable.
	Attached   bool
	Accessible bool
	// Identity is what Resolve derived from Attributes.
	Identity Identity
	// Ambiguous is set by ResolveAll for an identity that more than one
	// attached device claims (R14 §11).
	Ambiguous bool
	// Support is the verdict and Reason its cause. Discovery leaves a device
	// unverified unless it cannot be attached or read.
	Support Support
	Reason  Reason
}

// Resolve derives the identity from the USB attributes alone, so it is known
// before the port is opened (R5): the serial when it is present and not a
// placeholder, the topology path otherwise.
func Resolve(a Attributes) Identity {
	// T021 fills in the derivation.
	return Identity{}
}

// PlaceholderSerial reports whether serial carries no identity: empty, "0001",
// or an all-zero value. A device reporting one of those falls back to the USB
// topology instead of binding a selection to a value many sticks share (R5).
func PlaceholderSerial(serial string) bool {
	// T021 fills in the placeholder rules.
	return false
}

// ResolveAll resolves every device's identity and marks collisions: when more
// than one attached device resolves to the same id, each of them is ambiguous
// and the hub refuses to guess which is which (R14 §11). The returned slice is
// a copy; the input is not modified.
func ResolveAll(devices []Device) []Device {
	// T021 fills in duplicate detection.
	return devices
}

// Selectable reports whether the device may be activated
// (contracts/openapi.yaml Coordinator.selectable): attached, not ambiguous, and
// not unsupported. An unverified device is selectable, because activation is
// what identifies it (FR-003). Accessible is deliberately not consulted: a
// permission-denied device is a connection fault the operator can fix on the
// host (R14 §6), not a final verdict on the hardware.
func Selectable(d Device) bool {
	// T021 fills in the rule.
	return false
}
