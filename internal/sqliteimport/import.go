package sqliteimport

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/sierragridteam/sierra-data/internal/store/migrations"
)

// importLogSQL creates the import_log table. The importer runs it inside its
// own transaction, so a failed import leaves no table behind either. It is
// written to become migration 0003_import_log.sql verbatim once the writer
// lease's 0002 has landed (the runner refuses gaps in the ladder, so it cannot
// be a migration on a branch without 0002); IF NOT EXISTS keeps both paths
// valid.
//
//go:embed import_log.sql
var importLogSQL string

// ErrNotEmpty is returned when the target already holds Grid data and the
// caller did not allow replacing it.
var ErrNotEmpty = errors.New("sqliteimport: the target database is not empty")

// ImportOptions configure Import.
type ImportOptions struct {
	SourcePath  string    // local SQLite file (a snapshot)
	SourceLabel string    // what to record as the source (the s3:// URL); defaults to SourcePath
	Manifest    *Manifest // the snapshot's manifest; nil imports without one (counts then compare source to target only)
	TargetURL   string    // PostgreSQL URL
	// AllowNonEmpty replaces existing Grid data (rehearsal re-runs): the
	// imported tables are truncated inside the import transaction, so a
	// failed re-import leaves the previous data as it was.
	AllowNonEmpty bool
	Version       string    // gridctl version, for import_log
	Log           io.Writer // progress lines; nil = silent
	// LockTimeout bounds the wait for table locks (a live writer); 0 = 10s.
	LockTimeout time.Duration
}

func (o ImportOptions) logf(format string, args ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format+"\n", args...)
	}
}

// Import loads a legacy SQLite snapshot into the PostgreSQL Grid store in ONE
// transaction and verifies it before committing (migration plan section 4):
//
//  1. Check the file against its manifest (SHA-256, size), open it mode=ro in
//     one read transaction, and refuse anything but a v14 store.
//  2. Refuse a target that does not compare bytewise, apply the PostgreSQL
//     migrations (the schema; committed on their own, like any writer's
//     migration), then in one transaction: lock the tables, refuse existing
//     data unless AllowNonEmpty, COPY every table in dependency order,
//     derive events.first_ingested_at and the bbox columns, reset the
//     mesh_observations identity past MAX(id) and the compaction watermark,
//     seed versions, record the SQLite ladder in schema_migrations.
//  3. Verify in the same transaction (runVerify): row counts against the
//     manifest and the source, a SHA-256 of each table's canonical row
//     stream on both sides, every event/revision/lite/place blob
//     rehydrated with its index columns agreeing, and the database-level
//     invariants. Any problem rolls everything back and returns
//     ErrVerification with the report.
//  4. Write an import_log row and commit.
func Import(ctx context.Context, opts ImportOptions) (*Report, error) {
	r := &Report{Source: opts.SourceLabel, StartedAt: time.Now().UTC()}
	if r.Source == "" {
		r.Source = opts.SourcePath
	}
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = 10 * time.Second
	}

	var err error
	if r.SourceSHA256, r.SourceSize, err = FileSHA256(opts.SourcePath); err != nil {
		return nil, fmt.Errorf("sqliteimport: hashing the source: %w", err)
	}
	if m := opts.Manifest; m != nil {
		if m.SHA256 != r.SourceSHA256 || m.Size != r.SourceSize {
			return nil, fmt.Errorf("sqliteimport: source %s (sha256 %s, %d bytes) does not match its manifest (sha256 %s, %d bytes)",
				opts.SourcePath, r.SourceSHA256, r.SourceSize, m.SHA256, m.Size)
		}
	}
	opts.logf("source %s: %d bytes, sha256 %s", r.Source, r.SourceSize, r.SourceSHA256)

	src, err := openSource(ctx, opts.SourcePath)
	if err != nil {
		return nil, err
	}
	defer src.close()

	conn, err := connectTarget(ctx, opts.TargetURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	if err := applyMigrations(ctx, conn); err != nil {
		return nil, err
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqliteimport: begin: %w", err)
	}
	defer tx.Rollback(context.Background()) // no-op after Commit

	if _, err := tx.Exec(ctx, importLogSQL); err != nil {
		return nil, fmt.Errorf("sqliteimport: creating import_log: %w", err)
	}
	if err := lockTargets(ctx, tx, opts.LockTimeout); err != nil {
		return nil, err
	}
	occupied, err := occupiedTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(occupied) > 0 {
		if !opts.AllowNonEmpty {
			return nil, fmt.Errorf("%w: %s hold data (a previous import or a running writer); "+
				"import only into an empty database, or pass --allow-nonempty to replace it (rehearsals only)",
				ErrNotEmpty, strings.Join(occupied, ", "))
		}
		opts.logf("replacing existing data in %s", strings.Join(occupied, ", "))
		if err := clearTargets(ctx, tx); err != nil {
			return nil, err
		}
		r.Replaced = true
	}

	copyStart := time.Now()
	for _, t := range importTables {
		n, secs, err := copyTable(ctx, src, tx, t)
		if err != nil {
			return nil, err
		}
		r.Tables = append(r.Tables, TableReport{Table: t.name, CopySeconds: secs})
		opts.logf("copied %-18s %9d rows in %6.2fs", t.name, n, secs)
	}
	if err := finishTargets(ctx, tx); err != nil {
		return nil, err
	}
	r.CopySeconds = time.Since(copyStart).Seconds()

	opts.logf("verifying")
	if err := runVerify(ctx, src, tx, opts.Manifest, r); err != nil {
		return nil, err
	}
	r.FinishedAt = time.Now().UTC()
	if !r.OK() {
		return r, fmt.Errorf("%w: %d problems, rolled back (first: %s)", ErrVerification, len(r.Problems), r.Problems[0])
	}
	if err := writeImportLog(ctx, tx, opts, r); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("sqliteimport: commit: %w", err)
	}
	return r, nil
}

// Verify compares an already-imported database with a SQLite source,
// read-only on both sides (a REPEATABLE READ READ ONLY transaction on
// PostgreSQL). It never migrates or writes the target. Run right after an
// import it must pass; once the writer has run, the counts legitimately
// differ.
func Verify(ctx context.Context, sourcePath string, manifest *Manifest, targetURL string) (*Report, error) {
	r := &Report{Source: sourcePath, StartedAt: time.Now().UTC()}
	var err error
	if r.SourceSHA256, r.SourceSize, err = FileSHA256(sourcePath); err != nil {
		return nil, err
	}
	if manifest != nil && (manifest.SHA256 != r.SourceSHA256 || manifest.Size != r.SourceSize) {
		r.problem("source does not match its manifest (sha256 %s, %d bytes)", manifest.SHA256, manifest.Size)
	}
	src, err := openSource(ctx, sourcePath)
	if err != nil {
		return nil, err
	}
	defer src.close()
	conn, err := connectTarget(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("sqliteimport: begin read: %w", err)
	}
	defer tx.Rollback(context.Background())
	if err := runVerify(ctx, src, tx, manifest, r); err != nil {
		return nil, err
	}
	r.FinishedAt = time.Now().UTC()
	if !r.OK() {
		return r, fmt.Errorf("%w: %d problems (first: %s)", ErrVerification, len(r.Problems), r.Problems[0])
	}
	return r, nil
}

func connectTarget(ctx context.Context, url string) (*pgx.Conn, error) {
	if url == "" {
		return nil, errors.New("sqliteimport: no target database URL")
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("sqliteimport: connecting to the target: %w", err)
	}
	// The same behavioural test store.Open applies: keyset cursors and the
	// row-stream comparison both need byte order.
	var bytewise bool
	if err := conn.QueryRow(ctx, `SELECT ('a-z' < 'a:a') AND ('B' < 'a') AND ('a:z' < 'aa')`).Scan(&bytewise); err != nil {
		conn.Close(ctx)
		return nil, fmt.Errorf("sqliteimport: checking collation: %w", err)
	}
	if !bytewise {
		conn.Close(ctx)
		return nil, errors.New("sqliteimport: the target database does not use byte-order collation; " +
			"create it with TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'")
	}
	return conn, nil
}

func applyMigrations(ctx context.Context, conn *pgx.Conn) error {
	db := stdlib.OpenDB(*conn.Config())
	defer db.Close()
	if _, err := migrations.Apply(ctx, db); err != nil {
		return fmt.Errorf("sqliteimport: %w", err)
	}
	_, err := migrations.CheckVersion(ctx, db)
	return err
}

// targetTables are every table the import writes: the imported ones plus
// versions. Locked EXCLUSIVE (reads allowed, writes wait) for the whole
// transaction, so a writer that starts during the import cannot interleave.
func targetTables() []string {
	out := []string{"versions"}
	seen := map[string]bool{"versions": true}
	for _, t := range importTables {
		if !seen[t.name] {
			out = append(out, t.name)
			seen[t.name] = true
		}
	}
	return out
}

func lockTargets(ctx context.Context, tx pgx.Tx, timeout time.Duration) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, timeout.Milliseconds())); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE `+strings.Join(targetTables(), ", ")+` IN EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("sqliteimport: locking the target tables (is a writer running?): %w", err)
	}
	_, err := tx.Exec(ctx, `SET LOCAL lock_timeout = 0`)
	return err
}

// occupiedTables lists target tables that already hold Grid data.
func occupiedTables(ctx context.Context, tx pgx.Tx) ([]string, error) {
	var out []string
	probe := func(name, q string) error {
		var n int64
		if err := tx.QueryRow(ctx, q).Scan(&n); err != nil {
			return fmt.Errorf("sqliteimport: probing %s: %w", name, err)
		}
		if n > 0 {
			out = append(out, name)
		}
		return nil
	}
	for _, t := range importTables {
		if err := probe(t.name, t.pgCountSQL()); err != nil {
			return nil, err
		}
	}
	if err := probe("versions", `SELECT count(*) FROM versions`); err != nil {
		return nil, err
	}
	return out, probe("import_log", `SELECT count(*) FROM import_log`)
}

// clearTargets empties every target table inside the transaction (the
// previous import_log rows stay: they are the history of what was loaded).
func clearTargets(ctx context.Context, tx pgx.Tx) error {
	var names []string
	for _, t := range importTables {
		if t.name != "schema_migrations" {
			names = append(names, t.name)
		}
	}
	stmts := []string{
		`TRUNCATE ` + strings.Join(names, ", ") + ` RESTART IDENTITY`,
		`DELETE FROM versions`,
		`DELETE FROM schema_migrations WHERE source = 'sqlite'`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("sqliteimport: clearing the target: %w", err)
		}
	}
	return nil
}

// copySource feeds CopyFrom from the SQLite copy query.
type copySource struct {
	rows *sqliteRows
	t    *table
	row  []any
	err  error
	n    int64
}

func (c *copySource) Next() bool {
	if c.err != nil || !c.rows.Next() {
		return false
	}
	row, err := c.rows.Row()
	if err == nil && c.t.toPG != nil {
		row, err = c.t.toPG(row)
	}
	if err != nil {
		c.err = fmt.Errorf("sqliteimport: %s row %d: %w", c.t.name, c.n+1, err)
		return false
	}
	c.row = row
	c.n++
	return true
}

func (c *copySource) Values() ([]any, error) { return c.row, nil }

func (c *copySource) Err() error {
	if c.err != nil {
		return c.err
	}
	return c.rows.Err()
}

func copyTable(ctx context.Context, src *source, tx pgx.Tx, t *table) (int64, float64, error) {
	start := time.Now()
	rows, err := querySQLite(ctx, src.tx, t.sqliteCopySQL(), t.kinds(t.srcCopyExtra))
	if err != nil {
		return 0, 0, fmt.Errorf("sqliteimport: reading %s: %w", t.name, err)
	}
	defer rows.Close()
	cs := &copySource{rows: rows, t: t}
	n, err := tx.CopyFrom(ctx, pgx.Identifier{t.name}, t.copyCols(), cs)
	if err != nil {
		return 0, 0, fmt.Errorf("sqliteimport: copying %s (row %d): %w", t.name, cs.n, err)
	}
	return n, time.Since(start).Seconds(), nil
}

// finishTargets resets the mesh_observations identity and seeds versions.
func finishTargets(ctx context.Context, tx pgx.Tx) error {
	stmts := []string{
		// The next id must be past every imported id AND the compaction
		// watermark: compaction treats id <= watermark as already rolled up,
		// and prune deletes those, so a new row at or below it would be lost
		// from the topology history.
		`SELECT setval(pg_get_serial_sequence('mesh_observations', 'id'), v, true)
		   FROM (SELECT GREATEST(COALESCE((SELECT MAX(id) FROM mesh_observations), 0),
		                         COALESCE((SELECT value FROM mesh_meta WHERE key = 'compaction_watermark'), 0)) AS v) m
		  WHERE v > 0`,
		// DataVersion counts revisions written: equal to the imported count.
		// places and mesh_activity only need to be positive, so a reader's
		// first validator never collides with an empty store's.
		`INSERT INTO versions (key, value) VALUES
		   ('revisions', (SELECT count(*) FROM event_revisions)),
		   ('places', GREATEST((SELECT count(*) FROM places), 1)),
		   ('mesh_activity', GREATEST((SELECT count(*) FROM mesh_activity), 1))`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("sqliteimport: finishing the target: %w", err)
		}
	}
	return nil
}

func writeImportLog(ctx context.Context, tx pgx.Tx, opts ImportOptions, r *Report) error {
	rows := map[string]int64{}
	for _, t := range r.Tables {
		rows[t.Table] = t.TargetRows
	}
	rowsJSON, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	reportJSON, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var snapAt *time.Time
	if opts.Manifest != nil {
		snapAt = &opts.Manifest.CreatedAt
	}
	version := opts.Version
	if version == "" {
		version = "unknown"
	}
	_, err = tx.Exec(ctx, `INSERT INTO import_log
		(source, source_sha256, source_size, source_schema_version, snapshot_created_at,
		 rows_per_table, report, replaced, gridctl_version, started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		r.Source, r.SourceSHA256, r.SourceSize, SourceSchemaVersion, snapAt,
		rowsJSON, reportJSON, r.Replaced, version, r.StartedAt, r.FinishedAt)
	if err != nil {
		return fmt.Errorf("sqliteimport: writing import_log: %w", err)
	}
	return nil
}
