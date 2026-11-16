package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrConfirmationInvalid reports a token that cannot authorize the operation it
// was presented for: unknown, expired, already consumed, or bound to a different
// operation or address. Every one of those is the same error on purpose, so a
// caller cannot tell a token that never existed from one that has aged out or
// been pruned (data-model.md §6).
var ErrConfirmationInvalid = errors.New("store: confirmation token is invalid")

// ErrConfirmationUnusable is the invalid token that was issued for the
// presented operation and address and can no longer be redeemed: it has expired,
// or it has already been used. It wraps ErrConfirmationInvalid, so a caller that
// only needs "refused" keeps testing for the sentinel, while a caller that must
// tell an expired token (410) from an unknown one (404) tests this one with
// errors.Is (FR-013, contracts/openapi.yaml).
var ErrConfirmationUnusable = fmt.Errorf("%w: it has expired or was already used", ErrConfirmationInvalid)

// ConfirmationOperation names an action a confirmation token can authorize. The
// set is closed: a token is valid only for the operation it was issued for, so a
// mistyped or invented operation is refused rather than stored.
type ConfirmationOperation string

// The two operations that require a confirmation token (FR-013, FR-017).
const (
	// ConfirmationKeysExport authorizes a backup export.
	ConfirmationKeysExport ConfirmationOperation = "keys.export"
	// ConfirmationNetworkReset authorizes a network reset.
	ConfirmationNetworkReset ConfirmationOperation = "network.reset"
)

const (
	// ConfirmationTTL is how long an issued token stays redeemable. It is the
	// window data-model.md §6 gives expires_at.
	ConfirmationTTL = 120 * time.Second

	// ConfirmationRetention is how long a row is kept after its expiry. An older
	// row is pruned, and its token then reads exactly like one that never
	// existed, which is the intended behaviour.
	ConfirmationRetention = 24 * time.Hour

	// confirmationTokenBytes is the entropy behind a token. 32 bytes render as
	// 43 unpadded base64url characters.
	confirmationTokenBytes = 32
)

// IssuedConfirmation is a token handed to its requester exactly once. Only the
// SHA-256 of the token is stored, so the token cannot be recovered from the
// database and its hash is the only name a failure message may ever use.
type IssuedConfirmation struct {
	// Token is the unpadded base64url secret. It is never logged, never stored
	// and never named in an error.
	Token string
	// Operation is the only operation the token authorizes.
	Operation ConfirmationOperation
	// ActorAddr is the only address the token may be presented from.
	ActorAddr string
	// IssuedAt and ExpiresAt are the stored window, in UTC at millisecond
	// precision, so the expiry the caller holds is the expiry the store
	// enforces.
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// confirmationStamp renders t in the form the confirmations columns hold: RFC
// 3339 UTC with milliseconds (data-model.md §6). Values are formatted after
// .UTC(), so the offset always renders as Z and the text sorts in time order,
// which is what makes the confirmations_expires_at index and the prune cutoff
// comparison meaningful.
func confirmationStamp(t time.Time) string {
	return t.UTC().Format(coordinatorTimeLayout)
}

// confirmationTokenHash is the stored form of a token: the lowercase hex SHA-256
// of the token string. Only this digest reaches the database or a query
// argument.
func confirmationTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newConfirmationToken draws confirmationTokenBytes from crypto/rand and encodes
// them as unpadded base64url. The caller must not put the result in an error or
// a log line.
func newConfirmationToken() (string, error) {
	buf := make([]byte, confirmationTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: generate confirmation token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// confirmationOperationDefined reports whether op is one the table's CHECK
// clause accepts.
func confirmationOperationDefined(op ConfirmationOperation) bool {
	return op == ConfirmationKeysExport || op == ConfirmationNetworkReset
}

// IssueConfirmation mints a token bound to one operation and one actor address
// and records its hash with a ConfirmationTTL expiry. It refuses an undefined
// operation or an empty actor address before any row is written. A zero now
// means the current time. Errors name the field, never the token or its hash.
func (s *Store) IssueConfirmation(ctx context.Context, op ConfirmationOperation, actorAddr string, now time.Time) (IssuedConfirmation, error) {
	if !confirmationOperationDefined(op) {
		return IssuedConfirmation{}, errors.New("store: confirmation operation is not keys.export or network.reset")
	}
	if actorAddr == "" {
		return IssuedConfirmation{}, errors.New("store: confirmation actor address is empty")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// Truncating to the stored precision keeps the returned window identical to
	// the one expires_at enforces; formatting alone would already drop the
	// sub-millisecond part.
	issuedAt := now.UTC().Truncate(time.Millisecond)
	expiresAt := issuedAt.Add(ConfirmationTTL)

	token, err := newConfirmationToken()
	if err != nil {
		return IssuedConfirmation{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO confirmations
			(token_hash, operation, actor_addr, issued_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		confirmationTokenHash(token), string(op), actorAddr,
		confirmationStamp(issuedAt), confirmationStamp(expiresAt))
	if err != nil {
		return IssuedConfirmation{}, fmt.Errorf("store: insert confirmation: %w", err)
	}
	return IssuedConfirmation{
		Token:     token,
		Operation: op,
		ActorAddr: actorAddr,
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
	}, nil
}

// ConsumeConfirmation redeems a token for the operation and address it was
// issued for and marks the row consumed. It returns ErrConfirmationInvalid for
// an unknown or foreign token and ErrConfirmationUnusable for one that was
// issued for this operation and address but has expired or been consumed, and
// leaves the row untouched either way: a spent row is never reused and a refused
// attempt costs nothing, so the legitimate holder can still redeem. The expiry
// instant itself is too late; the window is half-open, [issued_at, expires_at).
func (s *Store) ConsumeConfirmation(ctx context.Context, token string, op ConfirmationOperation, actorAddr string, now time.Time) error {
	if token == "" {
		return ErrConfirmationInvalid
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	at := confirmationStamp(now)

	// One conditional statement is the whole check. The row must match all four
	// bindings — token, operation, address and an unredeemed, unexpired window —
	// or nothing is written and the caller sees a sentinel.
	res, err := s.db.ExecContext(ctx, `UPDATE confirmations SET consumed_at = ?
		WHERE token_hash = ? AND operation = ? AND actor_addr = ?
			AND consumed_at IS NULL AND expires_at > ?`,
		at, confirmationTokenHash(token), string(op), actorAddr, at)
	if err != nil {
		return fmt.Errorf("store: consume confirmation: %w", err)
	}
	consumed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: read consumed confirmations: %w", err)
	}
	if consumed == 0 {
		return s.classifyConfirmation(ctx, token, op, actorAddr)
	}
	return nil
}

// classifyConfirmation tells a token that was never issued to this caller, or
// was issued for another operation, from one that was issued and has since
// expired or been used. It repeats the three bindings the UPDATE checked and
// ignores the two it failed: a row that exists for this token, operation and
// address can only have been refused for its window or its consumption state.
// The query runs on the refusal path alone.
func (s *Store) classifyConfirmation(ctx context.Context, token string, op ConfirmationOperation, actorAddr string) error {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM confirmations
		WHERE token_hash = ? AND operation = ? AND actor_addr = ?`,
		confirmationTokenHash(token), string(op), actorAddr).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConfirmationInvalid
	}
	if err != nil {
		return fmt.Errorf("store: classify confirmation: %w", err)
	}
	return ErrConfirmationUnusable
}

// PruneConfirmations deletes rows whose expiry is more than ConfirmationRetention
// before now and reports how many it removed. A zero now means the current time.
// The cutoff is compared as the same formatted text expires_at holds, so the
// confirmations_expires_at index serves the range scan.
func (s *Store) PruneConfirmations(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := confirmationStamp(now.Add(-ConfirmationRetention))

	res, err := s.db.ExecContext(ctx, `DELETE FROM confirmations WHERE expires_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune confirmations: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: read pruned confirmations: %w", err)
	}
	return deleted, nil
}
