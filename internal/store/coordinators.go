package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound reports a coordinator id the store has no row for.
var ErrNotFound = errors.New("store: not found")

// coordinatorTimeLayout is the RFC 3339 UTC form data-model.md §2 gives
// first_seen_at and last_seen_at.
const coordinatorTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// CoordinatorRow is one row of coordinators. Nullable columns are plain strings
// and pointers here, because "absent" and "empty" mean the same to every reader
// and the SQL layer binds the empty string as NULL.
type CoordinatorRow struct {
	// ID is usb:<vid>:<pid>:<serial> or usb:<vid>:<pid>:port:<topology>.
	ID string
	// VID and PID are four lowercase hex digits each.
	VID string
	PID string
	// Serial is NULL for an absent or placeholder serial.
	Serial string
	// IdentityKind is serial or topology.
	IdentityKind string
	Manufacturer string
	Product      string

	// The Z-Wave fields stay empty until the driver has identified the
	// controller (data-model.md §2).
	FirmwareVersion     string
	SDKVersion          string
	ZWaveManufacturerID *int
	ZWaveProductType    *int
	ZWaveProductID      *int
	ZWaveChipType       string

	// Support is supported, unsupported or unverified.
	Support string
	// SupportReason is one of the FR-006 codes, or empty.
	SupportReason string
	// LastPath is the preferred /dev path last seen, kept after unplugging.
	LastPath    string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// DiscoveredCoordinator is what one discovery poll learns about a device. It
// carries only facts discovery can know: the verdict, the Z-Wave fields and the
// first-seen time are the store's to keep.
type DiscoveredCoordinator struct {
	ID           string
	VID          string
	PID          string
	Serial       string
	IdentityKind string
	Manufacturer string
	Product      string
	// Path is the preferred path seen this poll; empty leaves last_path alone,
	// so a detached device keeps the path it was last seen at.
	Path string
	// SeenAt is the poll time and becomes last_seen_at.
	SeenAt time.Time
}

// Provisioning is the singleton provisioning row (data-model.md §10).
type Provisioning struct {
	// ActiveCoordinatorID is empty when nothing is selected.
	ActiveCoordinatorID string
	// HomeID is empty when no network identifier is recorded.
	HomeID string
	// KeyRevision counts committed key changes; zero means none.
	KeyRevision int
	// AppliedRevision is the revision in effect on the running driver, or nil
	// when none has been applied.
	AppliedRevision *int
	UpdatedAt       time.Time
}

// Support transitions (data-model.md §2). A verdict only ever leaves
// unverified; a supported device is never downgraded by a later failed open.
const (
	supportSupported   = "supported"
	supportUnsupported = "unsupported"
	supportUnverified  = "unverified"
)

// SupportTransitionAllowed reports whether support may move from one verdict to
// another. Staying put is always allowed.
func SupportTransitionAllowed(from, to string) bool {
	if from == to {
		return true
	}
	return from == supportUnverified && (to == supportSupported || to == supportUnsupported)
}

// queryer is the part of *sql.DB and *sql.Tx the coordinator statements need,
// so one set of statements serves both a Store call and a command's Tx.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const coordinatorColumns = `id, usb_vid, usb_pid, usb_serial, identity_kind, manufacturer, product,
	firmware_version, sdk_version, zwave_manufacturer_id, zwave_product_type, zwave_product_id,
	zwave_chip_type, support, support_reason, last_path, first_seen_at, last_seen_at`

// UpsertCoordinator records one discovered device: a new id inserts an
// unverified row, a known id refreshes its descriptive fields, last_path and
// last_seen_at and never touches the verdict or the Z-Wave fields. It reports
// whether the row is new.
func (s *Store) UpsertCoordinator(ctx context.Context, d DiscoveredCoordinator) (bool, error) {
	return upsertCoordinator(ctx, s.db, d)
}

// UpsertCoordinator is the in-transaction form of Store.UpsertCoordinator.
func (t *Tx) UpsertCoordinator(ctx context.Context, d DiscoveredCoordinator) (bool, error) {
	if t == nil || t.tx == nil {
		return false, errors.New("store: transaction is nil")
	}
	return upsertCoordinator(ctx, t.tx, d)
}

func upsertCoordinator(ctx context.Context, q queryer, d DiscoveredCoordinator) (bool, error) {
	if d.ID == "" || d.VID == "" || d.PID == "" {
		return false, errors.New("store: coordinator id, vid and pid are required")
	}
	if d.IdentityKind != "serial" && d.IdentityKind != "topology" {
		return false, errors.New("store: coordinator identity kind is not serial or topology")
	}
	seen := d.SeenAt
	if seen.IsZero() {
		seen = time.Now()
	}
	at := seen.UTC().Format(coordinatorTimeLayout)

	var exists int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM coordinators WHERE id = ?`, d.ID).Scan(&exists)
	isNew := errors.Is(err, sql.ErrNoRows)
	if err != nil && !isNew {
		return false, fmt.Errorf("store: look up coordinator: %w", err)
	}

	if isNew {
		_, err = q.ExecContext(ctx, `INSERT INTO coordinators
			(id, usb_vid, usb_pid, usb_serial, identity_kind, manufacturer, product,
			 support, last_path, first_seen_at, last_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'unverified', ?, ?, ?)`,
			d.ID, d.VID, d.PID, nullable(d.Serial), d.IdentityKind,
			nullable(d.Manufacturer), nullable(d.Product), nullable(d.Path), at, at)
		if err != nil {
			return false, fmt.Errorf("store: insert coordinator: %w", err)
		}
		return true, nil
	}

	// COALESCE keeps last_path when the device is detached this poll, so the
	// last known path survives an unplug (data-model.md §2).
	_, err = q.ExecContext(ctx, `UPDATE coordinators SET
			manufacturer = ?, product = ?, usb_serial = ?,
			last_path = COALESCE(?, last_path), last_seen_at = ?
		WHERE id = ?`,
		nullable(d.Manufacturer), nullable(d.Product), nullable(d.Serial),
		nullable(d.Path), at, d.ID)
	if err != nil {
		return false, fmt.Errorf("store: update coordinator: %w", err)
	}
	return false, nil
}

// SetSupport applies a verdict to one coordinator under the transition rules.
// It returns the verdict now stored, and whether it changed. A move the rules
// forbid — supported to unsupported, or any move out of a final verdict — is
// not an error and not applied: the stored verdict is returned unchanged, so a
// later failed open cannot downgrade a supported device.
func (s *Store) SetSupport(ctx context.Context, id, support, reason string) (string, bool, error) {
	return setSupport(ctx, s.db, id, support, reason)
}

// SetSupport is the in-transaction form of Store.SetSupport.
func (t *Tx) SetSupport(ctx context.Context, id, support, reason string) (string, bool, error) {
	if t == nil || t.tx == nil {
		return "", false, errors.New("store: transaction is nil")
	}
	return setSupport(ctx, t.tx, id, support, reason)
}

func setSupport(ctx context.Context, q queryer, id, support, reason string) (string, bool, error) {
	switch support {
	case supportSupported, supportUnsupported, supportUnverified:
	default:
		return "", false, errors.New("store: support is not supported, unsupported or unverified")
	}
	var current string
	err := q.QueryRowContext(ctx, `SELECT support FROM coordinators WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read support: %w", err)
	}
	if !SupportTransitionAllowed(current, support) {
		return current, false, nil
	}
	if _, err := q.ExecContext(ctx, `UPDATE coordinators SET support = ?, support_reason = ? WHERE id = ?`,
		support, nullable(reason), id); err != nil {
		return "", false, fmt.Errorf("store: update support: %w", err)
	}
	return support, current != support, nil
}

// SetControllerIdentity records what the driver reported about a coordinator.
// A nil number is stored as NULL.
func (s *Store) SetControllerIdentity(ctx context.Context, id string, c ControllerIdentity) error {
	res, err := s.db.ExecContext(ctx, `UPDATE coordinators SET
			firmware_version = ?, sdk_version = ?, zwave_manufacturer_id = ?,
			zwave_product_type = ?, zwave_product_id = ?, zwave_chip_type = ?
		WHERE id = ?`,
		nullable(c.FirmwareVersion), nullable(c.SDKVersion), c.ManufacturerID,
		c.ProductType, c.ProductID, nullable(c.ChipType), id)
	if err != nil {
		return fmt.Errorf("store: update controller identity: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// ControllerIdentity is the Z-Wave controller identity the driver reports
// after identification.
type ControllerIdentity struct {
	FirmwareVersion string
	SDKVersion      string
	ManufacturerID  *int
	ProductType     *int
	ProductID       *int
	ChipType        string
}

// GetCoordinator returns one row, or ErrNotFound.
func (s *Store) GetCoordinator(ctx context.Context, id string) (CoordinatorRow, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+coordinatorColumns+` FROM coordinators WHERE id = ?`, id)
	c, err := scanCoordinator(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CoordinatorRow{}, ErrNotFound
	}
	return c, err
}

// GetCoordinator is the in-transaction form of Store.GetCoordinator.
func (t *Tx) GetCoordinator(ctx context.Context, id string) (CoordinatorRow, error) {
	if t == nil || t.tx == nil {
		return CoordinatorRow{}, errors.New("store: transaction is nil")
	}
	row := t.tx.QueryRowContext(ctx, `SELECT `+coordinatorColumns+` FROM coordinators WHERE id = ?`, id)
	c, err := scanCoordinator(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CoordinatorRow{}, ErrNotFound
	}
	return c, err
}

// ListCoordinators returns every row ordered by id, including devices that are
// currently unplugged.
func (s *Store) ListCoordinators(ctx context.Context) ([]CoordinatorRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+coordinatorColumns+` FROM coordinators ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list coordinators: %w", err)
	}
	defer rows.Close()
	out := []CoordinatorRow{}
	for rows.Next() {
		c, err := scanCoordinator(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read coordinators: %w", err)
	}
	return out, nil
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCoordinator(r rowScanner) (CoordinatorRow, error) {
	var (
		c                                                  CoordinatorRow
		serial, manufacturer, product, firmware, sdk, chip sql.NullString
		reason, lastPath                                   sql.NullString
		mfrID, prodType, prodID                            sql.NullInt64
		firstSeen, lastSeen                                string
	)
	if err := r.Scan(&c.ID, &c.VID, &c.PID, &serial, &c.IdentityKind, &manufacturer, &product,
		&firmware, &sdk, &mfrID, &prodType, &prodID, &chip, &c.Support, &reason, &lastPath,
		&firstSeen, &lastSeen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CoordinatorRow{}, err
		}
		return CoordinatorRow{}, fmt.Errorf("store: scan coordinator: %w", err)
	}
	c.Serial, c.Manufacturer, c.Product = serial.String, manufacturer.String, product.String
	c.FirmwareVersion, c.SDKVersion, c.ZWaveChipType = firmware.String, sdk.String, chip.String
	c.SupportReason, c.LastPath = reason.String, lastPath.String
	c.ZWaveManufacturerID = nullableInt(mfrID)
	c.ZWaveProductType = nullableInt(prodType)
	c.ZWaveProductID = nullableInt(prodID)

	var err error
	if c.FirstSeenAt, err = time.Parse(time.RFC3339Nano, firstSeen); err != nil {
		return CoordinatorRow{}, fmt.Errorf("store: parse first_seen_at: %w", err)
	}
	if c.LastSeenAt, err = time.Parse(time.RFC3339Nano, lastSeen); err != nil {
		return CoordinatorRow{}, fmt.Errorf("store: parse last_seen_at: %w", err)
	}
	return c, nil
}

// GetProvisioning reads the singleton provisioning row.
func (s *Store) GetProvisioning(ctx context.Context) (Provisioning, error) {
	return getProvisioning(ctx, s.db)
}

// GetProvisioning is the in-transaction form of Store.GetProvisioning.
func (t *Tx) GetProvisioning(ctx context.Context) (Provisioning, error) {
	if t == nil || t.tx == nil {
		return Provisioning{}, errors.New("store: transaction is nil")
	}
	return getProvisioning(ctx, t.tx)
}

func getProvisioning(ctx context.Context, q queryer) (Provisioning, error) {
	var (
		p            Provisioning
		active, home sql.NullString
		applied      sql.NullInt64
		updated      string
	)
	err := q.QueryRowContext(ctx, `SELECT active_coordinator_id, home_id, key_revision,
		applied_revision, updated_at FROM provisioning WHERE id = 1`).
		Scan(&active, &home, &p.KeyRevision, &applied, &updated)
	if err != nil {
		return Provisioning{}, fmt.Errorf("store: read provisioning: %w", err)
	}
	p.ActiveCoordinatorID, p.HomeID = active.String, home.String
	p.AppliedRevision = nullableInt(applied)
	if p.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return Provisioning{}, fmt.Errorf("store: parse provisioning updated_at: %w", err)
	}
	return p, nil
}

// SetActiveCoordinator selects id, or clears the selection when id is empty. An
// unknown id fails on the foreign key rather than recording a dangling
// selection.
func (t *Tx) SetActiveCoordinator(ctx context.Context, id string) error {
	if t == nil || t.tx == nil {
		return errors.New("store: transaction is nil")
	}
	return touchProvisioning(ctx, t.tx, "active_coordinator_id = ?", nullable(id))
}

// SetHomeID records the network identifier the coordinator reported, or clears
// it when homeID is empty.
func (t *Tx) SetHomeID(ctx context.Context, homeID string) error {
	if t == nil || t.tx == nil {
		return errors.New("store: transaction is nil")
	}
	return touchProvisioning(ctx, t.tx, "home_id = ?", nullable(homeID))
}

// SetHomeID is the non-transactional form, for the driver adapter's ready
// report, which is not an audited command.
func (s *Store) SetHomeID(ctx context.Context, homeID string) error {
	return touchProvisioning(ctx, s.db, "home_id = ?", nullable(homeID))
}

// SetKeyRevision records the committed key revision.
func (t *Tx) SetKeyRevision(ctx context.Context, revision int) error {
	if t == nil || t.tx == nil {
		return errors.New("store: transaction is nil")
	}
	if revision < 0 {
		return errors.New("store: key revision is negative")
	}
	return touchProvisioning(ctx, t.tx, "key_revision = ?", revision)
}

// SetAppliedRevision records the revision in effect on the running driver, or
// clears it when revision is nil.
func (s *Store) SetAppliedRevision(ctx context.Context, revision *int) error {
	var v any
	if revision != nil {
		v = *revision
	}
	return touchProvisioning(ctx, s.db, "applied_revision = ?", v)
}

// touchProvisioning updates one column of the singleton row and stamps
// updated_at. set is a fixed fragment written in this file, never client input.
func touchProvisioning(ctx context.Context, q queryer, set string, arg any) error {
	at := time.Now().UTC().Format(coordinatorTimeLayout)
	res, err := q.ExecContext(ctx, `UPDATE provisioning SET `+set+`, updated_at = ? WHERE id = 1`, arg, at)
	if err != nil {
		return fmt.Errorf("store: update provisioning: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return errors.New("store: provisioning row is missing")
	}
	return nil
}

// nullable binds the empty string as SQL NULL.
func nullable(v string) sql.NullString {
	return sql.NullString{String: v, Valid: v != ""}
}

func nullableInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}
