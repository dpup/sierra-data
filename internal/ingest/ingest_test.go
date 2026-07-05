package ingest

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dpup/prefab/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/calfire"
	"github.com/dpup/info.ersn.net/server/internal/clients/caloes"
	"github.com/dpup/info.ersn.net/server/internal/clients/usgs"
	"github.com/dpup/info.ersn.net/server/internal/clients/wfigs"
	"github.com/dpup/info.ersn.net/server/internal/config"
)

// testCtx carries a logger so normalizers' logging.Warnw calls don't panic.
func testCtx() context.Context { return logging.EnsureLogger(context.Background()) }

// fakeDoer serves a canned body (or an error) for any request, recording the
// last URL — the repo's standard HTTPDoer injection for client tests.
type fakeDoer struct {
	resp    string
	err     error
	lastURL string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.lastURL = req.URL.String()
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(f.resp)),
		Header:     make(http.Header),
	}, nil
}

// testConfig has two hazard areas whose union bbox is
// (37.7, -120.9)..(38.5, -119.2), plus two incident areas.
func testConfig() *config.Config {
	return &config.Config{
		OpenAI: config.OpenAIClient{Model: "gpt-5-mini"},
		Roads: config.RoadsConfig{
			IncidentAreas: []config.IncidentArea{
				{ID: "mother-lode", Bounds: config.GeoBounds{MinLatitude: 37.9, MaxLatitude: 38.5, MinLongitude: -120.9, MaxLongitude: -120.0}},
				{ID: "high-country", Bounds: config.GeoBounds{MinLatitude: 38.0, MaxLatitude: 38.8, MinLongitude: -120.3, MaxLongitude: -119.5}},
			},
		},
		Hazards: config.HazardsConfig{
			Areas: []config.HazardArea{
				{
					ID:     "calaveras",
					Bounds: config.GeoBounds{MinLatitude: 38.0, MaxLatitude: 38.5, MinLongitude: -120.9, MaxLongitude: -120.0},
					Zones:  []string{"CAZ064", "CAZ065"},
				},
				{
					ID:     "tuolumne",
					Bounds: config.GeoBounds{MinLatitude: 37.7, MaxLatitude: 38.2, MinLongitude: -120.6, MaxLongitude: -119.2},
					Zones:  []string{"CAZ258"},
				},
			},
		},
	}
}

func TestSeverityFromLabel(t *testing.T) {
	assert.Equal(t, gridv1.Severity_INFO, SeverityFromLabel("INFO"))
	assert.Equal(t, gridv1.Severity_MINOR, SeverityFromLabel("MINOR"))
	assert.Equal(t, gridv1.Severity_MODERATE, SeverityFromLabel("MODERATE"))
	assert.Equal(t, gridv1.Severity_SEVERE, SeverityFromLabel("SEVERE"))
	assert.Equal(t, gridv1.Severity_EXTREME, SeverityFromLabel("EXTREME"))
	assert.Equal(t, gridv1.Severity_INFO, SeverityFromLabel("bogus"))
}

func TestGeometryFromGeoJSON(t *testing.T) {
	raw := []byte(`{"type":"Polygon","coordinates":[[[-120.5,38.0],[-120.4,38.0],[-120.4,38.2],[-120.5,38.2],[-120.5,38.0]]]}`)
	g, err := GeometryFromGeoJSON(raw)
	require.NoError(t, err)
	assert.Equal(t, raw, g.Geojson)
	assert.InDelta(t, 38.0, g.Bbox.MinLat, 1e-9)
	assert.InDelta(t, 38.2, g.Bbox.MaxLat, 1e-9)
	assert.InDelta(t, -120.5, g.Bbox.MinLng, 1e-9)
	assert.InDelta(t, -120.4, g.Bbox.MaxLng, 1e-9)
	assert.InDelta(t, 38.1, g.Centroid.Lat, 1e-9)
	assert.InDelta(t, -120.45, g.Centroid.Lng, 1e-9)

	_, err = GeometryFromGeoJSON([]byte(`{"type":"Nope","coordinates":[1,2]}`))
	assert.Error(t, err)
}

func TestGeometryFromPoint(t *testing.T) {
	g := GeometryFromPoint(38.2, -120.45)
	// RFC 7946 [lng, lat] order on the wire.
	assert.JSONEq(t, `{"type":"Point","coordinates":[-120.45,38.2]}`, string(g.Geojson))
	assert.Equal(t, 38.2, g.Bbox.MinLat)
	assert.Equal(t, 38.2, g.Bbox.MaxLat)
	assert.Equal(t, -120.45, g.Bbox.MinLng)
	assert.Equal(t, -120.45, g.Bbox.MaxLng)
	assert.Equal(t, 38.2, g.Centroid.Lat)
	assert.Equal(t, -120.45, g.Centroid.Lng)
}

func TestGeometryFromTyped(t *testing.T) {
	g, err := geometryFromTyped("Polygon", []byte(`[[[-120.3,38.0],[-120.2,38.0],[-120.2,38.1],[-120.3,38.1],[-120.3,38.0]]]`))
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"type":"Polygon","coordinates":[[[-120.3,38.0],[-120.2,38.0],[-120.2,38.1],[-120.3,38.1],[-120.3,38.0]]]}`,
		string(g.Geojson))
	assert.InDelta(t, 38.05, g.Centroid.Lat, 1e-9)

	_, err = geometryFromTyped("", []byte(`[]`))
	assert.Error(t, err)
	_, err = geometryFromTyped("Polygon", nil)
	assert.Error(t, err)
}

func TestTsProto(t *testing.T) {
	assert.Nil(t, tsProto(time.Time{}))
	at := time.Date(2026, 7, 4, 8, 0, 0, 0, time.UTC)
	require.NotNil(t, tsProto(at))
	assert.Equal(t, at, tsProto(at).AsTime())
}

// TestPollEmptyScopeIsError: a poller whose configured scope is empty must
// hard-error WITHOUT fetching. A success-empty PollResult here would let the
// scheduler's disappearance sweep RESOLVE every stored active event (e.g. a
// live evacuation ORDER after a bad prefab.yaml refactor emptied the areas)
// and mark the source healthy — a fabricated all-clear from a config mistake.
func TestPollEmptyScopeIsError(t *testing.T) {
	cfg := &config.Config{} // no hazard areas, no incident areas
	doer := &fakeDoer{resp: "{}"}
	roads := &fakeRoadsAPI{}
	norms := map[string]Normalizer{
		"evacuation": NewEvacuationNormalizer(cfg, caloes.NewClientWithHTTPDoer("https://caloes.test", doer)),
		"earthquake": NewEarthquakeNormalizer(cfg, usgs.NewClientWithHTTPDoer("https://usgs.test", doer)),
		"wildfire": NewWildfireNormalizer(cfg,
			calfire.NewClientWithHTTPDoer("https://calfire.test", doer),
			wfigs.NewClientWithHTTPDoer("https://wfigs.test", doer)),
		"road_incident": NewRoadIncidentNormalizer(cfg, roads),
	}
	for name, n := range norms {
		t.Run(name, func(t *testing.T) {
			res, err := n.Poll(testCtx(), nil)
			require.Error(t, err, "empty scope must fail loud, never a success-empty")
			assert.Nil(t, res)
		})
	}
	assert.Empty(t, doer.lastURL, "an empty scope must not fetch anything")
	assert.Empty(t, roads.calls)
}

func TestUnionBounds(t *testing.T) {
	minLat, minLng, maxLat, maxLng, ok := unionBounds(testConfig().Hazards.Areas)
	require.True(t, ok)
	assert.Equal(t, 37.7, minLat)
	assert.Equal(t, -120.9, minLng)
	assert.Equal(t, 38.5, maxLat)
	assert.Equal(t, -119.2, maxLng)

	_, _, _, _, ok = unionBounds(nil)
	assert.False(t, ok)
}

func TestZonesMatch(t *testing.T) {
	assert.True(t, zonesMatch(nil, []string{"CAZ064"}), "unscoped area matches everything")
	assert.True(t, zonesMatch([]string{"CAZ064"}, nil), "zoneless record can't be scoped; kept")
	assert.True(t, zonesMatch([]string{"CAZ064", "CAZ065"}, []string{"CAZ065"}))
	assert.False(t, zonesMatch([]string{"CAZ064"}, []string{"CAZ258"}))
}

func TestSafeURL(t *testing.T) {
	assert.Equal(t, "https://example.com/x", safeURL(" https://example.com/x "))
	assert.Equal(t, "http://example.com/x", safeURL("http://example.com/x"))
	assert.Equal(t, "", safeURL("javascript:alert(1)"))
	assert.Equal(t, "", safeURL(""))
}

func TestNewProvenance(t *testing.T) {
	p := NewProvenance("usgs", "USGS", "U.S. Geological Survey", "https://example.com")
	assert.Equal(t, "usgs", p.SourceId)
	assert.Equal(t, "USGS", p.SourceName)
	assert.Equal(t, "U.S. Geological Survey", p.Attribution)
	assert.Equal(t, "https://example.com", p.SourceUrl)
	assert.NotNil(t, p.FetchedAt)
}
