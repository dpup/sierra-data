package hazards

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/config"
)

func TestRoadStatusCauses(t *testing.T) {
	// A straight east-west segment, ~8.8 km long at this latitude.
	path := []LatLng{{Lat: 38.40, Lng: -120.20}, {Lat: 38.40, Lng: -120.10}}
	rd := &api.Road{Status: api.RoadStatus_CLOSED, StatusCauses: []*api.RoadStatusCause{
		{Status: api.RoadStatus_CLOSED, Source: "lane_closure", SourceId: "C4QB",
			Begin: &api.Coordinates{Latitude: 38.4001, Longitude: -120.15}, End: &api.Coordinates{Latitude: 38.40, Longitude: -120.12}},
		{Status: api.RoadStatus_RESTRICTED, Source: "incident", SourceId: "261110MR0204",
			Begin: &api.Coordinates{Latitude: 38.40, Longitude: -120.18}},
		{Status: api.RoadStatus_CLOSED, Source: "road_condition", Description: "SR 4 closed at the Ebbetts Pass gate"},
		{Status: api.RoadStatus_RESTRICTED, Source: "incident", Begin: &api.Coordinates{Latitude: 38.6, Longitude: -120.15}},
	}}
	got := roadStatusCauses(rd, path)
	require.Len(t, got, 4)

	span := got[0]
	assert.Equal(t, "SPAN", span.Extent)
	assert.InDelta(t, 4.4, *span.FromKm, 0.1)
	assert.InDelta(t, 7.0, *span.ToKm, 0.1)
	assert.Equal(t, []float64{-120.15, 38.4001}, span.Begin)

	point := got[1]
	assert.Equal(t, "POINT", point.Extent)
	assert.Equal(t, *point.FromKm, *point.ToKm)
	assert.Equal(t, "chp:261110MR0204", point.EventID)
	assert.Equal(t, "RESTRICTED", point.Status)

	text := got[2]
	assert.Equal(t, "UNKNOWN", text.Extent, "a text-only condition has no place on the segment")
	assert.Nil(t, text.FromKm)

	assert.Equal(t, "UNKNOWN", got[3].Extent, "22 km off the line is not on the segment")

	rd.Status = api.RoadStatus_OPEN
	assert.Nil(t, roadStatusCauses(rd, path))
}

// A closure that starts on the segment and ends past it covers the segment
// from its start to the segment's nearer end, whichever end is off the line.
func TestRoadStatusCausesRunPastTheSegment(t *testing.T) {
	path := []LatLng{{Lat: 38.40, Lng: -120.20}, {Lat: 38.40, Lng: -120.10}}
	rd := &api.Road{Status: api.RoadStatus_CLOSED, StatusCauses: []*api.RoadStatusCause{
		// Begins 4.4 km in, ends ~3 km past the east end.
		{Status: api.RoadStatus_CLOSED, Source: "lane_closure",
			Begin: &api.Coordinates{Latitude: 38.40, Longitude: -120.15}, End: &api.Coordinates{Latitude: 38.40, Longitude: -120.065}},
		// Ends swapped and running off the west end.
		{Status: api.RoadStatus_CLOSED, Source: "lane_closure",
			Begin: &api.Coordinates{Latitude: 38.41, Longitude: -120.24}, End: &api.Coordinates{Latitude: 38.40, Longitude: -120.15}},
	}}
	got := roadStatusCauses(rd, path)
	require.Len(t, got, 2)

	east := got[0]
	require.Equal(t, "SPAN", east.Extent)
	assert.InDelta(t, 4.4, *east.FromKm, 0.1)
	assert.InDelta(t, 8.7, *east.ToKm, 0.1, "to the segment's east end")

	west := got[1]
	require.Equal(t, "SPAN", west.Extent)
	assert.Equal(t, 0.0, *west.FromKm, "from the segment's west end")
	assert.InDelta(t, 4.4, *west.ToKm, 0.1)
}

var arnoldBearValley = config.MonitoredRoad{
	ID: "hwy4-arnold-bearvalley", Name: "Hwy 4", Section: "Arnold to Bear Valley",
	Origin:      config.Coordinates{Latitude: 38.265006, Longitude: -120.333654},
	Destination: config.Coordinates{Latitude: 38.461045, Longitude: -120.042368},
}

const ebbettsText = "Is closed from 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - Due to snow- Motorists are advised to use an alternate route"

func ebbettsBeyond() *api.RoadBeyond {
	return &api.RoadBeyond{
		Name: "Ebbetts Pass winter closure", Status: api.RoadStatus_CLOSED, Side: "END", Source: "road_condition", Reason: "snow",
		Description: ebbettsText,
		From:        "0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/", To: "the Jct of SR 89 /Ebbetts Pass/",
	}
}

// A seasonal closure past the segment's end rides beside an OPEN status, at
// its own severity and under a headline that does not call the segment closed.
func TestRoadBeyond(t *testing.T) {
	rd := &api.Road{Status: api.RoadStatus_OPEN, Beyond: []*api.RoadBeyond{ebbettsBeyond()}}
	got := roadBeyond(rd, arnoldBearValley)
	require.Len(t, got, 1)
	assert.Equal(t, RoadBeyondProps{
		Name: "Ebbetts Pass winter closure", Status: "CLOSED", Side: "END", Severity: SevSevere, SeverityRank: 3,
		Headline: "Hwy 4: Ebbetts Pass winter closure (past Bear Valley) — snow",
		Source:   "road_condition", Reason: "snow",
		Description: ebbettsText, From: rd.Beyond[0].From, To: rd.Beyond[0].To}, got[0])
	assert.Nil(t, roadStatusCauses(rd, nil), "an open segment has no causes")
	assert.Nil(t, roadBeyond(&api.Road{Status: api.RoadStatus_OPEN}, arnoldBearValley))
}

func TestBeyondHeadline(t *testing.T) {
	b := ebbettsBeyond()
	b.Reason = "winter_closure"
	assert.Equal(t, "Hwy 4: Ebbetts Pass winter closure (past Bear Valley)", beyondHeadline(b, arnoldBearValley))
	b.Reason, b.Side = "rockslide", "START"
	assert.Equal(t, "Hwy 4: Ebbetts Pass winter closure (before Arnold) — rockslide", beyondHeadline(b, arnoldBearValley))
	b.Reason, b.Side = "", "END"
	assert.Equal(t, "Ebbetts Pass winter closure", beyondHeadline(b, config.MonitoredRoad{}))
}

// The road_segment feature for Arnold to Bear Valley while Ebbetts Pass is
// closed: open, INFO, no closure text of its own, and the closure past its end.
func TestRoadSegments_ClosurePastTheEnd(t *testing.T) {
	cfg := &config.Config{Roads: config.RoadsConfig{MonitoredRoads: []config.MonitoredRoad{arnoldBearValley}}}
	s := &Service{cfg: cfg, roads: fakeRoads{roads: []*api.Road{{
		Id: "hwy4-arnold-bearvalley", Status: api.RoadStatus_OPEN, CongestionLevel: api.CongestionLevel_CLEAR, DurationMinutes: 28, DistanceKm: 38,
		Beyond: []*api.RoadBeyond{ebbettsBeyond()},
	}}}}
	feats, err := s.roadSegments(context.Background(), config.HazardArea{ID: "hwy4-arnold-bearvalley"})
	require.NoError(t, err)
	require.Len(t, feats, 1)
	b, err := json.Marshal(feats[0].Properties)
	require.NoError(t, err)
	t.Log(string(b))
	assert.JSONEq(t, `{
		"id": "road:hwy4-arnold-bearvalley", "layer": "ROAD_SEGMENT", "kind": "Road segment",
		"severity": "INFO", "severityRank": 0,
		"headline": "Hwy 4 — Arnold to Bear Valley", "status": "OPEN", "areaLabel": "Arnold to Bear Valley",
		"source": {"id": "google", "name": "Google Routes + Caltrans", "attribution": "Google · quickmap.dot.ca.gov"},
		"road": {"roadId": "hwy4-arnold-bearvalley", "congestion": "CLEAR", "delayMinutes": 0, "durationMinutes": 28, "distanceKm": 38,
			"beyond": [{"name": "Ebbetts Pass winter closure", "status": "CLOSED", "side": "END",
				"severity": "SEVERE", "severityRank": 3,
				"headline": "Hwy 4: Ebbetts Pass winter closure (past Bear Valley) — snow",
				"source": "road_condition", "reason": "snow",
				"description": "`+ebbettsText+`",
				"from": "0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/", "to": "the Jct of SR 89 /Ebbetts Pass/"}]}
	}`, string(b))
}
