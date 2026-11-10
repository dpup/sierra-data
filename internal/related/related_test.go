package related

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/lib/geojson"
	"github.com/sierragridteam/sierra-data/internal/store"
)

var t0 = time.Date(2026, 8, 20, 20, 0, 0, 0, time.UTC)

func ts(h float64) *timestamppb.Timestamp {
	return timestamppb.New(t0.Add(time.Duration(h * float64(time.Hour))))
}

// box is a small square polygon around (lat, lng), r degrees across.
func box(lat, lng, r float64) *gridv1.Geometry {
	return &gridv1.Geometry{Geojson: geojson.BboxPolygonGeoJSON(lat-r, lng-r, lat+r, lng+r)}
}

func point(lat, lng float64) *gridv1.Geometry {
	return &gridv1.Geometry{Geojson: geojson.PointGeoJSON(lat, lng)}
}

func ev(id string, layer gridv1.Layer, cat, headline string, start float64, g *gridv1.Geometry) *gridv1.Event {
	prefix, _, _ := cut(id)
	src := map[string]string{"wx": "nws", "evac": "caloes"}[prefix]
	if src == "" {
		src = prefix
	}
	return &gridv1.Event{Id: id, Layer: layer, Category: cat, Headline: headline, Status: gridv1.EventStatus_ACTIVE,
		Severity: gridv1.Severity_MODERATE, Effective: ts(start), ObservedAt: ts(start), Geometry: g,
		Provenance: &gridv1.Provenance{SourceId: src, SourceName: src}}
}

func cut(id string) (string, string, bool) {
	for i := range id {
		if id[i] == ':' {
			return id[:i], id[i+1:], true
		}
	}
	return id, "", false
}

func newEngine(t *testing.T, events ...*gridv1.Event) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, src := range []string{"calfire", "caloes", "pge", "psps", "nws", "chp"} {
		require.NoError(t, st.SeedSources(ctx, []store.SourceSeed{{ID: src, Name: src, PollInterval: time.Minute}}))
	}
	for _, e := range events {
		_, err := st.UpsertEvent(ctx, e)
		require.NoError(t, err)
	}
	e := New(st)
	e.Now = func() time.Time { return t0.Add(10 * 24 * time.Hour) }
	return e, st
}

func ids(rs []Relation) map[string]string {
	out := map[string]string{}
	for _, r := range rs {
		out[r.Event.GetId()] = r.Rule
	}
	return out
}

func TestFiresAndEvacuations(t *testing.T) {
	ross := ev("calfire:ross", gridv1.Layer_WILDFIRE, "wildfire", "Ross Fire — 20 ac, 100% contained", 0, box(38.11, -120.50, 0.01))
	loma := ev("calfire:loma", gridv1.Layer_WILDFIRE, "wildfire", "Loma Fire — 19 ac", -24, box(38.12, -120.32, 0.01))
	named := ev("evac:named", gridv1.Layer_EVACUATION, "warning", "Evacuation Warning — CCU-133", 2, box(38.30, -120.50, 0.01))
	named.Description = "Ross Fire\n\nFire is approximately 19.9 acres near Canyon Ct"
	nearRoss := ev("evac:near-ross", gridv1.Layer_EVACUATION, "order", "Evacuation Order — CCU-1", 1, box(38.13, -120.50, 0.005))
	between := ev("evac:between", gridv1.Layer_EVACUATION, "order", "Evacuation Order — CCU-2", 1, box(38.115, -120.38, 0.005))
	late := ev("evac:late", gridv1.Layer_EVACUATION, "order", "Evacuation Order — CCU-3", 100, box(38.12, -120.50, 0.005))
	e, _ := newEngine(t, ross, loma, named, nearRoss, between, late)
	ctx := context.Background()

	rs, err := e.Related(ctx, ross)
	require.NoError(t, err)
	got := ids(rs)
	assert.Equal(t, "fire-evacuation-named", got["evac:named"], "named in the notes, 21 km away")
	assert.Equal(t, "fire-evacuation-near", got["evac:near-ross"])
	assert.NotContains(t, got, "evac:between", "nearer to Loma: belongs to Loma only")
	assert.NotContains(t, got, "evac:late", "issued four days after the fire started")
	assert.Equal(t, High, rs[0].Confidence, "strongest first")
	assert.Equal(t, "Cal OES notes name the Ross Fire", rs[0].Reason)

	rs, err = e.Related(ctx, between)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"calfire:loma": "fire-evacuation-near"}, ids(rs), "the zone points back to its nearest fire")

	rs, err = e.Related(ctx, named)
	require.NoError(t, err)
	assert.Equal(t, "fire-evacuation-named", ids(rs)["calfire:ross"])
}

func TestFireOutagesAndShutoffs(t *testing.T) {
	fire := ev("calfire:loma", gridv1.Layer_WILDFIRE, "wildfire", "Loma Fire — 19 ac", 0, box(38.12, -120.32, 0.01))
	byFire := ev("pge:1", gridv1.Layer_POWER, "unplanned", "Power outage — 125 customers (fire)", 2, point(38.14, -120.33))
	byFire.Detail = &gridv1.Event_Power{Power: &gridv1.PowerDetail{Cause: "FIRE"}}
	other := ev("pge:2", gridv1.Layer_POWER, "unplanned", "Power outage — 1 customer", 2, point(38.13, -120.33))
	other.Detail = &gridv1.Event_Power{Power: &gridv1.PowerDetail{Cause: "BRKN UG EQUIPMNT"}}
	shutoff := ev("psps:e1:1", gridv1.Layer_POWER, "psps", "PSPS", 5, box(38.2, -120.3, 0.05))
	e, _ := newEngine(t, fire, byFire, other, shutoff)

	rs, err := e.Related(context.Background(), fire)
	require.NoError(t, err)
	got := ids(rs)
	assert.Equal(t, "fire-outage-cause", got["pge:1"])
	assert.NotContains(t, got, "pge:2", "an equipment fault near a fire is not related")
	assert.Equal(t, "fire-shutoff-near", got["psps:e1:1"])
}

// A shutoff or outage inside a wind or winter storm product's area while it
// is in effect; nothing outside its area, nothing for unrelated products.
func TestWeatherAlertRelations(t *testing.T) {
	wind := ev("wx:wind", gridv1.Layer_WEATHER_ALERT, "High Wind Warning", "High Wind Warning", 0, box(38.4, -120.2, 0.2))
	wind.Expires = ts(24)
	snow := ev("wx:snow", gridv1.Layer_WEATHER_ALERT, "Winter Storm Warning", "Winter Storm Warning", 0, box(38.45, -120.05, 0.1))
	snow.Expires = ts(36)
	heat := ev("wx:heat", gridv1.Layer_WEATHER_ALERT, "Heat Advisory", "Heat Advisory", 0, box(38.4, -120.2, 0.5))
	heat.Expires = ts(24)
	shutoff := ev("psps:e2:1", gridv1.Layer_POWER, "psps", "PSPS", 6, point(38.35, -120.25))
	outage := ev("pge:3", gridv1.Layer_POWER, "unplanned", "Power outage — 40 customers", 8, point(38.45, -120.05))
	outside := ev("pge:4", gridv1.Layer_POWER, "unplanned", "Power outage — 2 customers", 8, point(37.5, -121.0))
	closure := ev("chp:snow1", gridv1.Layer_ROAD_INCIDENT, "incident", "Spun out vehicle blocking lane", 10, point(38.46, -120.04))
	e, _ := newEngine(t, wind, snow, heat, shutoff, outage, outside, closure)
	ctx := context.Background()

	rs, err := e.Related(ctx, wind)
	require.NoError(t, err)
	got := ids(rs)
	assert.Equal(t, "weather-shutoff", got["psps:e2:1"])
	assert.Equal(t, "weather-outage", got["pge:3"])
	assert.NotContains(t, got, "pge:4")

	rs, err = e.Related(ctx, closure)
	require.NoError(t, err)
	got = ids(rs)
	assert.Equal(t, "weather-road", got["wx:snow"])
	assert.NotContains(t, got, "wx:heat", "a heat product says nothing about a spin-out")

	rs, err = e.Related(ctx, shutoff)
	require.NoError(t, err)
	assert.Equal(t, "weather-shutoff", ids(rs)["wx:wind"])
	assert.Equal(t, Low, rs[0].Confidence)
	assert.Equal(t, "PG&E shutoff during the High Wind Warning, inside its area", rs[0].Reason)
}

func TestFireName(t *testing.T) {
	for in, want := range map[string]string{
		"Gann Fire — 10,339 ac, 100% contained": "Gann",
		"GANN — 6495 ac":                       "Gann",
		"Oneil Fire — 3 ac, 0% contained":       "Oneil",
	} {
		assert.Equal(t, want, fireName(&gridv1.Event{Headline: in}))
	}
	assert.True(t, namesFire("EMERGENCY: Ross Fire near Canyon Ct", "Ross"))
	assert.False(t, namesFire("Evacuate Ross Rd residents", "Ross"), "the word Fire is required")
}

// A Cal OES zone states no start. Ended days later, its last update is when
// it ended; its start is when the Grid first stored it, which is what places
// it beside the fire. (Built on the real clock: the store stamps ingest time.)
func TestZoneWithoutEffectiveStartsWhenFirstStored(t *testing.T) {
	now := time.Now().UTC()
	gann := ev("calfire:gann", gridv1.Layer_WILDFIRE, "wildfire", "Gann Fire — 10,339 ac", 0, box(38.10, -120.70, 0.03))
	gann.Effective, gann.ObservedAt = timestamppb.New(now.Add(-2*time.Hour)), timestamppb.New(now.Add(-2*time.Hour))
	zone := ev("evac:ccu-139", gridv1.Layer_EVACUATION, "order", "Evacuation Order — CALAVERAS", 0, box(38.10, -120.66, 0.01))
	zone.Effective, zone.ObservedAt = nil, timestamppb.New(now)
	e, st := newEngine(t, gann, zone)
	e.Now = func() time.Time { return now.Add(10 * 24 * time.Hour) }
	ctx := context.Background()
	// Six days on, the zone is resolved: its last update is the end.
	require.NoError(t, st.TransitionEvents(ctx, []string{"evac:ccu-139"}, gridv1.EventStatus_RESOLVED, now.Add(6*24*time.Hour)))

	rs, err := e.Related(ctx, gann)
	require.NoError(t, err)
	assert.Contains(t, ids(rs), "evac:ccu-139")
}
