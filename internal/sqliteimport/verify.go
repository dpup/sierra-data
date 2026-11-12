package sqliteimport

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrVerification is returned (wrapped) when the comparison finds a
// difference. The Report says which.
var ErrVerification = errors.New("sqliteimport: verification failed")

// TableReport is one table's comparison.
type TableReport struct {
	Table         string   `json:"table"`
	SourceRows    int64    `json:"sourceRows"`
	TargetRows    int64    `json:"targetRows"`
	ManifestRows  *int64   `json:"manifestRows,omitempty"`
	SourceSHA256  string   `json:"sourceSha256"`
	TargetSHA256  string   `json:"targetSha256"`
	RowMismatches int64    `json:"rowMismatches"`
	Differences   []string `json:"differences,omitempty"`
	CheckFailures int64    `json:"checkFailures"`
	CheckExamples []string `json:"checkExamples,omitempty"`
	CopySeconds   float64  `json:"copySeconds,omitempty"`
	VerifySeconds float64  `json:"verifySeconds"`
}

// Report is the outcome of an import or a verify.
type Report struct {
	Source        string            `json:"source"`
	SourceSHA256  string            `json:"sourceSha256,omitempty"`
	SourceSize    int64             `json:"sourceSize,omitempty"`
	Tables        []TableReport     `json:"tables"`
	Skipped       map[string]int64  `json:"skippedTables"` // not imported, with source row counts
	Unknown       []string          `json:"unknownTables,omitempty"`
	Checks        map[string]string `json:"checks"` // database-level checks: name -> "ok" or the problem
	Info          map[string]int64  `json:"info,omitempty"`
	Problems      []string          `json:"problems,omitempty"`
	Replaced      bool              `json:"replacedExistingData,omitempty"`
	StartedAt     time.Time         `json:"startedAt"`
	FinishedAt    time.Time         `json:"finishedAt"`
	CopySeconds   float64           `json:"copySeconds,omitempty"`
	VerifySeconds float64           `json:"verifySeconds"`
}

// OK reports whether nothing differed.
func (r *Report) OK() bool { return len(r.Problems) == 0 }

func (r *Report) problem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// sideRows iterates one side's canonical rows.
type sideRows interface {
	Next() bool
	Row() ([]any, error)
	Err() error
	Close()
}

type sqliteRows struct {
	rows  *sql.Rows
	kinds []kind
	vals  []any
	ptrs  []any
}

func querySQLite(ctx context.Context, q queryer, query string, kinds []kind) (*sqliteRows, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	r := &sqliteRows{rows: rows, kinds: kinds, vals: make([]any, len(kinds)), ptrs: make([]any, len(kinds))}
	for n := range r.vals {
		r.ptrs[n] = &r.vals[n]
	}
	return r, nil
}

func (r *sqliteRows) Next() bool { return r.rows.Next() }
func (r *sqliteRows) Err() error { return r.rows.Err() }
func (r *sqliteRows) Close()     { r.rows.Close() }
func (r *sqliteRows) Row() ([]any, error) {
	if err := r.rows.Scan(r.ptrs...); err != nil {
		return nil, err
	}
	// Scan into *any copies []byte, so the row survives the next Next.
	row := append([]any(nil), r.vals...)
	return row, canonRow(r.kinds, row)
}

type pgRows struct {
	rows  pgx.Rows
	kinds []kind
}

func (r *pgRows) Next() bool { return r.rows.Next() }
func (r *pgRows) Err() error { return r.rows.Err() }
func (r *pgRows) Close()     { r.rows.Close() }
func (r *pgRows) Row() ([]any, error) {
	row, err := r.rows.Values()
	if err != nil {
		return nil, err
	}
	return row, canonRow(r.kinds, row)
}

// compareTable reads one table from both sides in key order, in lockstep:
// each pair of rows must be equal column for column; each side's row stream
// is hashed (SHA-256 over the canonical encoding) and counted; every
// PostgreSQL row goes through the table's rehydration check.
func compareTable(ctx context.Context, src queryer, dst pgx.Tx, t *table) (tr TableReport, drift int64, err error) {
	start := time.Now()
	tr = TableReport{Table: t.name}
	sr, err := querySQLite(ctx, src, t.sqliteHashSQL(), t.kinds(nil))
	if err != nil {
		return tr, 0, fmt.Errorf("sqliteimport: reading %s from SQLite: %w", t.name, err)
	}
	defer sr.Close()
	pr, err := dst.Query(ctx, t.pgHashSQL())
	if err != nil {
		return tr, 0, fmt.Errorf("sqliteimport: reading %s from PostgreSQL: %w", t.name, err)
	}
	tgt := &pgRows{rows: pr, kinds: t.kinds(t.pgHashExtra)}
	defer tgt.Close()

	sh, th := sha256.New(), sha256.New()
	chk := &checker{table: t.name}
	n := len(t.cols)
	note := func(format string, args ...any) {
		tr.RowMismatches++
		if len(tr.Differences) < maxExamples {
			tr.Differences = append(tr.Differences, fmt.Sprintf(format, args...))
		}
	}
	for {
		sOK, tOK := sr.Next(), tgt.Next()
		if !sOK && !tOK {
			break
		}
		var srow, trow []any
		if sOK {
			if srow, err = sr.Row(); err != nil {
				return tr, 0, fmt.Errorf("sqliteimport: %s source row %d: %w", t.name, tr.SourceRows+1, err)
			}
			tr.SourceRows++
			hashRow(sh, srow[:n])
		}
		if tOK {
			if trow, err = tgt.Row(); err != nil {
				return tr, 0, fmt.Errorf("sqliteimport: %s target row %d: %w", t.name, tr.TargetRows+1, err)
			}
			tr.TargetRows++
			hashRow(th, trow[:n])
			if t.check != nil {
				t.check(chk, trow)
			}
		}
		switch {
		case !tOK:
			note("row %d: source %s has no target row", tr.SourceRows, describeKey(srow, t.keyN))
		case !sOK:
			note("row %d: target %s has no source row", tr.TargetRows, describeKey(trow, t.keyN))
		default:
			for c := 0; c < n; c++ {
				if !equalCanon(srow[c], trow[c]) {
					note("row %d: source %s / target %s: column %s: %s != %s", tr.SourceRows,
						describeKey(srow, t.keyN), describeKey(trow, t.keyN), t.cols[c].name,
						describe(srow[c]), describe(trow[c]))
					break
				}
			}
		}
	}
	if err := sr.Err(); err != nil {
		return tr, 0, fmt.Errorf("sqliteimport: reading %s from SQLite: %w", t.name, err)
	}
	if err := tgt.Err(); err != nil {
		return tr, 0, fmt.Errorf("sqliteimport: reading %s from PostgreSQL: %w", t.name, err)
	}
	tr.SourceSHA256 = hex.EncodeToString(sh.Sum(nil))
	tr.TargetSHA256 = hex.EncodeToString(th.Sum(nil))
	tr.CheckFailures, tr.CheckExamples = chk.failures, chk.examples
	tr.VerifySeconds = time.Since(start).Seconds()
	return tr, chk.contentHashDrift, nil
}

func hashRow(h hash.Hash, row []any) {
	for _, v := range row {
		hashValue(h, v)
	}
	h.Write([]byte{0xff}) // row terminator
}

// runVerify compares every imported table and runs the database-level
// checks, filling r. manifest may be nil.
func runVerify(ctx context.Context, src *source, dst pgx.Tx, manifest *Manifest, r *Report) error {
	start := time.Now()
	if r.Checks == nil {
		r.Checks = map[string]string{}
	}
	if r.Info == nil {
		r.Info = map[string]int64{}
	}
	counts, err := countTables(ctx, src.tx, src.tables)
	if err != nil {
		return err
	}
	r.Skipped = map[string]int64{}
	for name := range skippedTables {
		if n, ok := counts[name]; ok {
			r.Skipped[name] = n
		}
	}
	if n := r.Skipped["subscriptions"]; n > 0 {
		r.problem("subscriptions holds %d rows; the PostgreSQL schema has no such table (it was never used)", n)
	}
	r.Unknown = unknownTables(src.tables)
	for _, u := range r.Unknown {
		r.problem("source table %s (%d rows) is unknown to the importer and was not imported", u, counts[u])
	}
	if manifest != nil {
		for name, n := range manifest.Tables {
			if counts[name] != n {
				r.problem("source %s has %d rows, its manifest says %d", name, counts[name], n)
			}
		}
	}

	for _, t := range importTables {
		tr, drift, err := compareTable(ctx, src.tx, dst, t)
		if err != nil {
			return err
		}
		if t.name == "events" {
			r.Info["eventsContentHashDiffersFromBlob"] = drift
		}
		if manifest != nil {
			if n, ok := manifest.Tables[t.name]; ok {
				tr.ManifestRows = &n
				if tr.TargetRows != n {
					r.problem("%s: %d rows imported, the manifest says %d", t.name, tr.TargetRows, n)
				}
			} else {
				r.problem("%s: not in the manifest", t.name)
			}
		}
		if tr.SourceRows != tr.TargetRows {
			r.problem("%s: %d source rows, %d target rows", t.name, tr.SourceRows, tr.TargetRows)
		}
		if tr.SourceSHA256 != tr.TargetSHA256 {
			r.problem("%s: row stream SHA-256 differs (%d rows differ; first: %v)", t.name, tr.RowMismatches, first(tr.Differences))
		}
		if tr.CheckFailures > 0 {
			r.problem("%s: %d rows fail the blob check (first: %v)", t.name, tr.CheckFailures, first(tr.CheckExamples))
		}
		r.setTable(tr)
	}

	if err := databaseChecks(ctx, dst, r); err != nil {
		return err
	}
	r.VerifySeconds = time.Since(start).Seconds()
	return nil
}

// setTable records tr, keeping the copy timing of an import's earlier pass.
func (r *Report) setTable(tr TableReport) {
	for n := range r.Tables {
		if r.Tables[n].Table == tr.Table {
			tr.CopySeconds = r.Tables[n].CopySeconds
			r.Tables[n] = tr
			return
		}
	}
	r.Tables = append(r.Tables, tr)
}

func first(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	return s[0]
}

// databaseChecks are the invariants of an imported database that no single
// table comparison covers.
func databaseChecks(ctx context.Context, tx pgx.Tx, r *Report) error {
	scalar := func(q string) (int64, error) {
		var v int64
		err := tx.QueryRow(ctx, q).Scan(&v)
		return v, err
	}
	check := func(name, q string, want func(int64) string) error {
		v, err := scalar(q)
		if err != nil {
			return fmt.Errorf("sqliteimport: check %s: %w", name, err)
		}
		if msg := want(v); msg != "" {
			r.Checks[name] = msg
			r.problem("%s: %s", name, msg)
		} else {
			r.Checks[name] = "ok"
		}
		return nil
	}
	zero := func(what string) func(int64) string {
		return func(v int64) string {
			if v != 0 {
				return fmt.Sprintf("%d %s", v, what)
			}
			return ""
		}
	}

	// first_ingested_at is revision 1's ingested_at.
	if err := check("firstIngestedAt",
		`SELECT count(*) FROM events e JOIN event_revisions r ON r.event_id = e.id AND r.revision = 1
		 WHERE r.ingested_at <> e.first_ingested_at`,
		zero("events whose first_ingested_at is not revision 1's ingested_at")); err != nil {
		return err
	}
	noRev1, err := scalar(`SELECT count(*) FROM events e WHERE NOT EXISTS
		(SELECT 1 FROM event_revisions r WHERE r.event_id = e.id AND r.revision = 1)`)
	if err != nil {
		return err
	}
	r.Info["eventsWithoutRevision1"] = noRev1

	// Every revision has its event.
	if err := check("revisionsHaveEvents",
		`SELECT count(*) FROM event_revisions r WHERE NOT EXISTS (SELECT 1 FROM events e WHERE e.id = r.event_id)`,
		zero("revisions whose event does not exist")); err != nil {
		return err
	}

	// versions: DataVersion counts revisions written; the others must be
	// positive so no reader's first validator collides with an empty store's.
	if err := check("versionsRevisions",
		`SELECT COALESCE((SELECT value FROM versions WHERE key = 'revisions'), -1) - (SELECT count(*) FROM event_revisions)`,
		func(d int64) string {
			if d != 0 {
				return fmt.Sprintf("versions.revisions differs from the event_revisions count by %d", d)
			}
			return ""
		}); err != nil {
		return err
	}
	for _, key := range []string{"places", "mesh_activity"} {
		if err := check("versions_"+key,
			`SELECT COALESCE((SELECT value FROM versions WHERE key = '`+key+`'), 0)`,
			func(v int64) string {
				if v <= 0 {
					return fmt.Sprintf("versions.%s is %d, want > 0", key, v)
				}
				return ""
			}); err != nil {
			return err
		}
	}

	// The mesh_observations identity hands out ids past every imported id
	// and past the compaction watermark (an id at or below the watermark
	// would be treated as already compacted, and pruned unrolled).
	if err := check("meshObservationsIdentity",
		`SELECT GREATEST(COALESCE((SELECT MAX(id) FROM mesh_observations), 0),
		                 COALESCE((SELECT value FROM mesh_meta WHERE key = 'compaction_watermark'), 0))
		      - COALESCE(pg_sequence_last_value(pg_get_serial_sequence('mesh_observations', 'id')), 0)`,
		func(d int64) string {
			if d > 0 {
				return fmt.Sprintf("the next mesh_observations id would be %d below the highest imported id or watermark", d)
			}
			return ""
		}); err != nil {
		return err
	}

	// The SQLite ladder is recorded beside the PostgreSQL baseline.
	return check("schemaMigrations",
		`SELECT (SELECT count(*) FROM schema_migrations WHERE source = 'postgres' AND version = 1)`,
		func(v int64) string {
			if v != 1 {
				return "the PostgreSQL baseline is not recorded"
			}
			return ""
		})
}
