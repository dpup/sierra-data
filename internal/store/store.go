// Package store is the SQLite persistence layer for the grid service: events
// with full revision history, the place directory, and the source registry.
//
// The proto blob is canonical — scalar columns exist only as query indexes and
// every read path rehydrates from the blob. Writes are serialized through an
// internal mutex (single-writer discipline: the ingest scheduler); reads go to
// the pool and serialize against the writer's brief commit via busy_timeout
// (the default TRUNCATE rollback journal has no WAL MVCC). The journal mode is
// configurable (WithJournalMode) so the DB can live on a network filesystem
// (NFS/EFS), where WAL's memory-mapped -shm file does not work.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dpup/sierra-data/internal/lib/geojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	_ "modernc.org/sqlite" // pure-Go driver: CGO_ENABLED=0 cross-compile requirement
)

//go:embed schema.sql
var schemaV1 string

// migrationV2 adds last-seen tracking (see TouchSeen): the lifecycle expire
// grace is measured from when a source last CONFIRMED an event, not from
// when its content last changed — a stable event must not expire just
// because it never produced a new revision. DEFAULT 0 leaves pre-migration
// rows with a zero LastSeenAt so callers can fall back to observed/ingested.
const migrationV2 = `ALTER TABLE events ADD COLUMN last_seen_at INTEGER NOT NULL DEFAULT 0`

// migrationV3 adds the MeshCore relay-topology tables (docs/mesh-topology-design.md):
//   - mesh_observations: the append-only reception firehose (Tier 0, short-lived) —
//     one row per advert we heard, our-clock timestamp, signal + relay path.
//   - mesh_link_rollup: the derived per-link-per-day topology history (Tier 1).
//   - mesh_meta: KV for the compaction watermark.
// These are pure DERIVED telemetry, NOT proto-blob-canonical system-of-record
// data — plain columns are correct, and they re-accumulate from the live feed if
// lost (unlike event history). Compaction rolls Tier 0 into Tier 1 and prunes.
const migrationV3 = `
CREATE TABLE mesh_observations (
  id         INTEGER PRIMARY KEY,       -- rowid, monotonic
  pubkey     TEXT NOT NULL,             -- advertising node
  heard_at   INTEGER NOT NULL,          -- OUR receive time (unix sec) — the trustworthy clock
  broker     TEXT NOT NULL DEFAULT '',  -- MQTT server heard on
  gateway    TEXT NOT NULL DEFAULT '',  -- origin_id / gateway that reported it
  snr        REAL,
  rssi       INTEGER,
  hop_count  INTEGER NOT NULL DEFAULT 0,
  path       TEXT NOT NULL DEFAULT '',  -- comma-joined hop prefix-hashes (hex), as received
  path_nodes TEXT NOT NULL DEFAULT ''   -- resolved pubkeys ('' where a hop was unresolved)
);
CREATE INDEX idx_mesh_obs_heard  ON mesh_observations(heard_at);
CREATE INDEX idx_mesh_obs_pubkey ON mesh_observations(pubkey, heard_at);

CREATE TABLE mesh_link_rollup (
  a_pubkey     TEXT NOT NULL,           -- canonical a < b
  b_pubkey     TEXT NOT NULL,
  bucket       INTEGER NOT NULL,        -- heard_at truncated to UTC day
  observations INTEGER NOT NULL DEFAULT 0,
  best_snr     REAL,
  first_seen   INTEGER NOT NULL,
  last_seen    INTEGER NOT NULL,
  PRIMARY KEY (a_pubkey, b_pubkey, bucket)
);
CREATE INDEX idx_mesh_link_bucket ON mesh_link_rollup(bucket);

CREATE TABLE mesh_meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);`

// migrations[i] is the DDL for schema version i+1. Applied versions are
// recorded in schema_migrations; already-applied versions are skipped, so
// Open is idempotent across restarts and an existing dev DB at an older
// version picks up only the missing migrations.
var migrations = []string{schemaV1, migrationV2, migrationV3}

// ErrNotFound is returned by point lookups (GetEvent, GetPlace) when no row
// matches. Callers map it to a 404.
var ErrNotFound = errors.New("store: not found")

// Store wraps the SQLite database. Safe for concurrent use: writes take mu and
// run one at a time; reads go straight to the pool and serialize against the
// writer's short commit via busy_timeout.
type Store struct {
	db       *sql.DB
	lockFile *os.File   // exclusive flock guard; released on Close/process exit
	mu       sync.Mutex // single-writer discipline

	// placesGeo caches parsed place geometries so matchPlaces (called per event
	// on every ingest tick, including hash-equal no-ops) does not re-SELECT and
	// re-parse every place polygon under the write mutex. Guarded by mu — both
	// matchPlaces (inside inTx) and UpsertPlace (the only invalidator) hold it.
	placesGeo      []parsedPlace
	placesGeoValid bool
}

// parsedPlace is a place's geometry pre-parsed for point-in-place / bbox tests.
type parsedPlace struct {
	id                             string
	geom                           *geojson.Geom
	minLat, minLng, maxLat, maxLng float64
	centLat, centLng               float64
	polygonal                      bool
}

// Option configures Open.
type Option func(*openConfig)

type openConfig struct{ journalMode string }

// journalModeSynchronous maps a journal mode to the synchronous level that
// keeps it crash-safe. Whitelisted (also guards against DSN injection): only
// these values are accepted.
//   - WAL: NORMAL is safe — a crash can only lose the last transaction, never
//     corrupt. Needs a real local disk (the -shm is memory-mapped, so WAL does
//     NOT work over NFS/EFS).
//   - DELETE/TRUNCATE/PERSIST (rollback journals): FULL, so the journal is
//     durably synced before the DB pages are overwritten — a rollback journal
//     with only NORMAL can corrupt on power loss. These modes have no shared-
//     memory file and work over a network filesystem (NFS/EFS). TRUNCATE is
//     preferred there: it zero-truncates the journal instead of unlinking it,
//     one fewer metadata round-trip per commit.
var journalModeSynchronous = map[string]string{
	"WAL":      "NORMAL",
	"DELETE":   "FULL",
	"TRUNCATE": "FULL",
	"PERSIST":  "FULL",
}

// WithJournalMode selects the SQLite journal mode (default TRUNCATE — safe on
// both local disk and a network filesystem). Use WAL only on a real local disk.
func WithJournalMode(mode string) Option {
	return func(c *openConfig) {
		if m := strings.ToUpper(strings.TrimSpace(mode)); m != "" {
			c.journalMode = m
		}
	}
}

// Open opens (creating if needed) the database at path, applies pragmas via
// the DSN, and runs any pending migrations. The parent directory is created.
// The default journal mode is TRUNCATE (works on local disk AND NFS/EFS);
// override with WithJournalMode.
func Open(path string, opts ...Option) (*Store, error) {
	cfg := openConfig{journalMode: "TRUNCATE"}
	for _, o := range opts {
		o(&cfg)
	}
	sync, ok := journalModeSynchronous[cfg.journalMode]
	if !ok {
		return nil, fmt.Errorf("store: unsupported journal_mode %q (want one of WAL, DELETE, TRUNCATE, PERSIST)", cfg.journalMode)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: creating db dir: %w", err)
		}
	}

	// Single-writer guard: two processes writing the same SQLite file corrupt it
	// on filesystems where SQLite's own locking isn't honored (bind mounts /
	// network FS). Take an exclusive advisory lock so a second opener fails loudly
	// instead. The flock is tied to the fd, so the kernel releases it on Close or
	// process death — no stale lock. (This coordinates within one kernel; a writer
	// on a separate host reaching the file via the mount can still slip past — for
	// that, don't share the db path.)
	lockFile, err := acquireDBLock(path)
	if err != nil {
		return nil, err
	}
	// _pragma entries apply to every pooled connection. busy_timeout absorbs the
	// reader/writer serialization of a rollback journal (no WAL MVCC): a reader
	// waits up to this long for the single writer's brief commit rather than
	// erroring SQLITE_BUSY.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(%s)&_pragma=foreign_keys(1)&_pragma=synchronous(%s)",
		path, cfg.journalMode, sync)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		lockFile.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &Store{db: db, lockFile: lockFile}
	if err := s.migrate(); err != nil {
		db.Close()
		lockFile.Close()
		return nil, err
	}
	return s, nil
}

// lockAcquireTimeout bounds how long Open waits for the db lock. A brief wait
// lets a rolling deploy's new task acquire once the previous writer drains and
// exits (releasing its flock), instead of failing on a few-second overlap on the
// shared volume. A genuinely stuck second writer still fails loud after this.
// Overridable in tests.
var lockAcquireTimeout = 15 * time.Second

// acquireDBLock takes an exclusive advisory lock on <path>.lock so a second
// process opening the same database fails loudly instead of silently corrupting
// it. The lock is an flock bound to the open fd — released automatically by the
// kernel on Close or if the process dies, so there is no stale lock to clean up.
// It waits up to lockAcquireTimeout for a contended lock (deploy drain window)
// before giving up.
func acquireDBLock(dbPath string) (*os.File, error) {
	lockPath := dbPath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: opening lock file %s: %w", lockPath, err)
	}
	deadline := time.Now().Add(lockAcquireTimeout)
	for {
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			break
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			f.Close()
			return nil, fmt.Errorf("store: database %q is already open by another process "+
				"(concurrent writers corrupt SQLite); stop the other server or point "+
				"PF__GRID__DBPATH at a different file: %w", dbPath, lockErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
	// Record our pid for humans inspecting the lock file (best-effort).
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	}
	return f, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.lockFile != nil {
		s.lockFile.Close() // releases the flock
	}
	return err
}

func (s *Store) migrate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
	); err != nil {
		return fmt.Errorf("store: creating schema_migrations: %w", err)
	}
	var current sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("store: reading schema version: %w", err)
	}
	for i, ddl := range migrations {
		version := int64(i + 1)
		if current.Valid && current.Int64 >= version {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("store: begin migration %d: %w", version, err)
		}
		if _, err := tx.Exec(ddl); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: applying migration %d: %w", version, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, time.Now().Unix(),
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: recording migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %d: %w", version, err)
		}
	}
	return nil
}

// inTx runs fn inside a write transaction under the store mutex.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// unixOrNil maps a possibly-nil proto timestamp to a nullable unix-seconds
// column value.
func unixOrNil(ts *timestamppb.Timestamp) any {
	if ts == nil {
		return nil
	}
	return ts.AsTime().Unix()
}

// tsFromNull maps a nullable unix-seconds column back to a proto timestamp.
func tsFromNull(v sql.NullInt64) *timestamppb.Timestamp {
	if !v.Valid {
		return nil
	}
	return timestamppb.New(time.Unix(v.Int64, 0))
}
