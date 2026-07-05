// Package store is the SQLite persistence layer for the grid service: events
// with full revision history, the place directory, and the source registry.
//
// The proto blob is canonical — scalar columns exist only as query indexes and
// every read path rehydrates from the blob. Writes are serialized through an
// internal mutex (single-writer discipline: the ingest scheduler); reads run
// concurrently under WAL.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

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

// migrations[i] is the DDL for schema version i+1. Applied versions are
// recorded in schema_migrations; already-applied versions are skipped, so
// Open is idempotent across restarts and an existing dev DB at an older
// version picks up only the missing migrations.
var migrations = []string{schemaV1, migrationV2}

// ErrNotFound is returned by point lookups (GetEvent, GetPlace) when no row
// matches. Callers map it to a 404.
var ErrNotFound = errors.New("store: not found")

// Store wraps the SQLite database. Safe for concurrent use: writes take mu,
// reads go straight to the pool (WAL allows readers alongside the writer).
type Store struct {
	db *sql.DB
	mu sync.Mutex // single-writer discipline
}

// Open opens (creating if needed) the database at path, applies pragmas via
// the DSN, and runs any pending migrations. The parent directory is created.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: creating db dir: %w", err)
		}
	}
	// _pragma entries apply to every pooled connection.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
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
