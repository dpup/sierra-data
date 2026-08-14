package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
)

func TestEventVersion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")

	// Unknown id: ok=false, no error (the ETag validator seam for a 404).
	rev, ok, err := s.EventVersion(ctx, "usgs:missing")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Zero(t, rev)

	ev := testEvent("usgs:q1", gridv1.Severity_MODERATE, gridv1.EventStatus_ACTIVE, "M4.2 near Murphys")
	_, err = s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	rev, ok, err = s.EventVersion(ctx, "usgs:q1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, int64(1), rev)

	// A content change bumps the revision the validator keys off.
	changed := proto.Clone(ev).(*gridv1.Event)
	changed.Headline = "M4.3 near Murphys (revised)"
	_, err = s.UpsertEvent(ctx, changed)
	require.NoError(t, err)

	rev, ok, err = s.EventVersion(ctx, "usgs:q1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, int64(2), rev)
}

func TestDataVersion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")

	v0, err := s.DataVersion(ctx)
	require.NoError(t, err)
	assert.Zero(t, v0, "empty store")

	ev := testEvent("usgs:q1", gridv1.Severity_MODERATE, gridv1.EventStatus_ACTIVE, "M4.2 near Murphys")
	_, err = s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	v1, err := s.DataVersion(ctx)
	require.NoError(t, err)
	assert.Greater(t, v1, v0, "a new event bumps the version")

	// Hash-equal no-op re-upsert: the version must NOT move (else every idle poll
	// would invalidate every list ETag).
	same := proto.Clone(ev).(*gridv1.Event)
	same.ObservedAt = timestamppb.New(baseTime.Add(time.Minute))
	same.Provenance.FetchedAt = timestamppb.New(baseTime.Add(time.Minute))
	_, err = s.UpsertEvent(ctx, same)
	require.NoError(t, err)
	v2, err := s.DataVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, v1, v2, "a hash-equal no-op must not bump the version")

	// A content change bumps it.
	changed := proto.Clone(ev).(*gridv1.Event)
	changed.Headline = "M4.3 near Murphys (revised)"
	_, err = s.UpsertEvent(ctx, changed)
	require.NoError(t, err)
	v3, err := s.DataVersion(ctx)
	require.NoError(t, err)
	assert.Greater(t, v3, v2, "a content change bumps the version")

	// A pure status transition (resolve) also bumps it — this is the never-stale-304
	// guarantee: a list result changing because an event resolved must invalidate.
	require.NoError(t, s.TransitionEvents(ctx, []string{"usgs:q1"}, gridv1.EventStatus_RESOLVED, baseTime.Add(time.Hour)))
	v4, err := s.DataVersion(ctx)
	require.NoError(t, err)
	assert.Greater(t, v4, v3, "a lifecycle transition bumps the version")
}

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

// A place seeded AFTER an event exists must attach on the next hash-equal
// upsert: place_ids are zeroed out of the content hash, so hash-equal must
// still recompute the place set (Finding: preset/newly-seeded places never
// attached to unchanged events).
func TestUpsertHashEqualRefreshesPlaces(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")

	ev := testEvent("usgs:q1", gridv1.Severity_MODERATE, gridv1.EventStatus_ACTIVE, "quake")
	ev.Geometry = pointGeometry(38.2, -120.45)
	res, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	require.True(t, res.Changed)

	got, err := s.GetEvent(ctx, "usgs:q1")
	require.NoError(t, err)
	require.Empty(t, got.GetPlaceIds(), "no places seeded yet")

	// The containing county arrives afterwards (boot order: events can be
	// ingested before the places seeder runs, and polygons get added later).
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:calaveras", "calaveras", "Calaveras County",
		gridv1.PlaceKind_COUNTY, polyGeometry(38.0, -120.9, 38.5, -120.0))))

	// Re-upsert identical content: no revision, but the place attaches.
	same := proto.Clone(ev).(*gridv1.Event)
	res, err = s.UpsertEvent(ctx, same)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: false, Revision: 1}, res)
	assert.Equal(t, 1, revisionCount(t, s, "usgs:q1"), "place refresh must not write a revision")

	got, err = s.GetEvent(ctx, "usgs:q1")
	require.NoError(t, err)
	assert.Equal(t, []string{"county:calaveras"}, got.GetPlaceIds(), "blob place_ids updated in place")

	events, _, err := s.QueryEvents(ctx, EventQuery{PlaceID: "county:calaveras"})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "usgs:q1", events[0].GetId())

	// A changed caller preset on identical content also attaches (still no
	// revision), unioned with the geometric matches.
	preset := proto.Clone(ev).(*gridv1.Event)
	preset.PlaceIds = []string{"area:calaveras"}
	res, err = s.UpsertEvent(ctx, preset)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: false, Revision: 1}, res)
	got, err = s.GetEvent(ctx, "usgs:q1")
	require.NoError(t, err)
	assert.Equal(t, []string{"area:calaveras", "county:calaveras"}, got.GetPlaceIds())
	assert.Equal(t, 1, revisionCount(t, s, "usgs:q1"))
}

// TouchSeen stamps last_seen_at in one UPDATE with no revision and no hash
// effect; UpsertEvent stamps it on the insert/change paths.
func TestTouchSeenAndLastSeenAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")

	before := time.Now().Add(-time.Second)
	ev := testEvent("usgs:q1", gridv1.Severity_MODERATE, gridv1.EventStatus_ACTIVE, "quake")
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	active, err := s.ActiveEventsBySource(ctx, "usgs")
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.False(t, active[0].LastSeenAt.IsZero(), "insert stamps last_seen_at")
	assert.False(t, active[0].LastSeenAt.Before(before))

	// Touch (unknown ids are simply not matched by the UPDATE).
	seenAt := baseTime.Add(30 * time.Hour)
	require.NoError(t, s.TouchSeen(ctx, []string{"usgs:q1", "usgs:unknown"}, seenAt))

	active, err = s.ActiveEventsBySource(ctx, "usgs")
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, seenAt.Unix(), active[0].LastSeenAt.Unix())
	assert.Equal(t, uint32(1), active[0].Event.GetRevision(), "touch writes no revision")
	assert.Equal(t, 1, revisionCount(t, s, "usgs:q1"))

	// Touch has no hash effect: an identical upsert is still a no-op.
	res, err := s.UpsertEvent(ctx, proto.Clone(ev).(*gridv1.Event))
	require.NoError(t, err)
	assert.False(t, res.Changed)

	// Empty id list is a no-op.
	require.NoError(t, s.TouchSeen(ctx, nil, seenAt))

	// A content change (the DO UPDATE path) re-stamps last_seen_at.
	changed := proto.Clone(ev).(*gridv1.Event)
	changed.Headline = "bigger quake"
	_, err = s.UpsertEvent(ctx, changed)
	require.NoError(t, err)
	active, err = s.ActiveEventsBySource(ctx, "usgs")
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.False(t, active[0].LastSeenAt.Before(before), "change path re-stamps last_seen_at")
	assert.NotEqual(t, seenAt.Unix(), active[0].LastSeenAt.Unix())
}

func TestPolygonEventPlaceMatching(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "firis")
	// Calaveras: a rectangle the perimeter genuinely overlaps (its SW corner is
	// inside the perimeter) — attaches.
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:calaveras", "calaveras", "Calaveras County",
		gridv1.PlaceKind_COUNTY, polyGeometry(38.0, -120.9, 38.5, -120.0))))
	// Faraway: disjoint bbox — never attaches.
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:faraway", "faraway", "Faraway County",
		gridv1.PlaceKind_COUNTY, polyGeometry(40.0, -122.0, 40.5, -121.0))))
	// Clipped: a triangle in the NE of its bbox. Its BBOX overlaps the perimeter
	// (the Dove case — a large county whose rectangular bbox clips a nearby fire),
	// but the triangle body is well NE of the perimeter, so the geometries do NOT
	// actually intersect → must NOT attach (the bbox-only rule wrongly would).
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:clipped", "clipped", "Clipped County", gridv1.PlaceKind_COUNTY,
		geomFromGeoJSON(`{"type":"Polygon","coordinates":[[[-120.4,39.0],[-119.9,39.0],[-119.9,38.5],[-120.4,39.0]]]}`))))

	// Perimeter straddling Calaveras's north edge: event centroid is outside every
	// county and no county's bbox center is inside the perimeter, so the
	// polygon-overlap rule decides — and it must attach ONLY the county the
	// perimeter actually intersects.
	ev := testEvent("firis:test-fire", gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE, "Test Fire")
	ev.Layer = gridv1.Layer_WILDFIRE
	ev.Provenance.SourceId = "firis"
	ev.Geometry = polyGeometry(38.4, -120.5, 38.7, -120.3)
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	got, err := s.GetEvent(ctx, "firis:test-fire")
	require.NoError(t, err)
	assert.Equal(t, []string{"county:calaveras"}, got.GetPlaceIds(),
		"genuinely-overlapping county attaches; bbox-only (clipped) and disjoint (faraway) do not")
}

func TestRtreeConsistencyAcrossGeometryChanges(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "firis")

	geoRows := func() (n int, minLat, maxLat, minLng, maxLng float64) {
		t.Helper()
		rows, err := s.db.Query(`
			SELECT g.min_lat, g.max_lat, g.min_lng, g.max_lng
			FROM event_geo g JOIN event_geo_map m ON m.rowid = g.rowid
			WHERE m.event_id = ?`, "firis:fire")
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			n++
			require.NoError(t, rows.Scan(&minLat, &maxLat, &minLng, &maxLng))
		}
		require.NoError(t, rows.Err())
		return
	}

	ev := testEvent("firis:fire", gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE, "fire v1")
	ev.Layer = gridv1.Layer_WILDFIRE
	ev.Provenance.SourceId = "firis"
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
		`SELECT COUNT(*) FROM event_geo_map WHERE event_id = ?`, "firis:fire").Scan(&mapRows))
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

// A NETWORK event's telemetry (SNR/RSSI/hops/gateways) is excluded from the
// content hash: a mesh node re-adverting with fresh signal metrics must not
// mint a revision, or the MeshCore advert firehose blows up event_revisions.
// Identity/name/status changes still do.
func TestContentHashIgnoresMeshTelemetry(t *testing.T) {
	node := func() *gridv1.Event {
		return &gridv1.Event{
			Id:         "meshcore:abc123",
			Layer:      gridv1.Layer_MESH,
			Severity:   gridv1.Severity_INFO,
			Status:     gridv1.EventStatus_ACTIVE,
			Headline:   "Murphys Ridge (repeater)",
			Provenance: &gridv1.Provenance{SourceId: "meshcore", SourceName: "MeshCore Mesh"},
			ObservedAt: timestamppb.New(baseTime),
			Detail: &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{
				PublicKey: "abc123",
				NodeType:  "repeater",
				Name:      "Murphys Ridge",
				Telemetry: &gridv1.MeshTelemetry{Snr: 4.5, Rssi: -93, HopCount: 1},
			}},
		}
	}
	base := ContentHash(node())

	// Fresh telemetry only: same hash.
	telem := node()
	telem.GetMesh().Telemetry = &gridv1.MeshTelemetry{
		Snr: -7.25, Rssi: -119, HopCount: 3,
		Gateways: []string{"ag loft rpt"},
	}
	assert.Equal(t, base, ContentHash(telem), "telemetry is excluded from the hash")

	// Nil telemetry vs populated telemetry: still the same hash.
	nilTelem := node()
	nilTelem.GetMesh().Telemetry = nil
	assert.Equal(t, base, ContentHash(nilTelem))

	// Stable-identity changes still flip the hash.
	renamed := node()
	renamed.GetMesh().Name = "Murphys Ridge East"
	renamed.Headline = "Murphys Ridge East (repeater)"
	assert.NotEqual(t, base, ContentHash(renamed), "name is hashed content")

	roled := node()
	roled.GetMesh().NodeType = "room_server"
	assert.NotEqual(t, base, ContentHash(roled), "role is hashed content")
}

// End-to-end store check: re-upserting a NETWORK node with only new telemetry
// is a hash-equal no-op (no new revision), matching the enhancement pattern.
func TestUpsertMeshTelemetryIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "meshcore")

	node := &gridv1.Event{
		Id:         "meshcore:abc123",
		Layer:      gridv1.Layer_MESH,
		Severity:   gridv1.Severity_INFO,
		Status:     gridv1.EventStatus_ACTIVE,
		Headline:   "Murphys Ridge (repeater)",
		Provenance: &gridv1.Provenance{SourceId: "meshcore", SourceName: "MeshCore Mesh"},
		ObservedAt: timestamppb.New(baseTime),
		Detail: &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{
			PublicKey: "abc123", NodeType: "repeater", Name: "Murphys Ridge",
			Telemetry: &gridv1.MeshTelemetry{Snr: 4.5, Rssi: -93},
		}},
	}
	res, err := s.UpsertEvent(ctx, node)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: true, Revision: 1}, res)
	assert.Equal(t, 1, revisionCount(t, s, "meshcore:abc123"))

	// A later advert, new signal metrics only: no revision.
	reheard := proto.Clone(node).(*gridv1.Event)
	reheard.ObservedAt = timestamppb.New(baseTime.Add(time.Hour))
	reheard.GetMesh().Telemetry = &gridv1.MeshTelemetry{Snr: -8, Rssi: -120, HopCount: 2}
	res, err = s.UpsertEvent(ctx, reheard)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: false, Revision: 1}, res)
	assert.Equal(t, 1, revisionCount(t, s, "meshcore:abc123"))

	// A name change does write a revision.
	renamed := proto.Clone(reheard).(*gridv1.Event)
	renamed.GetMesh().Name = "Murphys Ridge East"
	renamed.Headline = "Murphys Ridge East (repeater)"
	res, err = s.UpsertEvent(ctx, renamed)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: true, Revision: 2}, res)
	assert.Equal(t, 2, revisionCount(t, s, "meshcore:abc123"))
}

// An enhancement-only change (excluded from the content hash, so no new
// revision) must still PERSIST to the stored blob — a re-run enhancement with
// fresh enhanced_at/request/response is otherwise silently dropped on the
// hash-equal path.
func TestUpsertPersistsEnhancementOnHashEqual(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")

	ev := testEvent("chp:i1", gridv1.Severity_MODERATE, gridv1.EventStatus_ACTIVE, "Vehicle fire on Hwy 4")
	ev.Summary = "First AI narrative."
	ev.Enhancement = &gridv1.Enhancement{
		Model: "gpt-5-mini", EnhancedAt: timestamppb.New(baseTime),
		Fields: []string{"headline", "summary"}, Request: "prompt v1", Response: `{"v":1}`,
	}
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	// Same hashed content (headline/severity/status/geometry unchanged) but a
	// re-run enhancement: new time, request, response, and reworded summary.
	reEnhanced := proto.Clone(ev).(*gridv1.Event)
	reEnhanced.Summary = "Reworded AI narrative."
	reEnhanced.Enhancement = &gridv1.Enhancement{
		Model: "gpt-5-mini", EnhancedAt: timestamppb.New(baseTime.Add(24 * time.Hour)),
		Fields: []string{"headline", "summary"}, Request: "prompt v2", Response: `{"v":2}`,
	}
	res, err := s.UpsertEvent(ctx, reEnhanced)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: false, Revision: 1}, res, "no new revision")
	assert.Equal(t, 1, revisionCount(t, s, "chp:i1"))

	got, err := s.GetEvent(ctx, "chp:i1")
	require.NoError(t, err)
	assert.Equal(t, "Reworded AI narrative.", got.GetSummary(), "reworded summary persisted")
	assert.Equal(t, "prompt v2", got.GetEnhancement().GetRequest(), "fresh request persisted")
	assert.Equal(t, `{"v":2}`, got.GetEnhancement().GetResponse(), "fresh response persisted")
	assert.Equal(t, baseTime.Add(24*time.Hour).UTC(), got.GetEnhancement().GetEnhancedAt().AsTime(), "fresh enhanced_at persisted")

	// A plain no-op poll that carries NO enhancement must not erase the stored one.
	noEnh := proto.Clone(ev).(*gridv1.Event)
	noEnh.Summary = ""
	noEnh.Enhancement = nil
	_, err = s.UpsertEvent(ctx, noEnh)
	require.NoError(t, err)
	got, err = s.GetEvent(ctx, "chp:i1")
	require.NoError(t, err)
	require.NotNil(t, got.GetEnhancement(), "a no-enhancement poll must not erase the stored enhancement")
	assert.Equal(t, "prompt v2", got.GetEnhancement().GetRequest())
}

// newProximityStore is newTestStore with the wildfire proximity buffer enabled.
func newProximityStore(t *testing.T, meters float64) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "grid.db"), WithWildfireProximity(meters))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// seedProximityPlaces lays out one place of each kind around a common
// neighbourhood so the buffered-kinds rule can be checked in one pass. None of
// them contains or overlaps the fire used below.
func seedProximityPlaces(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	// Coverage area: 38.0..38.3 N, -120.6..-120.3 W.
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"area:ebbetts", "ebbetts", "Ebbetts Pass",
		gridv1.PlaceKind_AREA, polyGeometry(38.0, -120.6, 38.3, -120.3))))
	// Town point just inside the area's eastern half.
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"town:arnold", "arnold", "Arnold",
		gridv1.PlaceKind_TOWN, pointGeometry(38.25, -120.35))))
	// County covering the same ground, extended east past the fire.
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:calaveras", "calaveras", "Calaveras County",
		gridv1.PlaceKind_COUNTY, polyGeometry(38.0, -120.9, 38.3, -120.25))))
	// Corridor LineString running north-south inside the area.
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"corridor:hwy4", "hwy4", "Hwy 4",
		gridv1.PlaceKind_CORRIDOR,
		geomFromGeoJSON(`{"type":"LineString","coordinates":[[-120.5,38.05],[-120.4,38.25]]}`))))
}

// nearbyFire is a perimeter east of every place above: its western edge is at
// -120.2, i.e. ~0.1° (~8.8 km at this latitude) east of the area's -120.3 edge.
// It overlaps nothing.
func nearbyFire(id string) *gridv1.Event {
	ev := testEvent(id, gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE, "Nearby Fire")
	ev.Layer = gridv1.Layer_WILDFIRE
	ev.Provenance.SourceId = "firis"
	ev.Geometry = polyGeometry(38.1, -120.2, 38.2, -120.1)
	return ev
}

// The point of the buffer: a fire APPROACHING the coverage area attaches to it
// (and to the towns in it) before its perimeter crosses the boundary, so it
// shows on that place's map and summary while there is still time to act.
func TestWildfireProximityAttachesApproachingFire(t *testing.T) {
	ctx := context.Background()
	s := newProximityStore(t, 20000) // 20 km
	seedSource(t, s, "firis")
	seedProximityPlaces(t, s)

	_, err := s.UpsertEvent(ctx, nearbyFire("firis:nearby"))
	require.NoError(t, err)

	got, err := s.GetEvent(ctx, "firis:nearby")
	require.NoError(t, err)
	// area + town attach by proximity (~8.8 km / ~13 km). The county does NOT:
	// counties tile the map, so buffering them smears one fire across every
	// neighbour. The corridor keeps its own 1.5 km point rule.
	assert.Equal(t, []string{"area:ebbetts", "town:arnold"}, got.GetPlaceIds())
}

// Wider, not unbounded: the same fire outside the buffer attaches to nothing.
func TestWildfireProximityRespectsBufferDistance(t *testing.T) {
	ctx := context.Background()
	s := newProximityStore(t, 3000) // 3 km — closer than the ~8.8 km gap
	seedSource(t, s, "firis")
	seedProximityPlaces(t, s)

	_, err := s.UpsertEvent(ctx, nearbyFire("firis:nearby"))
	require.NoError(t, err)

	got, err := s.GetEvent(ctx, "firis:nearby")
	require.NoError(t, err)
	assert.Empty(t, got.GetPlaceIds())
}

// The buffer is wildfire-only. An identically-placed event on another layer
// keeps the strict overlap rules — a quake 9 km outside the area is not "in" it.
func TestWildfireProximityDoesNotApplyToOtherLayers(t *testing.T) {
	ctx := context.Background()
	s := newProximityStore(t, 20000)
	seedSource(t, s, "usgs")
	seedProximityPlaces(t, s)

	ev := nearbyFire("usgs:quake")
	ev.Layer = gridv1.Layer_EARTHQUAKE
	ev.Provenance.SourceId = "usgs"
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	got, err := s.GetEvent(ctx, "usgs:quake")
	require.NoError(t, err)
	assert.Empty(t, got.GetPlaceIds())
}

// Unset buffer (the store default) = today's behaviour exactly.
func TestWildfireProximityDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "firis")
	seedProximityPlaces(t, s)

	_, err := s.UpsertEvent(ctx, nearbyFire("firis:nearby"))
	require.NoError(t, err)

	got, err := s.GetEvent(ctx, "firis:nearby")
	require.NoError(t, err)
	assert.Empty(t, got.GetPlaceIds())
}

// A point-geometry fire (no FIRIS perimeter adopted) still gets the buffer —
// matchPlaces must synthesize a point geom rather than skipping the rule.
func TestWildfireProximityAppliesToPointFires(t *testing.T) {
	ctx := context.Background()
	s := newProximityStore(t, 20000)
	seedSource(t, s, "calfire")
	seedProximityPlaces(t, s)

	ev := testEvent("calfire:point-fire", gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE, "Point Fire")
	ev.Layer = gridv1.Layer_WILDFIRE
	ev.Provenance.SourceId = "calfire"
	ev.Geometry = pointGeometry(38.15, -120.15) // ~13 km east of the area edge
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	got, err := s.GetEvent(ctx, "calfire:point-fire")
	require.NoError(t, err)
	assert.Contains(t, got.GetPlaceIds(), "area:ebbetts")
}

// A fire that genuinely overlaps must still attach by the exact rules,
// including to counties the buffer deliberately excludes.
func TestWildfireProximityKeepsExactOverlapRules(t *testing.T) {
	ctx := context.Background()
	s := newProximityStore(t, 20000)
	seedSource(t, s, "firis")
	seedProximityPlaces(t, s)

	ev := nearbyFire("firis:overlapping")
	ev.Geometry = polyGeometry(38.1, -120.5, 38.2, -120.4) // inside the area + county
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)

	got, err := s.GetEvent(ctx, "firis:overlapping")
	require.NoError(t, err)
	assert.Contains(t, got.GetPlaceIds(), "county:calaveras", "exact overlap still attaches a county")
	assert.Contains(t, got.GetPlaceIds(), "area:ebbetts")
}

// TestCorridorAttachSurvivesBboxPrefilter pins the boundary case that the
// point-event bbox prefilter in eventGeo.matches could silently break.
//
// The prefilter exists because ~93% of events are mesh nodes that sit outside
// every place, and without it each ingest tick ran the exact geometry test —
// haversine per corridor segment — for all of them. But a corridor is a
// zero-WIDTH LineString: a point can be legitimately attached (within
// corridorBufferMeters of the line) while lying OUTSIDE the line's raw bbox.
// A prefilter that used the raw bbox would drop exactly those, and the loss
// would be invisible — the event simply stops appearing for that corridor.
// So the prefilter is widened by corridorBufferDeg, and this test is what
// proves the widening is real rather than assumed.
func TestCorridorAttachSurvivesBboxPrefilter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	// LineString from (38.05,-120.5) to (38.25,-120.4): raw bbox starts at
	// lat 38.05.
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"corridor:hwy4", "hwy4", "Hwy 4",
		gridv1.PlaceKind_CORRIDOR,
		geomFromGeoJSON(`{"type":"LineString","coordinates":[[-120.5,38.05],[-120.4,38.25]]}`))))

	// 0.01deg (~1.11 km) south of the line's southern endpoint: OUTSIDE the raw
	// bbox, INSIDE the 1500 m corridor buffer. Must still attach.
	justOutside := testEvent("usgs:near", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "near the road")
	justOutside.Geometry = pointGeometry(38.04, -120.5)
	_, err := s.UpsertEvent(ctx, justOutside)
	require.NoError(t, err)

	got, err := s.GetEvent(ctx, "usgs:near")
	require.NoError(t, err)
	assert.Contains(t, got.GetPlaceIds(), "corridor:hwy4",
		"a point outside the LineString's raw bbox but within corridorBufferMeters "+
			"must still attach — the prefilter has to be widened by corridorBufferDeg")

	// Control: far enough out that it genuinely does not belong.
	farAway := testEvent("usgs:far", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "not near the road")
	farAway.Geometry = pointGeometry(37.80, -120.5) // ~28 km south
	_, err = s.UpsertEvent(ctx, farAway)
	require.NoError(t, err)

	got, err = s.GetEvent(ctx, "usgs:far")
	require.NoError(t, err)
	assert.NotContains(t, got.GetPlaceIds(), "corridor:hwy4",
		"the prefilter must still reject points that are genuinely far away")
}
