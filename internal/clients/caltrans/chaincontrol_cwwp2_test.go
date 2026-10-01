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

// legacyKML is the real 2025-12-24 storm capture: R-1/R-2 controls in
// Districts 3, 9 and 10, road closures, and MAX/MIN/TS truck levels.
func legacyKML(t *testing.T) []ChainControlData {
	t.Helper()
	all, err := setupTestParser(t).ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, all)
	return all
}

// d10Checkpoints parses the real D10 capture, raising to `level` every
// checkpoint for which raise returns true.
func d10Checkpoints(t *testing.T, level cwwp2.Level, raise func(cwwp2.ChainControl) bool) stubSource {
	t.Helper()
	all, err := cwwp2.ParseChainControls(cwwp2Fixture(t, "cc_d10_20260930.json"))
	require.NoError(t, err)
	for i := range all {
		if raise(all[i]) {
			all[i].Level, all[i].RawStatus = level, level.String()
		}
	}
	return stubSource(all)
}

func bySource(controls []ChainControlData, src string) []ChainControlData {
	var out []ChainControlData
	for _, c := range controls {
		if c.Source == src {
			out = append(out, c)
		}
	}
	return out
}

func findKML(t *testing.T, controls []ChainControlData, highway, direction, location string) *ChainControlData {
	t.Helper()
	for i := range controls {
		c := &controls[i]
		if c.Source == SourceQuickMap && c.Highway == highway && c.Direction == direction && strings.HasPrefix(c.LocationName, location) {
			return c
		}
	}
	return nil
}

// cc.kml reports a storm while CWWP2 reports R-0 everywhere: the sources
// disagree, and the control must be SHOWN, not erased by the other source's
// silence. Every cc.kml entry survives; the ones on a quiet CWWP2 checkpoint
// are flagged.
func TestChainControlsCWWP2_DisagreementShowsTheControl(t *testing.T) {
	p := setupTestParser(t)
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, cwwp2Fixture(t, "cc_d10_20260930.json")}), []int{10})

	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)
	assert.Len(t, controls, len(legacyKML(t)))
	assert.Empty(t, bySource(controls, SourceCWWP2))

	tamarack := findKML(t, controls, "Highway 4", "Eastbound", "TAMARACK")
	require.NotNil(t, tamarack)
	assert.Equal(t, "R2", tamarack.Level)
	assert.True(t, tamarack.Uncorroborated, "on a CWWP2 checkpoint reporting R-0")

	twin := findKML(t, controls, "US 50", "Eastbound", "Twin Bridges")
	require.NotNil(t, twin, "District 3: CWWP2 isn't configured there, so cc.kml is the only word")
	assert.False(t, twin.Uncorroborated)
}

// The sources agree: each cc.kml control at an active CWWP2 checkpoint (same
// spot, same direction) is the same control reported twice, and only the
// CWWP2 copy is kept. Everything else in cc.kml stays.
func TestChainControlsCWWP2_AgreementDeduplicates(t *testing.T) {
	kml := legacyKML(t)
	// Raise exactly the D10 checkpoints cc.kml put an R-level on.
	src := d10Checkpoints(t, cwwp2.LevelR2, func(cp cwwp2.ChainControl) bool {
		c := chainControlFromCWWP2(cp)
		for _, k := range kml {
			if k.Level != "" && sameCheckpoint(k, c) {
				return true
			}
		}
		return false
	})
	p := setupTestParser(t)
	p.UseCWWP2ChainControls(src, []int{10})

	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)
	cw := bySource(controls, SourceCWWP2)
	require.NotEmpty(t, cw)
	// One cc.kml entry can sit on TWO CWWP2 checkpoints: RED LAKE CREEK and
	// RED LAKE CREEK - CARSON PASS are 1 m apart, both westbound (a chain
	// checkpoint and its closure gate). So count the dropped side directly.
	dropped := 0
	for _, k := range kml {
		if k.Level != "" && atActiveCheckpoint(k, cw) {
			dropped++
		}
	}
	require.Positive(t, dropped)
	assert.Len(t, controls, len(cw)+len(kml)-dropped)
	assert.Len(t, bySource(controls, SourceQuickMap), len(kml)-dropped)

	assert.Nil(t, findKML(t, controls, "Highway 4", "Eastbound", "TAMARACK"), "duplicate of CWWP2 TAMARACK East")
	assert.Nil(t, findKML(t, controls, "Highway 4", "Westbound", "TAMARACK"), "duplicate of CWWP2 TAMARACK West")
	for _, c := range controls {
		assert.False(t, c.Uncorroborated, "%+v", c)
	}

	// Outside District 10: kept from cc.kml.
	assert.NotNil(t, findKML(t, controls, "US 50", "Eastbound", "Twin Bridges"))
	assert.NotNil(t, findKML(t, controls, "Highway 108", "Eastbound", "3.8 Mi. W of Jct. 395"), "District 9, 21.8 km from any D10 checkpoint")
	// Closures stay even where CWWP2 is quiet.
	assert.NotNil(t, findKML(t, controls, "Highway 4", "Eastbound", "MOUNT REBA ROAD - EBBETTS"))
}

func TestChainControlsCWWP2_Storm(t *testing.T) {
	p := setupTestParser(t)
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, cwwp2Fixture(t, "cc_d10_synthetic_storm.json")}), []int{10})

	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)

	fromCWWP2 := bySource(controls, SourceCWWP2)
	require.Len(t, fromCWWP2, 7)
	// The synthetic checkpoints (Arnold..Cottage Springs, Pinecrest) are not
	// where cc.kml's 2025 storm put its controls, so nothing deduplicates.
	assert.Len(t, controls, 7+len(legacyKML(t)))

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

// CWWP2 down: cc.kml alone is what the service served before CWWP2, so its
// controls come back degraded rather than the layer going blank.
func TestChainControlsCWWP2_SourceDownFallsBackToKML(t *testing.T) {
	p := setupTestParser(t)
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{status: 500}), []int{10})
	controls, err := p.ParseChainControlsDetailed(context.Background())
	var partial *PartialError
	require.ErrorAs(t, err, &partial)
	assert.Len(t, controls, len(legacyKML(t)))
	require.NotNil(t, findKML(t, controls, "Highway 4", "Eastbound", "TAMARACK"))
}

// CWWP2 down and cc.kml empty: nothing confirms the quiet, so it is a hard
// error — never an empty success.
func TestChainControlsCWWP2_SourceDownWithEmptyKMLIsHardError(t *testing.T) {
	p := &FeedParser{HTTPClient: fixedDoer{200, []byte(`<kml><Document></Document></kml>`)}}
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{status: 500}), []int{10})
	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.Error(t, err)
	var partial *PartialError
	assert.False(t, errors.As(err, &partial))
	assert.Nil(t, controls)

	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, []byte(`{"data": []}`)}), []int{10})
	_, err = p.ParseChainControlsDetailed(context.Background())
	assert.ErrorIs(t, err, cwwp2.ErrEmptyFeed)

	p.HTTPClient = fixedDoer{status: 503} // both down
	_, err = p.ParseChainControlsDetailed(context.Background())
	require.Error(t, err)
	assert.False(t, errors.As(err, &partial))
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

// A pass-closure gate is likely to carry a non-R CWWP2 status all winter.
// Where cc.kml reports something at that checkpoint, cc.kml says what is there
// and the unreadable CWWP2 row is dropped instead of degrading the layer.
func TestChainControlsCWWP2_UnrecognizedExplainedByKML(t *testing.T) {
	src := d10Checkpoints(t, cwwp2.LevelUnknown, func(cp cwwp2.ChainControl) bool {
		return cp.Location.Name == "MOUNT REBA ROAD - EBBETTS PASS" || cp.Location.Name == "ARNOLD"
	})
	p := setupTestParser(t)
	p.UseCWWP2ChainControls(src, []int{10})
	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)

	var unrecognized []string
	for _, c := range controls {
		if c.Unrecognized {
			unrecognized = append(unrecognized, c.LocationName)
		}
	}
	// Mount Reba East has the 2025 "Road Closed" entry on it; Arnold has nothing.
	assert.ElementsMatch(t, []string{"ARNOLD", "ARNOLD"}, unrecognized)
	assert.NotNil(t, findKML(t, controls, "Highway 4", "Eastbound", "MOUNT REBA ROAD - EBBETTS"))
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
// lane-closure feeds did), no level or direction parses from its entries.
// Matching is positional, so the duplicate of an active checkpoint still
// drops, a control CWWP2 doesn't confirm is still shown, and a closure is
// still kept.
func TestChainControlsCWWP2_IWLayoutKML(t *testing.T) {
	const kml = `<?xml version="1.0" encoding="UTF-8"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document>
<Placemark><name> </name><styleUrl>#notclosed</styleUrl><description><![CDATA[<h2 class="iw-title">Chain Controls</h2><p class="iw-text">Arnold</p>]]></description><Point><coordinates>-120.3476,38.2565</coordinates></Point></Placemark>
<Placemark><name> </name><styleUrl>#notclosed</styleUrl><description><![CDATA[<h2 class="iw-title">Chain Controls</h2><p class="iw-text">Tamarack</p>]]></description><Point><coordinates>-120.076710,38.439340</coordinates></Point></Placemark>
<Placemark><name> </name><styleUrl>#full-closure</styleUrl><description><![CDATA[<h2 class="iw-title">Highway 4 Road Closed</h2><p class="iw-text">Closed to traffic.</p>]]></description><Point><coordinates>-120.01496,38.48042</coordinates></Point></Placemark>
</Document></kml>`
	p := &FeedParser{HTTPClient: fixedDoer{200, []byte(kml)}}
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, cwwp2Fixture(t, "cc_d10_synthetic_storm.json")}), []int{10})

	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)
	kmlEntries := bySource(controls, SourceQuickMap)
	require.Len(t, kmlEntries, 2, "the Arnold entry duplicates CWWP2's active ARNOLD East")
	assert.Len(t, controls, 9)
	for _, k := range kmlEntries {
		assert.NotEqual(t, -120.3476, k.Coordinates.Longitude)
	}
	// Tamarack is R-0 in CWWP2: shown, and flagged as a disagreement.
	var tamarack *ChainControlData
	for i := range kmlEntries {
		if kmlEntries[i].Coordinates.Latitude == 38.43934 {
			tamarack = &kmlEntries[i]
		}
	}
	require.NotNil(t, tamarack)
	assert.True(t, tamarack.Uncorroborated)
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

// Closure entries carry their highway and are flagged, so they render as a
// closure and are never mistaken for a chain requirement.
func TestChainControlDetails_RoadClosed(t *testing.T) {
	closure := findKML(t, legacyKML(t), "Highway 4", "Eastbound", "MOUNT REBA ROAD - EBBETTS")
	require.NotNil(t, closure)
	assert.True(t, closure.Closed)
	assert.Empty(t, closure.Level)

	r2 := findKML(t, legacyKML(t), "Highway 4", "Eastbound", "TAMARACK")
	require.NotNil(t, r2)
	assert.False(t, r2.Closed)
}
