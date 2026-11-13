package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KeySource records how a key row was created.
type KeySource string

// The two sources security_keys.key_source accepts (data-model.md §3).
const (
	// KeySourceGenerated marks a key the hub generated.
	KeySourceGenerated KeySource = "generated"
	// KeySourceSupplied marks a key an operator submitted.
	KeySourceSupplied KeySource = "supplied"
)

// StoredKey is one configured security class: sealed blob plus metadata. The
// plaintext is never part of this struct.
type StoredKey struct {
	// SecurityClass is one of the six class names of data-model.md §4.
	SecurityClass string
	// Blob is the sealed value, [0x01][nonce][ciphertext‖tag]. Only OpenKey can
	// read it.
	Blob   []byte
	Source KeySource
	// CreatedAt is the first time this class was configured; ChangedAt is the
	// last change.
	CreatedAt time.Time
	ChangedAt time.Time
}

const keyColumns = `security_class, key_blob, key_source, created_at, changed_at`

// ListKeys returns every configured class ordered by security_class. A class
// with no row is not configured and is absent from the result.
func (s *Store) ListKeys(ctx context.Context) ([]StoredKey, error) {
	return listKeys(ctx, s.db)
}

// ListKeys is the in-transaction form of Store.ListKeys.
func (t *Tx) ListKeys(ctx context.Context) ([]StoredKey, error) {
	if t == nil || t.tx == nil {
		return nil, errors.New("store: transaction is nil")
	}
	return listKeys(ctx, t.tx)
}

func listKeys(ctx context.Context, q queryer) ([]StoredKey, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+keyColumns+` FROM security_keys ORDER BY security_class`)
	if err != nil {
		return nil, fmt.Errorf("store: list keys: %w", err)
	}
	defer rows.Close()
	out := []StoredKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read keys: %w", err)
	}
	return out, nil
}

// GetKey returns one class's row, or ErrNotFound when the class is unknown,
// empty or not configured.
func (s *Store) GetKey(ctx context.Context, class string) (StoredKey, error) {
	return getKey(ctx, s.db, class)
}

// GetKey is the in-transaction form of Store.GetKey.
func (t *Tx) GetKey(ctx context.Context, class string) (StoredKey, error) {
	if t == nil || t.tx == nil {
		return StoredKey{}, errors.New("store: transaction is nil")
	}
	return getKey(ctx, t.tx, class)
}

func getKey(ctx context.Context, q queryer, class string) (StoredKey, error) {
	// No such row can exist for a class the table's CHECK refuses.
	if !keyClassValid(class) {
		return StoredKey{}, ErrNotFound
	}
	row := q.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM security_keys WHERE security_class = ?`, class)
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredKey{}, ErrNotFound
	}
	return k, err
}

// CheckKeys opens every stored blob and returns the classes that fail, in
// security_class order, or nil when all open. A corrupt blob, or a missing
// master key, is a finding and not an error; the error return is for a real
// store failure only. Nothing is repaired or rewritten, so a backup restore can
// still recover the row (data-model.md §3).
func (s *Store) CheckKeys(ctx context.Context) ([]string, error) {
	keys, err := s.ListKeys(ctx)
	if err != nil {
		return nil, err
	}
	var failed []string
	for _, k := range keys {
		plain, err := s.OpenKey(k.SecurityClass, k.Blob)
		if err != nil {
			failed = append(failed, k.SecurityClass)
			continue
		}
		for i := range plain {
			plain[i] = 0
		}
	}
	return failed, nil
}

// PutKey writes one class's sealed blob. A new class sets created_at and
// changed_at to now; an existing class keeps created_at and updates the blob,
// source and changed_at. A zero now means the current time. The caller bumps
// key_revision and writes the audit row on the same transaction.
func (t *Tx) PutKey(ctx context.Context, class string, blob []byte, source KeySource, now time.Time) error {
	if t == nil || t.tx == nil {
		return errors.New("store: transaction is nil")
	}
	if !keyClassValid(class) {
		return fmt.Errorf("store: put key: unknown security class %q", class)
	}
	if len(blob) < keyMinBlobSize {
		return fmt.Errorf("store: put key for %s: blob is shorter than %d bytes", class, keyMinBlobSize)
	}
	if source != KeySourceGenerated && source != KeySourceSupplied {
		return fmt.Errorf("store: put key for %s: source is not generated or supplied", class)
	}
	if now.IsZero() {
		now = time.Now()
	}
	at := now.UTC().Format(coordinatorTimeLayout)
	_, err := t.tx.ExecContext(ctx, `INSERT INTO security_keys
			(security_class, key_blob, key_source, created_at, changed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(security_class) DO UPDATE SET
			key_blob = excluded.key_blob,
			key_source = excluded.key_source,
			changed_at = excluded.changed_at`,
		class, blob, string(source), at, at)
	if err != nil {
		return fmt.Errorf("store: put key for %s: %w", class, err)
	}
	return nil
}

// DeleteKey removes one class's row. A class with no row is not an error.
func (t *Tx) DeleteKey(ctx context.Context, class string) error {
	if t == nil || t.tx == nil {
		return errors.New("store: transaction is nil")
	}
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM security_keys WHERE security_class = ?`, class); err != nil {
		return fmt.Errorf("store: delete key for %s: %w", class, err)
	}
	return nil
}

// DeleteAllKeys removes every row, as a confirmed network reset does.
func (t *Tx) DeleteAllKeys(ctx context.Context) error {
	if t == nil || t.tx == nil {
		return errors.New("store: transaction is nil")
	}
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM security_keys`); err != nil {
		return fmt.Errorf("store: delete all keys: %w", err)
	}
	return nil
}

// scanKey reads one row of keyColumns. The blob is copied, because the driver
// may reuse the buffer it scanned into.
func scanKey(r rowScanner) (StoredKey, error) {
	var (
		k                    StoredKey
		blob                 []byte
		source               string
		createdAt, changedAt string
	)
	if err := r.Scan(&k.SecurityClass, &blob, &source, &createdAt, &changedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return StoredKey{}, err
		}
		return StoredKey{}, fmt.Errorf("store: scan key: %w", err)
	}
	k.Blob = append([]byte(nil), blob...)
	k.Source = KeySource(source)

	var err error
	if k.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return StoredKey{}, fmt.Errorf("store: parse created_at for %s: %w", k.SecurityClass, err)
	}
	if k.ChangedAt, err = time.Parse(time.RFC3339Nano, changedAt); err != nil {
		return StoredKey{}, fmt.Errorf("store: parse changed_at for %s: %w", k.SecurityClass, err)
	}
	return k, nil
}
