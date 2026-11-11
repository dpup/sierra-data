// Package store is the PostgreSQL persistence layer for the grid service:
// events with full revision history, the place directory, and the source
// registry.
//
// The proto blob is canonical — scalar columns exist only as query indexes and
// every read path rehydrates from the blob. Writes are serialized through an
// internal mutex (single-writer discipline: the ingest scheduler) and, across
// processes, by the writer advisory lock Open takes (lease.go). Reads go
// straight to the connection pool under PostgreSQL's MVCC.
//
// The driver is pgx v5: a pgxpool.Pool, exposed to the existing
// database/sql code as *sql.DB through stdlib.OpenDBFromPool. The native pool
// stays reachable (Pool) for what database/sql cannot do: the lease
// connection, LISTEN, CopyFrom.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/lib/geojson"
	"github.com/sierragridteam/sierra-data/internal/store/migrations"
)

// ErrNotFound is returned by point lookups (GetEvent, GetPlace) when no row
// matches. Callers map it to a 404.
var ErrNotFound = errors.New("store: not found")

// Store wraps the database. Safe for concurrent use: writes take mu and run
// one at a time; reads go straight to the pool.
type Store struct {
	pool *pgxpool.Pool
	db   *sql.DB
	lock *writerLock
	mu   sync.Mutex // single-writer discipline within the process

	// placesGeo caches parsed place geometries so matchPlaces (called per event
	// on every ingest tick, including hash-equal no-ops) does not re-SELECT and
	// re-parse every place polygon under the write mutex. Guarded by mu — both
	// matchPlaces (inside inTx) and UpsertPlace (the only invalidator) hold it.
	// Safe as a process cache only because this process is the single writer.
	placesGeo      []parsedPlace
	placesGeoValid bool

	// wildfireBuffer is how close (metres) a WILDFIRE event may come to an AREA
	// or TOWN place and still attach to it. See WithWildfireProximity.
	wildfireBuffer float64

	// townRadius is how far (metres) from a TOWN's point a location may be and
	// still resolve to that town. See WithTownRadius.
	townRadius float64

	maxConns, minConns int32
	schemaVersion      int

	closeOnce sync.Once
	closeErr  error
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
	wildfireBuffer float64
	townRadius     float64
	maxConns       int32
	minConns       int32
	lockTimeout    time.Duration
	lockCtx        context.Context // ends a lock wait early; nil = wait the full timeout
	onLockLost     func(error)
}

// Pool defaults. A low-traffic read-mostly API: a handful of connections
// covers the concurrency, and every one is a backend process on a small RDS
// instance that two task sets share during a blue/green deploy.
const (
	DefaultMaxConns = 8
	DefaultMinConns = 1
)

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

// DefaultTownRadiusMeters is how close to a town's point a location must be to
// resolve to that town when grid.places.townRadiusMeters is unset. 3 km covers
// the built-up core of the service area's towns (Murphys, Arnold, Sonora)
// while the closest pair, Sonora and Columbia (~5.9 km apart), overlap only in
// a sliver, where both are returned, nearest first.
const DefaultTownRadiusMeters = 3000

// WithTownRadius sets how far (metres) from a TOWN place's point a location may
// be and still resolve to it (PlacesContaining). Towns are seeded as points, so
// "contains" has to mean "within this radius" for them, or no location would
// ever resolve to a town. Zero or negative keeps DefaultTownRadiusMeters.
// Resolve only: event attachment (matchPlaces) is unchanged.
func WithTownRadius(meters float64) Option {
	return func(c *openConfig) {
		if meters > 0 {
			c.townRadius = meters
		}
	}
}

// WithMaxConns bounds the pool (grid.database.maxConns). Zero or negative
// keeps DefaultMaxConns. The writer lock's dedicated connection is extra.
func WithMaxConns(n int) Option {
	return func(c *openConfig) {
		if n > 0 {
			c.maxConns = int32(n)
		}
	}
}

// WithMinConns keeps this many idle connections open (grid.database.minConns).
// Zero or negative keeps DefaultMinConns.
func WithMinConns(n int) Option {
	return func(c *openConfig) {
		if n > 0 {
			c.minConns = int32(n)
		}
	}
}

// WithLockTimeout bounds how long Open waits for the writer lock before
// failing (grid.database.lockTimeout). See DefaultLockTimeout for why waiting
// beats dying. Zero or negative keeps the default.
func WithLockTimeout(d time.Duration) Option {
	return func(c *openConfig) {
		if d > 0 {
			c.lockTimeout = d
		}
	}
}

// WithLockContext makes Open's lock wait end as soon as ctx does. A handoff
// activation (internal/handoff) passes its context so a SIGTERM during the
// wait aborts it at once, instead of holding shutdown for up to lockTimeout
// (90s, longer than ECS's 30s stop timeout).
func WithLockContext(ctx context.Context) Option {
	return func(c *openConfig) { c.lockCtx = ctx }
}

// WithLockLost registers a hook called (once, from a background goroutine)
// if the writer lock's connection fails its heartbeat after Open: the lock is
// gone and another process may now be writing. Today cmd/server only logs it;
// P3's lease turns it into "stop writing, drop to reader".
func WithLockLost(fn func(error)) Option {
	return func(c *openConfig) { c.onLockLost = fn }
}

// Open connects to the database at url (a PostgreSQL URL or DSN), takes the
// writer lock (waiting up to the lock timeout), applies pending migrations and
// returns the store. The database must use byte-order collation (see the
// baseline migration); Open refuses one that does not.
func Open(url string, opts ...Option) (*Store, error) {
	cfg := openConfig{maxConns: DefaultMaxConns, minConns: DefaultMinConns,
		lockTimeout: lockAcquireTimeout, townRadius: DefaultTownRadiusMeters}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.minConns > cfg.maxConns {
		cfg.minConns = cfg.maxConns
	}
	if url == "" {
		return nil, errors.New("store: no database URL (grid.database.url / PF__GRID__DATABASE__URL)")
	}
	ctx := cfg.lockCtx
	if ctx == nil {
		ctx = context.Background()
	}

	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parsing database URL: %w", err)
	}
	pcfg.MaxConns = cfg.maxConns
	pcfg.MinConns = cfg.minConns
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("store: creating pool: %w", err)
	}
	fail := func(err error) (*Store, error) {
		pool.Close()
		return nil, err
	}
	if err := checkCollation(ctx, pool); err != nil {
		return fail(err)
	}

	lock, err := acquireWriterLock(ctx, pool, cfg.lockTimeout, cfg.onLockLost)
	if err != nil {
		return fail(err)
	}
	db := stdlib.OpenDBFromPool(pool)
	s := &Store{pool: pool, db: db, lock: lock, wildfireBuffer: cfg.wildfireBuffer,
		townRadius: cfg.townRadius, maxConns: cfg.maxConns, minConns: cfg.minConns}
	// Migrations run only while holding the writer lock: the lock holder is
	// the one process allowed to change the database. They also take their
	// own transaction-scoped lock, so the runner is safe on its own.
	v, err := migrations.Apply(ctx, db)
	if err == nil {
		v, err = migrations.CheckVersion(ctx, db)
	}
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	s.schemaVersion = v
	return s, nil
}

// checkCollation refuses a database that does not compare text bytewise.
// Tested by behaviour rather than by name (C, POSIX, C.UTF-8 and the builtin
// provider all qualify): an en_US-style collation ignores punctuation at the
// first level and orders 'a-z' after 'a:a', which would silently reorder every
// keyset cursor over ids like "usgs:nc-1".
func checkCollation(ctx context.Context, pool *pgxpool.Pool) error {
	var bytewise bool
	if err := pool.QueryRow(ctx,
		`SELECT ('a-z' < 'a:a') AND ('B' < 'a') AND ('a:z' < 'aa')`).Scan(&bytewise); err != nil {
		return fmt.Errorf("store: connecting: %w", err)
	}
	if !bytewise {
		return errors.New("store: the database does not use byte-order collation; create it with " +
			"TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C' (see internal/store/migrations/0001_baseline.sql)")
	}
	return nil
}

// Close closes the pool and releases the writer lock. Once it returns, every
// query on the store fails and the next owner can take the lock at once.
// Idempotent: later calls return the first call's result.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		err := s.db.Close()
		if lerr := s.lock.release(); err == nil {
			err = lerr
		}
		s.pool.Close()
		s.closeErr = err
	})
	return s.closeErr
}

// Pool is the native pgx pool, for work database/sql cannot express (COPY,
// LISTEN, a dedicated connection). Ordinary reads and writes use the store's
// methods.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Settings describe the open store, for the startup log line.
type Settings struct {
	Database      string
	ServerVersion string
	SchemaVersion int
	MaxConns      int32
	MinConns      int32
}

// Settings reports the connection's effective configuration.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	st := Settings{SchemaVersion: s.schemaVersion, MaxConns: s.maxConns, MinConns: s.minConns}
	if err := s.db.QueryRowContext(ctx, `SELECT current_database(), current_setting('server_version')`).
		Scan(&st.Database, &st.ServerVersion); err != nil {
		return st, fmt.Errorf("store: reading settings: %w", err)
	}
	return st, nil
}

// inTx runs fn inside a write transaction under the store mutex. Read
// committed: the read-modify-write sequences inside (UpsertEvent's lookup,
// TransitionEvents, RecordAttempt, compaction) are safe because this process
// is the only writer (mu here, the writer lock across processes).
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

// readTx runs fn in a REPEATABLE READ READ ONLY transaction: one snapshot for
// a read that spans several statements, so a commit landing between them
// cannot produce an answer that never existed.
func (s *Store) readTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("store: begin read: %w", err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
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
