package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/lib/geojson"
	"github.com/sierragridteam/sierra-data/internal/store/migrations"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

var baseTime = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// newTestStore opens a store on a fresh, migrated database of its own
// (internal/storetest; skipped without TEST_DATABASE_URL).
func newTestStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	s, err := Open(storetest.URL(t), opts...)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func seedSource(t *testing.T, s *Store, id string) {
	t.Helper()
	require.NoError(t, s.SeedSources(context.Background(), []SourceSeed{{
		ID:           id,
		Name:         id,
		PollInterval: time.Minute,
	}}))
}

func testEvent(id string, sev gridv1.Severity, status gridv1.EventStatus, headline string) *gridv1.Event {
	return &gridv1.Event{
		Id:         id,
		Layer:      gridv1.Layer_EARTHQUAKE,
		Severity:   sev,
		Status:     status,
		Headline:   headline,
		Provenance: &gridv1.Provenance{SourceId: "usgs", SourceName: "USGS"},
		ObservedAt: timestamppb.New(baseTime),
	}
}

func pointGeometry(lat, lng float64) *gridv1.Geometry {
	// bbox/centroid deliberately unset: exercises the ingest-time backfill.
	return &gridv1.Geometry{Geojson: geojson.PointGeoJSON(lat, lng)}
}

func polyGeometry(minLat, minLng, maxLat, maxLng float64) *gridv1.Geometry {
	return &gridv1.Geometry{Geojson: geojson.BboxPolygonGeoJSON(minLat, minLng, maxLat, maxLng)}
}

// geomFromGeoJSON wraps a raw GeoJSON geometry (for non-rectangular test shapes;
// bbox/centroid are backfilled at ingest).
func geomFromGeoJSON(raw string) *gridv1.Geometry {
	return &gridv1.Geometry{Geojson: []byte(raw)}
}

func testPlace(id, slug, name string, kind gridv1.PlaceKind, geom *gridv1.Geometry) *gridv1.Place {
	return &gridv1.Place{Id: id, Slug: slug, Name: name, Kind: kind, Geometry: geom}
}

func revisionCount(t *testing.T, s *Store, eventID string) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM event_revisions WHERE event_id = $1`, eventID).Scan(&n))
	return n
}

func TestOpenRejectsConcurrentOpener(t *testing.T) {
	url := storetest.URL(t)
	s1, err := Open(url)
	require.NoError(t, err)

	// A second opener while the first holds the writer lock must fail loudly
	// after its wait, not open and become a second writer.
	start := time.Now()
	_, err = Open(url, WithLockTimeout(300*time.Millisecond))
	require.Error(t, err, "second concurrent open must be rejected")
	assert.True(t, errors.Is(err, ErrLockHeld), "%v", err)
	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "it waited the lock timeout")
	// The message must name the deploy-time cause: this error's most common
	// context by far is a deploy whose previous writer has not released yet.
	assert.Contains(t, err.Error(), "quiesced or yielded")

	// Closing the first releases the lock, so a fresh open then succeeds.
	require.NoError(t, s1.Close())
	s2, err := Open(url, WithLockTimeout(100*time.Millisecond))
	require.NoError(t, err, "reopen after close must succeed (lock released)")
	require.NoError(t, s2.Close())
}

// The writer lock is a session advisory lock on a dedicated connection: it
// shows in pg_locks while held and is gone after Close.
func TestWriterLockVisibleInPgLocks(t *testing.T) {
	ctx := context.Background()
	url := storetest.URL(t)
	s, err := Open(url)
	require.NoError(t, err)
	held := func(st *Store) int {
		var n int
		require.NoError(t, st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_locks
			WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			  AND ((classid::bigint << 32) | objid::bigint) = $1`, writerLockKey).Scan(&n))
		return n
	}
	assert.Equal(t, 1, held(s))
	require.NoError(t, s.Close())
	s2, err := Open(url)
	require.NoError(t, err)
	defer s2.Close()
	assert.Equal(t, 1, held(s2), "exactly one holder: the new owner")
}

func TestOpenMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	url := storetest.EmptyURL(t)

	s1, err := Open(url)
	require.NoError(t, err)
	seedSource(t, s1, "usgs")
	require.NoError(t, s1.Close())

	// Reopen re-runs migrate; applied versions must be skipped.
	s2, err := Open(url)
	require.NoError(t, err)
	defer s2.Close()

	var applied int
	require.NoError(t, s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE source = 'postgres'`).Scan(&applied))
	assert.Equal(t, migrations.Latest(), applied)

	srcs, err := s2.ListSources(ctx)
	require.NoError(t, err)
	require.Len(t, srcs, 1)
}

// Additive-only schema policy: a build accepts a database a newer release
// has migrated past it, and refuses one older than it needs.
func TestSchemaVersionPolicy(t *testing.T) {
	ctx := context.Background()
	url := storetest.URL(t)
	s, err := Open(url)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO schema_migrations (source, version) VALUES ('postgres', $1)`, migrations.Latest()+1)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s, err = Open(url)
	require.NoError(t, err, "a newer schema is accepted")
	st, err := s.Settings(ctx)
	require.NoError(t, err)
	assert.Equal(t, migrations.Latest()+1, st.SchemaVersion)

	_, err = s.db.ExecContext(ctx, `DELETE FROM schema_migrations`)
	require.NoError(t, err)
	_, err = migrations.CheckVersion(ctx, s.db)
	assert.Error(t, err, "an older schema is refused by a reader")
	require.NoError(t, s.Close())
}

// Legacy SQLite history lives beside the Postgres ladder under its own
// source, so recording it (the importer does) cannot collide with version 1.
func TestSchemaMigrationsKeyedBySource(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations (source, version)
		SELECT 'sqlite', v FROM generate_series(1, 14) AS v`)
	require.NoError(t, err)
	v, err := migrations.Current(ctx, s.db)
	require.NoError(t, err)
	assert.Equal(t, migrations.Latest(), v, "the sqlite ladder does not count as Postgres versions")
}

func TestOpenRequiresAURL(t *testing.T) {
	_, err := Open("")
	assert.ErrorContains(t, err, "PF__GRID__DATABASE__URL")
}

func TestReopenRehydrates(t *testing.T) {
	ctx := context.Background()
	url := storetest.URL(t)

	s, err := Open(url)
	require.NoError(t, err)
	seedSource(t, s, "usgs")
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:calaveras", "calaveras", "Calaveras County",
		gridv1.PlaceKind_COUNTY, polyGeometry(38.0, -120.9, 38.5, -120.0))))

	ev := testEvent("usgs:q1", gridv1.Severity_MODERATE, gridv1.EventStatus_ACTIVE, "M4.2 near Murphys")
	ev.Geometry = pointGeometry(38.2, -120.45)
	res, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	require.True(t, res.Changed)

	ev2 := testEvent("usgs:q1", gridv1.Severity_MODERATE, gridv1.EventStatus_ACTIVE, "M4.3 near Murphys (revised)")
	ev2.Geometry = pointGeometry(38.2, -120.45)
	_, err = s.UpsertEvent(ctx, ev2)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	// Restart: data, revisions, place attachments, and indexes must survive.
	s2, err := Open(url)
	require.NoError(t, err)
	defer s2.Close()

	got, err := s2.GetEvent(ctx, "usgs:q1")
	require.NoError(t, err)
	assert.Equal(t, "M4.3 near Murphys (revised)", got.GetHeadline())
	assert.Equal(t, uint32(2), got.GetRevision())
	assert.Equal(t, []string{"county:calaveras"}, got.GetPlaceIds())
	assert.NotNil(t, got.GetGeometry().GetBbox(), "bbox backfilled at ingest survives")

	revs, _, err := s2.EventHistory(ctx, "usgs:q1", 10, "")
	require.NoError(t, err)
	require.Len(t, revs, 2)
	assert.Equal(t, uint32(2), revs[0].GetRevision())
	assert.Equal(t, "M4.2 near Murphys", revs[1].GetEvent().GetHeadline())

	active, err := s2.ActiveEventsBySource(ctx, "usgs")
	require.NoError(t, err)
	assert.Len(t, active, 1)
}

func TestGetEventNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetEvent(context.Background(), "usgs:nope")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestUpsertEventRequiresSeededSource(t *testing.T) {
	s := newTestStore(t)
	// foreign_keys=ON: events.source_id references sources(id), so ingest
	// against an unseeded source fails loudly instead of orphaning rows.
	_, err := s.UpsertEvent(context.Background(),
		testEvent("usgs:q1", gridv1.Severity_INFO, gridv1.EventStatus_ACTIVE, "x"))
	assert.Error(t, err)
}

// Released means released (internal/handoff): once a store is closed, the
// lock is free for the next owner straight away, and the closed store refuses
// every query rather than reaching the file.
func TestClosedStoreReleasesTheLockAndRefusesQueries(t *testing.T) {
	ctx := context.Background()
	url := storetest.URL(t)
	s, err := Open(url)
	require.NoError(t, err)
	seedSource(t, s, "usgs")
	require.NoError(t, s.Close())

	next, err := Open(url, WithLockTimeout(100*time.Millisecond))
	require.NoError(t, err, "the next owner must not wait for the lock")
	defer next.Close()

	_, _, err = s.QueryEvents(ctx, EventQuery{PageSize: 10})
	assert.Error(t, err, "a released store must not query the database")
	_, err = s.UpsertEvent(ctx, testEvent("usgs:x", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "x"))
	assert.Error(t, err, "nor write to it")
}

// A lock wait ends when its context does (WithLockContext): a SIGTERM during a
// handoff activation must not sit out the full lockTimeout.
func TestOpenLockWaitHonoursContext(t *testing.T) {
	url := storetest.URL(t)
	holder, err := Open(url)
	require.NoError(t, err)
	defer holder.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err = Open(url, WithLockTimeout(time.Minute), WithLockContext(ctx))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 5*time.Second, "the wait ended with the context, not the minute")
}

func refreshVersion(t *testing.T, s *Store, layers []gridv1.Layer) int64 {
	t.Helper()
	v, err := s.RefreshVersion(context.Background(), layers)
	require.NoError(t, err)
	return v
}

func meshActivityVersion(t *testing.T, s *Store) int64 {
	t.Helper()
	v, err := s.MeshActivityVersion(context.Background())
	require.NoError(t, err)
	return v
}
