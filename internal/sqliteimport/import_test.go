package sqliteimport_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/sqliteimport"
	"github.com/sierragridteam/sierra-data/internal/sqliteimport/legacyfixture"
	"github.com/sierragridteam/sierra-data/internal/store"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

// fixture builds the small legacy store, snapshots it and returns the
// snapshot path, its manifest and what the builder wrote.
func fixture(t *testing.T) (string, *sqliteimport.Manifest, *legacyfixture.Summary) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	live := filepath.Join(dir, "grid.db")
	sum, err := legacyfixture.Build(ctx, live, legacyfixture.Small())
	require.NoError(t, err)
	snap := filepath.Join(dir, "grid-snap.db")
	m, err := sqliteimport.Snapshot(ctx, live, snap, sqliteimport.SnapshotOptions{Version: "test"})
	require.NoError(t, err)
	return snap, m, sum
}

func importInto(t *testing.T, url, snap string, m *sqliteimport.Manifest, allow bool) (*sqliteimport.Report, error) {
	t.Helper()
	return sqliteimport.Import(context.Background(), sqliteimport.ImportOptions{
		SourcePath: snap, Manifest: m, TargetURL: url, AllowNonEmpty: allow, Version: "test"})
}

func scalar(t *testing.T, url, q string, args ...any) int64 {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), url)
	require.NoError(t, err)
	defer conn.Close(context.Background())
	var v int64
	require.NoError(t, conn.QueryRow(context.Background(), q, args...).Scan(&v))
	return v
}

func execPG(t *testing.T, url, q string, args ...any) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), url)
	require.NoError(t, err)
	defer conn.Close(context.Background())
	_, err = conn.Exec(context.Background(), q, args...)
	require.NoError(t, err)
}

func TestSnapshotManifestDescribesTheCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	live := filepath.Join(dir, "grid.db")
	sum, err := legacyfixture.Build(ctx, live, legacyfixture.Small())
	require.NoError(t, err)
	before, _, err := sqliteimport.FileSHA256(live)
	require.NoError(t, err)
	sidecars := func() (out []string) {
		for _, ext := range []string{"-journal", "-wal", "-shm"} {
			if _, err := os.Stat(live + ext); err == nil {
				out = append(out, ext)
			}
		}
		return out
	}
	sidecarsBefore := sidecars()
	st, err := os.Stat(live)
	require.NoError(t, err)

	snap := filepath.Join(dir, "snap.db")
	m, err := sqliteimport.Snapshot(ctx, live, snap, sqliteimport.SnapshotOptions{Version: "v-test"})
	require.NoError(t, err)

	after, _, err := sqliteimport.FileSHA256(live)
	require.NoError(t, err)
	st2, err := os.Stat(live)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the source is never written")
	assert.Equal(t, st.ModTime(), st2.ModTime())
	assert.Equal(t, sidecarsBefore, sidecars(), "no journal or WAL file is created")

	sha, size, err := sqliteimport.FileSHA256(snap)
	require.NoError(t, err)
	assert.Equal(t, sha, m.SHA256)
	assert.Equal(t, size, m.Size)
	assert.Equal(t, "ok", m.QuickCheck)
	assert.Equal(t, 14, m.SchemaVersion)
	assert.Equal(t, "v-test", m.GridctlVersion)
	for table, n := range sum.Rows {
		assert.Equal(t, n, m.Tables[table], table)
	}
	read, err := sqliteimport.ReadManifest(snap + sqliteimport.ManifestSuffix)
	require.NoError(t, err)
	assert.Equal(t, m.SHA256, read.SHA256)

	_, err = sqliteimport.Snapshot(ctx, live, snap, sqliteimport.SnapshotOptions{})
	assert.ErrorContains(t, err, "already exists", "a snapshot never overwrites")
}

// The importer reads the source through a read-only connection: a file the
// process may not write is imported all the same.
func TestImportReadsAReadOnlyFile(t *testing.T) {
	snap, m, _ := fixture(t)
	require.NoError(t, os.Chmod(snap, 0o444))
	require.NoError(t, os.Chmod(filepath.Dir(snap), 0o555))
	t.Cleanup(func() { os.Chmod(filepath.Dir(snap), 0o755) })
	r, err := importInto(t, storetest.URL(t), snap, m, false)
	require.NoError(t, err)
	assert.True(t, r.OK())
}

func TestImportEndToEnd(t *testing.T) {
	ctx := context.Background()
	snap, m, sum := fixture(t)
	url := storetest.URL(t)

	r, err := importInto(t, url, snap, m, false)
	require.NoError(t, err, "problems: %v", r)
	require.True(t, r.OK(), r.Problems)
	for _, tr := range r.Tables {
		assert.Equal(t, tr.SourceRows, tr.TargetRows, tr.Table)
		assert.Equal(t, tr.SourceSHA256, tr.TargetSHA256, tr.Table)
		assert.Zero(t, tr.CheckFailures, tr.Table)
		if tr.Table != "schema_migrations" {
			assert.Equal(t, sum.Rows[tr.Table], tr.TargetRows, tr.Table)
		}
	}
	assert.Equal(t, int64(14), scalar(t, url, `SELECT count(*) FROM schema_migrations WHERE source = 'sqlite'`))
	assert.Equal(t, sum.Rows["event_geo"], r.Skipped["event_geo"])
	assert.Equal(t, int64(1), scalar(t, url, `SELECT count(*) FROM import_log`))

	// versions seeded.
	assert.Equal(t, sum.Rows["event_revisions"], scalar(t, url, `SELECT value FROM versions WHERE key = 'revisions'`))
	assert.Positive(t, scalar(t, url, `SELECT value FROM versions WHERE key = 'places'`))
	assert.Positive(t, scalar(t, url, `SELECT value FROM versions WHERE key = 'mesh_activity'`))

	// Derived columns: bbox with and without geometry, first_ingested_at.
	assert.Equal(t, int64(1), scalar(t, url, `SELECT count(*) FROM events WHERE id = $1 AND min_lat IS NOT NULL
		AND min_lat <= max_lat AND min_lng <= max_lng`, sum.GeoEventID))
	assert.Equal(t, int64(1), scalar(t, url, `SELECT count(*) FROM events WHERE id = $1 AND min_lat IS NULL
		AND max_lat IS NULL AND min_lng IS NULL AND max_lng IS NULL`, sum.NoGeoEventID))
	assert.Equal(t, int64(0), scalar(t, url, `SELECT count(*) FROM events e JOIN event_revisions r
		ON r.event_id = e.id AND r.revision = 1 WHERE r.ingested_at <> e.first_ingested_at`))

	// event_revisions ids follow insertion order.
	assert.Equal(t, int64(0), scalar(t, url, `SELECT count(*) FROM event_revisions a JOIN event_revisions b
		ON a.event_id = b.event_id AND a.revision < b.revision WHERE a.id > b.id`))

	// Verify on the imported database passes, read-only.
	vr, err := sqliteimport.Verify(ctx, snap, m, url)
	require.NoError(t, err)
	assert.True(t, vr.OK())

	// The real store opens it and reads through it.
	st, err := store.Open(url)
	require.NoError(t, err)
	defer st.Close()
	ev, err := st.GetEvent(ctx, sum.ResolvedEventID)
	require.NoError(t, err)
	assert.Contains(t, []gridv1.EventStatus{gridv1.EventStatus_RESOLVED, gridv1.EventStatus_EXPIRED}, ev.GetStatus())
	hist, _, err := st.EventHistory(ctx, sum.ResolvedEventID, 100, "")
	require.NoError(t, err)
	assert.Len(t, hist, int(ev.GetRevision()))
	places, err := st.ListPlaces(ctx, gridv1.PlaceKind_COUNTY, "")
	require.NoError(t, err)
	assert.NotEmpty(t, places)
	evs, _, err := st.QueryEvents(ctx, store.EventQuery{PlaceID: "county:calaveras-county",
		Statuses: []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_RESOLVED}})
	require.NoError(t, err)
	assert.NotEmpty(t, evs)
	dv, err := st.DataVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, sum.Rows["event_revisions"], dv)

	// A new observation gets an id past the watermark (setval), so
	// compaction rolls it up rather than treating it as done.
	require.NoError(t, st.InsertMeshObservations(ctx, []store.MeshObservation{{PubKey: "ab", HeardAt: time.Now()}}))
	assert.Greater(t, scalar(t, url, `SELECT id FROM mesh_observations WHERE pubkey = 'ab'`), sum.Watermark)
}

func TestSecondImportRefusesWithoutAllowNonEmpty(t *testing.T) {
	snap, m, sum := fixture(t)
	url := storetest.URL(t)
	_, err := importInto(t, url, snap, m, false)
	require.NoError(t, err)

	_, err = importInto(t, url, snap, m, false)
	require.ErrorIs(t, err, sqliteimport.ErrNotEmpty)
	assert.Equal(t, sum.Rows["events"], scalar(t, url, `SELECT count(*) FROM events`), "a refused import changes nothing")
	assert.Equal(t, int64(1), scalar(t, url, `SELECT count(*) FROM import_log`))

	r, err := importInto(t, url, snap, m, true)
	require.NoError(t, err)
	assert.True(t, r.Replaced)
	assert.Equal(t, sum.Rows["events"], scalar(t, url, `SELECT count(*) FROM events`))
	assert.Equal(t, int64(2), scalar(t, url, `SELECT count(*) FROM import_log`))
	assert.Equal(t, sum.Rows["event_revisions"], scalar(t, url, `SELECT value FROM versions WHERE key = 'revisions'`))
	assert.Equal(t, int64(1), scalar(t, url, `SELECT min(id) FROM event_revisions`), "identity restarted")
}

// Any mismatch rolls the whole import back: nothing is left behind, not even
// an import_log row (the table itself is migration 0003).
func TestFailedVerificationLeavesNothingBehind(t *testing.T) {
	snap, m, _ := fixture(t)
	url := storetest.URL(t)
	bad := *m
	bad.Tables = map[string]int64{}
	for k, v := range m.Tables {
		bad.Tables[k] = v
	}
	bad.Tables["event_revisions"]++

	r, err := importInto(t, url, snap, &bad, false)
	require.ErrorIs(t, err, sqliteimport.ErrVerification)
	require.NotNil(t, r)
	assert.Contains(t, strings.Join(r.Problems, "\n"), "event_revisions")
	for _, table := range []string{"events", "event_revisions", "places", "sources", "mesh_observations", "versions"} {
		assert.Zero(t, scalar(t, url, `SELECT count(*) FROM `+table), table)
	}
	assert.Zero(t, scalar(t, url, `SELECT count(*) FROM import_log`))
}

func TestImportRefusesAFileThatIsNotItsManifest(t *testing.T) {
	snap, m, _ := fixture(t)
	bad := *m
	bad.SHA256 = strings.Repeat("0", 64)
	_, err := importInto(t, storetest.URL(t), snap, &bad, false)
	assert.ErrorContains(t, err, "does not match its manifest")
}

// verify finds a change made after the import and names it.
func TestVerifyFindsDifferences(t *testing.T) {
	ctx := context.Background()
	snap, m, sum := fixture(t)
	url := storetest.URL(t)
	_, err := importInto(t, url, snap, m, false)
	require.NoError(t, err)

	execPG(t, url, `UPDATE event_revisions SET proto = proto || '\x00'::bytea WHERE event_id = $1 AND revision = 1`, sum.GeoEventID)
	execPG(t, url, `UPDATE events SET severity = severity + 1 WHERE id = $1`, sum.NoGeoEventID)
	execPG(t, url, `DELETE FROM road_samples WHERE (road_id, at) = (SELECT road_id, at FROM road_samples ORDER BY road_id, at LIMIT 1)`)

	r, err := sqliteimport.Verify(ctx, snap, m, url)
	require.ErrorIs(t, err, sqliteimport.ErrVerification)
	byTable := map[string]sqliteimport.TableReport{}
	for _, tr := range r.Tables {
		byTable[tr.Table] = tr
	}
	rev := byTable["event_revisions"]
	assert.NotEqual(t, rev.SourceSHA256, rev.TargetSHA256)
	require.NotEmpty(t, rev.Differences)
	assert.Contains(t, rev.Differences[0], sum.GeoEventID)
	assert.Contains(t, rev.Differences[0], "column proto")
	assert.Positive(t, rev.CheckFailures, "a trailing NUL byte is not a valid grid.v1.Event")

	ev := byTable["events"]
	assert.Positive(t, ev.CheckFailures, "the severity column no longer agrees with the blob")
	assert.Contains(t, strings.Join(ev.CheckExamples, "\n"), "column severity")

	rs := byTable["road_samples"]
	assert.Equal(t, rs.SourceRows-1, rs.TargetRows)
}

func TestImportRefusesOtherSchemaVersions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	_, err := legacyfixture.Build(ctx, path, legacyfixture.Small())
	require.NoError(t, err)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM schema_migrations WHERE version = 14`)
	require.NoError(t, err)
	db.Close()

	_, err = sqliteimport.Import(ctx, sqliteimport.ImportOptions{SourcePath: path, TargetURL: storetest.URL(t)})
	assert.ErrorContains(t, err, "schema is version 13")
}

func TestImportRefusesANonCCollatedTarget(t *testing.T) {
	admin := storetest.AdminURL(t)
	snap, m, _ := fixture(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	require.NoError(t, err)
	defer conn.Close(ctx)
	name := fmt.Sprintf("t_icu_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+name+` TEMPLATE template0 LOCALE_PROVIDER icu ICU_LOCALE 'en-US' LOCALE 'C'`); err != nil {
		t.Skipf("SKIP: this server cannot create an ICU database: %v", err)
	}
	t.Cleanup(func() { execPG(t, admin, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`) })
	pu, err := neturl.Parse(admin)
	require.NoError(t, err)
	pu.Path = "/" + name
	_, err = importInto(t, pu.String(), snap, m, false)
	assert.ErrorContains(t, err, "byte-order collation")
}

// D12: SQLite survives only in the importer. The server binary must never
// link it (it would bring a second database driver into the Grid).
func TestServerDoesNotLinkTheImporter(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/sierragridteam/sierra-data/cmd/server").CombinedOutput()
	if err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) {
			t.Skipf("SKIP: no go tool: %v", err)
		}
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "sqliteimport") || strings.HasPrefix(line, "modernc.org/sqlite") {
			t.Errorf("cmd/server depends on %s", line)
		}
	}
}

// The import is a writer like any other (decision 3, 2026-11-12): it takes
// the writer lease through store.AcquireLease and fences its transaction with
// the epoch. While a server holds the lease the import is refused, whether or
// not --allow-nonempty is set, and nothing is written; once the holder
// releases it, the import proceeds, bumps the epoch and leaves the lease free.
func TestImportRefusesWhileAWriterHoldsTheLease(t *testing.T) {
	snap, m, sum := fixture(t)
	url := storetest.URL(t)
	holder, err := store.OpenReader(url)
	require.NoError(t, err)
	defer holder.Close()
	lease, err := holder.AcquireLease(context.Background(), store.LeaseOptions{Holder: "server-a", Address: "10.0.0.1:8080"})
	require.NoError(t, err)

	for _, allow := range []bool{false, true} {
		start := time.Now()
		r, err := sqliteimport.Import(context.Background(), sqliteimport.ImportOptions{
			SourcePath: snap, Manifest: m, TargetURL: url, AllowNonEmpty: allow, Version: "test",
			LockTimeout: 300 * time.Millisecond})
		require.ErrorIs(t, err, sqliteimport.ErrWriterLive, "allow-nonempty=%v", allow)
		assert.Nil(t, r)
		assert.ErrorContains(t, err, `"server-a"`, "the message names the holder")
		assert.ErrorContains(t, err, "--allow-nonempty does not override")
		assert.Less(t, time.Since(start), 5*time.Second)
	}
	assert.Zero(t, scalar(t, url, `SELECT count(*) FROM events`))
	assert.Zero(t, scalar(t, url, `SELECT count(*) FROM import_log`))
	assert.True(t, holder.HoldsLease(), "the holder kept its lease")

	require.NoError(t, holder.ReleaseLease())
	r, err := importInto(t, url, snap, m, false)
	require.NoError(t, err)
	require.True(t, r.OK())
	assert.Equal(t, sum.Rows["events"], scalar(t, url, `SELECT count(*) FROM events`))
	assert.Equal(t, lease.Epoch()+1, scalar(t, url, `SELECT epoch FROM writer_lease WHERE id = 1`),
		"the import held its own term")
	assert.Zero(t, scalar(t, url, `SELECT count(*) FROM writer_lease WHERE holder <> '' OR address <> ''`),
		"the import released the lease")

	// And a server takes over after it, in a newer term.
	l2, err := holder.AcquireLease(context.Background(), store.LeaseOptions{Holder: "server-a", Wait: 2 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, lease.Epoch()+2, l2.Epoch())
}

// A write that a stale term started cannot land after the import took the
// lease: the importer's epoch bump fences it (the server's own store fence,
// exercised here through the importer as the next term).
func TestImportFencesAStaleWriter(t *testing.T) {
	snap, m, _ := fixture(t)
	url := storetest.URL(t)
	old, err := store.OpenReader(url)
	require.NoError(t, err)
	defer old.Close()
	_, err = old.AcquireLease(context.Background(), store.LeaseOptions{Holder: "old"})
	require.NoError(t, err)
	// The old writer's session dies without it noticing yet: its lock is gone,
	// its pool still works.
	execPG(t, url, `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND pid <> pg_backend_pid()
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`)

	r, err := importInto(t, url, snap, m, false)
	require.NoError(t, err)
	require.True(t, r.OK())
	err = old.SeedSources(context.Background(), []store.SourceSeed{{ID: "usgs", Name: "u", PollInterval: time.Minute}})
	assert.ErrorIs(t, err, store.ErrNotWriter, "the stale writer is fenced out")
}
