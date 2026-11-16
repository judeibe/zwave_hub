package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testDiscovered(id string, seen time.Time) DiscoveredCoordinator {
	return DiscoveredCoordinator{
		ID:           id,
		VID:          "10c4",
		PID:          "ea60",
		Serial:       "00a1b2c3",
		IdentityKind: "serial",
		Manufacturer: "Silicon Labs",
		Product:      "CP2102N",
		Path:         "/dev/serial/by-id/usb-a",
		SeenAt:       seen,
	}
}

// TestUpsertCoordinator proves a new id inserts an unverified row and a known
// id refreshes last_seen_at and the path without touching first_seen_at or the
// verdict; a poll that sees no path keeps the last known one.
func TestUpsertCoordinator(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	t1 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)

	isNew, err := s.UpsertCoordinator(ctx, testDiscovered("usb:10c4:ea60:aaa", t1))
	if err != nil || !isNew {
		t.Fatalf("first upsert = (%t, %v), want a new row", isNew, err)
	}
	if _, _, err := s.SetSupport(ctx, "usb:10c4:ea60:aaa", "supported", ""); err != nil {
		t.Fatalf("set support: %v", err)
	}

	d := testDiscovered("usb:10c4:ea60:aaa", t2)
	d.Path = ""
	isNew, err = s.UpsertCoordinator(ctx, d)
	if err != nil || isNew {
		t.Fatalf("second upsert = (%t, %v), want an update", isNew, err)
	}

	got, err := s.GetCoordinator(ctx, "usb:10c4:ea60:aaa")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.FirstSeenAt.Equal(t1) || !got.LastSeenAt.Equal(t2) {
		t.Errorf("first/last seen = %s / %s, want %s / %s", got.FirstSeenAt, got.LastSeenAt, t1, t2)
	}
	if got.LastPath != "/dev/serial/by-id/usb-a" {
		t.Errorf("last_path = %q, want the path kept across a detached poll", got.LastPath)
	}
	if got.Support != "supported" {
		t.Errorf("support = %q after a refresh, want it untouched", got.Support)
	}
	if got.Serial != "00a1b2c3" || got.Manufacturer != "Silicon Labs" {
		t.Errorf("descriptive fields = %+v", got)
	}
}

// TestUpsertCoordinatorNullsPlaceholderSerial proves an empty serial is stored
// as NULL, as data-model.md §2 requires for a topology identity.
func TestUpsertCoordinatorNullsPlaceholderSerial(t *testing.T) {
	s := openTestStore(t)
	d := testDiscovered("usb:10c4:ea60:port:1-2", time.Now())
	d.Serial, d.IdentityKind = "", "topology"
	if _, err := s.UpsertCoordinator(context.Background(), d); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var isNull bool
	if err := s.db.QueryRow(`SELECT usb_serial IS NULL FROM coordinators WHERE id = ?`, d.ID).Scan(&isNull); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !isNull {
		t.Error("usb_serial is not NULL for a topology identity")
	}
}

// TestUpsertCoordinatorRejectsBadInput proves a row the CHECK constraint or the
// identity rules would refuse fails before the statement.
func TestUpsertCoordinatorRejectsBadInput(t *testing.T) {
	s := openTestStore(t)
	for name, mutate := range map[string]func(*DiscoveredCoordinator){
		"no id":      func(d *DiscoveredCoordinator) { d.ID = "" },
		"no vid":     func(d *DiscoveredCoordinator) { d.VID = "" },
		"bad kind":   func(d *DiscoveredCoordinator) { d.IdentityKind = "mac" },
		"empty kind": func(d *DiscoveredCoordinator) { d.IdentityKind = "" },
	} {
		t.Run(name, func(t *testing.T) {
			d := testDiscovered("usb:10c4:ea60:aaa", time.Now())
			mutate(&d)
			if _, err := s.UpsertCoordinator(context.Background(), d); err == nil {
				t.Error("upsert returned no error")
			}
		})
	}
}

// TestSetSupportTransitions proves data-model.md §2: unverified moves to
// supported or unsupported, and nothing moves out of a final verdict — a
// supported device is never downgraded by a later failed open.
func TestSetSupportTransitions(t *testing.T) {
	tests := []struct {
		name        string
		start       string
		to          string
		want        string
		wantChanged bool
	}{
		{"unverified to supported", "unverified", "supported", "supported", true},
		{"unverified to unsupported", "unverified", "unsupported", "unsupported", true},
		{"unverified stays", "unverified", "unverified", "unverified", false},
		{"supported never downgrades", "supported", "unsupported", "supported", false},
		{"supported never returns to unverified", "supported", "unverified", "supported", false},
		{"unsupported is final", "unsupported", "supported", "unsupported", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			id := "usb:10c4:ea60:aaa"
			if _, err := s.UpsertCoordinator(ctx, testDiscovered(id, time.Now())); err != nil {
				t.Fatalf("upsert: %v", err)
			}
			if tt.start != "unverified" {
				if _, _, err := s.SetSupport(ctx, id, tt.start, ""); err != nil {
					t.Fatalf("seed support: %v", err)
				}
			}
			got, changed, err := s.SetSupport(ctx, id, tt.to, "not_a_zwave_controller")
			if err != nil {
				t.Fatalf("set support: %v", err)
			}
			if got != tt.want || changed != tt.wantChanged {
				t.Errorf("SetSupport = (%q, %t), want (%q, %t)", got, changed, tt.want, tt.wantChanged)
			}
		})
	}
}

// TestSetSupportUnknownAndInvalid proves an unknown id is ErrNotFound and a
// verdict outside the closed set is refused.
func TestSetSupportUnknownAndInvalid(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, _, err := s.SetSupport(ctx, "usb:none", "supported", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id error = %v, want ErrNotFound", err)
	}
	if _, _, err := s.SetSupport(ctx, "usb:none", "maybe", ""); err == nil {
		t.Error("an invalid verdict returned no error")
	}
}

// TestListCoordinatorsKeepsUnplugged proves rows persist and sort by id, so a
// device that has gone stays visible (FR-004).
func TestListCoordinatorsKeepsUnplugged(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"usb:10c4:ea60:bbb", "usb:10c4:ea60:aaa"} {
		if _, err := s.UpsertCoordinator(ctx, testDiscovered(id, time.Now())); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	rows, err := s.ListCoordinators(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "usb:10c4:ea60:aaa" || rows[1].ID != "usb:10c4:ea60:bbb" {
		t.Errorf("list = %+v, want both rows sorted by id", rows)
	}
	if _, err := s.GetCoordinator(ctx, "usb:none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get unknown error = %v, want ErrNotFound", err)
	}
}

// TestSetControllerIdentity proves the driver's identification is stored and
// round-trips, with absent numbers staying NULL.
func TestSetControllerIdentity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := "usb:10c4:ea60:aaa"
	if _, err := s.UpsertCoordinator(ctx, testDiscovered(id, time.Now())); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	mfr, ptype := 0x0086, 0x0001
	err := s.SetControllerIdentity(ctx, id, ControllerIdentity{
		FirmwareVersion: "7.21.1", SDKVersion: "7.21.1", ManufacturerID: &mfr, ProductType: &ptype, ChipType: "ZW800",
	})
	if err != nil {
		t.Fatalf("set controller identity: %v", err)
	}
	got, _ := s.GetCoordinator(ctx, id)
	if got.FirmwareVersion != "7.21.1" || got.ZWaveChipType != "ZW800" {
		t.Errorf("identity = %+v", got)
	}
	if got.ZWaveManufacturerID == nil || *got.ZWaveManufacturerID != mfr {
		t.Errorf("manufacturer id = %v, want %d", got.ZWaveManufacturerID, mfr)
	}
	if got.ZWaveProductID != nil {
		t.Errorf("product id = %v, want NULL", *got.ZWaveProductID)
	}
	if err := s.SetControllerIdentity(ctx, "usb:none", ControllerIdentity{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id error = %v, want ErrNotFound", err)
	}
}

// TestProvisioningRoundTrip proves the singleton row's initial state and the
// writers for active coordinator, home id and both revisions, including the
// foreign key that refuses a selection of an unknown coordinator.
func TestProvisioningRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	p, err := s.GetProvisioning(ctx)
	if err != nil {
		t.Fatalf("get provisioning: %v", err)
	}
	if p.ActiveCoordinatorID != "" || p.HomeID != "" || p.KeyRevision != 0 || p.AppliedRevision != nil {
		t.Fatalf("initial provisioning = %+v, want the empty singleton", p)
	}

	id := "usb:10c4:ea60:aaa"
	if _, err := s.UpsertCoordinator(ctx, testDiscovered(id, time.Now())); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	applied := 3
	err = s.WithTx(ctx, func(tx *Tx) error {
		if err := tx.SetActiveCoordinator(ctx, id); err != nil {
			return err
		}
		if err := tx.SetHomeID(ctx, "0xdeadbeef"); err != nil {
			return err
		}
		return tx.SetKeyRevision(ctx, 3)
	})
	if err != nil {
		t.Fatalf("write provisioning: %v", err)
	}
	if err := s.SetAppliedRevision(ctx, &applied); err != nil {
		t.Fatalf("set applied revision: %v", err)
	}

	p, _ = s.GetProvisioning(ctx)
	if p.ActiveCoordinatorID != id || p.HomeID != "0xdeadbeef" || p.KeyRevision != 3 {
		t.Errorf("provisioning = %+v", p)
	}
	if p.AppliedRevision == nil || *p.AppliedRevision != 3 {
		t.Errorf("applied revision = %v, want 3", p.AppliedRevision)
	}

	if err := s.SetAppliedRevision(ctx, nil); err != nil {
		t.Fatalf("clear applied revision: %v", err)
	}
	if p, _ = s.GetProvisioning(ctx); p.AppliedRevision != nil {
		t.Errorf("applied revision = %d after clearing, want NULL", *p.AppliedRevision)
	}

	err = s.WithTx(ctx, func(tx *Tx) error { return tx.SetActiveCoordinator(ctx, "usb:none") })
	if err == nil {
		t.Error("selecting an unknown coordinator returned no error, want the foreign key to refuse it")
	}
}

// TestProvisioningRollsBackWithTx proves selection state commits or rolls back
// with its transaction, which is what lets it share a transaction with the
// audit row.
func TestProvisioningRollsBackWithTx(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	sentinel := errors.New("abort")
	err := s.WithTx(ctx, func(tx *Tx) error {
		if err := tx.SetHomeID(ctx, "0xdeadbeef"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx = %v, want the sentinel", err)
	}
	if p, _ := s.GetProvisioning(ctx); p.HomeID != "" {
		t.Errorf("home id = %q after a rolled-back transaction, want none", p.HomeID)
	}
}
