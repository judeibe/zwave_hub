package store

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/judeibe/zwave_hub/internal/event"
)

// openTestStore opens a store in a fresh temp directory that is removed with
// the test.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// countEvents counts emitted catalogue events by their stable `event` field.
func countEvents(out, name string) int {
	return strings.Count(out, `"event":"`+name+`"`)
}

func pragmaInt(t *testing.T, s *Store, name string) int {
	t.Helper()
	var value int
	if err := s.db.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&value); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return value
}

func pragmaText(t *testing.T, s *Store, name string) string {
	t.Helper()
	var value string
	if err := s.db.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&value); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return value
}

func tableNames(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), "SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	names := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	return names
}

func auditCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM audit_entries").Scan(&count); err != nil {
		t.Fatalf("count audit entries: %v", err)
	}
	return count
}

func TestOpenMigratesEmptyDatabase(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	var out bytes.Buffer
	opts := Options{DataDir: dir, Emitter: event.New(&out, 0)}

	s, err := Open(ctx, opts)
	if err != nil {
		t.Fatalf("Open empty directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hub.db")); err != nil {
		t.Fatalf("hub.db was not created: %v", err)
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 1 {
		t.Fatalf("user_version = %d, want 1", version)
	}

	have := tableNames(t, s)
	for _, name := range []string{"coordinators", "provisioning", "security_keys", "confirmations", "audit_entries"} {
		if !have[name] {
			t.Errorf("table %q was not created", name)
		}
	}

	if got := pragmaText(t, s, "journal_mode"); got != "wal" {
		t.Errorf("journal_mode = %q, want wal", got)
	}
	if got := pragmaInt(t, s, "synchronous"); got != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL)", got)
	}
	if got := pragmaInt(t, s, "foreign_keys"); got != 1 {
		t.Errorf("foreign_keys = %d, want 1", got)
	}
	if got := countEvents(out.String(), "store.migrated"); got != 1 {
		t.Fatalf("first Open emitted %d store.migrated events, want 1:\n%s", got, out.String())
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(ctx, opts)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	if got := countEvents(out.String(), "store.migrated"); got != 1 {
		t.Fatalf("reopen emitted store.migrated %d times in total, want 1 (a no-op migration):\n%s", got, out.String())
	}
	if version, err := reopened.SchemaVersion(ctx); err != nil || version != 1 {
		t.Fatalf("user_version after reopen = %d, %v; want 1, nil", version, err)
	}
}

func TestOpenMigratesPreExistingZeroLengthFile(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create zero-length hub.db: %v", err)
	}

	var out bytes.Buffer
	s, err := Open(ctx, Options{DataDir: dir, Emitter: event.New(&out, 0)})
	if err != nil {
		t.Fatalf("Open over zero-length hub.db: %v", err)
	}
	defer func() { _ = s.Close() }()

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 1 {
		t.Fatalf("user_version = %d, want 1", version)
	}
	have := tableNames(t, s)
	for _, name := range []string{"coordinators", "provisioning", "security_keys", "confirmations", "audit_entries"} {
		if !have[name] {
			t.Errorf("table %q was not created", name)
		}
	}

	if got := countEvents(out.String(), "store.migrated"); got != 1 {
		t.Fatalf("emitted %d store.migrated events, want 1:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), `"from_version":0`) || !strings.Contains(out.String(), `"to_version":1`) {
		t.Fatalf("migration did not run from user_version 0 to 1:\n%s", out.String())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat hub.db: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("hub.db is still zero-length after migration")
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")

	// Build the file with the store's own DSN so its journal mode already
	// matches what Open expects; the only thing this test varies is the
	// schema version.
	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database before Open: %v", err)
	}

	s, err := Open(ctx, Options{DataDir: dir})
	if err == nil {
		_ = s.Close()
		t.Fatal("Open accepted a database with user_version 99")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("error %q does not mention the database version 99", err)
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error %q does not say the schema is newer than this build", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database after Open: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("Open changed the database file while refusing a newer schema")
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)

	_, err := s.db.ExecContext(ctx,
		`UPDATE provisioning SET active_coordinator_id = ? WHERE id = 1`,
		"usb:0000:0000:no-such-coordinator")
	if err == nil {
		t.Fatal("provisioning accepted a coordinator id with no coordinators row")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Fatalf("error = %q, want a FOREIGN KEY constraint failure", err)
	}

	var active sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT active_coordinator_id FROM provisioning WHERE id = 1`).Scan(&active); err != nil {
		t.Fatalf("read provisioning row: %v", err)
	}
	if active.Valid {
		t.Fatalf("active_coordinator_id = %q after the refused update, want NULL", active.String)
	}
}

func TestIntegrityCheckRefusesCorruptFile(t *testing.T) {
	corrupt := []struct {
		name    string
		content []byte
	}{
		{"all ones", bytes.Repeat([]byte{0xff}, 4096)},
		{"not a database", []byte("not a database")},
	}
	for _, tc := range corrupt {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "hub.db"), tc.content, 0o600); err != nil {
				t.Fatalf("write corrupt hub.db: %v", err)
			}

			var out bytes.Buffer
			var (
				s   *Store
				err error
			)
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("Open panicked on a corrupt database: %v", recovered)
					}
				}()
				s, err = Open(t.Context(), Options{DataDir: dir, Emitter: event.New(&out, 0)})
			}()
			if s != nil {
				_ = s.Close()
				t.Fatal("Open returned a store for a corrupt database")
			}
			if err == nil {
				t.Fatal("Open accepted a corrupt database")
			}

			emitted := out.String()
			if !strings.Contains(emitted, `"event":"store.integrity_failed"`) {
				t.Fatalf("no store.integrity_failed event was emitted:\n%s", emitted)
			}
			if !strings.Contains(emitted, `"detail":"integrity_check_failed"`) {
				t.Fatalf("event does not carry the fixed detail code:\n%s", emitted)
			}
			// The event carries the fixed code only; SQLite's own message,
			// which names pages and offsets, belongs in the returned error.
			if strings.Contains(emitted, "not a database") {
				t.Fatalf("event leaked SQLite's message:\n%s", emitted)
			}
		})
	}
}

func TestMigratedEvent(t *testing.T) {
	var out bytes.Buffer
	s, err := Open(t.Context(), Options{DataDir: t.TempDir(), Emitter: event.New(&out, 0)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var migrated []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(line, `"event":"store.migrated"`) {
			migrated = append(migrated, line)
		}
	}
	if len(migrated) != 1 {
		t.Fatalf("emitted %d store.migrated lines, want 1:\n%s", len(migrated), out.String())
	}
	for _, want := range []string{`"from_version":0`, `"to_version":1`, `"duration_ms":`} {
		if !strings.Contains(migrated[0], want) {
			t.Errorf("store.migrated line lacks %s:\n%s", want, migrated[0])
		}
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	key, err := LoadMasterKey(dir, nil)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if len(key) != keyMasterSize {
		t.Fatalf("master key is %d bytes, want %d", len(key), keyMasterSize)
	}

	info, err := os.Stat(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("stat master.key: %v", err)
	}
	if got := info.Mode().Perm(); got != keyFileMode {
		t.Errorf("master.key mode = %04o, want %04o", got, keyFileMode)
	}

	again, err := LoadMasterKey(dir, nil)
	if err != nil {
		t.Fatalf("second LoadMasterKey: %v", err)
	}
	if !bytes.Equal(key, again) {
		t.Fatal("two loads of master.key returned different keys")
	}

	supplied := bytes.Repeat([]byte{0x2a}, keyMasterSize)
	won, err := LoadMasterKey(dir, supplied)
	if err != nil {
		t.Fatalf("LoadMasterKey with a supplied key: %v", err)
	}
	if !bytes.Equal(won, supplied) {
		t.Fatal("a supplied master key did not win over master.key")
	}

	s, err := Open(ctx, Options{DataDir: dir, MasterKey: key})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	plain := []byte("0123456789abcdef")
	const class = "S2_Authenticated"
	blob, err := s.SealKey(class, plain)
	if err != nil {
		t.Fatalf("SealKey: %v", err)
	}
	const wantBlobSize = keyHeaderSize + keyPlainSize + keyTagSize
	if len(blob) != wantBlobSize {
		t.Fatalf("blob is %d bytes, want %d", len(blob), wantBlobSize)
	}
	if blob[0] != keyVersion {
		t.Fatalf("blob[0] = %#02x, want %#02x", blob[0], byte(keyVersion))
	}

	opened, err := s.OpenKey(class, blob)
	if err != nil {
		t.Fatalf("OpenKey: %v", err)
	}
	if !bytes.Equal(opened, plain) {
		t.Fatalf("OpenKey returned %x, want %x", opened, plain)
	}
}

func TestOpenKeyRejectsTampering(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	key, err := LoadMasterKey(dir, nil)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	s, err := Open(ctx, Options{DataDir: dir, MasterKey: key})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	plain := []byte("fedcba9876543210")
	const class = "S0_Legacy"
	blob, err := s.SealKey(class, plain)
	if err != nil {
		t.Fatalf("SealKey: %v", err)
	}

	t.Run("blob opened under another class", func(t *testing.T) {
		if _, err := s.OpenKey("S2_AccessControl", blob); !errors.Is(err, ErrKeyCorrupt) {
			t.Fatalf("OpenKey under another class = %v, want ErrKeyCorrupt", err)
		}
	})

	t.Run("flipped ciphertext byte", func(t *testing.T) {
		tampered := bytes.Clone(blob)
		tampered[keyHeaderSize] ^= 0x80
		if _, err := s.OpenKey(class, tampered); !errors.Is(err, ErrKeyCorrupt) {
			t.Fatalf("OpenKey of a tampered blob = %v, want ErrKeyCorrupt", err)
		}
	})

	t.Run("unknown version byte", func(t *testing.T) {
		tampered := bytes.Clone(blob)
		tampered[0] = keyVersion + 1
		if _, err := s.OpenKey(class, tampered); !errors.Is(err, ErrKeyCorrupt) {
			t.Fatalf("OpenKey of an unknown version = %v, want ErrKeyCorrupt", err)
		}
	})

	t.Run("blob shorter than the minimum", func(t *testing.T) {
		if _, err := s.OpenKey(class, blob[:keyMinBlobSize-1]); !errors.Is(err, ErrKeyCorrupt) {
			t.Fatalf("OpenKey of a short blob = %v, want ErrKeyCorrupt", err)
		}
	})

	t.Run("too-short key on seal", func(t *testing.T) {
		if _, err := s.SealKey(class, make([]byte, keyPlainSize-1)); err == nil {
			t.Fatal("SealKey accepted a 15-byte key")
		}
	})

	t.Run("unknown class", func(t *testing.T) {
		if _, err := s.SealKey("S3_Imaginary", plain); err == nil {
			t.Fatal("SealKey accepted an unknown security class")
		}
		if _, err := s.OpenKey("S3_Imaginary", blob); err == nil {
			t.Fatal("OpenKey accepted an unknown security class")
		}
	})

	t.Run("no master key", func(t *testing.T) {
		keyless := &Store{}
		if _, err := keyless.SealKey(class, plain); !errors.Is(err, ErrNoMasterKey) {
			t.Fatalf("SealKey without a master key = %v, want ErrNoMasterKey", err)
		}
		if _, err := keyless.OpenKey(class, blob); !errors.Is(err, ErrNoMasterKey) {
			t.Fatalf("OpenKey without a master key = %v, want ErrNoMasterKey", err)
		}
	})
}

func TestMasterKeyFileIsNeverRegenerated(t *testing.T) {
	const content = "short"
	dir := t.TempDir()
	path := filepath.Join(dir, keyFileName)
	if err := os.WriteFile(path, []byte(content), keyFileMode); err != nil {
		t.Fatalf("write malformed master.key: %v", err)
	}

	_, err := LoadMasterKey(dir, nil)
	if err == nil {
		t.Fatal("LoadMasterKey accepted a malformed master.key")
	}
	if strings.Contains(err.Error(), content) {
		t.Errorf("error echoes the file's contents: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read master.key after the failed load: %v", err)
	}
	if string(after) != content {
		t.Fatalf("master.key = %q, want %q unchanged", after, content)
	}

	// A decodable file of the wrong length is refused the same way, and its
	// contents are not echoed either.
	wrong := make([]byte, keyMasterSize+1)
	for i := range wrong {
		wrong[i] = byte(i)
	}
	encoded := base64.StdEncoding.EncodeToString(wrong)
	if err := os.WriteFile(path, []byte(encoded+"\n"), keyFileMode); err != nil {
		t.Fatalf("write wrong-length master.key: %v", err)
	}
	if _, err := LoadMasterKey(dir, nil); err == nil {
		t.Fatal("LoadMasterKey accepted a 33-byte key file")
	} else if strings.Contains(err.Error(), encoded) {
		t.Errorf("error echoes the file's contents: %v", err)
	}
	after, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read master.key after the second failed load: %v", err)
	}
	if string(after) != encoded+"\n" {
		t.Fatal("master.key was rewritten by a failed load")
	}
}

func TestAppendAuditAndList(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)

	entry := AuditEntry{
		Action:     AuditCoordinatorSelected,
		ActorAddr:  "192.0.2.10",
		ActorLabel: "operator",
		RequestID:  "req-1",
		Outcome:    OutcomeSucceeded,
	}
	id, err := s.AppendAudit(ctx, entry)
	if err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	if id != 1 {
		t.Fatalf("first audit entry id = %d, want 1", id)
	}
	if got := auditCount(t, s); got != 1 {
		t.Fatalf("audit row count = %d, want 1", got)
	}

	refused := []struct {
		name   string
		mutate func(*AuditEntry)
	}{
		{"undefined action", func(e *AuditEntry) { e.Action = "keys.rotated" }},
		{"wrong outcome for the action", func(e *AuditEntry) { e.Outcome = OutcomeRejected }},
		{"empty actor address", func(e *AuditEntry) { e.ActorAddr = "" }},
		{"empty request id", func(e *AuditEntry) { e.RequestID = "" }},
		{"actor label longer than the limit", func(e *AuditEntry) {
			e.ActorLabel = strings.Repeat("é", auditMaxLabelRunes+1)
		}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			bad := entry
			tc.mutate(&bad)
			if _, err := s.AppendAudit(ctx, bad); err == nil {
				t.Fatalf("AppendAudit accepted an entry with %s", tc.name)
			}
			if got := auditCount(t, s); got != 1 {
				t.Fatalf("audit row count = %d after a refused entry, want 1", got)
			}
		})
	}

	more := []AuditAction{
		AuditKeysGenerated,
		AuditCoordinatorSelected,
		AuditCoordinatorSelected,
		AuditCoordinatorSelected,
	}
	wantIDs := []int64{id}
	for i, action := range more {
		next := entry
		next.Action = action
		next.RequestID = fmt.Sprintf("req-%d", i+2)
		newID, err := s.AppendAudit(ctx, next)
		if err != nil {
			t.Fatalf("AppendAudit %d: %v", i+2, err)
		}
		if newID != int64(i+2) {
			t.Fatalf("audit entry id = %d, want %d", newID, i+2)
		}
		wantIDs = append(wantIDs, newID)
	}
	if got := auditCount(t, s); got != 5 {
		t.Fatalf("audit row count = %d, want 5", got)
	}

	slices.Reverse(wantIDs) // newest first
	var walked []int64
	before := int64(0)
	pages := 0
	for {
		page, err := s.ListAudit(ctx, AuditQuery{Limit: 2, Before: before})
		if err != nil {
			t.Fatalf("ListAudit page %d: %v", pages+1, err)
		}
		pages++
		if len(page.Items) > 2 {
			t.Fatalf("page %d has %d entries, want at most 2", pages, len(page.Items))
		}
		for _, item := range page.Items {
			walked = append(walked, item.ID)
		}
		if page.NextBefore == nil {
			break
		}
		if last := page.Items[len(page.Items)-1].ID; *page.NextBefore != last {
			t.Fatalf("NextBefore = %d, want the last id on the page (%d)", *page.NextBefore, last)
		}
		before = *page.NextBefore
		if pages == 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 {
		t.Fatalf("walked %d pages, want 3", pages)
	}
	if !slices.Equal(walked, wantIDs) {
		t.Fatalf("walked ids %v, want %v (newest first, no repeats, no skips)", walked, wantIDs)
	}

	newest, err := s.ListAudit(ctx, AuditQuery{Limit: 2})
	if err != nil {
		t.Fatalf("ListAudit newest page: %v", err)
	}
	if len(newest.Items) != 2 || newest.Items[0].ID != 5 {
		t.Fatalf("newest page = %+v, want two entries starting at id 5", newest.Items)
	}
	first := newest.Items[0]
	if first.Action != AuditCoordinatorSelected || first.ActorAddr != "192.0.2.10" ||
		first.ActorLabel != "operator" || first.RequestID != "req-5" ||
		first.Outcome != OutcomeSucceeded {
		t.Errorf("stored entry = %+v, want the inserted fields back", first)
	}

	filtered, err := s.ListAudit(ctx, AuditQuery{Limit: 2, Action: AuditCoordinatorSelected})
	if err != nil {
		t.Fatalf("ListAudit filtered page: %v", err)
	}
	if len(filtered.Items) != 2 {
		t.Fatalf("filtered page has %d entries, want 2", len(filtered.Items))
	}
	for _, item := range filtered.Items {
		if item.Action != AuditCoordinatorSelected {
			t.Fatalf("filtered page carries action %q, want %q", item.Action, AuditCoordinatorSelected)
		}
	}
	if filtered.NextBefore == nil {
		t.Fatal("filtered page has no cursor, but four entries match the action")
	}

	rest, err := s.ListAudit(ctx, AuditQuery{Limit: 2, Before: *filtered.NextBefore, Action: AuditCoordinatorSelected})
	if err != nil {
		t.Fatalf("ListAudit last filtered page: %v", err)
	}
	if rest.NextBefore != nil {
		t.Fatalf("last filtered page has a cursor (%d), want nil", *rest.NextBefore)
	}
	if got := len(filtered.Items) + len(rest.Items); got != 4 {
		t.Fatalf("action filter matched %d entries, want 4 of the 5", got)
	}
}

func TestWithTxRollsBackAudit(t *testing.T) {
	ctx := t.Context()
	s := openTestStore(t)

	entry := AuditEntry{
		Action:    AuditKeysReplaced,
		ActorAddr: "192.0.2.11",
		RequestID: "req-rollback",
		Outcome:   OutcomeSucceeded,
	}

	boom := errors.New("command failed")
	err := s.WithTx(ctx, func(tx *Tx) error {
		if _, err := tx.AppendAudit(ctx, entry); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx error = %v, want %v", err, boom)
	}
	if got := auditCount(t, s); got != 0 {
		t.Fatalf("rolled-back transaction left %d audit rows, want 0", got)
	}

	var committedID int64
	err = s.WithTx(ctx, func(tx *Tx) error {
		var err error
		committedID, err = tx.AppendAudit(ctx, entry)
		return err
	})
	if err != nil {
		t.Fatalf("committing WithTx: %v", err)
	}
	if committedID <= 0 {
		t.Fatalf("committed audit id = %d, want a positive id", committedID)
	}
	if got := auditCount(t, s); got != 1 {
		t.Fatalf("audit row count = %d after commit, want 1", got)
	}
}
