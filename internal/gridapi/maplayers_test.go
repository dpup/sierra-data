package gridapi

// Tests for GET /v1/places/{place}/map/{layer}.geojson: store-backed event
// layers (features + the source-health metadata matrix), condition-layer
// delegation to the hazards builders (via a fake and via a real
// hazards.Service over stub roads/weather), non-area place synthesis, and
// the envelope plumbing (content type, ETag/304, unknown-layer 404). All
// offline — the store is a temp SQLite file, upstreams are fakes.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/store"
)

// fcBody mirrors the shipped GeoJSON envelope for assertions.
type fcBody struct {
	Type     string `json:"type"`
	Features []struct {
		Type       string          `json:"type"`
		Geometry   json.RawMessage `json:"geometry"`
		Properties struct {
			ID           string `json:"id"`
			Layer        string `json:"layer"`
			Severity     string `json:"severity"`
			SeverityRank int    `json:"severityRank"`
			Headline     string `json:"headline"`
		} `json:"properties"`
	} `json:"features"`
	Metadata struct {
		Layer            string `json:"layer"`
		Area             string `json:"area"`
		GeneratedAt      string `json:"generatedAt"`
		SourceStatus     string `json:"sourceStatus"`
		LastSourceUpdate string `json:"lastSourceUpdate"`
		Attribution      string `json:"attribution"`
		SourceURL        string `json:"sourceUrl"`
		SchemaVersion    int    `json:"schemaVersion"`
	} `json:"metadata"`
}

func getFC(t *testing.T, s *Service, path string) fcBody {
	t.Helper()
	rec := get(t, s, path)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, "application/geo+json", rec.Header().Get("Content-Type"))
	var fc fcBody
	decode(t, rec, &fc)
	require.Equal(t, "FeatureCollection", fc.Type)
	return fc
}

// recordOK / recordErr drive the source-health rows the status matrix reads.
func recordOK(t *testing.T, st *store.Store, id string) {
	t.Helper()
	require.NoError(t, st.RecordAttempt(context.Background(), id, nil))
}

func recordErr(t *testing.T, st *store.Store, id string) {
	t.Helper()
	require.NoError(t, st.RecordAttempt(context.Background(), id, errors.New("upstream 500")))
}

// seedSource adds a registry row seedEvents doesn't (wfigs, caloes, caltrans).
func seedSource(t *testing.T, st *store.Store, id string) {
	t.Helper()
	require.NoError(t, st.SeedSources(context.Background(),
		[]store.SourceSeed{{ID: id, Name: id, PollInterval: 5 * time.Minute}}))
}

// TestMapLayer_EventLayerEnvelope: a store-backed layer serves the place's
// ACTIVE events through the shared projection with the shipped metadata block.
func TestMapLayer_EventLayerEnvelope(t *testing.T) {
	s := newTestService(t)
	recordOK(t, s.Store, "usgs")

	fc := getFC(t, s, "/v1/places/calaveras/map/earthquake.geojson")
	require.Len(t, fc.Features, 1)
	f := fc.Features[0]
	assert.Equal(t, "Feature", f.Type)
	assert.Equal(t, "usgs:q1", f.Properties.ID)
	assert.Equal(t, "earthquake", f.Properties.Layer)
	assert.Equal(t, "MODERATE", f.Properties.Severity)
	assert.Equal(t, 2, f.Properties.SeverityRank)
	assert.JSONEq(t, `{"type":"Point","coordinates":[-120.1,38.4]}`, string(f.Geometry))

	md := fc.Metadata
	assert.Equal(t, "earthquake", md.Layer)
	assert.Equal(t, "calaveras", md.Area)
	assert.Equal(t, base.Format(time.RFC3339), md.GeneratedAt) // injected clock
	assert.Equal(t, "OK", md.SourceStatus)
	assert.Empty(t, md.LastSourceUpdate, "OK needs no freshness caveat")
	assert.Equal(t, 1, md.SchemaVersion)
}

// TestMapLayer_LifecycleScoping: the live map serves ACTIVE+SCHEDULED only —
// a RESOLVED incident is history, a SCHEDULED alert is already shown. Also
// covers place scoping via preset place_ids (zone-carrying weather alerts).
func TestMapLayer_LifecycleScoping(t *testing.T) {
	s := newTestService(t)

	// chp:i1 is RESOLVED => road_incident renders empty (but valid).
	fc := getFC(t, s, "/v1/places/calaveras/map/road_incident.geojson")
	assert.Empty(t, fc.Features)

	// wx:a1 is SCHEDULED with place_ids [area:calaveras] => included.
	fc = getFC(t, s, "/v1/places/calaveras/map/weather_alert.geojson")
	require.Len(t, fc.Features, 1)
	assert.Equal(t, "wx:a1", fc.Features[0].Properties.ID)
}

// TestMapLayer_SourceStatusMatrix pins the metadata.source_status derivation
// against source rows manipulated through store.RecordAttempt.
func TestMapLayer_SourceStatusMatrix(t *testing.T) {
	t.Run("single source OK", func(t *testing.T) {
		s := newTestService(t)
		recordOK(t, s.Store, "usgs")
		md := getFC(t, s, "/v1/places/calaveras/map/earthquake.geojson").Metadata
		assert.Equal(t, "OK", md.SourceStatus)
		assert.Empty(t, md.LastSourceUpdate)
	})

	t.Run("single source STALE after failure within window", func(t *testing.T) {
		s := newTestService(t)
		recordOK(t, s.Store, "usgs")
		recordErr(t, s.Store, "usgs") // within stale_after of the success
		md := getFC(t, s, "/v1/places/calaveras/map/earthquake.geojson").Metadata
		assert.Equal(t, "STALE", md.SourceStatus)
		assert.NotEmpty(t, md.LastSourceUpdate, "degraded must carry last_success_at")
	})

	t.Run("source down with stored events degrades to STALE", func(t *testing.T) {
		// The store holds last-good data (usgs:q1): a down source serves it
		// as STALE, exactly like the store-backed /api/v1/hazards path —
		// UNAVAILABLE with features would make a contract-following map
		// client hide live data.
		s := newTestService(t)
		recordErr(t, s.Store, "usgs")
		fc := getFC(t, s, "/v1/places/calaveras/map/earthquake.geojson")
		assert.Equal(t, "STALE", fc.Metadata.SourceStatus)
		require.Len(t, fc.Features, 1, "stored last-good features are still served")
		assert.Empty(t, fc.Metadata.LastSourceUpdate, "never succeeded: no fetch time to vouch for")
	})

	t.Run("source down with nothing stored is UNAVAILABLE", func(t *testing.T) {
		// No stored evacuation events: nothing vouches for an empty feed.
		s := newTestService(t)
		seedSource(t, s.Store, "caloes")
		recordErr(t, s.Store, "caloes")
		fc := getFC(t, s, "/v1/places/calaveras/map/evacuation.geojson")
		assert.Equal(t, "UNAVAILABLE", fc.Metadata.SourceStatus)
		assert.Empty(t, fc.Features, "UNAVAILABLE always means empty features")
		assert.Empty(t, fc.Metadata.LastSourceUpdate, "the UNAVAILABLE envelope never carries freshness")
	})

	t.Run("never-polled source with stored events is STALE", func(t *testing.T) {
		// Seeded but no RecordAttempt yet: health unknown is not OK, but the
		// stored event (usgs:q1) still degrades the answer to STALE, not
		// UNAVAILABLE — same store state must read the same as /api/v1.
		s := newTestService(t)
		md := getFC(t, s, "/v1/places/calaveras/map/earthquake.geojson").Metadata
		assert.Equal(t, "STALE", md.SourceStatus)
	})

	t.Run("multi source one down is STALE", func(t *testing.T) {
		s := newTestService(t)
		seedSource(t, s.Store, "wfigs")
		recordOK(t, s.Store, "calfire")
		recordErr(t, s.Store, "wfigs") // never succeeded => down
		md := getFC(t, s, "/v1/places/calaveras/map/wildfire.geojson").Metadata
		assert.Equal(t, "STALE", md.SourceStatus, "partial data must not present as complete")
		assert.NotEmpty(t, md.LastSourceUpdate, "carries the surviving source's last success")
	})

	t.Run("multi source all down with stored events is STALE", func(t *testing.T) {
		s := newTestService(t)
		seedSource(t, s.Store, "wfigs")
		recordErr(t, s.Store, "calfire")
		recordErr(t, s.Store, "wfigs")
		fc := getFC(t, s, "/v1/places/calaveras/map/wildfire.geojson")
		assert.Equal(t, "STALE", fc.Metadata.SourceStatus, "stored calfire:f1 is last-good data")
		require.Len(t, fc.Features, 1)
	})

	t.Run("multi source all down with nothing stored is UNAVAILABLE", func(t *testing.T) {
		s := newTestService(t)
		seedSource(t, s.Store, "caltrans")
		recordErr(t, s.Store, "chp")
		recordErr(t, s.Store, "caltrans")
		// chp:i1 is RESOLVED, so the live road_incident layer has no data.
		fc := getFC(t, s, "/v1/places/calaveras/map/road_incident.geojson")
		assert.Equal(t, "UNAVAILABLE", fc.Metadata.SourceStatus)
		assert.Empty(t, fc.Features)
	})

	t.Run("multi source all OK is OK", func(t *testing.T) {
		s := newTestService(t)
		seedSource(t, s.Store, "wfigs")
		recordOK(t, s.Store, "calfire")
		recordOK(t, s.Store, "wfigs")
		md := getFC(t, s, "/v1/places/calaveras/map/wildfire.geojson").Metadata
		assert.Equal(t, "OK", md.SourceStatus)
	})
}

// TestLayerSourceStatus_Unit pins the pure derivation, including the rows the
// endpoint matrix can't easily produce (missing registry row, unknown layer).
func TestLayerSourceStatus_Unit(t *testing.T) {
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	src := func(id string, st gridv1.SourceStatus, success time.Time) *gridv1.Source {
		s := &gridv1.Source{Id: id, Status: st}
		if !success.IsZero() {
			s.LastSuccessAt = timestamppb.New(success)
		}
		return s
	}

	status, last := LayerSourceStatus(nil, "earthquake")
	assert.Equal(t, "UNAVAILABLE", status, "missing registry row counts as down")
	assert.True(t, last.IsZero())

	status, _ = LayerSourceStatus([]*gridv1.Source{src("usgs", gridv1.SourceStatus_STALE, now)}, "earthquake")
	assert.Equal(t, "STALE", status)

	// Multi-source: STALE carries the MOST RECENT last_success_at.
	status, last = LayerSourceStatus([]*gridv1.Source{
		src("calfire", gridv1.SourceStatus_OK, now),
		src("wfigs", gridv1.SourceStatus_UNAVAILABLE, now.Add(-time.Hour)),
	}, "wildfire")
	assert.Equal(t, "STALE", status)
	assert.Equal(t, now, last)

	// Both merely STALE is still STALE, not UNAVAILABLE.
	status, _ = LayerSourceStatus([]*gridv1.Source{
		src("chp", gridv1.SourceStatus_STALE, now),
		src("caltrans", gridv1.SourceStatus_STALE, now),
	}, "road_incident")
	assert.Equal(t, "STALE", status)

	status, _ = LayerSourceStatus(nil, "road_segment")
	assert.Equal(t, "UNAVAILABLE", status, "no source rows vouch for a non-event layer")
}

// TestMapLayer_EvacuationMetadataEveryState: the evacuation layer carries the
// Cal OES attribution + authoritative Genasys source_url in EVERY source
// state — the life-safety framing must not depend on health.
func TestMapLayer_EvacuationMetadataEveryState(t *testing.T) {
	s := newTestService(t)

	// caloes not even seeded => UNAVAILABLE, links still present.
	md := getFC(t, s, "/v1/places/calaveras/map/evacuation.geojson").Metadata
	assert.Equal(t, "UNAVAILABLE", md.SourceStatus)
	assert.Equal(t, "Cal OES / California County Governments — reference only", md.Attribution)
	assert.Equal(t, "https://protect.genasys.com/", md.SourceURL)

	seedSource(t, s.Store, "caloes")
	recordOK(t, s.Store, "caloes")
	md = getFC(t, s, "/v1/places/calaveras/map/evacuation.geojson").Metadata
	assert.Equal(t, "OK", md.SourceStatus)
	assert.Equal(t, "https://protect.genasys.com/", md.SourceURL)
	assert.NotEmpty(t, md.Attribution)
}

// fakeHazards is the hazardsBuilder stub for the condition-layer delegation.
type fakeHazards struct {
	gotArea  config.HazardArea
	gotLayer string

	features    []hazards.Feature
	status      string
	last        time.Time
	attribution string
	sourceURL   string
	ok          bool
}

func (f *fakeHazards) BuildLayer(_ context.Context, area config.HazardArea, layer string) ([]hazards.Feature, string, time.Time, string, string, bool) {
	f.gotArea, f.gotLayer = area, layer
	return f.features, f.status, f.last, f.attribution, f.sourceURL, f.ok
}

// serveCondition drives serveConditionLayer directly with a fake builder (the
// router path binds the concrete s.Hazards; the interface seam is here).
func serveCondition(t *testing.T, s *Service, hb hazardsBuilder, placeKey, layer string) *httptest.ResponseRecorder {
	t.Helper()
	place, err := s.Store.GetPlace(context.Background(), placeKey)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/v1/places/"+placeKey+"/map/"+layer+".geojson", nil)
	rec := httptest.NewRecorder()
	s.serveConditionLayer(rec, req, hb, place, layer)
	return rec
}

// TestMapLayer_ConditionDelegation: a condition layer passes the resolved
// area + layer to the hazards builder and re-emits its result verbatim in the
// shipped envelope.
func TestMapLayer_ConditionDelegation(t *testing.T) {
	s := newTestService(t)
	stale := base.Add(-30 * time.Minute)
	p := hazards.Properties{ID: "road:hwy-4", Layer: hazards.LayerRoadSegment, Headline: "Hwy 4"}
	fake := &fakeHazards{
		features: []hazards.Feature{{Type: "Feature", Geometry: nil, Properties: p}},
		status:   "STALE",
		last:     stale,
		ok:       true,
	}

	rec := serveCondition(t, s, fake, "calaveras", "road_segment")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, "application/geo+json", rec.Header().Get("Content-Type"))

	// AREA place: the delegate must receive the CONFIGURED area verbatim.
	assert.Equal(t, "road_segment", fake.gotLayer)
	assert.Equal(t, s.Cfg.Hazards.Areas[0], fake.gotArea)

	var fc fcBody
	decode(t, rec, &fc)
	require.Len(t, fc.Features, 1)
	assert.Equal(t, "road:hwy-4", fc.Features[0].Properties.ID)
	assert.Equal(t, "STALE", fc.Metadata.SourceStatus)
	assert.Equal(t, stale.Format(time.RFC3339), fc.Metadata.LastSourceUpdate)
	assert.Equal(t, "calaveras", fc.Metadata.Area)
	assert.Equal(t, 1, fc.Metadata.SchemaVersion)
}

// TestMapLayer_NonAreaPlaceSynthesis: a non-AREA place (the town of Arnold, a
// point) synthesizes a HazardArea — bounds from the place bbox, Zones as the
// deduped union across intersecting configured areas, IncidentArea inherited
// from the first intersecting one.
func TestMapLayer_NonAreaPlaceSynthesis(t *testing.T) {
	s := newTestService(t)
	// testConfig areas carry no zones/incident areas; add them here. Arnold's
	// point (38.2552, -120.3512) is inside both areas' bounds.
	s.Cfg.Hazards.Areas[0].Zones = []string{"CAZ064", "CAZ065"}
	s.Cfg.Hazards.Areas[0].IncidentArea = "mother-lode"
	s.Cfg.Hazards.Areas[1].Zones = []string{"CAZ065", "CAZ067"}
	s.Cfg.Hazards.Areas[1].IncidentArea = "high-country"

	fake := &fakeHazards{status: "OK", ok: true}
	rec := serveCondition(t, s, fake, "arnold", "fire_weather")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	got := fake.gotArea
	assert.Equal(t, "arnold", got.ID)
	assert.Equal(t, "mother-lode", got.IncidentArea, "inherited from the first intersecting area")
	assert.Equal(t, []string{"CAZ064", "CAZ065", "CAZ067"}, got.Zones, "deduped union of intersecting areas' zones")
	// A point place's bbox is degenerate but correct.
	assert.InDelta(t, 38.2552, got.Bounds.MinLatitude, 1e-6)
	assert.InDelta(t, 38.2552, got.Bounds.MaxLatitude, 1e-6)
	assert.InDelta(t, -120.3512, got.Bounds.MinLongitude, 1e-6)
	assert.InDelta(t, -120.3512, got.Bounds.MaxLongitude, 1e-6)

	var fc fcBody
	decode(t, rec, &fc)
	assert.Equal(t, "arnold", fc.Metadata.Area, "metadata.area is the place slug")

	// The event-layer path also works for non-area places (router end to end):
	// nothing attaches to a point place, so an honest empty collection.
	fc = getFC(t, s, "/v1/places/arnold/map/earthquake.geojson")
	assert.Empty(t, fc.Features)
	assert.Equal(t, "arnold", fc.Metadata.Area)
}

// hazRoads / hazWeather satisfy hazards.RoadsAPI / hazards.WeatherAPI for the
// real-hazards.Service router test (the gridapi fakes are narrower slices).
type hazRoads struct{ resp *api.ListRoadsResponse }

func (h *hazRoads) ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error) {
	return h.resp, nil
}
func (h *hazRoads) ListIncidents(context.Context, *api.ListIncidentsRequest) (*api.ListIncidentsResponse, error) {
	return &api.ListIncidentsResponse{}, nil
}

type hazWeather struct{ resp *api.ListWeatherResponse }

func (h *hazWeather) ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error) {
	return h.resp, nil
}
func (h *hazWeather) ListWeatherAlerts(context.Context, *api.ListWeatherAlertsRequest) (*api.ListWeatherAlertsResponse, error) {
	return &api.ListWeatherAlertsResponse{}, nil
}

// TestMapLayer_ConditionThroughRouter: full path with a REAL hazards.Service
// (over stub roads/weather) proving *hazards.Service satisfies the delegation
// seam and the router routes condition layers to it.
func TestMapLayer_ConditionThroughRouter(t *testing.T) {
	s := newTestService(t)
	s.Hazards = hazards.NewServiceWithAPIs(s.Cfg, &hazRoads{resp: roadsResp()}, &hazWeather{resp: weatherResp()}, nil, nil)

	fc := getFC(t, s, "/v1/places/calaveras/map/road_segment.geojson")
	assert.Equal(t, "OK", fc.Metadata.SourceStatus)
	require.Len(t, fc.Features, 2, "both monitored roads have an endpoint in calaveras")
	assert.Equal(t, "road:hwy-4", fc.Features[0].Properties.ID)
	assert.Equal(t, "road:hwy-108", fc.Features[1].Properties.ID)
}

// TestMapLayer_ConditionUnwired: a Service constructed without a hazards
// service (the entity-only wiring) fails loud on condition layers.
func TestMapLayer_ConditionUnwired(t *testing.T) {
	s := newTestService(t) // Hazards is nil
	rec := get(t, s, "/v1/places/calaveras/map/road_segment.geojson")
	requireStatus(t, rec, http.StatusNotImplemented, 12)
}

// TestMapLayer_NotFound: unknown layers and unknown places 404 with the
// google.rpc.Status body.
func TestMapLayer_NotFound(t *testing.T) {
	s := newTestService(t)

	rec := get(t, s, "/v1/places/calaveras/map/volcano.geojson")
	sb := requireStatus(t, rec, http.StatusNotFound, 5)
	assert.Contains(t, sb.Message, "volcano")

	rec = get(t, s, "/v1/places/atlantis/map/earthquake.geojson")
	sb = requireStatus(t, rec, http.StatusNotFound, 5)
	assert.Contains(t, sb.Message, "atlantis")
}

// TestMapLayer_ETag: bodies are deterministic under the injected clock, so
// the strong ETag revalidates to 304 with headers intact.
func TestMapLayer_ETag(t *testing.T) {
	s := newTestService(t)
	recordOK(t, s.Store, "usgs")

	rec := get(t, s, "/v1/places/calaveras/map/earthquake.geojson")
	require.Equal(t, http.StatusOK, rec.Code)
	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag)
	assert.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))

	rec = get(t, s, "/v1/places/calaveras/map/earthquake.geojson", "If-None-Match", etag)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Equal(t, etag, rec.Header().Get("ETag"))
}
