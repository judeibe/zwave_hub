package coordinator

import "testing"

// unverifiedDevice is a discovery candidate before ResolveAll has run, which
// is the state discovery produces: attributes known, identity not yet derived,
// and the unverified verdict every device carries until identification.
func unverifiedDevice(tty string, a Attributes, attached, accessible bool) Device {
	return Device{
		TTY:        tty,
		Attributes: a,
		Attached:   attached,
		Accessible: accessible,
		Support:    SupportUnverified,
	}
}

// TestResolveSerialIdentity proves the normal case of FR-004: a reported USB
// serial becomes usb:<vid>:<pid>:<serial> from normalised attributes, so one
// stick serialises to one id whatever case or padding sysfs reports.
func TestResolveSerialIdentity(t *testing.T) {
	tests := []struct {
		name string
		attr Attributes
		want Identity
	}{
		{
			name: "lowercase attributes",
			attr: Attributes{VID: "10c4", PID: "ea60", Serial: "00a1b2c3", Topology: "1-2.3"},
			want: Identity{ID: "usb:10c4:ea60:00a1b2c3", Kind: IdentitySerial, Serial: "00a1b2c3"},
		},
		{
			name: "uppercase vid and pid render lowercase",
			attr: Attributes{VID: "10C4", PID: "EA60", Serial: "00a1b2c3", Topology: "1-2.3"},
			want: Identity{ID: "usb:10c4:ea60:00a1b2c3", Kind: IdentitySerial, Serial: "00a1b2c3"},
		},
		{
			name: "surrounding whitespace is trimmed from every field",
			attr: Attributes{VID: " 10C4\n", PID: "\tEA60 ", Serial: "  00a1b2c3\t", Topology: " 1-2.3\n"},
			want: Identity{ID: "usb:10c4:ea60:00a1b2c3", Kind: IdentitySerial, Serial: "00a1b2c3"},
		},
		{
			name: "topology does not appear in a serial identity",
			attr: Attributes{VID: "10c4", PID: "ea60", Serial: "00a1b2c3", Topology: ""},
			want: Identity{ID: "usb:10c4:ea60:00a1b2c3", Kind: IdentitySerial, Serial: "00a1b2c3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.attr)
			if got != tt.want {
				t.Errorf("Resolve(%+v) = %+v, want %+v", tt.attr, got, tt.want)
			}
		})
	}
}

// TestResolveTopologyFallback proves the weaker identity of FR-004's caveat: a
// device with no usable serial resolves to usb:<vid>:<pid>:port:<topology>,
// reports that kind, and carries no serial at all.
func TestResolveTopologyFallback(t *testing.T) {
	tests := []struct {
		name string
		attr Attributes
		want Identity
	}{
		{
			name: "empty serial",
			attr: Attributes{VID: "10c4", PID: "ea60", Topology: "1-2.3"},
			want: Identity{ID: "usb:10c4:ea60:port:1-2.3", Kind: IdentityTopology},
		},
		{
			name: "whitespace-only serial",
			attr: Attributes{VID: "10c4", PID: "ea60", Serial: " \n\t", Topology: "1-2.3"},
			want: Identity{ID: "usb:10c4:ea60:port:1-2.3", Kind: IdentityTopology},
		},
		{
			name: "placeholder 0001",
			attr: Attributes{VID: "10c4", PID: "ea60", Serial: "0001", Topology: "1-2.3"},
			want: Identity{ID: "usb:10c4:ea60:port:1-2.3", Kind: IdentityTopology},
		},
		{
			name: "all-zero serial",
			attr: Attributes{VID: "10c4", PID: "ea60", Serial: "0000000000", Topology: "1-2.3"},
			want: Identity{ID: "usb:10c4:ea60:port:1-2.3", Kind: IdentityTopology},
		},
		{
			name: "single zero serial",
			attr: Attributes{VID: "10c4", PID: "ea60", Serial: "0", Topology: "1-2.3"},
			want: Identity{ID: "usb:10c4:ea60:port:1-2.3", Kind: IdentityTopology},
		},
		{
			name: "placeholder padded with whitespace",
			attr: Attributes{VID: "10c4", PID: "ea60", Serial: " 0001\n", Topology: "1-2.3"},
			want: Identity{ID: "usb:10c4:ea60:port:1-2.3", Kind: IdentityTopology},
		},
		{
			name: "uppercase vid and pid with topology",
			attr: Attributes{VID: "10C4", PID: "EA60", Topology: "1-2.3"},
			want: Identity{ID: "usb:10c4:ea60:port:1-2.3", Kind: IdentityTopology},
		},
		{
			name: "topology padded with whitespace",
			attr: Attributes{VID: "10c4", PID: "ea60", Topology: " 1-2.3\n"},
			want: Identity{ID: "usb:10c4:ea60:port:1-2.3", Kind: IdentityTopology},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.attr)
			if got != tt.want {
				t.Errorf("Resolve(%+v) = %+v, want %+v", tt.attr, got, tt.want)
			}
		})
	}
}

// TestPlaceholderSerial pins the values that carry no identity (R5): empty,
// "0001", and all-zero serials. Anything else is real, including the near
// misses "00010" and "0002", so a shared value never becomes an identity.
func TestPlaceholderSerial(t *testing.T) {
	tests := []struct {
		name   string
		serial string
		want   bool
	}{
		{name: "empty serial", serial: "", want: true},
		{name: "placeholder 0001", serial: "0001", want: true},
		{name: "all-zero serial", serial: "0000000000", want: true},
		{name: "single zero serial", serial: "0", want: true},
		{name: "real serial", serial: "00a1b2c3", want: false},
		{name: "00010 is neither 0001 nor all zeros", serial: "00010", want: false},
		{name: "0002 is not a known placeholder", serial: "0002", want: false},
		{name: "000100 is not all zeros", serial: "000100", want: false},
		{name: "single one", serial: "1", want: false},
		{name: "0001 with a trailing hex digit", serial: "0001a", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlaceholderSerial(tt.serial); got != tt.want {
				t.Errorf("PlaceholderSerial(%q) = %t, want %t", tt.serial, got, tt.want)
			}
		})
	}
}

// TestResolveAllMarksAmbiguous proves R14 §11: when two attached devices
// resolve to the same id, both are ambiguous, unverified with
// ambiguous_identity, and unselectable, while a detached duplicate or a third
// distinct device leaves a device selectable.
func TestResolveAllMarksAmbiguous(t *testing.T) {
	stick := Attributes{VID: "10c4", PID: "ea60", Serial: "00a1b2c3", Topology: "1-2.3"}
	other := Attributes{VID: "10c4", PID: "ea60", Serial: "00a1b2c4", Topology: "1-2.4"}
	portA := Attributes{VID: "10c4", PID: "ea60", Topology: "1-2.3"}
	portB := Attributes{VID: "10c4", PID: "ea60", Topology: "1-2.4"}

	type want struct {
		id         string
		ambiguous  bool
		support    Support
		reason     Reason
		selectable bool
	}

	tests := []struct {
		name    string
		devices []Device
		want    []want
	}{
		{
			name:    "one attached device",
			devices: []Device{unverifiedDevice("ttyUSB0", stick, true, true)},
			want: []want{
				{id: "usb:10c4:ea60:00a1b2c3", support: SupportUnverified, selectable: true},
			},
		},
		{
			name: "two attached devices with different serials",
			devices: []Device{
				unverifiedDevice("ttyUSB0", stick, true, true),
				unverifiedDevice("ttyUSB1", other, true, true),
			},
			want: []want{
				{id: "usb:10c4:ea60:00a1b2c3", support: SupportUnverified, selectable: true},
				{id: "usb:10c4:ea60:00a1b2c4", support: SupportUnverified, selectable: true},
			},
		},
		{
			name: "two attached devices with the same cloned serial",
			devices: []Device{
				unverifiedDevice("ttyUSB0", stick, true, true),
				unverifiedDevice("ttyUSB1", stick, true, true),
			},
			want: []want{
				{
					id:        "usb:10c4:ea60:00a1b2c3",
					ambiguous: true,
					support:   SupportUnverified,
					reason:    ReasonAmbiguousIdentity,
				},
				{
					id:        "usb:10c4:ea60:00a1b2c3",
					ambiguous: true,
					support:   SupportUnverified,
					reason:    ReasonAmbiguousIdentity,
				},
			},
		},
		{
			name: "a cloned pair does not taint a third distinct device",
			devices: []Device{
				unverifiedDevice("ttyUSB0", stick, true, true),
				unverifiedDevice("ttyUSB1", stick, true, true),
				unverifiedDevice("ttyUSB2", other, true, true),
			},
			want: []want{
				{
					id:        "usb:10c4:ea60:00a1b2c3",
					ambiguous: true,
					support:   SupportUnverified,
					reason:    ReasonAmbiguousIdentity,
				},
				{
					id:        "usb:10c4:ea60:00a1b2c3",
					ambiguous: true,
					support:   SupportUnverified,
					reason:    ReasonAmbiguousIdentity,
				},
				{id: "usb:10c4:ea60:00a1b2c4", support: SupportUnverified, selectable: true},
			},
		},
		{
			name: "an unplugged duplicate does not make an attached device ambiguous",
			devices: []Device{
				unverifiedDevice("ttyUSB0", stick, true, true),
				unverifiedDevice("ttyUSB1", stick, false, false),
			},
			want: []want{
				{id: "usb:10c4:ea60:00a1b2c3", support: SupportUnverified, selectable: true},
				{id: "usb:10c4:ea60:00a1b2c3", support: SupportUnverified},
			},
		},
		{
			name: "two serial-less devices claiming the same topology",
			devices: []Device{
				unverifiedDevice("ttyUSB0", portA, true, true),
				unverifiedDevice("ttyUSB1", portA, true, true),
			},
			want: []want{
				{
					id:        "usb:10c4:ea60:port:1-2.3",
					ambiguous: true,
					support:   SupportUnverified,
					reason:    ReasonAmbiguousIdentity,
				},
				{
					id:        "usb:10c4:ea60:port:1-2.3",
					ambiguous: true,
					support:   SupportUnverified,
					reason:    ReasonAmbiguousIdentity,
				},
			},
		},
		{
			name: "two serial-less devices on different ports",
			devices: []Device{
				unverifiedDevice("ttyUSB0", portA, true, true),
				unverifiedDevice("ttyUSB1", portB, true, true),
			},
			want: []want{
				{id: "usb:10c4:ea60:port:1-2.3", support: SupportUnverified, selectable: true},
				{id: "usb:10c4:ea60:port:1-2.4", support: SupportUnverified, selectable: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveAll(tt.devices)
			if len(got) != len(tt.want) {
				t.Fatalf("ResolveAll returned %d devices, want %d", len(got), len(tt.want))
			}
			for i, w := range tt.want {
				g := got[i]
				if g.Identity.ID != w.id {
					t.Errorf("device %d id = %q, want %q", i, g.Identity.ID, w.id)
				}
				if g.Ambiguous != w.ambiguous {
					t.Errorf("device %d ambiguous = %t, want %t", i, g.Ambiguous, w.ambiguous)
				}
				if g.Support != w.support {
					t.Errorf("device %d support = %q, want %q", i, g.Support, w.support)
				}
				if g.Reason != w.reason {
					t.Errorf("device %d reason = %q, want %q", i, g.Reason, w.reason)
				}
				if sel := Selectable(g); sel != w.selectable {
					t.Errorf("device %d selectable = %t, want %t", i, sel, w.selectable)
				}
			}
		})
	}
}

// TestResolveAllReturnsACopy proves ResolveAll writes the resolved identities
// and ambiguity flags into a new slice: discovery publishes the same candidate
// slice to readers, so the caller's elements must stay untouched.
func TestResolveAllReturnsACopy(t *testing.T) {
	attr := Attributes{VID: "10c4", PID: "ea60", Serial: "00a1b2c3", Topology: "1-2.3"}
	in := []Device{
		unverifiedDevice("ttyUSB0", attr, true, true),
		unverifiedDevice("ttyUSB1", attr, true, true),
	}

	out := ResolveAll(in)

	for i, d := range in {
		if d.Identity != (Identity{}) {
			t.Errorf("input device %d identity = %+v, want the zero value: ResolveAll modified the caller's slice", i, d.Identity)
		}
		if d.Ambiguous {
			t.Errorf("input device %d ambiguous = true: ResolveAll modified the caller's slice", i)
		}
		if d.Reason != "" {
			t.Errorf("input device %d reason = %q, want empty: ResolveAll modified the caller's slice", i, d.Reason)
		}
	}

	if len(out) != len(in) {
		t.Fatalf("ResolveAll returned %d devices, want %d", len(out), len(in))
	}
	if !out[0].Ambiguous || !out[1].Ambiguous {
		t.Errorf("returned devices are not both ambiguous: %+v", out)
	}
	if out[0].Identity.ID != "usb:10c4:ea60:00a1b2c3" {
		t.Errorf("returned device 0 id = %q, want %q", out[0].Identity.ID, "usb:10c4:ea60:00a1b2c3")
	}
}

// TestSelectable proves the activation gate of data-model.md §2 and the API's
// selectable description: only an attached, unambiguous device that is not
// unsupported may be activated, and an unverified device may, because
// activation identifies it (FR-003). Accessible is not part of the verdict: a
// permission-denied device is a connection fault the operator fixes on the host
// (R14 §6), so it stays selectable.
func TestSelectable(t *testing.T) {
	tests := []struct {
		name   string
		device Device
		want   bool
	}{
		{
			name:   "supported and accessible",
			device: Device{Attached: true, Accessible: true, Support: SupportSupported},
			want:   true,
		},
		{
			name:   "unverified is selectable because activation identifies it",
			device: Device{Attached: true, Accessible: true, Support: SupportUnverified},
			want:   true,
		},
		{
			name:   "unsupported is not selectable",
			device: Device{Attached: true, Accessible: true, Support: SupportUnsupported},
			want:   false,
		},
		{
			name:   "detached is not selectable",
			device: Device{Attached: false, Accessible: true, Support: SupportSupported},
			want:   false,
		},
		{
			name:   "ambiguous is not selectable",
			device: Device{Attached: true, Accessible: true, Ambiguous: true, Support: SupportUnverified},
			want:   false,
		},
		{
			name:   "inaccessible supported device stays selectable",
			device: Device{Attached: true, Accessible: false, Support: SupportSupported},
			want:   true,
		},
		{
			name:   "inaccessible unverified device stays selectable",
			device: Device{Attached: true, Accessible: false, Support: SupportUnverified},
			want:   true,
		},
		{
			name:   "zero value is not selectable",
			device: Device{},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Selectable(tt.device); got != tt.want {
				t.Errorf("Selectable(%+v) = %t, want %t", tt.device, got, tt.want)
			}
		})
	}
}
