package coordinator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The sysfs attribute files this package reads. They belong to the USB device,
// not to the tty class entry: the class directory only names the node, and the
// device behind it carries the attributes (R5).
const (
	attrVID          = "idVendor"
	attrPID          = "idProduct"
	attrSerial       = "serial"
	attrManufacturer = "manufacturer"
	attrProduct      = "product"
)

// Enumerate maps every tty under sysRoot to the USB device behind it and
// returns the USB serial devices, sorted by tty name (R5). sysRoot is injected
// instead of read from a fixed /sys, so tests run against testdata/sys and the
// caller owns the host assumption; nothing here reads the environment.
//
// A tty that does not resolve to a USB device — a platform serial port, a
// dangling class entry — is skipped rather than failing the poll: one odd node
// must not hide every real coordinator. Only an unreadable class directory is
// an error, because then the picture is incomplete in a way the caller has to
// know about.
//
// Enumerate does not touch /dev. Whether the node is visible in the container,
// readable, and preferred at a by-id path is the discovery poller's cross-check
// (T022), so this function answers exactly one question: which USB devices does
// sysfs list?
func Enumerate(sysRoot string) ([]Device, error) {
	root, err := filepath.Abs(sysRoot)
	if err != nil {
		return nil, fmt.Errorf("coordinator: resolve sysfs root %q: %w", sysRoot, err)
	}
	classDir := filepath.Join(root, "class", "tty")
	entries, err := os.ReadDir(classDir)
	if err != nil {
		return nil, fmt.Errorf("coordinator: read %s: %w", classDir, err)
	}

	devices := make([]Device, 0, len(entries))
	for _, entry := range entries {
		device, ok := enumerateTTY(root, classDir, entry.Name())
		if !ok {
			continue
		}
		devices = append(devices, device)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].TTY < devices[j].TTY })
	return devices, nil
}

// enumerateTTY resolves one class entry to its USB device. A class entry is a
// symlink in real sysfs, so every entry is tried and the ones that do not
// resolve are silently skipped.
func enumerateTTY(sysRoot, classDir, tty string) (Device, bool) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(classDir, tty, "device"))
	if err != nil {
		return Device{}, false
	}
	usbDir, ok := usbDeviceDir(sysRoot, resolved)
	if !ok {
		return Device{}, false
	}
	vid, ok := readAttribute(usbDir, attrVID)
	if !ok || vid == "" {
		return Device{}, false
	}
	pid, ok := readAttribute(usbDir, attrPID)
	if !ok || pid == "" {
		return Device{}, false
	}
	serial, _ := readAttribute(usbDir, attrSerial)
	manufacturer, _ := readAttribute(usbDir, attrManufacturer)
	product, _ := readAttribute(usbDir, attrProduct)

	return Device{
		TTY: tty,
		Attributes: Attributes{
			VID:      vid,
			PID:      pid,
			Serial:   serial,
			Topology: filepath.Base(usbDir),
		},
		Manufacturer: manufacturer,
		Product:      product,
		// Discovery can only assert that it has not identified the device
		// yet; the driver makes the verdict final (FR-002).
		Support: SupportUnverified,
	}, true
}

// usbDeviceDir walks up from a tty's device directory to the USB device that
// owns it: the first ancestor carrying both idVendor and idProduct. The walk
// stops at sysRoot, so a platform tty cannot pick up attributes from a USB
// device elsewhere in the tree.
func usbDeviceDir(sysRoot, dir string) (string, bool) {
	for {
		vid, hasVID := readAttribute(dir, attrVID)
		pid, hasPID := readAttribute(dir, attrPID)
		if hasVID && hasPID && vid != "" && pid != "" {
			return dir, true
		}
		if dir == sysRoot {
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// readAttribute reads one sysfs attribute, reporting false when it is absent or
// unreadable. sysfs terminates every value with a newline, so surrounding
// whitespace is trimmed; a missing file is normal and never an error.
func readAttribute(dir, name string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}
