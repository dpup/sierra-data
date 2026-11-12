// Package sqliteimport carries the legacy SQLite Grid store (schema v14, the
// legacy-efs-final tag) into the PostgreSQL store: a consistent snapshot of a
// live file, a one-transaction import with verification, and a read-only
// verify of an imported database (migration plan D11).
//
// This package and cmd/gridctl are the only code that links a SQLite driver
// for the Grid store (D12): the server never imports it
// (TestServerDoesNotLinkTheImporter). It never writes to a source file: every
// connection is opened mode=ro.
package sqliteimport

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go: the CGO_ENABLED=0 image build keeps working
)

// DefaultBusyTimeout is how long a read waits on a writer's lock before
// failing (SQLITE_BUSY). The legacy writer's commits are short; a snapshot of
// a live file waits them out.
const DefaultBusyTimeout = 30 * time.Second

// readOnlyDSN opens path through a URI with mode=ro: SQLite then never
// writes the file, never creates it, and cannot roll back a hot journal (it
// reports it instead), so even a misbehaving network filesystem lock cannot
// let this process damage the source. No journal_mode pragma is issued, so a
// WAL file is never touched either.
func readOnlyDSN(path string, busy time.Duration) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	u := url.URL{Path: abs}
	return fmt.Sprintf("file:%s?mode=ro&_busy_timeout=%d", u.EscapedPath(), busy.Milliseconds()), nil
}

// openReadOnly opens a single-connection, read-only handle on path.
func openReadOnly(path string, busy time.Duration) (*sql.DB, error) {
	dsn, err := readOnlyDSN(path, busy)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqliteimport: opening %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqliteimport: opening %s read-only: %w", path, err)
	}
	return db, nil
}

// source is an open legacy database inside one read transaction, so every
// table is read from the same snapshot.
type source struct {
	db     *sql.DB
	tx     *sql.Tx
	tables []string // user tables present
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func openSource(ctx context.Context, path string) (*source, error) {
	db, err := openReadOnly(path, DefaultBusyTimeout)
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("sqliteimport: begin read: %w", err)
	}
	src := &source{db: db, tx: tx}
	if src.tables, err = listTables(ctx, tx); err != nil {
		src.close()
		return nil, err
	}
	if err := checkSourceSchema(ctx, tx, src.tables); err != nil {
		src.close()
		return nil, err
	}
	return src, nil
}

func (s *source) close() {
	s.tx.Rollback()
	s.db.Close()
}

func listTables(ctx context.Context, q queryer) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sqliteimport: listing tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// schemaVersion reads the legacy ladder's position (MAX(version)).
func schemaVersion(ctx context.Context, q queryer) (int, error) {
	var v sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("sqliteimport: reading schema_migrations (is this a Grid store?): %w", err)
	}
	return int(v.Int64), nil
}

// checkSourceSchema refuses anything but a complete v14 ladder: the column
// lists below are v14's, and an older file would need its own mapping.
func checkSourceSchema(ctx context.Context, q queryer, tables []string) error {
	v, err := schemaVersion(ctx, q)
	if err != nil {
		return err
	}
	if v != SourceSchemaVersion {
		return fmt.Errorf("sqliteimport: source schema is version %d; this importer reads version %d (legacy-efs-final)", v, SourceSchemaVersion)
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version BETWEEN 1 AND ?`,
		SourceSchemaVersion).Scan(&n); err != nil {
		return err
	}
	if n != SourceSchemaVersion {
		return fmt.Errorf("sqliteimport: source schema_migrations records %d of versions 1..%d", n, SourceSchemaVersion)
	}
	have := map[string]bool{}
	for _, t := range tables {
		have[t] = true
	}
	var missing []string
	for _, t := range importTables {
		if !have[t.name] {
			missing = append(missing, t.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("sqliteimport: source lacks tables %s", strings.Join(missing, ", "))
	}
	return nil
}

// countTables counts every user table (imported, skipped or unknown).
func countTables(ctx context.Context, q queryer, tables []string) (map[string]int64, error) {
	out := make(map[string]int64, len(tables))
	for _, t := range tables {
		var n int64
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM "`+strings.ReplaceAll(t, `"`, `""`)+`"`).Scan(&n); err != nil {
			return nil, fmt.Errorf("sqliteimport: counting %s: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}

// unknownTables are user tables the importer neither imports nor knowingly
// skips. They are reported, not imported.
func unknownTables(tables []string) []string {
	known := map[string]bool{}
	for _, t := range importTables {
		known[t.name] = true
	}
	var out []string
	for _, t := range tables {
		if !known[t] && skippedTables[t] == "" {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}
