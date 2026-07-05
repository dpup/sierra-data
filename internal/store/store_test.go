package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/lib/geojson"
)

var baseTime = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// newTestStore opens a store under a nested temp path (exercises the
// parent-dir mkdir in Open).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "nested", "grid.db"))
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

func testPlace(id, slug, name string, kind gridv1.PlaceKind, geom *gridv1.Geometry) *gridv1.Place {
	return &gridv1.Place{Id: id, Slug: slug, Name: name, Kind: kind, Geometry: geom}
}

func revisionCount(t *testing.T, s *Store, eventID string) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM event_revisions WHERE event_id = ?`, eventID).Scan(&n))
	return n
}

func TestOpenMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "grid.db")

	s1, err := Open(path)
	require.NoError(t, err)
	seedSource(t, s1, "usgs")
	require.NoError(t, s1.Close())

	// Reopen re-runs migrate; applied versions must be skipped.
	s2, err := Open(path)
	require.NoError(t, err)
	defer s2.Close()

	var applied int
	require.NoError(t, s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied))
	assert.Equal(t, 1, applied)

	srcs, err := s2.ListSources(ctx)
	require.NoError(t, err)
	require.Len(t, srcs, 1)
}

func TestReopenRehydrates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "grid.db")

	s, err := Open(path)
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
	s2, err := Open(path)
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
