package hazards

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/cache"
	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/dpup/sierra-data/internal/config"
)

// signSource serves pre-parsed signs per district, or an error per district.
type signSource struct {
	signs map[int][]cwwp2.MessageSign
	errs  map[int]error
	calls int
}

func (f *signSource) MessageSigns(_ context.Context, district int) ([]cwwp2.MessageSign, error) {
	f.calls++
	if err := f.errs[district]; err != nil {
		return nil, err
	}
	return f.signs[district], nil
}

func loadSigns(t *testing.T, name string) []cwwp2.MessageSign {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "testdata", "cwwp2", name))
	require.NoError(t, err)
	signs, err := cwwp2.ParseMessageSigns(b)
	require.NoError(t, err)
	return signs
}

func signService(src MessageSignAPI, districts ...int) *Service {
	s := NewServiceWithAPIs(&config.Config{}, nil, nil, nil, cache.NewCache())
	s.UseMessageSigns(src, districts)
	return s
}

func featureByID(t *testing.T, fs []Feature, id string) Feature {
	t.Helper()
	for _, f := range fs {
		if f.Properties.ID == id {
			return f
		}
	}
	t.Fatalf("feature %s not in layer", id)
	return Feature{}
}

// The live 2026-10-01 capture through the ebbetts-pass rectangle: the six
// District 10 signs inside it, each with what it was showing.
func TestMessageSignLayer_LiveD10(t *testing.T) {
	s := signService(&signSource{signs: map[int][]cwwp2.MessageSign{10: loadSigns(t, "cms_d10_20261001.json")}}, 10)
	features, status, _, _, _, ok := s.BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	require.True(t, ok)
	assert.Equal(t, "OK", status)

	var ids []string
	categories := map[string]int{}
	for _, f := range features {
		p := f.Properties
		ids = append(ids, p.ID)
		categories[p.Category]++
		assert.Equal(t, "MESSAGE_SIGN", p.Layer)
		assert.Equal(t, "Message sign", p.Kind)
		assert.Equal(t, SevInfo, p.Severity, "context, never a hazard: %s", p.ID)
		assert.Equal(t, 0, p.SeverityRank)
		assert.Equal(t, "caltrans", p.Source.ID)
		assert.Equal(t, caltrans.SourceCWWP2, p.Source.Attribution)
		require.NotNil(t, p.MessageSign, p.ID)
		assert.Equal(t, 10, p.MessageSign.District)
	}
	// Hwy 108 Soulsbyville, "EB 120 W/O YOSEMITE", Hwy 4 west of Murphys,
	// Hwy 88 Pine Grove and Dew Drop (plus its "Virtual CMS" twin).
	assert.Equal(t, []string{"cms:10:V42", "cms:10:V43", "cms:10:V50", "cms:10:V80", "cms:10:V81", "cms:10:V111"}, ids)
	assert.Equal(t, map[string]int{"message": 4, "blank": 2}, categories)

	murphys := featureByID(t, features, "cms:10:V50")
	p := murphys.Properties
	assert.Equal(t, "message", p.Category)
	assert.Equal(t, "BE THE DRIVER WHO SAVES LIVES DON'T SPEED", p.Headline, "verbatim, entity-decoded")
	assert.Equal(t, "EB 4 W/O MURPHYS", p.AreaLabel, "sign number dropped from the name")
	assert.Equal(t, "2026-09-30T15:00:02Z", p.Effective, "when the message last changed")
	assert.Equal(t, "V50", p.MessageSign.SignID)
	assert.Equal(t, "SR-4", p.MessageSign.Route)
	assert.Equal(t, "East", p.MessageSign.Direction)
	assert.True(t, p.MessageSign.InService)
	assert.Equal(t, [][3]string{{"BE THE DRIVER", "WHO SAVES LIVES", "DON'T SPEED"}}, p.MessageSign.Pages)
	require.NotNil(t, murphys.Geometry)
	assert.Equal(t, "Point", murphys.Geometry.Type)
	assert.Equal(t, []float64{-120.45138, 38.14302}, murphys.Geometry.Coordinates, "[lng, lat], 5 decimals")

	dewDrop := featureByID(t, features, "cms:10:V81").Properties
	assert.Equal(t, "blank", dewDrop.Category)
	assert.Equal(t, "Sign is blank", dewDrop.Headline)
	assert.Empty(t, dewDrop.MessageSign.Pages)
	assert.Equal(t, "2026-05-29T02:45:02Z", dewDrop.Effective)
}

// The JSON a map client reads: the block is camelCase and a page keeps its
// blank lines so a client can draw the three-line face.
func TestMessageSignLayer_JSONShape(t *testing.T) {
	sign := cwwp2.MessageSign{
		ID:        "V7",
		Location:  cwwp2.Location{Name: "7 - EB 4 AT ARNOLD", Route: "SR-4", Direction: "East", Latitude: 38.25, Longitude: -120.35, HasPosition: true},
		InService: true,
		Display:   cwwp2.DisplayTwoPage,
		Phase1:    [3]string{"", "CHAINS REQUIRED", ""},
		Phase2:    [3]string{"4WD W/", "SNOW TIRES", "EXEMPT"},
	}
	s := signService(&signSource{signs: map[int][]cwwp2.MessageSign{10: {sign}}}, 10)
	features, status, _, _, _, _ := s.BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	assert.Equal(t, "OK", status)
	require.Len(t, features, 1)
	p := features[0].Properties
	assert.Equal(t, "CHAINS REQUIRED / 4WD W/ SNOW TIRES EXEMPT", p.Headline)
	assert.Equal(t, SevInfo, p.Severity, "even a chain message: the text is context, chain_control carries the requirement")

	b, err := json.Marshal(p)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, map[string]any{
		"signId":    "V7",
		"district":  float64(10),
		"route":     "SR-4",
		"direction": "East",
		"inService": true,
		"pages":     []any{[]any{"", "CHAINS REQUIRED", ""}, []any{"4WD W/", "SNOW TIRES", "EXEMPT"}},
	}, got["messageSign"])
}

// A sign the portal can't read is listed as unknown — the layer still knows
// every sign in the area, so it is not degraded.
func TestMessageSignLayer_UnknownSignIsListedNotDegraded(t *testing.T) {
	at := cwwp2.Location{Latitude: 38.2, Longitude: -120.4, HasPosition: true}
	signs := []cwwp2.MessageSign{
		{ID: "V1", Location: at, InService: false, Display: cwwp2.DisplayUnknown, Phase1: [3]string{"Not Reported", "Not Reported", "Not Reported"}},
		{ID: "V2", Location: at, InService: true, Display: cwwp2.DisplayUnknown},
	}
	s := signService(&signSource{signs: map[int][]cwwp2.MessageSign{10: signs}}, 10)
	features, status, _, _, _, _ := s.BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	assert.Equal(t, "OK", status)
	require.Len(t, features, 2)

	off := featureByID(t, features, "cms:10:V1").Properties
	assert.Equal(t, "unknown", off.Category)
	assert.Equal(t, "Sign out of service", off.Headline)
	assert.False(t, off.MessageSign.InService)
	assert.Empty(t, off.MessageSign.Pages, "the placeholder is never sign text")

	unread := featureByID(t, features, "cms:10:V2").Properties
	assert.Equal(t, "unknown", unread.Category)
	assert.Equal(t, "Sign message unknown", unread.Headline)
}

// An index that isn't unique in its district (D12 numbers every sign "1")
// can't be an id; the sign's position stands in.
func TestMessageSignLayer_DuplicateIndexFallsBackToPosition(t *testing.T) {
	signs := []cwwp2.MessageSign{
		{ID: "1", Location: cwwp2.Location{Latitude: 38.1, Longitude: -120.5, HasPosition: true}, Display: cwwp2.DisplayBlank},
		{ID: "1", Location: cwwp2.Location{Latitude: 38.3, Longitude: -120.2, HasPosition: true}, Display: cwwp2.DisplayBlank},
		{ID: "N/A", Location: cwwp2.Location{Latitude: 38.4, Longitude: -120.1, HasPosition: true}, Display: cwwp2.DisplayBlank},
		{ID: "9", Location: cwwp2.Location{Latitude: 38.0, Longitude: -120.6, HasPosition: true}, Display: cwwp2.DisplayBlank},
	}
	s := signService(&signSource{signs: map[int][]cwwp2.MessageSign{12: signs}}, 12)
	features, _, _, _, _, _ := s.BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	var ids []string
	for _, f := range features {
		ids = append(ids, f.Properties.ID)
	}
	assert.Equal(t, []string{"cms:12:38.10000,-120.50000", "cms:12:38.30000,-120.20000", "cms:12:38.40000,-120.10000", "cms:12:9"}, ids)
}

// Signs outside the area are left out; an area with none is a clean empty.
func TestMessageSignLayer_ScopedToArea(t *testing.T) {
	s := signService(&signSource{signs: map[int][]cwwp2.MessageSign{10: loadSigns(t, "cms_d10_20261001.json")}}, 10)
	nowhere := config.HazardArea{ID: "nowhere", Bounds: config.GeoBounds{MinLatitude: 41, MaxLatitude: 42, MinLongitude: -124, MaxLongitude: -123}}
	features, status, _, _, _, _ := s.BuildLayer(testCtx(), nowhere, LayerMessageSign)
	assert.Equal(t, "OK", status)
	assert.Empty(t, features)
}

func TestMessageSignLayer_FailLoud(t *testing.T) {
	d10 := loadSigns(t, "cms_d10_20261001.json")

	// The feed failing (frozen, empty, 500) is UNAVAILABLE, never "no signs".
	s := signService(&signSource{errs: map[int]error{10: cwwp2.ErrStaleFeed}}, 10)
	features, status, _, _, _, _ := s.BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	assert.Equal(t, "UNAVAILABLE", status)
	assert.Empty(t, features)

	// All or nothing across districts: a missing district would read as "no
	// signs" across its footprint.
	s = signService(&signSource{
		signs: map[int][]cwwp2.MessageSign{10: d10},
		errs:  map[int]error{9: errors.New("HTTP 500")},
	}, 10, 9)
	_, status, _, _, _, _ = s.BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	assert.Equal(t, "UNAVAILABLE", status)

	// Not configured: the layer has no other source.
	_, status, _, _, _, _ = signService(nil).BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	assert.Equal(t, "UNAVAILABLE", status)
	_, status, _, _, _, _ = signService(&signSource{}).BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	assert.Equal(t, "UNAVAILABLE", status)
}

// A sign with no position can't be ruled in or out of the area: the rest are
// served, STALE.
func TestMessageSignLayer_PositionlessDegrades(t *testing.T) {
	signs := loadSigns(t, "cms_d10_20261001.json")
	signs = append(signs, cwwp2.MessageSign{ID: "0", Display: cwwp2.DisplayBlank})
	s := signService(&signSource{signs: map[int][]cwwp2.MessageSign{10: signs}}, 10)
	features, status, _, _, _, _ := s.BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	assert.Equal(t, "STALE", status)
	assert.Len(t, features, 6)
}

// Cached for five minutes like chain_control; an upstream failure after a
// good fetch serves the last good signs as STALE.
func TestMessageSignLayer_CachedAndStaleOnError(t *testing.T) {
	src := &signSource{signs: map[int][]cwwp2.MessageSign{10: loadSigns(t, "cms_d10_20261001.json")}}
	s := signService(src, 10)
	_, status, _, _, _, _ := s.BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	assert.Equal(t, "OK", status)
	_, status, _, _, _, _ = s.BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	assert.Equal(t, "OK", status)
	assert.Equal(t, 1, src.calls, "second request served from cache")

	// Past the TTL with the portal frozen: last good signs, flagged STALE.
	s.cache.Backdate("hazard:ebbetts-pass:"+LayerMessageSign, 10*time.Minute)
	src.errs = map[int]error{10: cwwp2.ErrStaleFeed}
	features, status, last, _, _, _ := s.BuildLayer(testCtx(), ebbettsArea, LayerMessageSign)
	assert.Equal(t, "STALE", status)
	assert.Len(t, features, 6)
	assert.False(t, last.IsZero(), "STALE carries lastSourceUpdate")
	assert.Equal(t, 2, src.calls)
}
