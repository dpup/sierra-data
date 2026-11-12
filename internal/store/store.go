// Package store is the PostgreSQL persistence layer for the grid service:
// events with full revision history, the place directory, and the source
// registry.
//
// The proto blob is canonical — scalar columns exist only as query indexes and
// every read path rehydrates from the blob. Writes are serialized through an
// internal mutex (single-writer discipline: the ingest scheduler) and, across
// processes, by the writer lease (lease.go): every process opens a reader
// (OpenReader), one at a time holds the lease (AcquireLease), and every write
// transaction is fenced by its epoch. Reads go straight to the connection pool
// under PostgreSQL's MVCC.
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
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
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
// one at a time, and only while this process holds the writer lease
// (lease.go); reads go straight to the pool.
type Store struct {
	pool *pgxpool.Pool
	db   *sql.DB
	mu   sync.Mutex // single-writer discipline within the process

	// The writer lease, nil for a reader. Swapped under leaseMu (acquire,
	// release) and mu (so an in-flight write finishes before a release).
	leaseMu sync.Mutex
	lease   atomic.Pointer[Lease]
	// txEpoch is the epoch of the write transaction in progress (0 outside
	// one). Guarded by mu. loadPlaceGeoms checks it, so the place cache can
	// only be built or used inside a fenced write.
	txEpoch int64

	// placesGeo caches parsed place geometries so matchPlaces (called per event
	// on every ingest tick, including hash-equal no-ops) does not re-SELECT and
	// re-parse every place polygon under the write mutex. Guarded by mu — both
	// matchPlaces (inside inTx) and UpsertPlace (the only invalidator) hold it.
	// Valid only for the lease term that built it (placesGeoEpoch): another
	// writer may change places while this process is a reader.
	placesGeo      []parsedPlace
	placesGeoValid bool
	placesGeoEpoch int64

	// versions caches the validator counters for readers (versions.go).
	versions  versionCache
	watchStop context.CancelFunc
	watchDone chan struct{}

	// wildfireBuffer is how close (metres) a WILDFIRE event may come to an AREA
	// or TOWN place and still attach to it. See WithWildfireProximity.
	wildfireBuffer float64

	// townRadius is how far (metres) from a TOWN's point a location may be and
	// still resolve to that town. See WithTownRadius.
	townRadius float64

	maxConns, minConns int32
	schemaVersion      atomic.Int64

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
	versionPoll    time.Duration
	versionListen  bool
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

// WithLockTimeout bounds how long Open waits for the writer lease before
// failing (grid.database.lockTimeout). Zero or negative keeps the default.
// OpenReader ignores it; AcquireLease takes its own wait.
func WithLockTimeout(d time.Duration) Option {
	return func(c *openConfig) {
		if d > 0 {
			c.lockTimeout = d
		}
	}
}

// WithLockContext makes Open's lease wait end as soon as ctx does.
func WithLockContext(ctx context.Context) Option {
	return func(c *openConfig) { c.lockCtx = ctx }
}

// WithLockLost registers the OnLost hook for the lease Open takes: called
// once, from a background goroutine, if the lease is lost (its session died
// or another process bumped the epoch).
func WithLockLost(fn func(error)) Option {
	return func(c *openConfig) { c.onLockLost = fn }
}

// DefaultVersionPoll is how often a store re-reads the validator counters
// even without a notification: the bound on how long a reader can answer a
// stale 304 when its LISTEN connection is down.
const DefaultVersionPoll = 10 * time.Second

// WithVersionPoll sets the validator backstop poll (DefaultVersionPoll).
func WithVersionPoll(d time.Duration) Option {
	return func(c *openConfig) {
		if d > 0 {
			c.versionPoll = d
		}
	}
}

// WithoutVersionListen turns off the LISTEN fast path, leaving the poll
// (tests of the backstop).
func WithoutVersionListen() Option {
	return func(c *openConfig) { c.versionListen = false }
}

// Open opens the store and takes the writer lease, waiting up to the lock
// timeout: OpenReader plus AcquireLease. It is the one-process path (tools,
// tests); the server opens a reader and campaigns separately (cmd/server).
func Open(url string, opts ...Option) (*Store, error) {
	s, cfg, err := openReader(url, opts...)
	if err != nil {
		return nil, err
	}
	ctx := cfg.lockCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := s.AcquireLease(ctx, LeaseOptions{Holder: defaultHolder(),
		Wait: cfg.lockTimeout, OnLost: cfg.onLockLost}); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// OpenReader connects to the database at url (a PostgreSQL URL or DSN) as a
// reader: the pool, the collation check, pending migrations, and the
// validator watch. It takes no writer lease and writes no data.
//
// Every process migrates at boot (migrations package doc): OpenReader runs
// migrations.Apply under the migration advisory lock before it returns, so a
// store that opened is always at a schema this build reads. A database
// already NEWER than this build is accepted as it is (additive-only policy).
// A migration that fails fails the open. The database must use byte-order
// collation (see the baseline migration); OpenReader refuses one that does
// not.
func OpenReader(url string, opts ...Option) (*Store, error) {
	s, _, err := openReader(url, opts...)
	return s, err
}

func openReader(url string, opts ...Option) (*Store, openConfig, error) {
	cfg := openConfig{maxConns: DefaultMaxConns, minConns: DefaultMinConns,
		lockTimeout: lockAcquireTimeout, townRadius: DefaultTownRadiusMeters,
		versionPoll: DefaultVersionPoll, versionListen: true}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.minConns > cfg.maxConns {
		cfg.minConns = cfg.maxConns
	}
	if url == "" {
		return nil, cfg, errors.New("store: no database URL (grid.database.url / PF__GRID__DATABASE__URL)")
	}
	ctx := cfg.lockCtx
	if ctx == nil {
		ctx = context.Background()
	}

	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, cfg, fmt.Errorf("store: parsing database URL: %w", err)
	}
	pcfg.MaxConns = cfg.maxConns
	pcfg.MinConns = cfg.minConns
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, cfg, fmt.Errorf("store: creating pool: %w", err)
	}
	if err := checkCollation(ctx, pool); err != nil {
		pool.Close()
		return nil, cfg, err
	}
	db := stdlib.OpenDBFromPool(pool)
	s := &Store{pool: pool, db: db, wildfireBuffer: cfg.wildfireBuffer,
		townRadius: cfg.townRadius, maxConns: cfg.maxConns, minConns: cfg.minConns}
	v, err := migrations.Apply(ctx, db)
	if err != nil {
		s.Close()
		return nil, cfg, fmt.Errorf("store: %w", err)
	}
	s.schemaVersion.Store(int64(v))
	s.startVersionWatch(cfg.versionPoll, cfg.versionListen)
	return s, cfg, nil
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

// Close releases the writer lease (if held), stops the validator watch and
// closes the pool. Once it returns, every query on the store fails and the
// next writer can take the lease at once. Idempotent: later calls return the
// first call's result.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		err := s.ReleaseLease()
		if s.watchStop != nil {
			s.watchStop()
			<-s.watchDone
		}
		if derr := s.db.Close(); err == nil {
			err = derr
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
	st := Settings{SchemaVersion: int(s.schemaVersion.Load()), MaxConns: s.maxConns, MinConns: s.minConns}
	if err := s.db.QueryRowContext(ctx, `SELECT current_database(), current_setting('server_version')`).
		Scan(&st.Database, &st.ServerVersion); err != nil {
		return st, fmt.Errorf("store: reading settings: %w", err)
	}
	return st, nil
}

// inTx runs fn inside a write transaction under the store mutex, fenced by
// the writer lease: it refuses without a live lease (ErrNotWriter), and its
// first statement checks writer_lease.epoch with a key-share lock held to
// commit (ErrFenced when another process has bumped it; see lease.go for why
// that ordering is airtight). Read committed: the read-modify-write sequences
// inside (UpsertEvent's lookup, TransitionEvents, RecordAttempt, compaction)
// are safe because this process is the only writer (mu here, the lease
// across processes).
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lease.Load()
	if !l.live() {
		return ErrNotWriter
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM writer_lease WHERE id = 1 AND epoch = $1 FOR KEY SHARE`, l.epoch).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		l.end(ErrFenced, true)
		return ErrFenced
	}
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("store: checking the writer lease: %w", err)
	}
	s.txEpoch = l.epoch
	defer func() { s.txEpoch = 0 }()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.versions.markDirty() // this process reads its own writes' validators at once
	return nil
}

// WritePgxTx runs fn in a fenced write transaction on a native pgx
// connection, for writes database/sql cannot express (COPY: the SQLite
// importer). The guarantees are inTx's: refused with ErrNotWriter without a
// live writer lease, the epoch fence (FOR KEY SHARE) runs first and holds for
// the whole transaction, and it runs under the write mutex. fn's error rolls
// back; nil commits.
func (s *Store) WritePgxTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lease.Load()
	if !l.live() {
		return ErrNotWriter
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(context.Background()) // no-op after Commit
	var one int
	err = tx.QueryRow(ctx,
		`SELECT 1 FROM writer_lease WHERE id = 1 AND epoch = $1 FOR KEY SHARE`, l.epoch).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		l.end(ErrFenced, true)
		return ErrFenced
	}
	if err != nil {
		return fmt.Errorf("store: checking the writer lease: %w", err)
	}
	s.txEpoch = l.epoch
	defer func() { s.txEpoch = 0 }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.versions.markDirty()
	return nil
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
