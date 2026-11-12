// Package migrations holds the Grid store's PostgreSQL schema as ordered,
// embedded SQL files and the hand-rolled runner that applies them.
//
// Files are named NNNN_slug.sql; NNNN is the schema version. The runner
// applies each pending version in its own transaction, under a
// transaction-scoped advisory lock so two processes starting together cannot
// both migrate, and records it in schema_migrations. DDL is transactional in
// PostgreSQL, so a failed migration leaves nothing behind.
//
// Who migrates: EVERY process, at boot (store.OpenReader runs Apply before the
// open completes), reader or writer; the lease holder runs it again when it
// takes the lease, which is a no-op. Migrating needs only the migration
// advisory lock, not the writer lease. That is safe only because of the
// policy below: while a new release migrates, the previous release's readers
// AND its writer keep running against the newer schema.
//
// Policy (binding on every new file):
//   - Append-only. Never edit, rename or delete a shipped file: a deployed
//     database has recorded its version and will skip it. Add a new one.
//   - Additive only. New tables, new columns, new indexes, new keys. Never
//     rename or drop a table, column, index, constraint, trigger or function a
//     released reader or writer uses; never narrow a type or add a constraint
//     an older writer's rows could violate. Removing something is a later,
//     separate release, once no running build uses it (expand/contract).
//   - Every new column is nullable or has a DEFAULT, so the previous release's
//     writer, which does not know the column, can still INSERT rows.
//   - A backfill runs as DML in the same file; keep it cheap (it runs at a
//     process's boot, under the lock, while the old release serves).
//
// A process accepts a database at a version >= its own (CheckVersion; Apply
// returns the higher version), so an OLDER build keeps serving a database a
// newer release has migrated.
//
// It is a leaf package (no dependency on internal/store) so the test
// harness, internal/storetest, can build its template database with it.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.sql
var files embed.FS

// Source is the schema_migrations.source value for versions applied by this
// runner. The SQLite importer records the legacy ladder (1-14) under "sqlite",
// which is why the key is (source, version) and not version alone.
const Source = "postgres"

// lockKey is the pg_advisory_xact_lock key serializing migration runs within
// one database. Distinct from the store's writer lock key.
const lockKey int64 = 0x5349455252414d31 // "SIERRAM1"

// Migration is one schema version.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

var all = mustLoad()

func mustLoad() []Migration {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		panic(err)
	}
	var out []Migration
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			panic(fmt.Sprintf("migrations: %s is not named NNNN_slug.sql", name))
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			panic(err)
		}
		out = append(out, Migration{Version: v, Name: name, SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			panic(fmt.Sprintf("migrations: versions must run 1..n without gaps; %s is at position %d", m.Name, i+1))
		}
	}
	return out
}

// All returns the migrations in version order.
func All() []Migration { return append([]Migration(nil), all...) }

// Latest is the schema version this build was written for.
func Latest() int { return len(all) }

const createTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
  source     text NOT NULL DEFAULT 'postgres',
  version    integer NOT NULL,
  applied_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (source, version)
)`

// Apply brings the database up to Latest, one version per transaction. Each
// transaction first takes the migration advisory lock, so concurrent callers
// queue and the second finds nothing left to do. Returns the version the
// database is at afterwards (which may exceed Latest: a newer release
// migrated it, and that is accepted).
func Apply(ctx context.Context, db *sql.DB) (int, error) {
	return ApplyThrough(ctx, db, Latest())
}

// ApplyThrough is Apply stopping at version target (at most Latest): tests
// use it to build a database as an older release left it.
func ApplyThrough(ctx context.Context, db *sql.DB, target int) (int, error) {
	if target > Latest() {
		target = Latest()
	}
	for {
		v, done, err := applyNext(ctx, db, target)
		if err != nil || done {
			return v, err
		}
	}
}

func applyNext(ctx context.Context, db *sql.DB, target int) (version int, done bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("migrations: begin: %w", err)
	}
	defer func() {
		if err != nil || done {
			tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return 0, false, fmt.Errorf("migrations: lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, createTable); err != nil {
		return 0, false, fmt.Errorf("migrations: creating schema_migrations: %w", err)
	}
	current, err := currentIn(ctx, tx)
	if err != nil {
		return 0, false, err
	}
	if current >= target {
		return current, true, nil
	}
	m := all[current] // version current+1
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return current, false, fmt.Errorf("migrations: applying %s: %w", m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (source, version) VALUES ($1, $2)`, Source, m.Version); err != nil {
		return current, false, fmt.Errorf("migrations: recording %s: %w", m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return current, false, fmt.Errorf("migrations: commit %s: %w", m.Name, err)
	}
	return m.Version, false, nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func currentIn(ctx context.Context, q querier) (int, error) {
	var v sql.NullInt64
	if err := q.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations WHERE source = $1`, Source).Scan(&v); err != nil {
		return 0, fmt.Errorf("migrations: reading schema version: %w", err)
	}
	return int(v.Int64), nil
}

// Current returns the database's schema version (0 when it has none).
func Current(ctx context.Context, db *sql.DB) (int, error) {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, fmt.Errorf("migrations: looking for schema_migrations: %w", err)
	}
	if !exists {
		return 0, nil
	}
	return currentIn(ctx, db)
}

// CheckVersion fails unless the database is at a schema version this build
// can read: Latest or newer (additive-only policy). Tools that must not
// migrate (gridctl verify) call this instead of Apply.
func CheckVersion(ctx context.Context, db *sql.DB) (int, error) {
	v, err := Current(ctx, db)
	if err != nil {
		return 0, err
	}
	if v < Latest() {
		return v, fmt.Errorf("migrations: database schema is at version %d, this build needs %d (any process of this build migrates it at boot)", v, Latest())
	}
	return v, nil
}
