package coordinator

import "strings"

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
// placeholder, the topology path otherwise. Attributes are normalised first —
// sysfs pads its values and a device may report upper-case hex — so the same
// stick serialises to the same id whatever case sysfs happens to use. A serial
// that does carry identity is kept verbatim: it is the vendor's string, and
// changing its case would bind the selection to a different id than the one
// the hub stored.
func Resolve(a Attributes) Identity {
	vid := normaliseHex(a.VID)
	pid := normaliseHex(a.PID)
	serial := strings.TrimSpace(a.Serial)
	if !PlaceholderSerial(serial) {
		return Identity{
			ID:     "usb:" + vid + ":" + pid + ":" + serial,
			Kind:   IdentitySerial,
			Serial: serial,
		}
	}
	return Identity{
		ID:   "usb:" + vid + ":" + pid + ":port:" + strings.TrimSpace(a.Topology),
		Kind: IdentityTopology,
	}
}

// PlaceholderSerial reports whether serial carries no identity: empty, "0001",
// or an all-zero value. A device reporting one of those falls back to the USB
// topology instead of binding a selection to a value many sticks share (R5).
// Surrounding whitespace is ignored, so a padded value sysfs handed through
// still matches; near misses such as "00010" are real serials and do not.
func PlaceholderSerial(serial string) bool {
	s := strings.TrimSpace(serial)
	if s == "" || s == "0001" {
		return true
	}
	for _, r := range s {
		if r != '0' {
			return false
		}
	}
	return true
}

// ResolveAll resolves every device's identity and marks collisions: when more
// than one attached device resolves to the same id, each of them is ambiguous
// and the hub refuses to guess which is which (R14 §11). Only attached devices
// count — an unplugged stick does not make its clone ambiguous — and the
// durable support verdict is left alone, because ambiguity is a property of the
// current device set, not a final judgement of the hardware. The returned slice
// is a copy; the input is not modified.
func ResolveAll(devices []Device) []Device {
	resolved := make([]Device, len(devices))
	copy(resolved, devices)

	counts := make(map[string]int, len(resolved))
	for i := range resolved {
		resolved[i].Identity = Resolve(resolved[i].Attributes)
		if resolved[i].Attached {
			counts[resolved[i].Identity.ID]++
		}
	}
	for i := range resolved {
		if !resolved[i].Attached || counts[resolved[i].Identity.ID] < 2 {
			continue
		}
		resolved[i].Ambiguous = true
		resolved[i].Reason = ReasonAmbiguousIdentity
		if resolved[i].Support != SupportSupported && resolved[i].Support != SupportUnsupported {
			resolved[i].Support = SupportUnverified
		}
	}
	return resolved
}

// Selectable reports whether the device may be activated
// (contracts/openapi.yaml Coordinator.selectable): attached, not ambiguous, and
// not unsupported. An unverified device is selectable, because activation is
// what identifies it (FR-003). Accessible is deliberately not consulted: a
// permission-denied device is a connection fault the operator can fix on the
// host (R14 §6), not a final verdict on the hardware.
func Selectable(d Device) bool {
	if !d.Attached || d.Ambiguous {
		return false
	}
	return d.Support == SupportSupported || d.Support == SupportUnverified
}

// normaliseHex renders a sysfs hex attribute the way the id needs it: trimmed
// and lower-case. Sysfs itself reports lower-case, but a fixture or a future
// kernel is not required to, and an id that changed case between boots would
// break the selection it is supposed to survive.
func normaliseHex(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}
