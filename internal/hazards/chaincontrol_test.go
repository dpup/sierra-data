package hazards

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/cache"
	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/dpup/sierra-data/internal/config"
)

// emptyCCKML is cc.kml as served on a quiet day: a document with no placemarks.
const emptyCCKML = `<?xml version="1.0" encoding="UTF-8"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document><name>Caltrans Chain Controls</name></Document></kml>`

type kmlDoer struct {
	status int
	body   string
}

func (d kmlDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: d.status, Body: io.NopCloser(strings.NewReader(d.body))}, nil
}

// fixtureSource serves pre-parsed checkpoints from a CWWP2 fixture file.
type fixtureSource struct{ checkpoints []cwwp2.ChainControl }

func (f fixtureSource) ChainControls(context.Context, int) ([]cwwp2.ChainControl, error) {
	return f.checkpoints, nil
}

func loadCheckpoints(t *testing.T, name string) []cwwp2.ChainControl {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "testdata", "cwwp2", name))
	require.NoError(t, err)
	cps, err := cwwp2.ParseChainControls(b)
	require.NoError(t, err)
	return cps
}

// ebbettsArea is prefab.yaml's ebbetts-pass fetch rectangle.
var ebbettsArea = config.HazardArea{ID: "ebbetts-pass", Bounds: config.GeoBounds{
	MinLatitude: 37.87, MaxLatitude: 38.59, MinLongitude: -120.72, MaxLongitude: -119.89,
}}

func chainService(kml kmlDoer, cps []cwwp2.ChainControl) *Service {
	p := &caltrans.FeedParser{HTTPClient: kml}
	p.UseCWWP2ChainControls(fixtureSource{cps}, []int{10})
	return NewServiceWithAPIs(&config.Config{}, nil, nil, p, cache.NewCache())
}

// Quiet day from the live capture: every checkpoint confirmed R-0 -> OK, zero
// features. This is the confirmed-empty that cc.kml alone could not give.
func TestChainControlLayer_CWWP2QuietDayIsConfirmedEmpty(t *testing.T) {
	s := chainService(kmlDoer{200, emptyCCKML}, loadCheckpoints(t, "cc_d10_20260930.json"))
	features, status, _, _, _, ok := s.BuildLayer(testCtx(), ebbettsArea, LayerChainControl)
	require.True(t, ok)
	assert.Equal(t, "OK", status)
	assert.Empty(t, features)
}

func TestChainControlLayer_CWWP2Storm(t *testing.T) {
	s := chainService(kmlDoer{200, emptyCCKML}, loadCheckpoints(t, "cc_d10_synthetic_storm.json"))
	features, status, _, _, _, _ := s.BuildLayer(testCtx(), ebbettsArea, LayerChainControl)
	assert.Equal(t, "OK", status)
	// All seven raised checkpoints (Hwy 4 Arnold..Cottage Springs, Hwy 108
	// Pinecrest) fall inside the ebbetts-pass rectangle.
	require.Len(t, features, 7)

	levels := map[string]int{}
	for _, f := range features {
		p := f.Properties
		levels[p.ChainControl.Level]++
		assert.True(t, strings.HasPrefix(p.ID, "cc:10-"), p.ID)
		assert.Equal(t, caltrans.SourceCWWP2, p.Source.Attribution)
		assert.Contains(t, []string{"Highway 4", "Highway 108"}, p.ChainControl.Highway)
	}
	assert.Equal(t, map[string]int{"R2": 6, "R1": 1}, levels)
}

// cc.kml down: the CWWP2 levels are still served, but the layer can't claim
// to know about road closures -> STALE with the features kept.
func TestChainControlLayer_KMLSupplementDownIsStale(t *testing.T) {
	s := chainService(kmlDoer{status: 503}, loadCheckpoints(t, "cc_d10_synthetic_storm.json"))
	features, status, _, _, _, _ := s.BuildLayer(testCtx(), ebbettsArea, LayerChainControl)
	assert.Equal(t, "STALE", status)
	assert.Len(t, features, 7)
}

// An in-area checkpoint with an unreadable status is kept off the map and
// degrades the layer; one outside the area does not.
func TestChainControlLayer_UnrecognizedStatus(t *testing.T) {
	cps := loadCheckpoints(t, "cc_d10_20260930.json")
	inArea, outOfArea := -1, -1
	for i, cp := range cps {
		in := ebbettsArea.Bounds.Contains(cp.Location.Latitude, cp.Location.Longitude)
		if in && inArea < 0 {
			inArea = i
		}
		if !in && outOfArea < 0 {
			outOfArea = i
		}
	}
	require.True(t, inArea >= 0 && outOfArea >= 0)

	garble := func(i int) []cwwp2.ChainControl {
		c := append([]cwwp2.ChainControl(nil), cps...)
		c[i].Level, c[i].RawStatus = cwwp2.LevelUnknown, "-118.1307759"
		return c
	}

	features, status, _, _, _, _ := chainService(kmlDoer{200, emptyCCKML}, garble(inArea)).BuildLayer(testCtx(), ebbettsArea, LayerChainControl)
	assert.Equal(t, "STALE", status)
	assert.Empty(t, features)

	features, status, _, _, _, _ = chainService(kmlDoer{200, emptyCCKML}, garble(outOfArea)).BuildLayer(testCtx(), ebbettsArea, LayerChainControl)
	assert.Equal(t, "OK", status)
	assert.Empty(t, features)
}
