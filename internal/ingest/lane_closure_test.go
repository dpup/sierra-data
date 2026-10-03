package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/store"
)

// lcsFixtureNow is just after the D10 capture's record stamp (the same instant
// the cwwp2 client tests pin), so the fixture's phases are the live ones.
var lcsFixtureNow = time.Date(2026, 9, 30, 4, 0, 0, 0, time.UTC)

// fakeLaneClosures serves canned rows per district.
type fakeLaneClosures struct {
	rows  map[int][]cwwp2.LaneClosure
	errs  map[int]error
	calls []int
}

func (f *fakeLaneClosures) LaneClosures(ctx context.Context, district int) ([]cwwp2.LaneClosure, error) {
	f.calls = append(f.calls, district)
	if err := f.errs[district]; err != nil {
		return nil, err
	}
	return f.rows[district], nil
}

// d10Fixture is the committed 2026-09-30 District 10 capture (84 rows, our
// counties), as cwwp2.Client.LaneClosures would return it.
func d10Fixture(t *testing.T) []cwwp2.LaneClosure {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "testdata", "cwwp2", "lcs_d10_20260930.json"))
	require.NoError(t, err)
	rows, err := cwwp2.ParseLaneClosures(b)
	require.NoError(t, err)
	for i := range rows {
		rows[i].District = 10
	}
	return rows
}

func laneClosureConfig(districts ...int) *config.Config {
	cfg := testConfig()
	cfg.Roads.CaltransFeeds.CWWP2.LaneClosureDistricts = districts
	return cfg
}

func newTestLaneClosureNormalizer(cfg *config.Config, client laneClosureAPI, now time.Time) *LaneClosureNormalizer {
	n := NewLaneClosureNormalizer(cfg, client)
	n.now = func() time.Time { return now }
	return n
}

func findEvent(events []*gridv1.Event, id string) *gridv1.Event {
	for _, ev := range events {
		if ev.GetId() == id {
			return ev
		}
	}
	return nil
}

// motherLode is prefab.yaml's production incident box.
var motherLode = config.IncidentArea{ID: "mother-lode", Bounds: config.GeoBounds{
	MinLatitude: 37.62, MaxLatitude: 38.84, MinLongitude: -120.97, MaxLongitude: -119.64}}

func TestLaneClosurePoll_Live(t *testing.T) {
	client := &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: d10Fixture(t)}}
	cfg := laneClosureConfig(10)
	cfg.Roads.IncidentAreas = []config.IncidentArea{motherLode}
	n := newTestLaneClosureNormalizer(cfg, client, lcsFixtureNow)
	assert.Equal(t, []string{"caltrans"}, n.SourceIDs())

	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Nil(t, res.PerSource, "a clean fetch with every row readable is a success")
	assert.Equal(t, []int{10}, client.calls)
	require.NotEmpty(t, res.Events)

	seen := map[string]bool{}
	for _, ev := range res.Events {
		assert.False(t, seen[ev.GetId()], "duplicate id %s", ev.GetId())
		seen[ev.GetId()] = true
		assert.Contains(t, []gridv1.EventStatus{gridv1.EventStatus_SCHEDULED, gridv1.EventStatus_ACTIVE}, ev.GetStatus(), ev.GetId())
		assert.Equal(t, gridv1.Layer_ROAD_INCIDENT, ev.GetLayer())
		assert.Equal(t, "closure", ev.GetCategory())
		assert.Equal(t, "caltrans", ev.GetProvenance().GetSourceId())
		assert.Equal(t, "cwwp2.dot.ca.gov", ev.GetProvenance().GetAttribution())
		assert.Nil(t, ev.GetEnhancement(), "closures are composed, never AI-enhanced")
		assert.Nil(t, ev.GetExpires(), "a planned end is an estimate, not an expiry")
		assert.True(t, strings.HasPrefix(ev.GetId(), "caltrans:d10-"), ev.GetId())
		assert.NotRegexp(t, legacyClosureIDRe, ev.GetId(), "a new id must never look like a KML-era one")
	}
	// The capture holds 57 scheduled + 3 active windows (the cwwp2 phase test);
	// three scheduled Alpine windows lie east of the box's -119.64 edge.
	assert.Len(t, res.Events, 57)
	statuses := map[gridv1.EventStatus]int{}
	for _, ev := range res.Events {
		statuses[ev.GetStatus()]++
	}
	assert.Equal(t, map[gridv1.EventStatus]int{gridv1.EventStatus_SCHEDULED: 54, gridv1.EventStatus_ACTIVE: 3}, statuses)

	// A scheduled window: the shoulder of SR-4 at Avery, two days out.
	sched := findEvent(res.Events, "caltrans:d10-C4QB-0004-2026-10-02-070100")
	require.NotNil(t, sched)
	assert.Equal(t, gridv1.EventStatus_SCHEDULED, sched.GetStatus())
	assert.Equal(t, gridv1.Severity_MINOR, sched.GetSeverity(), "shoulder only")
	assert.Equal(t, "Hwy 4 shoulder closure (Drainage Work)", sched.GetHeadline())
	assert.Equal(t, "Hwy 4 at Avery", sched.GetAreaLabel(), "the nearby place is Avery itself")
	assert.Equal(t, "Closed: right shoulder (of 2 lanes).", sched.GetDescription())
	assert.Equal(t, time.Date(2026, 10, 2, 14, 1, 0, 0, time.UTC), sched.GetEffective().AsTime())
	d := sched.GetRoadIncident().GetClosure()
	require.NotNil(t, d)
	assert.Equal(t, "C4QB-0004-2026-10-02-07:01:00", d.GetWindowId())
	assert.Equal(t, int32(10), d.GetDistrict())
	assert.Equal(t, "C4QB", d.GetClosureId())
	assert.Equal(t, "4", d.GetLogNumber())
	assert.Equal(t, "C4QB", sched.GetRoadIncident().GetLogNumber(), "logNumber keeps carrying the closure id")
	assert.Equal(t, "SR-4", d.GetRoute())
	assert.Equal(t, time.Date(2026, 10, 2, 21, 59, 0, 0, time.UTC), d.GetPlannedEnd().AsTime())
	assert.Nil(t, d.GetSetUpAt())
	assert.Equal(t, "Avery", d.GetBeginLocation())

	// An active window: the long-term full closure of SR-26 at Mokelumne Hill.
	active := findEvent(res.Events, "caltrans:d10-C26EA-0001-2026-08-24-070100")
	require.NotNil(t, active)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, active.GetStatus())
	assert.Equal(t, gridv1.Severity_SEVERE, active.GetSeverity())
	assert.True(t, strings.HasPrefix(active.GetHeadline(), "Hwy 26 full closure"), active.GetHeadline())
	// effective is the 10-97 set-up call, not the planned start.
	assert.Equal(t, time.Date(2026, 8, 24, 14, 58, 1, 0, time.UTC), active.GetEffective().AsTime())
	assert.Equal(t, active.GetEffective().AsTime(), active.GetRoadIncident().GetClosure().GetSetUpAt().AsTime())

	// An overrun: set up, window past, not picked up. Still in place.
	overrun := findEvent(res.Events, "caltrans:d10-P88AA-0004-2026-09-29-080100")
	require.NotNil(t, overrun)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, overrun.GetStatus())

	// Over, picked up, or cancelled: not emitted, so the sweep resolves them.
	for _, gone := range []string{
		"caltrans:d10-C4JA-0002-2026-09-29-080100",  // window over
		"caltrans:d10-C88UA-0001-2026-08-31-080100", // 10-98
		"caltrans:d10-C4JA-0003-2026-09-30-080100",  // 10-22
	} {
		assert.Nil(t, findEvent(res.Events, gone), gone)
	}
}

// Every emitted event must sit in an incident area, judged by either endpoint.
func TestLaneClosurePoll_ScopesToIncidentAreas(t *testing.T) {
	rows := d10Fixture(t)
	cfg := laneClosureConfig(10)
	res, err := newTestLaneClosureNormalizer(cfg, &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: rows}}, lcsFixtureNow).
		Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)

	byWindow := map[string]cwwp2.LaneClosure{}
	for _, lc := range rows {
		byWindow[lc.ID] = lc
	}
	outside := 0
	for _, lc := range rows {
		if in, _ := inIncidentAreas(lc, cfg.Roads.IncidentAreas); !in {
			outside++
		}
	}
	require.Positive(t, outside, "the fixture should exercise the scope test")
	for _, ev := range res.Events {
		lc := byWindow[ev.GetRoadIncident().GetClosure().GetWindowId()]
		in, _ := inIncidentAreas(lc, cfg.Roads.IncidentAreas)
		assert.True(t, in, ev.GetId())
	}
}

func TestLaneClosurePoll_EmptyScopeIsHardError(t *testing.T) {
	client := &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: d10Fixture(t)}}

	cfg := laneClosureConfig(10)
	cfg.Roads.IncidentAreas = nil
	_, err := newTestLaneClosureNormalizer(cfg, client, lcsFixtureNow).Poll(testCtx(), &fakePrior{})
	assert.Error(t, err)

	_, err = newTestLaneClosureNormalizer(laneClosureConfig(), client, lcsFixtureNow).Poll(testCtx(), &fakePrior{})
	assert.Error(t, err, "no districts is an empty scope, never a successful empty poll")
	assert.Empty(t, client.calls)
}

// One district down (HTTP error, stale or empty file) degrades the source so
// the sweep skips it; every district down is a hard error.
func TestLaneClosurePoll_DistrictFailures(t *testing.T) {
	client := &fakeLaneClosures{
		rows: map[int][]cwwp2.LaneClosure{10: d10Fixture(t)},
		errs: map[int]error{3: cwwp2.ErrStaleFeed},
	}
	res, err := newTestLaneClosureNormalizer(laneClosureConfig(3, 10), client, lcsFixtureNow).Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.ErrorIs(t, res.PerSource["caltrans"], cwwp2.ErrStaleFeed)
	assert.NotEmpty(t, res.Events, "the healthy district's closures still land")

	client.errs[10] = cwwp2.ErrEmptyFeed
	_, err = newTestLaneClosureNormalizer(laneClosureConfig(3, 10), client, lcsFixtureNow).Poll(testCtx(), &fakePrior{})
	assert.ErrorIs(t, err, cwwp2.ErrEmptyFeed)
	assert.ErrorIs(t, err, cwwp2.ErrStaleFeed)
}

// A row the client could not read degrades the source when it might be ours:
// in scope, or with no position at all. Out of scope it is not our problem.
func TestLaneClosurePoll_UnrecognizedRows(t *testing.T) {
	poll := func(mutate func(*cwwp2.LaneClosure)) *PollResult {
		t.Helper()
		rows := d10Fixture(t)
		for i := range rows {
			if rows[i].ID == "C26EA-0001-2026-08-24-07:01:00" {
				mutate(&rows[i])
			}
		}
		res, err := newTestLaneClosureNormalizer(laneClosureConfig(10), &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: rows}}, lcsFixtureNow).
			Poll(testCtx(), &fakePrior{})
		require.NoError(t, err)
		return res
	}

	res := poll(func(lc *cwwp2.LaneClosure) { lc.Unrecognized = "unreadable 10-97 flag" })
	require.Error(t, res.PerSource["caltrans"])
	assert.Contains(t, res.PerSource["caltrans"].Error(), "C26EA-0001")
	assert.Nil(t, findEvent(res.Events, "caltrans:d10-C26EA-0001-2026-08-24-070100"), "its phase is unknown: not guessed")
	assert.NotEmpty(t, res.Events, "every readable row still lands")

	res = poll(func(lc *cwwp2.LaneClosure) {
		lc.Unrecognized = "no position"
		lc.Begin, lc.End = cwwp2.Location{}, cwwp2.Location{}
	})
	assert.Error(t, res.PerSource["caltrans"], "a row we cannot place might be ours")

	res = poll(func(lc *cwwp2.LaneClosure) {
		lc.Unrecognized = "unreadable 10-22 flag"
		lc.Begin = cwwp2.Location{Latitude: 37.95, Longitude: -121.3, HasPosition: true} // Stockton
		lc.End = lc.Begin
	})
	assert.Nil(t, res.PerSource, "an unreadable row outside every area is not our problem")
}

// The same window must keep one id through its whole life — the id is the
// lifecycle handle under `resolve`.
func TestLaneClosureEventID_StableAcrossPhases(t *testing.T) {
	start := time.Date(2026, 10, 2, 14, 1, 0, 0, time.UTC)
	lc := cwwp2.LaneClosure{
		ID: "C4QB-0004-2026-10-02-07:01:00", District: 10, ClosureID: "C4QB", LogNumber: "4",
		Start: start, EndTime: start.Add(8 * time.Hour), ClosureType: "Lane", LanesClosed: "1", TotalLanes: 2,
		Begin: cwwp2.Location{Route: "SR-4", Name: "Avery", Latitude: 38.205083, Longitude: -120.369406, HasPosition: true},
		End:   cwwp2.Location{Route: "SR-4", Name: "Avery", Latitude: 38.205083, Longitude: -120.369406, HasPosition: true},
	}
	client := &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: {lc}}}
	n := newTestLaneClosureNormalizer(laneClosureConfig(10), client, start.Add(-time.Hour))

	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.Equal(t, gridv1.EventStatus_SCHEDULED, res.Events[0].GetStatus())
	id := res.Events[0].GetId()
	assert.Equal(t, "caltrans:d10-C4QB-0004-2026-10-02-070100", id)

	// Window open, crew late: still SCHEDULED.
	n.now = func() time.Time { return start.Add(time.Hour) }
	res, err = n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_SCHEDULED, res.Events[0].GetStatus())

	// 10-97: ACTIVE under the same id, effective moves to the set-up call.
	setUp := start.Add(90 * time.Minute)
	client.rows[10][0].SetUp, client.rows[10][0].SetUpAt = true, setUp
	n.now = func() time.Time { return start.Add(2 * time.Hour) }
	res, err = n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Equal(t, id, res.Events[0].GetId())
	assert.Equal(t, gridv1.EventStatus_ACTIVE, res.Events[0].GetStatus())
	assert.Equal(t, setUp, res.Events[0].GetEffective().AsTime())

	// 10-98: gone from the poll.
	client.rows[10][0].PickedUp = true
	res, err = n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Empty(t, res.Events)
	assert.Nil(t, res.PerSource)
}

func TestLaneClosureText(t *testing.T) {
	loc := func(route, name string) cwwp2.Location {
		return cwwp2.Location{Route: route, Name: name, Latitude: 38.5, Longitude: -120.2, HasPosition: true}
	}
	cases := []struct {
		name                 string
		lc                   cwwp2.LaneClosure
		headline, area, desc string
	}{
		{
			name: "one-way traffic at one spot, both directions",
			lc: cwwp2.LaneClosure{FlowDirection: "East / West", Facility: "Conventional Hwy", ClosureType: "One-Way Traffic",
				WorkType: "ITS/Various Electrical Works", LanesClosed: "1, RShoulder", TotalLanes: 2,
				Begin: withNearby(loc("SR-88", "Schneidr Road (Left)"), "Markleeville"), End: loc("SR-88", "Schneidr Road (Left)")},
			headline: "Hwy 88 one-way traffic control (ITS/Various Electrical Works)",
			area:     "Hwy 88 at Schneidr Road (Left), near Markleeville",
			desc:     "Closed: lane 1, right shoulder (of 2 lanes).",
		},
		{
			name: "directional alternating closure between two points, US route, delay",
			lc: cwwp2.LaneClosure{FlowDirection: "West", ClosureType: "Alternating Lanes", WorkType: "Drainage Work",
				LanesClosed: "Median, LShoulder, 1, 2, Auxiliary, RShoulder", TotalLanes: 2, EstimatedDelay: 10,
				Begin: withNearby(loc("US-50", "Sly Park Rd"), "Pollock Pines"), End: loc("US-50", "Bedford Ave")},
			headline: "US 50 westbound alternating lane closure (Drainage Work)",
			area:     "US 50 from Sly Park Rd to Bedford Ave, near Pollock Pines",
			desc:     "Closed: median, left shoulder, lane 1, lane 2, auxiliary lane, right shoulder (of 2 lanes). Estimated delay: 10 min.",
		},
		{
			name: "off-ramp, names only in the free-form description",
			lc: cwwp2.LaneClosure{FlowDirection: "West", Facility: "Off Ramp", ClosureType: "Full", WorkType: "Utility Work",
				LanesClosed: "All", TotalLanes: 1,
				Begin: cwwp2.Location{Route: "US-50", FreeFormDesc: "Camino / Cedar Grove", Latitude: 38.7, Longitude: -120.6, HasPosition: true},
				End:   cwwp2.Location{Route: "US-50"}},
			headline: "US 50 westbound off-ramp full closure (Utility Work)",
			area:     "US 50 at Camino / Cedar Grove",
			desc:     "Closed: all lanes (of 1 lane).",
		},
		{
			name: "hand-typed spacing, and connectives the free-form text brings",
			lc: cwwp2.LaneClosure{FlowDirection: "West", Facility: "Off Ramp", ClosureType: "Lane", WorkType: "Utility Work",
				LanesClosed: "RShoulder", TotalLanes: 2,
				Begin: cwwp2.Location{Route: "US-50", FreeFormDesc: "from  17.78", NearbyPlace: "Placerville", Latitude: 38.7, Longitude: -120.8, HasPosition: true},
				End:   cwwp2.Location{Route: "US-50", FreeFormDesc: "to 17.78"}},
			headline: "US 50 westbound off-ramp shoulder closure (Utility Work)",
			area:     "US 50 at 17.78, near Placerville",
			desc:     "Closed: right shoulder (of 2 lanes).",
		},
		{
			name: "parenthesis spacing",
			lc: cwwp2.LaneClosure{FlowDirection: "West", ClosureType: "Lane", LanesClosed: "RShoulder",
				Begin: loc("SR-88", "Lake Kirkwood ( Left)"), End: loc("SR-88", "Main Street ( Mokelumne Hill )")},
			headline: "Hwy 88 westbound shoulder closure",
			area:     "Hwy 88 from Lake Kirkwood (Left) to Main Street (Mokelumne Hill)",
			desc:     "Closed: right shoulder.",
		},
		{
			name:     "nothing but a type",
			lc:       cwwp2.LaneClosure{ClosureType: "Traffic Break"},
			headline: "Traffic break",
			area:     "",
			desc:     "",
		},
	}
	for _, c := range cases {
		assert.Equal(t, c.headline, laneClosureHeadline(c.lc), c.name)
		assert.Equal(t, c.area, laneClosureAreaLabel(c.lc), c.name)
		assert.Equal(t, c.desc, laneClosureDescription(c.lc), c.name)
	}
}

func withNearby(l cwwp2.Location, near string) cwwp2.Location {
	l.NearbyPlace = near
	return l
}

// legacyClosureEvent is a closure as the lcs2way.kml path stored it.
func legacyClosureEvent(id string, lat, lng float64) *gridv1.Event {
	ev := NewEvent(id, gridv1.Layer_ROAD_INCIDENT, gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE,
		"Full closure of both directions for roadway excavation; detours in place.")
	ev.Category = "closure"
	ev.Geometry = GeometryFromPoint(lat, lng)
	ev.Provenance = NewProvenance("caltrans", "Caltrans", "quickmap.dot.ca.gov", "")
	return ev
}

// The switch of feed must not resolve closures that are physically in place.
// Real ids and coordinates from the live store and feed on 2026-10-01.
func TestLaneClosurePoll_AdoptsLegacyIDs(t *testing.T) {
	client := &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: d10Fixture(t)}}
	n := newTestLaneClosureNormalizer(laneClosureConfig(10), client, lcsFixtureNow)

	prior := &fakePrior{events: []*gridv1.Event{
		// SR-26 full closure: one ACTIVE window, at the KML point.
		legacyClosureEvent("caltrans:C26EA-1-bfa4ac", 38.296771, -120.707192),
		// Same closure id + log, but 5 km away: not this closure.
		legacyClosureEvent("caltrans:C49GA-1-42a84b", 38.34, -120.70),
		// C4QB log 4 has only SCHEDULED windows here: lcs2way.kml never listed
		// it as set up, so nothing to adopt.
		legacyClosureEvent("caltrans:C4QB-4-54fdd7", 38.205083, -120.369406),
		// Not a legacy id at all (a current CWWP2 event): left alone.
		legacyClosureEvent("caltrans:d10-P88AA-0004-2026-09-29-080100", 38.4, -120.6),
	}}
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)

	adopted := findEvent(res.Events, "caltrans:C26EA-1-bfa4ac")
	require.NotNil(t, adopted, "the active window takes the KML-era id")
	assert.Equal(t, "C26EA-0001-2026-08-24-07:01:00", adopted.GetRoadIncident().GetClosure().GetWindowId())
	assert.Nil(t, findEvent(res.Events, "caltrans:d10-C26EA-0001-2026-08-24-070100"), "and is not emitted twice")

	assert.Nil(t, findEvent(res.Events, "caltrans:C49GA-1-42a84b"), "too far away to be the same closure")
	assert.Nil(t, findEvent(res.Events, "caltrans:C4QB-4-54fdd7"), "a SCHEDULED window is never adopted")
	assert.NotNil(t, findEvent(res.Events, "caltrans:d10-C4QB-0004-2026-10-02-070100"))

	// Next tick the store holds the adopted event (geometry now the CWWP2 begin
	// point): the same match recurs, so the id is stable.
	res2, err := n.Poll(testCtx(), &fakePrior{events: []*gridv1.Event{adopted}})
	require.NoError(t, err)
	assert.NotNil(t, findEvent(res2.Events, "caltrans:C26EA-1-bfa4ac"))
}

// Two legacy events that both match one window (or one legacy event that
// matches two) adopt nothing: guessing would put one closure's history on
// another.
func TestLaneClosurePoll_AmbiguousLegacyAdoptsNothing(t *testing.T) {
	start := lcsFixtureNow.Add(-time.Hour)
	at := cwwp2.Location{Route: "SR-4", Name: "Avery", Latitude: 38.205083, Longitude: -120.369406, HasPosition: true}
	window := func(id string) cwwp2.LaneClosure {
		return cwwp2.LaneClosure{ID: id, District: 10, ClosureID: "C4QB", LogNumber: "8", Start: start,
			EndTime: start.Add(8 * time.Hour), SetUp: true, SetUpAt: start, ClosureType: "Lane", LanesClosed: "RShoulder",
			Begin: at, End: at}
	}
	client := &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: {
		window("C4QB-0008-2026-09-29-20:00:00"),
		window("C4QB-0008-2026-09-29-21:00:00"),
	}}}
	n := newTestLaneClosureNormalizer(laneClosureConfig(10), client, lcsFixtureNow)
	res, err := n.Poll(testCtx(), &fakePrior{events: []*gridv1.Event{legacyClosureEvent("caltrans:C4QB-8-54fdd7", at.Latitude, at.Longitude)}})
	require.NoError(t, err)
	assert.Nil(t, findEvent(res.Events, "caltrans:C4QB-8-54fdd7"))
	assert.Len(t, res.Events, 2)
}

func TestSameLogNumber(t *testing.T) {
	assert.True(t, sameLogNumber("4", "0004"))
	assert.True(t, sameLogNumber("19", "19"))
	assert.True(t, sameLogNumber("0", "0000"))
	assert.False(t, sameLogNumber("4", "40"))
}

// End to end through the scheduler and a real store: on the first tick after
// deploy, the KML-era closure keeps its id, its history and its ACTIVE status
// (one revision for the new content, no RESOLVED), and it resolves only when
// CWWP2 says the crew picked it up.
func TestTickLegacyClosureSurvivesFeedSwitch(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "caltrans")
	_, err := st.UpsertEvent(ctx, legacyClosureEvent("caltrans:C26EA-1-bfa4ac", 38.296771, -120.707192))
	require.NoError(t, err)

	rows := d10Fixture(t)
	client := &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: rows}}
	n := newTestLaneClosureNormalizer(laneClosureConfig(10), client, lcsFixtureNow)
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"caltrans": {Disappearance: store.DisappearanceResolve}},
	})
	ps := &pollerState{}
	spec := PollerSpec{Normalizer: n, Interval: time.Minute}

	sched.tick(ctx, spec, ps)
	ev, err := st.GetEvent(ctx, "caltrans:C26EA-1-bfa4ac")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.GetStatus(), "never resolved by the feed switch")
	assert.Equal(t, uint32(2), ev.GetRevision(), "one revision: the content now comes from CWWP2")
	assert.Equal(t, "C26EA-0001-2026-08-24-07:01:00", ev.GetRoadIncident().GetClosure().GetWindowId())
	_, err = st.GetEvent(ctx, "caltrans:d10-C26EA-0001-2026-08-24-070100")
	assert.Error(t, err, "no second event for the same closure")
	assert.Equal(t, gridv1.SourceStatus_OK, sourceByID(t, st, "caltrans").GetStatus())

	// Unchanged feed: no new revision.
	sched.tick(ctx, spec, ps)
	ev, err = st.GetEvent(ctx, "caltrans:C26EA-1-bfa4ac")
	require.NoError(t, err)
	assert.Equal(t, uint32(2), ev.GetRevision())

	// The crew radios 10-98: now, and only now, it resolves.
	for i := range rows {
		if rows[i].ID == "C26EA-0001-2026-08-24-07:01:00" {
			rows[i].PickedUp = true
		}
	}
	sched.tick(ctx, spec, ps)
	ev, err = st.GetEvent(ctx, "caltrans:C26EA-1-bfa4ac")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, ev.GetStatus())
}

// A degraded district must not resolve anything, including closures it
// alone was listing.
func TestTickLaneClosureDistrictDownNeverResolves(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "caltrans")

	client := &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: d10Fixture(t)}}
	n := newTestLaneClosureNormalizer(laneClosureConfig(10), client, lcsFixtureNow)
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"caltrans": {Disappearance: store.DisappearanceResolve}},
	})
	ps := &pollerState{}
	spec := PollerSpec{Normalizer: n, Interval: time.Minute}
	sched.tick(ctx, spec, ps)
	const id = "caltrans:d10-C26EA-0001-2026-08-24-070100"
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, id))

	// The file freezes: the client refuses it, the whole poll fails.
	client.errs = map[int]error{10: cwwp2.ErrStaleFeed}
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, id))
	assert.NotEmpty(t, sourceByID(t, st, "caltrans").GetLastError())
}

// An overrun the crew never radios picked up stays ACTIVE for the configured
// grace past its planned end, then drops out of the poll and the sweep
// resolves it. P88AA log 4 was set up on 2026-09-29 and planned to end at
// 16:01 PDT (23:01 UTC); it has no 10-98.
func TestTickLaneClosureOverrunGraceResolves(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "caltrans")

	const overrun = "caltrans:d10-P88AA-0004-2026-09-29-080100"
	const longTerm = "caltrans:d10-C26EA-0001-2026-08-24-070100" // planned to 2026-11-09
	plannedEnd := time.Date(2026, 9, 29, 23, 1, 0, 0, time.UTC)

	cfg := laneClosureConfig(10)
	cfg.Roads.CaltransFeeds.CWWP2.LaneClosureOverrunGrace = 6 * time.Hour
	cfg.Roads.IncidentAreas = []config.IncidentArea{motherLode} // P88AA is outside the test box
	client := &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: d10Fixture(t)}}
	n := newTestLaneClosureNormalizer(cfg, client, lcsFixtureNow) // 4h59m past its end
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"caltrans": {Disappearance: store.DisappearanceResolve}},
	})
	ps := &pollerState{}
	spec := PollerSpec{Normalizer: n, Interval: time.Minute}

	sched.tick(ctx, spec, ps)
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, overrun), "a normal overrun: codes win")

	n.now = func() time.Time { return plannedEnd.Add(6 * time.Hour) }
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, overrun), "past the grace: presumed picked up")
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, longTerm), "a window still inside its plan is untouched")
	assert.Equal(t, gridv1.SourceStatus_OK, sourceByID(t, st, "caltrans").GetStatus())
}

// Unset config means the conservative default, not "no limit".
func TestLaneClosurePoll_OverrunGraceDefault(t *testing.T) {
	plannedEnd := time.Date(2026, 9, 29, 23, 1, 0, 0, time.UTC)
	client := &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: d10Fixture(t)}}
	const overrun = "caltrans:d10-P88AA-0004-2026-09-29-080100"

	cfg := laneClosureConfig(10)
	cfg.Roads.IncidentAreas = []config.IncidentArea{motherLode}
	n := newTestLaneClosureNormalizer(cfg, client, plannedEnd.Add(cwwp2.DefaultOverrunGrace-time.Minute))
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.NotNil(t, findEvent(res.Events, overrun))
	assert.Equal(t, gridv1.EventStatus_ACTIVE, findEvent(res.Events, overrun).GetStatus())

	n.now = func() time.Time { return plannedEnd.Add(cwwp2.DefaultOverrunGrace) }
	res, err = n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Nil(t, findEvent(res.Events, overrun), "past the default grace: not emitted, so the sweep resolves it")
	assert.Nil(t, res.PerSource)
}
