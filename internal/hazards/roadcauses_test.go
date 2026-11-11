package hazards

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/sierragridteam/sierra-data/api/v1"
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
