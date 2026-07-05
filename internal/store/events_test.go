package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
)

func TestUpsertRevisionGating(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")

	ev := testEvent("usgs:q1", gridv1.Severity_MODERATE, gridv1.EventStatus_ACTIVE, "M4.2 near Murphys")
	res, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: true, Revision: 1}, res)
	assert.Equal(t, 1, revisionCount(t, s, "usgs:q1"))

	// Identical content, re-stamped volatile fields: no writes.
	same := proto.Clone(ev).(*gridv1.Event)
	same.ObservedAt = timestamppb.New(baseTime.Add(10 * time.Minute))
	same.Provenance.FetchedAt = timestamppb.New(baseTime.Add(10 * time.Minute))
	res, err = s.UpsertEvent(ctx, same)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: false, Revision: 1}, res)
	assert.Equal(t, 1, revisionCount(t, s, "usgs:q1"))

	// Changed headline: revision 2.
	changed := proto.Clone(ev).(*gridv1.Event)
	changed.Headline = "M4.3 near Murphys (revised)"
	res, err = s.UpsertEvent(ctx, changed)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: true, Revision: 2}, res)
	assert.Equal(t, 2, revisionCount(t, s, "usgs:q1"))

	// Summary/enhancement-only change: zeroed in the hash, so no revision —
	// enhancement output must never cause hash churn.
	enhanced := proto.Clone(changed).(*gridv1.Event)
	enhanced.Summary = "AI-condensed summary"
	enhanced.Enhancement = &gridv1.Enhancement{
		Model:      "gpt-5-mini",
		EnhancedAt: timestamppb.New(baseTime),
		Fields:     []string{"summary"},
	}
	res, err = s.UpsertEvent(ctx, enhanced)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: false, Revision: 2}, res)
	assert.Equal(t, 2, revisionCount(t, s, "usgs:q1"))
}

func TestNeedsUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")

	ev := testEvent("usgs:q1", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "quake")
	need, err := s.NeedsUpdate(ctx, ev)
	require.NoError(t, err)
	assert.True(t, need, "unknown event needs update")

	_, err = s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	need, err = s.NeedsUpdate(ctx, ev)
	require.NoError(t, err)
	assert.False(t, need)

	summaryOnly := proto.Clone(ev).(*gridv1.Event)
	summaryOnly.Summary = "new summary"
	need, err = s.NeedsUpdate(ctx, summaryOnly)
	require.NoError(t, err)
	assert.False(t, need, "summary is excluded from the content hash")

	changed := proto.Clone(ev).(*gridv1.Event)
	changed.Headline = "bigger quake"
	need, err = s.NeedsUpdate(ctx, changed)
	require.NoError(t, err)
	assert.True(t, need)
}

func TestTransitionEvents(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")

	ev := testEvent("usgs:q1", gridv1.Severity_MODERATE, gridv1.EventStatus_ACTIVE, "quake")
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	resolvedAt := baseTime.Add(time.Hour)
	require.NoError(t, s.TransitionEvents(ctx, []string{"usgs:q1", "usgs:missing"},
		gridv1.EventStatus_RESOLVED, resolvedAt))

	got, err := s.GetEvent(ctx, "usgs:q1")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, got.GetStatus())
	assert.Equal(t, uint32(2), got.GetRevision())
	assert.Equal(t, resolvedAt.Unix(), got.GetObservedAt().AsTime().Unix())
	assert.Equal(t, 2, revisionCount(t, s, "usgs:q1"), "the all-clear is a revision")

	// Idempotent: already-RESOLVED ids are skipped.
	require.NoError(t, s.TransitionEvents(ctx, []string{"usgs:q1"},
		gridv1.EventStatus_RESOLVED, resolvedAt.Add(time.Minute)))
	got, err = s.GetEvent(ctx, "usgs:q1")
	require.NoError(t, err)
	assert.Equal(t, uint32(2), got.GetRevision())
	assert.Equal(t, 2, revisionCount(t, s, "usgs:q1"))

	// Resolved events leave the active set.
	active, err := s.ActiveEventsBySource(ctx, "usgs")
	require.NoError(t, err)
	assert.Empty(t, active)

	// Reappearance in the feed (status differs => hash differs) reactivates
	// with a new revision.
	res, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: true, Revision: 3}, res)
	got, err = s.GetEvent(ctx, "usgs:q1")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, got.GetStatus())
}

func TestEventPlacesUnionKeepsPresetIDs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "nws")
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:calaveras", "calaveras", "Calaveras County",
		gridv1.PlaceKind_COUNTY, polyGeometry(38.0, -120.9, 38.5, -120.0))))

	// Zone-carrying weather alert: the caller pre-sets place_ids from the
	// zone->area mapping; geometry also lands in the county. Both survive.
	ev := testEvent("wx:alert1", gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE, "Winter Storm Warning")
	ev.Layer = gridv1.Layer_WEATHER_ALERT
	ev.Provenance.SourceId = "nws"
	ev.PlaceIds = []string{"area:calaveras"}
	ev.Geometry = pointGeometry(38.2, -120.45)
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	got, err := s.GetEvent(ctx, "wx:alert1")
	require.NoError(t, err)
	assert.Equal(t, []string{"area:calaveras", "county:calaveras"}, got.GetPlaceIds())

	rows, err := s.db.Query(`SELECT place_id FROM event_places WHERE event_id = ? ORDER BY place_id`, "wx:alert1")
	require.NoError(t, err)
	defer rows.Close()
	var placeIDs []string
	for rows.Next() {
		var pid string
		require.NoError(t, rows.Scan(&pid))
		placeIDs = append(placeIDs, pid)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"area:calaveras", "county:calaveras"}, placeIDs)
}

func TestPolygonEventPlaceMatching(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "wfigs")
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:calaveras", "calaveras", "Calaveras County",
		gridv1.PlaceKind_COUNTY, polyGeometry(38.0, -120.9, 38.5, -120.0))))
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:faraway", "faraway", "Faraway County",
		gridv1.PlaceKind_COUNTY, polyGeometry(40.0, -122.0, 40.5, -121.0))))

	// Perimeter straddling the county's north edge: event centroid is outside
	// the county and the county's bbox center is outside the perimeter, so
	// only the permissive polygon-polygon bbox-overlap rule attaches it —
	// over-attach beats missing a perimeter crossing a boundary.
	ev := testEvent("wfigs:test-fire", gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE, "Test Fire")
	ev.Layer = gridv1.Layer_WILDFIRE
	ev.Provenance.SourceId = "wfigs"
	ev.Geometry = polyGeometry(38.4, -120.5, 38.7, -120.3)
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	got, err := s.GetEvent(ctx, "wfigs:test-fire")
	require.NoError(t, err)
	assert.Equal(t, []string{"county:calaveras"}, got.GetPlaceIds(),
		"bbox-overlapping county attaches; disjoint county does not")
}

func TestRtreeConsistencyAcrossGeometryChanges(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "wfigs")

	geoRows := func() (n int, minLat, maxLat, minLng, maxLng float64) {
		t.Helper()
		rows, err := s.db.Query(`
			SELECT g.min_lat, g.max_lat, g.min_lng, g.max_lng
			FROM event_geo g JOIN event_geo_map m ON m.rowid = g.rowid
			WHERE m.event_id = ?`, "wfigs:fire")
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			n++
			require.NoError(t, rows.Scan(&minLat, &maxLat, &minLng, &maxLng))
		}
		require.NoError(t, rows.Err())
		return
	}

	ev := testEvent("wfigs:fire", gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE, "fire v1")
	ev.Layer = gridv1.Layer_WILDFIRE
	ev.Provenance.SourceId = "wfigs"
	ev.Geometry = polyGeometry(38.1, -120.6, 38.3, -120.4)
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	// R*Tree stores coordinates as 32-bit floats rounded outward, so compare
	// at ~1e-4 (a few meters), not float64 precision.
	n, minLat, maxLat, minLng, maxLng := geoRows()
	require.Equal(t, 1, n)
	assert.InDelta(t, 38.1, minLat, 1e-4)
	assert.InDelta(t, 38.3, maxLat, 1e-4)
	assert.InDelta(t, -120.6, minLng, 1e-4)
	assert.InDelta(t, -120.4, maxLng, 1e-4)

	// Perimeter grows: still exactly one R*Tree row, with the new bbox.
	ev2 := proto.Clone(ev).(*gridv1.Event)
	ev2.Headline = "fire v2"
	ev2.Geometry = polyGeometry(38.0, -120.7, 38.4, -120.2)
	_, err = s.UpsertEvent(ctx, ev2)
	require.NoError(t, err)

	n, minLat, maxLat, minLng, maxLng = geoRows()
	require.Equal(t, 1, n)
	assert.InDelta(t, 38.0, minLat, 1e-4)
	assert.InDelta(t, 38.4, maxLat, 1e-4)
	assert.InDelta(t, -120.7, minLng, 1e-4)
	assert.InDelta(t, -120.2, maxLng, 1e-4)

	// Geometry dropped entirely: both the rtree row and the map row go away.
	ev3 := proto.Clone(ev).(*gridv1.Event)
	ev3.Headline = "fire v3"
	ev3.Geometry = nil
	_, err = s.UpsertEvent(ctx, ev3)
	require.NoError(t, err)

	n, _, _, _, _ = geoRows()
	assert.Equal(t, 0, n)
	var mapRows int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM event_geo_map WHERE event_id = ?`, "wfigs:fire").Scan(&mapRows))
	assert.Equal(t, 0, mapRows)
}

func TestContentHashIgnoresVolatileFields(t *testing.T) {
	ev := testEvent("usgs:q1", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "quake")
	base := ContentHash(ev)

	restamped := proto.Clone(ev).(*gridv1.Event)
	restamped.Revision = 7
	restamped.IngestedAt = timestamppb.New(baseTime.Add(time.Hour))
	restamped.ObservedAt = timestamppb.New(baseTime.Add(time.Hour))
	restamped.Provenance.FetchedAt = timestamppb.New(baseTime.Add(time.Hour))
	restamped.Summary = "enhanced"
	restamped.Enhancement = &gridv1.Enhancement{Model: "gpt-5-mini"}
	restamped.PlaceIds = []string{"area:calaveras"}
	assert.Equal(t, base, ContentHash(restamped))

	changed := proto.Clone(ev).(*gridv1.Event)
	changed.Severity = gridv1.Severity_SEVERE
	assert.NotEqual(t, base, ContentHash(changed))

	statusChanged := proto.Clone(ev).(*gridv1.Event)
	statusChanged.Status = gridv1.EventStatus_RESOLVED
	assert.NotEqual(t, base, ContentHash(statusChanged), "status is hashed content")
}
