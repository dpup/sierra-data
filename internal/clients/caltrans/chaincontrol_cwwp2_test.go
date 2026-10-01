package caltrans

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedDoer answers every request with one status and body.
type fixedDoer struct {
	status int
	body   []byte
}

func (f fixedDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(string(f.body)))}, nil
}

func cwwp2Fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "testdata", "cwwp2", name))
	require.NoError(t, err)
	return b
}

// cwwp2Client serves a CWWP2 fixture with the clock pinned to its capture.
func cwwp2Client(d cwwp2.HTTPDoer) *cwwp2.Client {
	c := cwwp2.NewClientWithHTTPDoer(d)
	c.Now = func() time.Time { return time.Date(2026, 9, 30, 4, 0, 0, 0, time.UTC) }
	return c
}

// kmlOnlyEntries is how many cc.kml entries the CWWP2 mode keeps: road
// closures and truck-only levels. On the legacy 2025 capture that is exactly
// the set with no parseable R-level, which pins the positive classifier
// against the old "no level" rule it replaced.
func kmlOnlyEntries(t *testing.T) int {
	t.Helper()
	p := setupTestParser(t)
	incidents, err := p.ParseChainControls(context.Background())
	require.NoError(t, err)
	kept, levelless := 0, 0
	for _, in := range incidents {
		if isKMLSupplement(in) {
			kept++
		}
	}
	for _, c := range p.parseChainControlDetails(incidents) {
		if c.Level == "" {
			levelless++
		}
	}
	require.Positive(t, kept, "fixture should hold road-closed / truck entries")
	require.Equal(t, levelless, kept)
	return kept
}

// Quiet day: 149 checkpoints all R-0 contribute nothing, and the KML
// supplement still carries its closures.
func TestChainControlsCWWP2_QuietDay(t *testing.T) {
	p := setupTestParser(t)
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, cwwp2Fixture(t, "cc_d10_20260930.json")}), []int{10})

	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)
	assert.Len(t, controls, kmlOnlyEntries(t))
	for _, c := range controls {
		assert.Empty(t, c.Level, "cc.kml R-levels must not leak through: %+v", c)
		assert.Equal(t, SourceQuickMap, c.Source)
	}
}

func TestChainControlsCWWP2_Storm(t *testing.T) {
	p := setupTestParser(t)
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, cwwp2Fixture(t, "cc_d10_synthetic_storm.json")}), []int{10})

	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)

	var fromCWWP2 []ChainControlData
	for _, c := range controls {
		if c.Source == SourceCWWP2 {
			fromCWWP2 = append(fromCWWP2, c)
		}
	}
	require.Len(t, fromCWWP2, 7)
	assert.Len(t, controls, 7+kmlOnlyEntries(t))

	var arnold *ChainControlData
	for i := range fromCWWP2 {
		if fromCWWP2[i].LocationName == "ARNOLD" && fromCWWP2[i].Direction == "Eastbound" {
			arnold = &fromCWWP2[i]
		}
	}
	require.NotNil(t, arnold)
	assert.Equal(t, "Highway 4", arnold.Highway)
	assert.Equal(t, "R2", arnold.Level)
	assert.Equal(t, "10", arnold.District)
	assert.True(t, strings.HasPrefix(arnold.MessageID, "10-CAL-4-"), arnold.MessageID)
	assert.Contains(t, arnold.Description, "traction devices")
	assert.Equal(t, "2026-09-29T18:05:00-07:00", arnold.EffectiveTime)
	require.NotNil(t, arnold.Coordinates)
	assert.False(t, arnold.Unrecognized)
}

// A CWWP2 failure is a hard error: serving cc.kml's closures alone would read
// as "no chain controls" over the whole district.
func TestChainControlsCWWP2_SourceDownIsHardError(t *testing.T) {
	p := setupTestParser(t)
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{status: 500}), []int{10})
	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.Error(t, err)
	var partial *PartialError
	assert.False(t, errors.As(err, &partial))
	assert.Nil(t, controls)

	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, []byte(`{"data": []}`)}), []int{10})
	_, err = p.ParseChainControlsDetailed(context.Background())
	assert.ErrorIs(t, err, cwwp2.ErrEmptyFeed)
}

// cc.kml down while CWWP2 is healthy: the levels are still good, so they come
// back with a PartialError rather than nothing.
func TestChainControlsCWWP2_KMLDownIsPartial(t *testing.T) {
	p := &FeedParser{HTTPClient: fixedDoer{status: 503}}
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, cwwp2Fixture(t, "cc_d10_synthetic_storm.json")}), []int{10})

	controls, err := p.ParseChainControlsDetailed(context.Background())
	var partial *PartialError
	require.ErrorAs(t, err, &partial)
	assert.Len(t, controls, 7)
}

func TestChainControlFromCWWP2_Unrecognized(t *testing.T) {
	all, err := cwwp2.ParseChainControls(cwwp2Fixture(t, "cc_d07_bad_status_20260930.json"))
	require.NoError(t, err)
	var got []ChainControlData
	for _, cp := range all {
		if cp.Level != cwwp2.LevelNone {
			got = append(got, chainControlFromCWWP2(cp))
		}
	}
	require.Len(t, got, 2)
	for _, c := range got {
		assert.True(t, c.Unrecognized)
		assert.Empty(t, c.Level, "an unreadable status must not become a level")
		assert.Equal(t, "Highway 2", c.Highway)
		assert.Contains(t, c.Description, "unrecognized")
		assert.NotEmpty(t, c.RawStatus)
	}
}

func TestHighwayAndDirectionLabels(t *testing.T) {
	for in, want := range map[string]string{"SR-4": "Highway 4", "SR-108": "Highway 108", "US-50": "US 50", "I-80": "I-80", "": ""} {
		assert.Equal(t, want, highwayLabel(in), in)
	}
	for in, want := range map[string]string{"East": "Eastbound", "west": "Westbound", "North": "Northbound", "South": "Southbound", "": ""} {
		assert.Equal(t, want, directionLabel(in), in)
	}
}

// If cc.kml moves to the 2026 iw-* layout (blank <name>, as the CHP and
// lane-closure feeds did), no R-level parses from its entries. They must NOT
// leak through as level-less duplicates of the CWWP2 checkpoints; only the
// positively-identified closure survives.
func TestChainControlsCWWP2_IWLayoutKMLDoesNotLeakControls(t *testing.T) {
	const kml = `<?xml version="1.0" encoding="UTF-8"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document>
<Placemark><name> </name><styleUrl>#notclosed</styleUrl><description><![CDATA[<h2 class="iw-title">Chain Controls</h2><p class="iw-text">Arnold</p>]]></description><Point><coordinates>-120.35,38.25</coordinates></Point></Placemark>
<Placemark><name> </name><styleUrl>#notclosed</styleUrl><description><![CDATA[<h2 class="iw-title">Chain Controls</h2><p class="iw-text">Dorrington</p>]]></description><Point><coordinates>-120.27,38.30</coordinates></Point></Placemark>
<Placemark><name> </name><styleUrl>#full-closure</styleUrl><description><![CDATA[<h2 class="iw-title">Highway 4 Road Closed</h2><p class="iw-text">Closed to traffic.</p>]]></description><Point><coordinates>-119.92,38.50</coordinates></Point></Placemark>
</Document></kml>`
	p := &FeedParser{HTTPClient: fixedDoer{200, []byte(kml)}}
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, cwwp2Fixture(t, "cc_d10_synthetic_storm.json")}), []int{10})

	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)
	var kmlEntries []ChainControlData
	for _, c := range controls {
		if c.Source == SourceQuickMap {
			kmlEntries = append(kmlEntries, c)
		}
	}
	require.Len(t, kmlEntries, 1, "only the #full-closure entry may come from cc.kml")
	assert.Len(t, controls, 8)
}

func TestIsKMLSupplement(t *testing.T) {
	assert.True(t, isKMLSupplement(CaltransIncident{StyleUrl: "#full-closure"}))
	assert.True(t, isKMLSupplement(CaltransIncident{StyleUrl: "#notclosed", Name: "Westbound Interstate 80 Chain Control level MAX"}))
	assert.True(t, isKMLSupplement(CaltransIncident{StyleUrl: "#notclosed", Name: "Eastbound Interstate 80 Chain Control level TS"}))
	assert.True(t, isKMLSupplement(CaltransIncident{DescriptionText: "Truck chain requirements are maximum for all trucks."}))
	assert.False(t, isKMLSupplement(CaltransIncident{StyleUrl: "#notclosed", Name: "Eastbound Highway 4 Chain Control level R-2"}))
	assert.False(t, isKMLSupplement(CaltransIncident{StyleUrl: "#notclosed", Name: ""}), "unclassifiable entries are CWWP2's")
	assert.False(t, isKMLSupplement(CaltransIncident{Name: "Highway 4 at Maximilian Rd"}), "MAX must be the level word, not a substring")
}

// stubSource serves fixed checkpoints.
type stubSource []cwwp2.ChainControl

func (s stubSource) ChainControls(context.Context, int) ([]cwwp2.ChainControl, error) { return s, nil }

// A sign we can't poll that still reports a requirement is shown; one that
// reports nothing is skipped.
func TestChainControlsCWWP2_OutOfService(t *testing.T) {
	loc := cwwp2.Location{Route: "SR-4", Direction: "East", Name: "ARNOLD", Latitude: 38.25, Longitude: -120.35, HasPosition: true}
	src := stubSource{
		{ID: "a", Location: loc, InService: false, Level: cwwp2.LevelR2, RawStatus: "R-2"},
		{ID: "b", Location: loc, InService: false, Level: cwwp2.LevelNone, RawStatus: "R-0"},
	}
	p := &FeedParser{HTTPClient: fixedDoer{200, []byte(`<kml><Document></Document></kml>`)}}
	p.UseCWWP2ChainControls(src, []int{10})
	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)
	require.Len(t, controls, 1)
	assert.Equal(t, "a", controls[0].MessageID)
	assert.Equal(t, "R2", controls[0].Level)
}
