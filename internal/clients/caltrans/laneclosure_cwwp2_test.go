package caltrans

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/clients/cwwp2"
)

func cwwp2ClosureFixture(t *testing.T, id string) cwwp2.LaneClosure {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "testdata", "cwwp2", "lcs_d10_20260930.json"))
	require.NoError(t, err)
	rows, err := cwwp2.ParseLaneClosures(b)
	require.NoError(t, err)
	for _, r := range rows {
		if r.ID == id {
			r.District = 10
			return r
		}
	}
	t.Fatalf("fixture row %s not found", id)
	return cwwp2.LaneClosure{}
}

// A CWWP2 window must reach the roads service in the shape lcs2way.kml did,
// so route matching, the stable alert id and the AI status call all apply
// unchanged.
func TestIncidentFromCWWP2LaneClosure(t *testing.T) {
	lc := cwwp2ClosureFixture(t, "C26EA-0001-2026-08-24-07:01:00") // SR-26 full closure, set up
	fetched := time.Date(2026, 9, 30, 4, 0, 0, 0, time.UTC)

	inc := IncidentFromCWWP2LaneClosure(lc, fetched)

	assert.Equal(t, LANE_CLOSURE, inc.FeedType)
	assert.Equal(t, "Route 26 Full Closure", inc.Name)
	require.NotNil(t, inc.Coordinates)
	assert.InDelta(t, 38.297818, inc.Coordinates.Latitude, 1e-9, "the begin point")
	assert.InDelta(t, -120.704668, inc.Coordinates.Longitude, 1e-9)
	require.NotNil(t, inc.AffectedArea, "both endpoints, for polyline route matching")
	assert.Len(t, inc.AffectedArea.Points, 2)
	assert.Equal(t, fetched, inc.LastFetched)

	text := inc.DescriptionText
	assert.Contains(t, text, "Closure ID: C26EA, Log Number: 1", "logNumberFromText keys the alert id on this")
	assert.Contains(t, text, "Full closure")
	assert.Contains(t, text, "from Route 49 Mokelumne Hill to Main Street (Left)")
	assert.Contains(t, text, "Roadway Excavation")
	assert.Contains(t, text, "Lanes closed: All of 2")
	assert.Contains(t, text, "in place")
	assert.Equal(t, text, inc.DescriptionHtml)

	// The text is the AI cache key's main input: it must not change between
	// refreshes of the same window.
	again := IncidentFromCWWP2LaneClosure(lc, fetched.Add(15*time.Minute))
	assert.Equal(t, inc.Name, again.Name)
	assert.Equal(t, text, again.DescriptionText)
}

func TestIncidentFromCWWP2LaneClosure_OneEndpointAndOneWay(t *testing.T) {
	lc := cwwp2ClosureFixture(t, "P88AA-0004-2026-09-29-08:01:00") // SR-88 one-way, begin == end
	inc := IncidentFromCWWP2LaneClosure(lc, time.Now())
	assert.Equal(t, "Route 88 One-way Traffic Operation", inc.Name, "the lcs2way.kml title style")
	assert.True(t, strings.Contains(inc.DescriptionText, "at Sierra Pines Entrance (Left)"), inc.DescriptionText)
	assert.Contains(t, inc.DescriptionText, "Estimated delay: 3 min")

	lc.Begin.HasPosition = false
	inc = IncidentFromCWWP2LaneClosure(lc, time.Now())
	require.NotNil(t, inc.Coordinates, "falls back to the end point")
	assert.Nil(t, inc.AffectedArea, "one usable point is a point alert")
}
