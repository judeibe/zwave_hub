package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/judeibe/zwave_hub/internal/event"
)

// migrationFiles carries the schema with the binary so a deployed hub can
// migrate its own data volume; nothing reads migrations from disk.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

const (
	// integrityFailureDetail is the fixed `detail` code of
	// store.integrity_failed (contracts/events.md §3.1). SQLite's own message
	// names pages and file offsets, so it never reaches the event stream; the
	// operator gets the detail in the returned error instead.
	integrityFailureDetail = "integrity_check_failed"

	migrationDir    = "migrations"
	migrationSuffix = ".sql"
)

// Options is what Open needs. MasterKey is the decoded ZWAVE_HUB_MASTER_KEY;
// when nil, sealed keys belong to <DataDir>/master.key, which LoadMasterKey
// creates on first use. Open never reads the environment itself (constitution
// III).
type Options struct {
	// DataDir holds hub.db and, when no master key is supplied, master.key.
	DataDir string
	// MasterKey is the 32-byte key from the environment, or nil.
	MasterKey []byte
	// Emitter receives store.migrated and store.integrity_failed. It may be
	// nil in tests, in which case nothing is emitted.
	Emitter *event.Emitter
}

// Store is the SQLite handle and the derived data-directory paths. One
// connection serves every caller: SQLite has a single writer anyway, and a
// fixed one-connection pool means the DSN pragmas cannot be missed by a
// connection the pool opened later. It is safe for concurrent use.
type Store struct {
	db        *sql.DB
	dataDir   string
	masterKey []byte
	emitter   *event.Emitter
}

// Open creates the data directory if needed, opens hub.db with the durability
// pragmas of research.md R3, verifies the file with PRAGMA integrity_check and
// steps PRAGMA user_version through the embedded migrations, each inside one
// transaction. A database whose schema is newer than this binary is refused:
// that is a rollback, and constitution VII runs the previous binary against
// the same volume rather than downgrading it in place.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.DataDir == "" {
		return nil, errors.New("store: data directory is empty")
	}
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create data directory: %w", err)
	}

	db, err := sql.Open("sqlite", dataSourceName(filepath.Join(opts.DataDir, "hub.db")))
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{
		db:        db,
		dataDir:   opts.DataDir,
		masterKey: opts.MasterKey,
		emitter:   opts.Emitter,
	}
	// A connection that cannot even be established is evidence the file is
	// unusable: the DSN pragmas read the database header, so a truncated or
	// overwritten file fails here before integrity_check can run. This is the
	// most likely real corruption, so it emits the same event.
	if err := db.PingContext(ctx); err != nil {
		s.emit(ctx, &event.StoreIntegrityFailed{Detail: integrityFailureDetail})
		db.Close()
		return nil, fmt.Errorf("store: connect to database: %w", err)
	}
	if err := s.checkIntegrity(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// dataSourceName builds the modernc DSN. The pragmas ride in the DSN rather
// than in a post-open Exec so every connection the pool ever opens has them.
func dataSourceName(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	return "file:" + filepath.ToSlash(path) + "?" + q.Encode()
}

// Close releases the database file. It is the only shutdown step the store
// has; WAL checkpointing happens as connections close.
func (s *Store) Close() error {
	return s.db.Close()
}

// DataDir is the directory holding hub.db, master.key and the zwave-js cache.
func (s *Store) DataDir() string {
	return s.dataDir
}

// SchemaVersion reports the database's PRAGMA user_version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("store: read user_version: %w", err)
	}
	return version, nil
}

// checkIntegrity runs PRAGMA integrity_check before anything is written. A
// failing file is reported and refused: migrating a corrupt database would
// spread the damage. Every failure path emits store.integrity_failed, because
// a pragma that cannot even run is itself evidence the file is unusable.
func (s *Store) checkIntegrity(ctx context.Context) error {
	problems, err := s.integrityProblems(ctx)
	if err != nil {
		s.emit(ctx, &event.StoreIntegrityFailed{Detail: integrityFailureDetail})
		return err
	}
	if problems > 0 {
		s.emit(ctx, &event.StoreIntegrityFailed{Detail: integrityFailureDetail})
		return fmt.Errorf("store: integrity check failed with %d problem(s) (%s)", problems, integrityFailureDetail)
	}
	return nil
}

// integrityProblems counts the non-"ok" rows PRAGMA integrity_check returns.
func (s *Store) integrityProblems(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return 0, fmt.Errorf("store: integrity check: %w", err)
	}
	defer rows.Close()

	problems := 0
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return 0, fmt.Errorf("store: read integrity check: %w", err)
		}
		if line != "ok" {
			problems++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: read integrity check: %w", err)
	}
	return problems, nil
}

// migration is one embedded schema step.
type migration struct {
	version int
	name    string
	body    string
}

// migrate applies every embedded migration newer than the database's
// user_version, in ascending order. Versions may have gaps, so a migration can
// be withdrawn without renumbering its successors.
func (s *Store) migrate(ctx context.Context) error {
	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	steps, err := loadMigrations()
	if err != nil {
		return err
	}
	latest := steps[len(steps)-1].version
	if current > latest {
		return fmt.Errorf("store: database schema %d is newer than this build supports (%d)", current, latest)
	}
	for _, step := range steps {
		if step.version <= current {
			continue
		}
		started := time.Now()
		if err := s.applyMigration(ctx, step); err != nil {
			return err
		}
		s.emit(ctx, &event.StoreMigrated{
			FromVersion: current,
			ToVersion:   step.version,
			DurationMS:  time.Since(started).Milliseconds(),
		})
		current = step.version
	}
	// The file itself may set user_version, and a migration that forgets to
	// does not fail applyMigration; this is the one place that proves the
	// database really reached the latest version.
	after, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if after != latest {
		return fmt.Errorf("store: schema is at user_version %d after migrating, want %d", after, latest)
	}
	return nil
}

// applyMigration runs one file and its user_version bump in a single
// transaction, then proves from inside that transaction that the version
// advanced before committing.
func (s *Store) applyMigration(ctx context.Context, step migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", step.name, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, step.body); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", step.name, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", step.version)); err != nil {
		return fmt.Errorf("store: set user_version for %s: %w", step.name, err)
	}
	var got int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&got); err != nil {
		return fmt.Errorf("store: read user_version after %s: %w", step.name, err)
	}
	if got != step.version {
		return fmt.Errorf("store: migration %s left user_version at %d, want %d", step.name, got, step.version)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", step.name, err)
	}
	return nil
}

// loadMigrations reads and sorts the embedded files by the numeric prefix of
// their names.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, migrationDir)
	if err != nil {
		return nil, fmt.Errorf("store: read embedded migrations: %w", err)
	}
	steps := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), migrationSuffix) {
			continue
		}
		base := strings.TrimSuffix(entry.Name(), migrationSuffix)
		versionText, _, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("store: migration %q is not named <version>_<name>.sql", entry.Name())
		}
		version, err := strconv.Atoi(versionText)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("store: migration %q has no numeric version prefix", entry.Name())
		}
		body, err := migrationFiles.ReadFile(migrationDir + "/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", entry.Name(), err)
		}
		steps = append(steps, migration{version: version, name: entry.Name(), body: string(body)})
	}
	if len(steps) == 0 {
		return nil, errors.New("store: no migrations are embedded")
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].version < steps[j].version })
	for i := 1; i < len(steps); i++ {
		if steps[i].version == steps[i-1].version {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", steps[i-1].name, steps[i].name, steps[i].version)
		}
	}
	return steps, nil
}

// emit writes a catalogue event when an emitter was supplied.
func (s *Store) emit(ctx context.Context, ev event.Event) {
	if s.emitter == nil {
		return
	}
	s.emitter.Emit(ctx, ev, event.Correlation{})
}
