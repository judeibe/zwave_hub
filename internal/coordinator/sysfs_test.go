package coordinator

import (
	"reflect"
	"testing"
)

// TestEnumerate proves R5's sysfs half: a tty is only a coordinator candidate
// once it resolves to a USB device with an idVendor and idProduct, the
// attributes and topology come from that device, and a tty that resolves to
// something else is skipped instead of failing the poll.
func TestEnumerate(t *testing.T) {
	devices, err := Enumerate("testdata/sys")
	if err != nil {
		t.Fatalf("enumerate testdata/sys: %v", err)
	}

	want := []Device{
		{
			TTY: "ttyUSB0",
			Attributes: Attributes{
				VID:      "10c4",
				PID:      "ea60",
				Serial:   "00a1b2c3",
				Topology: "1-2",
			},
			Manufacturer: "Silicon Labs",
			Product:      "CP2102N USB to UART Bridge Controller",
			Support:      SupportUnverified,
		},
		{
			TTY: "ttyUSB1",
			Attributes: Attributes{
				VID:      "0658",
				PID:      "0200",
				Topology: "1-2.3",
			},
			Manufacturer: "Sigma Designs",
			Product:      "Z-Wave Stick",
			Support:      SupportUnverified,
		},
	}

	if !reflect.DeepEqual(devices, want) {
		t.Errorf("Enumerate(testdata/sys) = %+v, want %+v", devices, want)
	}
}

// TestEnumerateSkipsNonUSBTTY pins the ttyS0 case on its own: a tty that
// resolves to a platform device has no idVendor anywhere in its ancestry, so it
// is not a candidate and must not appear as one with empty attributes.
func TestEnumerateSkipsNonUSBTTY(t *testing.T) {
	devices, err := Enumerate("testdata/sys")
	if err != nil {
		t.Fatalf("enumerate testdata/sys: %v", err)
	}
	for _, d := range devices {
		if d.TTY == "ttyS0" || d.TTY == "ttyS1" {
			t.Errorf("Enumerate returned %q, want the non-USB entries skipped", d.TTY)
		}
	}
}

// TestEnumerateMissingClassDir proves an unreadable sysfs root is an error: the
// poller has to be able to tell "no coordinators" from "cannot look", which is
// the difference between an idle hub and a hub that has lost its host.
func TestEnumerateMissingClassDir(t *testing.T) {
	if _, err := Enumerate(t.TempDir()); err == nil {
		t.Fatal("Enumerate on a tree with no class/tty returned no error")
	}
}
