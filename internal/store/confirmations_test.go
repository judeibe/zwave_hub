package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// confirmationTestHash is the lowercase hex SHA-256 the store keeps in place of
// a token. Failure messages name a token by this digest, never by the token.
func confirmationTestHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// issueTestConfirmation issues a token or fails the test; it never prints the
// token.
func issueTestConfirmation(t *testing.T, s *Store, op ConfirmationOperation, actor string, now time.Time) IssuedConfirmation {
	t.Helper()
	issued, err := s.IssueConfirmation(t.Context(), op, actor, now)
	if err != nil {
		t.Fatalf("IssueConfirmation(%q, %q): %v", op, actor, err)
	}
	return issued
}

// confirmationTestRowCount counts the rows in the confirmations table.
func confirmationTestRowCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM confirmations`).Scan(&count); err != nil {
		t.Fatalf("count confirmations: %v", err)
	}
	return count
}

// TestConfirmationTTLAndRetention pins the two windows of data-model.md §6: a
// token lives 120 seconds and a row is kept for 24 hours.
func TestConfirmationTTLAndRetention(t *testing.T) {
	if got := ConfirmationTTL; got != 120*time.Second {
		t.Errorf("ConfirmationTTL = %s, want 120s", got)
	}
	if got := ConfirmationRetention; got != 24*time.Hour {
		t.Errorf("ConfirmationRetention = %s, want 24h", got)
	}
}

// TestIssueConfirmation proves an issue returns a 32-byte base64url token bound
// to the operation and the actor, stamped at the injected clock with a 120
// second expiry, and that two issues differ.
func TestIssueConfirmation(t *testing.T) {
	s := openTestStore(t)
	const actor = "192.0.2.10"
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	first := issueTestConfirmation(t, s, ConfirmationKeysExport, actor, now)

	if len(first.Token) != 43 {
		t.Fatalf("token is %d characters, want 43", len(first.Token))
	}
	raw, err := base64.RawURLEncoding.DecodeString(first.Token)
	if err != nil {
		t.Fatalf("token does not decode as unpadded base64url (sha256 %s)", confirmationTestHash(first.Token))
	}
	if len(raw) != 32 {
		t.Fatalf("token decodes to %d bytes, want 32", len(raw))
	}
	if first.Operation != ConfirmationKeysExport {
		t.Errorf("Operation = %q, want %q", first.Operation, ConfirmationKeysExport)
	}
	if first.ActorAddr != actor {
		t.Errorf("ActorAddr = %q, want %q", first.ActorAddr, actor)
	}
	if !first.IssuedAt.Equal(now) {
		t.Errorf("IssuedAt = %s, want %s", first.IssuedAt, now)
	}
	if want := now.Add(120 * time.Second); !first.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want %s", first.ExpiresAt, want)
	}

	second := issueTestConfirmation(t, s, ConfirmationKeysExport, actor, now)
	if first.Token == second.Token {
		t.Fatal("two issues returned the same token")
	}
}

// TestIssueConfirmationStoresOnlyTheHash proves the table holds the SHA-256 hex
// of the token and no copy of the token itself (data-model.md §6).
func TestIssueConfirmationStoresOnlyTheHash(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	issued := issueTestConfirmation(t, s, ConfirmationKeysExport, "192.0.2.10", base)

	rows, err := s.db.QueryContext(ctx,
		`SELECT token_hash, operation, actor_addr, issued_at, expires_at, consumed_at FROM confirmations`)
	if err != nil {
		t.Fatalf("select confirmations: %v", err)
	}
	defer rows.Close()

	type storedRow struct {
		tokenHash, operation, actorAddr, issuedAt, expiresAt string
		consumedAt                                           sql.NullString
	}
	var stored []storedRow
	for rows.Next() {
		var r storedRow
		if err := rows.Scan(&r.tokenHash, &r.operation, &r.actorAddr, &r.issuedAt, &r.expiresAt, &r.consumedAt); err != nil {
			t.Fatalf("scan confirmation row: %v", err)
		}
		stored = append(stored, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read confirmations: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("%d confirmation rows, want 1", len(stored))
	}

	row := stored[0]
	// Equal to the lowercase hex digest proves both the hash and the casing.
	if row.tokenHash != confirmationTestHash(issued.Token) {
		// The value is withheld on purpose: a store that keeps the token
		// instead of its hash would put the token in this column.
		t.Error("token_hash is not the lowercase hex SHA-256 of the issued token")
	}
	for name, value := range map[string]string{
		"token_hash":  row.tokenHash,
		"operation":   row.operation,
		"actor_addr":  row.actorAddr,
		"issued_at":   row.issuedAt,
		"expires_at":  row.expiresAt,
		"consumed_at": row.consumedAt.String,
	} {
		if strings.Contains(value, issued.Token) {
			t.Errorf("column %s holds the plaintext token", name)
		}
	}
}

// TestConsumeConfirmationOnce proves a redeemed token is marked consumed and is
// never accepted a second time.
func TestConsumeConfirmationOnce(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	const actor = "192.0.2.10"
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	issued := issueTestConfirmation(t, s, ConfirmationKeysExport, actor, base)
	at := base.Add(30 * time.Second)

	if err := s.ConsumeConfirmation(ctx, issued.Token, ConfirmationKeysExport, actor, at); err != nil {
		t.Fatalf("first ConsumeConfirmation: %v", err)
	}

	var consumedAt sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT consumed_at FROM confirmations WHERE token_hash = ?`,
		confirmationTestHash(issued.Token)).Scan(&consumedAt); err != nil {
		t.Fatalf("read consumed_at: %v", err)
	}
	if !consumedAt.Valid {
		t.Error("consumed_at is NULL after a successful redemption")
	}

	if err := s.ConsumeConfirmation(ctx, issued.Token, ConfirmationKeysExport, actor, at); !errors.Is(err, ErrConfirmationInvalid) {
		t.Fatalf("second ConsumeConfirmation = %v, want ErrConfirmationInvalid", err)
	}
}

// TestConsumeConfirmationExpiry proves the 120 second window: the expiry
// instant itself is too late, one millisecond earlier is not.
func TestConsumeConfirmationExpiry(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	t.Run("exactly at expiry", func(t *testing.T) {
		issued := issueTestConfirmation(t, s, ConfirmationNetworkReset, "192.0.2.10", base)
		at := issued.IssuedAt.Add(ConfirmationTTL)
		if err := s.ConsumeConfirmation(ctx, issued.Token, ConfirmationNetworkReset, "192.0.2.10", at); !errors.Is(err, ErrConfirmationInvalid) {
			t.Fatalf("consume at the expiry instant = %v, want ErrConfirmationInvalid", err)
		}
	})

	t.Run("one millisecond before expiry", func(t *testing.T) {
		issued := issueTestConfirmation(t, s, ConfirmationNetworkReset, "192.0.2.11", base)
		at := issued.ExpiresAt.Add(-time.Millisecond)
		if err := s.ConsumeConfirmation(ctx, issued.Token, ConfirmationNetworkReset, "192.0.2.11", at); err != nil {
			t.Fatalf("consume one millisecond before expiry: %v", err)
		}
	})
}

// TestConsumeConfirmationOperationBinding proves a token only authorizes the
// operation it was issued for, and that a refused attempt consumes nothing.
func TestConsumeConfirmationOperationBinding(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	const actor = "192.0.2.10"
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	at := base.Add(time.Second)

	export := issueTestConfirmation(t, s, ConfirmationKeysExport, actor, base)
	if err := s.ConsumeConfirmation(ctx, export.Token, ConfirmationNetworkReset, actor, at); !errors.Is(err, ErrConfirmationInvalid) {
		t.Fatalf("keys.export token presented as network.reset = %v, want ErrConfirmationInvalid", err)
	}
	if err := s.ConsumeConfirmation(ctx, export.Token, ConfirmationKeysExport, actor, at); err != nil {
		t.Fatalf("keys.export token presented as keys.export after a refused attempt: %v", err)
	}

	reset := issueTestConfirmation(t, s, ConfirmationNetworkReset, actor, base)
	if err := s.ConsumeConfirmation(ctx, reset.Token, ConfirmationKeysExport, actor, at); !errors.Is(err, ErrConfirmationInvalid) {
		t.Fatalf("network.reset token presented as keys.export = %v, want ErrConfirmationInvalid", err)
	}
	if err := s.ConsumeConfirmation(ctx, reset.Token, ConfirmationNetworkReset, actor, at); err != nil {
		t.Fatalf("network.reset token presented as network.reset after a refused attempt: %v", err)
	}
}

// TestConsumeConfirmationActorBinding proves a token is bound to the address it
// was issued to, and that a refused attempt consumes nothing.
func TestConsumeConfirmationActorBinding(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	issued := issueTestConfirmation(t, s, ConfirmationKeysExport, "192.0.2.10", base)

	if err := s.ConsumeConfirmation(ctx, issued.Token, ConfirmationKeysExport, "192.0.2.11", base.Add(time.Second)); !errors.Is(err, ErrConfirmationInvalid) {
		t.Fatalf("token presented from 192.0.2.11 = %v, want ErrConfirmationInvalid", err)
	}
	if err := s.ConsumeConfirmation(ctx, issued.Token, ConfirmationKeysExport, "192.0.2.10", base.Add(2*time.Second)); err != nil {
		t.Fatalf("token presented from its own address after a refused attempt: %v", err)
	}
}

// TestConsumeConfirmationUnknownToken proves an unissued token is refused the
// same way as an expired or consumed one, so the two are indistinguishable.
func TestConsumeConfirmationUnknownToken(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	// A well-formed token that was never issued.
	unknown := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	if err := s.ConsumeConfirmation(ctx, unknown, ConfirmationKeysExport, "192.0.2.10", base); !errors.Is(err, ErrConfirmationInvalid) {
		t.Fatalf("consume of an unknown token = %v, want ErrConfirmationInvalid", err)
	}
}

// TestIssueConfirmationValidation proves an undefined operation and an empty
// actor address are refused before any row is written.
func TestIssueConfirmationValidation(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	refused := []struct {
		name  string
		op    ConfirmationOperation
		actor string
	}{
		{"undefined operation", ConfirmationOperation("keys.delete"), "192.0.2.10"},
		{"empty actor address", ConfirmationKeysExport, ""},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.IssueConfirmation(ctx, tc.op, tc.actor, base); err == nil {
				t.Fatalf("IssueConfirmation accepted an %s", tc.name)
			}
			if got := confirmationTestRowCount(t, s); got != 0 {
				t.Fatalf("%d confirmation rows after a refused issue, want 0", got)
			}
		})
	}
}

// TestPruneConfirmations proves the 24 hour retention: rows whose expiry is
// more than 24 hours before now are deleted, a younger one survives and stays
// redeemable inside its window, and a pruned token is indistinguishable from
// one that never existed.
func TestPruneConfirmations(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	old := issueTestConfirmation(t, s, ConfirmationKeysExport, "192.0.2.10", now.Add(-48*time.Hour))
	issueTestConfirmation(t, s, ConfirmationNetworkReset, "192.0.2.11", now.Add(-25*time.Hour))
	recent := issueTestConfirmation(t, s, ConfirmationKeysExport, "192.0.2.12", now.Add(-23*time.Hour))

	deleted, err := s.PruneConfirmations(ctx, now)
	if err != nil {
		t.Fatalf("PruneConfirmations: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("PruneConfirmations deleted %d rows, want 2 (the 48h and 25h old ones)", deleted)
	}
	if got := confirmationTestRowCount(t, s); got != 1 {
		t.Fatalf("%d confirmation rows after pruning, want 1", got)
	}

	if err := s.ConsumeConfirmation(ctx, recent.Token, ConfirmationKeysExport, "192.0.2.12", recent.IssuedAt.Add(time.Minute)); err != nil {
		t.Fatalf("consume of the surviving row: %v", err)
	}
	if err := s.ConsumeConfirmation(ctx, old.Token, ConfirmationKeysExport, "192.0.2.10", old.IssuedAt.Add(time.Minute)); !errors.Is(err, ErrConfirmationInvalid) {
		t.Fatalf("consume of a pruned token = %v, want ErrConfirmationInvalid", err)
	}
}

// TestPruneConfirmationsNothingOldEnough proves a prune with no row past the
// retention window deletes nothing.
func TestPruneConfirmationsNothingOldEnough(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	issueTestConfirmation(t, s, ConfirmationKeysExport, "192.0.2.10", now.Add(-23*time.Hour))

	deleted, err := s.PruneConfirmations(ctx, now)
	if err != nil {
		t.Fatalf("PruneConfirmations: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("PruneConfirmations deleted %d rows, want 0", deleted)
	}
	if got := confirmationTestRowCount(t, s); got != 1 {
		t.Fatalf("%d confirmation rows after pruning, want 1", got)
	}
}
