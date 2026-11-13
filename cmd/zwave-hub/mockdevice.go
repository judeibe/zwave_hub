package main

import (
	"github.com/judeibe/zwave_hub/internal/coordinator"
)

// mockDevice is the one stick mock mode reports. There is no sysfs on a
// development Mac, so discovery is replaced by this fixed device, which is the
// "discovery for real, minus sysfs" of quickstart.md §6. The serial is fake and
// the path names no real node: mock mode never opens it, because the child runs
// with --mock-driver.
func mockDevice() coordinator.Device {
	const serial = "mock0001"
	return coordinator.Device{
		TTY: "mock0",
		Attributes: coordinator.Attributes{
			VID: "10c4", PID: "ea60", Serial: serial,
		},
		Manufacturer: "zwave-hub",
		Product:      "mock driver",
		Path:         "/dev/serial/by-id/usb-zwave-hub_mock-" + serial,
		PathKind:     coordinator.PathByID,
		Attached:     true,
		Accessible:   true,
		Identity: coordinator.Identity{
			ID:     "usb:10c4:ea60:" + serial,
			Kind:   coordinator.IdentitySerial,
			Serial: serial,
		},
		Support: coordinator.SupportUnverified,
	}
}
