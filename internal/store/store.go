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
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
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
//
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

// migrationV4 indexes event_revisions by observed_at so QueryHistory (the
// cross-event /api/v1/history archive) can walk an index instead of scanning
// every revision and sorting. The column order mirrors that query's ORDER BY
// exactly — observed_at DESC, event_id ASC, revision DESC — so SQLite satisfies
// both the [from, to) range and the sort from the same index, and the keyset
// cursor seeks rather than skips. Without it the endpoint degraded to 6–40s at
// page_size=50 in production (measured 2026-08-06); the per-event timeline
// (EventHistory) was always fine because it keys on the PRIMARY KEY prefix.
const migrationV4 = `
CREATE INDEX idx_revisions_observed
  ON event_revisions(observed_at DESC, event_id ASC, revision DESC);`

// migrations[i] is the DDL for schema version i+1. Applied versions are
// recorded in schema_migrations; already-applied versions are skipped, so
// Open is idempotent across restarts and an existing dev DB at an older
// version picks up only the missing migrations.
var migrations = []string{schemaV1, migrationV2, migrationV3, migrationV4}

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

	// wildfireBuffer is how close (metres) a WILDFIRE event may come to an AREA
	// or TOWN place and still attach to it. See WithWildfireProximity.
	wildfireBuffer float64

	// maxOpenConns is the pool bound actually applied, kept for Settings.
	maxOpenConns int

	// placesVersion increments whenever the place set changes. Callers that
	// cache work DERIVED from place geometry use it to know when that derived
	// state is stale — specifically the ingest scheduler, which skips hash-equal
	// upserts and so would otherwise never recompute event->place attachments
	// for a place seeded after an event first arrived. Guarded by mu.
	placesVersion uint64
}

// PlacesVersion returns a counter that changes whenever the place set changes.
// It is the signal that derived event->place attachments need recomputing; see
// the ingest scheduler's full-reconcile pass.
func (s *Store) PlacesVersion() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.placesVersion
}

// parsedPlace is a place's geometry pre-parsed for point-in-place / bbox tests.
type parsedPlace struct {
	id                             string
	kind                           gridv1.PlaceKind
	geom                           *geojson.Geom
	minLat, minLng, maxLat, maxLng float64
	centLat, centLng               float64
	polygonal                      bool
}

// Option configures Open.
type Option func(*openConfig)

type openConfig struct {
	journalMode    string
	wildfireBuffer float64
	cacheSizeMB    int
	maxOpenConns   int
	lockTimeout    time.Duration
}

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

// WithWildfireProximity sets how close (in metres) a WILDFIRE event has to come
// to an AREA or TOWN place to attach to it, even without overlapping — so an
// approaching fire appears on that place's map and summary before its perimeter
// crosses the boundary. Zero (the default) keeps the strict overlap rules for
// every layer. Configured via grid.wildfire.placeBufferMeters; see matchPlaces
// for why only these two place kinds are buffered.
func WithWildfireProximity(meters float64) Option {
	return func(c *openConfig) {
		if meters > 0 {
			c.wildfireBuffer = meters
		}
	}
}

// WithCacheSizeMB sets SQLite's page cache PER POOLED CONNECTION. Worst-case
// resident cache is this times MaxOpenConns, so size it against the task's
// memory limit. Zero or negative keeps DefaultCacheSizeMB.
func WithCacheSizeMB(mb int) Option {
	return func(c *openConfig) {
		if mb > 0 {
			c.cacheSizeMB = mb
		}
	}
}

// WithLockTimeout bounds how long Open waits for the database lock before
// failing. See DefaultLockTimeout for why waiting beats dying. Zero or negative
// keeps the default.
func WithLockTimeout(d time.Duration) Option {
	return func(c *openConfig) {
		if d > 0 {
			c.lockTimeout = d
		}
	}
}

// WithMaxOpenConns bounds concurrent connections (and so multiplies the
// per-connection page cache when budgeting memory). Zero or negative keeps
// DefaultMaxOpenConns.
func WithMaxOpenConns(n int) Option {
	return func(c *openConfig) {
		if n > 0 {
			c.maxOpenConns = n
		}
	}
}

// Connection-pool shape. These exist because of how SQLite caches pages: the
// page cache is PER CONNECTION, so a connection that database/sql closes takes
// its warm cache with it.
//
// Go's default is MaxIdleConns=2 with unlimited open connections, which is the
// pathological combination here — under any concurrency most queries run on a
// freshly created connection with a COLD cache, and on EFS every page it then
// misses is a network round trip rather than a disk read. Keeping idle equal to
// open means connections are never closed for being surplus, so their caches
// stay warm for the life of the process.
//
// MaxOpenConns also bounds how many readers can pile onto the rollback journal
// at once (TRUNCATE has no WAL MVCC, so readers serialize against the writer's
// commit via busy_timeout).
const (
	// DefaultMaxOpenConns bounds concurrent connections. It is also the
	// MULTIPLIER on per-connection cache when budgeting memory, which is the
	// reason to think about the two together: resident cache is
	// cacheSizeMb x maxOpenConns.
	//
	// Fewer, fatter connections generally beat more, thinner ones here. This is
	// a low-traffic read-mostly API, so concurrency past a handful buys nothing,
	// while a bigger per-connection cache directly cuts EFS round trips. It also
	// reduces how many readers pile onto the rollback journal at once (TRUNCATE
	// has no WAL MVCC, so readers serialize against the writer's commit via
	// busy_timeout).
	DefaultMaxOpenConns = 8
	// DefaultCacheSizeMB is the per-connection page cache: 8 MB x 8 conns =
	// 64 MB worst case, deliberately conservative because the task's memory
	// limit is not knowable from the code. Size it against the HOT set, not the
	// file: event_revisions and mesh_observations dominate the file but are cold
	// for /events. See internal/store/CLAUDE.md.
	DefaultCacheSizeMB = 8
)

// Open opens (creating if needed) the database at path, applies pragmas via
// the DSN, and runs any pending migrations. The parent directory is created.
// The default journal mode is TRUNCATE (works on local disk AND NFS/EFS);
// override with WithJournalMode.
func Open(path string, opts ...Option) (*Store, error) {
	cfg := openConfig{journalMode: "TRUNCATE", cacheSizeMB: DefaultCacheSizeMB,
		maxOpenConns: DefaultMaxOpenConns, lockTimeout: lockAcquireTimeout}
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
	lockFile, err := acquireDBLock(path, cfg.lockTimeout)
	if err != nil {
		return nil, err
	}
	// _pragma entries apply to every pooled connection. busy_timeout absorbs the
	// reader/writer serialization of a rollback journal (no WAL MVCC): a reader
	// waits up to this long for the single writer's brief commit rather than
	// erroring SQLITE_BUSY.
	// cache_size is negative to mean KiB rather than pages (a page-count cache
	// silently changes size with page_size). mmap_size is deliberately NOT set:
	// memory-mapped I/O over NFS is the same class of hazard that makes WAL
	// unusable on EFS, and the page cache above already covers the read path.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(%s)&_pragma=foreign_keys(1)"+
			"&_pragma=synchronous(%s)&_pragma=cache_size(-%d)",
		path, cfg.journalMode, sync, cfg.cacheSizeMB*1024)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		lockFile.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Idle == open so a pooled connection is never closed merely for being
	// surplus, and its warm page cache survives. ConnMaxLifetime stays 0 (no
	// expiry) for the same reason — recycling a connection here buys nothing
	// and throws away the cache it accumulated.
	db.SetMaxOpenConns(cfg.maxOpenConns)
	db.SetMaxIdleConns(cfg.maxOpenConns)

	// Read the pragmas back off a pooled connection. Pragmas are PER CONNECTION,
	// so nothing outside this process can confirm what actually applied — and a
	// journal mode can silently not apply (WAL degrades to a rollback journal on
	// a filesystem that cannot support it, which is precisely the EFS case). A
	// mismatch is a hard error rather than a warning: the difference decides
	// whether readers block on the writer and whether the durability reasoning in
	// journalModeSynchronous still holds, and both are worth refusing to start on.
	applied, err := readSettings(db, cfg.maxOpenConns)
	if err != nil {
		db.Close()
		lockFile.Close()
		return nil, err
	}
	if !strings.EqualFold(applied.JournalMode, cfg.journalMode) {
		db.Close()
		lockFile.Close()
		return nil, fmt.Errorf(
			"store: requested journal_mode %q but SQLite applied %q — the filesystem at %s "+
				"probably cannot support it (WAL needs a real local disk; use TRUNCATE on NFS/EFS)",
			cfg.journalMode, applied.JournalMode, path)
	}
	s := &Store{db: db, lockFile: lockFile, wildfireBuffer: cfg.wildfireBuffer, maxOpenConns: cfg.maxOpenConns}
	if err := s.migrate(); err != nil {
		db.Close()
		lockFile.Close()
		return nil, err
	}
	return s, nil
}

// DefaultLockTimeout bounds how long Open waits for the db lock, letting a
// rolling deploy's new task acquire once the previous writer drains and exits
// (releasing its flock). A genuinely stuck second writer still fails loud after
// this.
//
// It is 90s, not the original 15s, because 15s was shorter than a real ECS task
// drain. What that produced was not one clean failure but a CRASH LOOP: the new
// task gave up after 15s, main.go log.Fatalf'd, ECS restarted it, and it failed
// again — for as long as the old task took to go away. The service was 503 that
// whole time, and the logs said "already open by another process" rather than
// anything about draining. WAITING is strictly better than dying here: the task
// has nothing useful to do until the lock is free either way.
//
// The ceiling on this value is the container health check, not patience: the
// listener does not open until Open returns, so a wait longer than the health
// check's grace gets the container killed mid-wait. The Dockerfile HEALTHCHECK
// start-period is set to accommodate this — raise them together.
//
// The other half of the fix is NOT here: the old task must actually die
// promptly. prefab drains in ~2s on SIGTERM, so a multi-minute drain is the load
// balancer's deregistration delay (default 300s), not this process.
const DefaultLockTimeout = 90 * time.Second

// lockAcquireTimeout is the effective value; overridden per-Open via
// WithLockTimeout, and by tests.
var lockAcquireTimeout = DefaultLockTimeout

// acquireDBLock takes an exclusive advisory lock on <path>.lock so a second
// process opening the same database fails loudly instead of silently corrupting
// it. The lock is an flock bound to the open fd — released automatically by the
// kernel on Close or if the process dies, so there is no stale lock to clean up.
// It waits up to lockAcquireTimeout for a contended lock (deploy drain window)
// before giving up.
func acquireDBLock(dbPath string, timeout time.Duration) (*os.File, error) {
	lockPath := dbPath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: opening lock file %s: %w", lockPath, err)
	}
	start := time.Now()
	deadline := start.Add(timeout)
	announced := false
	for {
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			if announced {
				// Say so explicitly: this is the line that distinguishes "the
				// deploy was slow because the old task was draining" from every
				// other cause of a slow start.
				log.Printf("store: acquired database lock after waiting %s for the previous writer to exit",
					time.Since(start).Round(100*time.Millisecond))
			}
			break
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			f.Close()
			return nil, fmt.Errorf("store: database %q is still locked by another process after %s "+
				"(concurrent writers corrupt SQLite). On a deploy this means the previous task has "+
				"not exited yet — check the load balancer's deregistration delay and the service's "+
				"minimumHealthyPercent. Otherwise stop the other server or point PF__GRID__DB_PATH "+
				"at a different file: %w", dbPath, timeout, lockErr)
		}
		if !announced {
			// Announced ONCE, up front, so a task that is merely waiting out a
			// drain is distinguishable from one that is hung — previously this
			// loop was completely silent and a crash-looping deploy gave the
			// operator nothing to go on.
			log.Printf("store: database %q is locked by another process; waiting up to %s for it to exit "+
				"(normal during a deploy while the previous task drains)", dbPath, timeout)
			announced = true
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

// Settings are the pragmas SQLite actually applied, read back off a pooled
// connection. Worth logging at startup: pragmas are per-connection, so this is
// the only place the effective configuration is observable.
type Settings struct {
	JournalMode  string
	CacheSizeKB  int // negative pragma value normalized to KiB; 0 if page-based
	Synchronous  int
	MaxOpenConns int
}

// Settings reports the pragmas in force. See the Settings type.
func (s *Store) Settings() (Settings, error) { return readSettings(s.db, s.maxOpenConns) }

func readSettings(db *sql.DB, maxOpenConns int) (Settings, error) {
	var st Settings
	st.MaxOpenConns = maxOpenConns
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&st.JournalMode); err != nil {
		return st, fmt.Errorf("store: reading journal_mode: %w", err)
	}
	var cache int
	if err := db.QueryRow(`PRAGMA cache_size`).Scan(&cache); err != nil {
		return st, fmt.Errorf("store: reading cache_size: %w", err)
	}
	// SQLite reports a negative cache_size in KiB and a positive one in pages.
	if cache < 0 {
		st.CacheSizeKB = -cache
	}
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&st.Synchronous); err != nil {
		return st, fmt.Errorf("store: reading synchronous: %w", err)
	}
	return st, nil
}

// Analyze refreshes SQLite's index statistics (sqlite_stat1). It must be run
// periodically, and it is NOT optional tuning — without stats the place-scoped
// event query degrades without bound.
//
// The query is `FROM events e JOIN event_places ep ON ... ep.place_id = ?
// WHERE e.status IN (ACTIVE, SCHEDULED)`. With no stats SQLite assumes an
// equality on an indexed column is highly selective, so it drives from
// event_places and fetches EVERY event that place has ever been attached to
// out of the wide events table, only to discard the dead ones. Attachments are
// deliberately never deleted on a lifecycle transition (place-scoped history
// needs them), so that dead set grows forever: measured 62-96% dead after five
// days, 99% at a year's projection. With stats SQLite sees that the ACTIVE set
// is small and bounded and drives from idx_events_active instead, discarding
// the dead rows inside the index.
//
// Measured on a synthetic reproducing production's exact query plan (local
// disk, warm cache — production is on EFS, where every discarded row fetch is
// an extra network round trip, so the real spread is far wider):
//
//	attachments   no stats    with stats
//	        500     2.2 ms       2.0 ms
//	      6,400    10.9 ms       4.0 ms
//	     50,400    69.1 ms       3.9 ms
//
// Do NOT use `PRAGMA analysis_limit` here: at the documented 400-row sample the
// plan does not change at all (68.8 ms at the largest size), because the sample
// is far too small to see the skew. A full ANALYZE cost 119 ms at that size.
// `PRAGMA optimize` is likewise a no-op on a freshly pooled connection.
func (s *Store) Analyze(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `ANALYZE`); err != nil {
		return fmt.Errorf("store: analyze: %w", err)
	}
	return nil
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
