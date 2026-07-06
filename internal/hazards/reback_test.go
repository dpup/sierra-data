// T14 re-back tests (docs/v2-implementation-plan.md): the five event-backed
// /api/v1 hazards layers served from the grid event store via
// hazards.StoreBackend, proving the fail-loud invariants survive the swap:
//
//	(a) caloes UNAVAILABLE + stored active zones  -> situation serves the
//	    stored count with evacuation_status STALE (the store is last-good);
//	(b) caloes down, never succeeded, no stored zones -> active_evacuations
//	    null + UNAVAILABLE ("an error never becomes a 0");
//	(c) caloes OK with zero events -> 0 + OK (the caveated confirmed-empty);
//	(d) area scoping: a second configured area does NOT inherit the first's
//	    events (event_places scoping through the real store);
//	(e) an event-backed layer never calls the live builder when a backend is
//	    present (no HTTP doer calls, no roads/weather API calls).
//
// The store-backed tests drive the REAL internal/store through a test adapter
// that mirrors cmd/server/gridadapter.go (package main is unimportable here):
// QueryEvents(ACTIVE+SCHEDULED) + gridapi.ProjectEvents +
// gridapi.LayerSourceStatus.
package hazards_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpup/prefab/logging"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/calfire"
	"github.com/dpup/sierra-data/internal/clients/caloes"
	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/clients/usgs"
	"github.com/dpup/sierra-data/internal/clients/wfigs"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/gridapi"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/store"
)

// rebackConfig has TWO areas so the scoping test can prove the second never
// inherits the first's events. Bounds are disjoint; zones differ.
func rebackConfig() *config.Config {
	return &config.Config{
		Hazards: config.HazardsConfig{
			Areas: []config.HazardArea{
				{
					ID: "one", Name: "Area One",
					Bounds: config.GeoBounds{MinLatitude: 37.7, MaxLatitude: 38.5, MinLongitude: -120.9, MaxLongitude: -119.2},
					Zones:  []string{"CAZ064"},
				},
				{
					ID: "two", Name: "Area Two",
					Bounds: config.GeoBounds{MinLatitude: 39.0, MaxLatitude: 39.8, MinLongitude: -121.5, MaxLongitude: -120.9},
					Zones:  []string{"CAZ999"},
				},
			},
		},
	}
}

// --- fakes ---

// rebackDoer fails every request and counts calls: store-backed layers must
// never reach a live upstream, so any count > 0 is a strangler leak. Atomic —
// /situation fans out layer builds concurrently.
type rebackDoer struct{ calls int32 }

func (d *rebackDoer) Do(*http.Request) (*http.Response, error) {
	atomic.AddInt32(&d.calls, 1)
	return nil, errors.New("unexpected live upstream fetch (layer must be store-backed)")
}
func (d *rebackDoer) count() int32 { return atomic.LoadInt32(&d.calls) }

// rebackRoads implements hazards.RoadsAPI. ListRoads succeeds empty (the
// road_segment condition layer stays live); ListIncidents counts and fails —
// the road_incident event layer must not consult it in store mode.
type rebackRoads struct{ incidentCalls int32 }

func (r *rebackRoads) ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error) {
	return &api.ListRoadsResponse{}, nil
}

func (r *rebackRoads) ListIncidents(context.Context, *api.ListIncidentsRequest) (*api.ListIncidentsResponse, error) {
	atomic.AddInt32(&r.incidentCalls, 1)
	return nil, errors.New("live incident feed must not be consulted in store mode")
}

// rebackWeather implements hazards.WeatherAPI. ListWeather succeeds empty
// (fire_weather stays live); ListWeatherAlerts counts and fails — the
// weather_alert event layer must not consult it in store mode.
type rebackWeather struct{ alertCalls int32 }

func (w *rebackWeather) ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error) {
	return &api.ListWeatherResponse{}, nil
}

func (w *rebackWeather) ListWeatherAlerts(context.Context, *api.ListWeatherAlertsRequest) (*api.ListWeatherAlertsResponse, error) {
	atomic.AddInt32(&w.alertCalls, 1)
	return nil, errors.New("live weather alerts must not be consulted in store mode")
}

// storeBackedGrid is the test twin of cmd/server/gridadapter.go's
// gridStoreBackend (that file is package main): real store scoped by placeID,
// ACTIVE+SCHEDULED only, T13 projection, source-registry health.
type storeBackedGrid struct{ st *store.Store }

var rebackLayerEnums = map[string]gridv1.Layer{
	hazards.LayerWildfire:     gridv1.Layer_WILDFIRE,
	hazards.LayerEvacuation:   gridv1.Layer_EVACUATION,
	hazards.LayerWeatherAlert: gridv1.Layer_WEATHER_ALERT,
	hazards.LayerEarthquake:   gridv1.Layer_EARTHQUAKE,
	hazards.LayerRoadIncident: gridv1.Layer_ROAD_INCIDENT,
}

func (b *storeBackedGrid) QueryActive(ctx context.Context, placeID, layer string) ([]hazards.Feature, string, time.Time, error) {
	lyr, ok := rebackLayerEnums[layer]
	if !ok {
		return nil, "", time.Time{}, fmt.Errorf("layer %q is not event-backed", layer)
	}
	events, next, err := b.st.QueryEvents(ctx, store.EventQuery{
		PlaceID:  placeID,
		Layers:   []gridv1.Layer{lyr},
		Statuses: []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED},
		PageSize: 200,
	})
	if err != nil {
		return nil, "", time.Time{}, err
	}
	if next != "" {
		return nil, "", time.Time{}, fmt.Errorf("test fixtures must fit one page")
	}
	sources, err := b.st.ListSources(ctx)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	status, lastUpdate := gridapi.LayerSourceStatus(sources, layer)
	return gridapi.ProjectEvents(layer, events), status, lastUpdate, nil
}

// recordingBackend is a pure fake for the builder-bypass test: it records
// every (placeID, layer) query and serves canned features with status OK.
type recordingBackend struct {
	mu    sync.Mutex
	calls []string
	feats map[string][]hazards.Feature
}

func (b *recordingBackend) QueryActive(_ context.Context, placeID, layer string) ([]hazards.Feature, string, time.Time, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, placeID+"/"+layer)
	return b.feats[layer], "OK", time.Time{}, nil
}

func (b *recordingBackend) recorded() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// --- fixture plumbing ---

type rebackFixture struct {
	svc     *hazards.Service
	doer    *rebackDoer // shared by the four event-layer upstream clients
	roads   *rebackRoads
	weather *rebackWeather
}

// offlineDoer fails every request with a neutral error — for the caltrans
// chain-control fetch, a condition layer that legitimately stays live (its
// UNAVAILABLE outcome is expected, not a strangler leak).
type offlineDoer struct{}

func (offlineDoer) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("offline test: no network")
}

// newRebackFixture builds a hazards.Service in store-backed mode with every
// live event-layer upstream trapped behind a counting, always-failing doer.
// The caltrans chain-control fetch (a condition layer, legitimately live)
// gets a separate neutral doer so it can't pollute the event-layer counter.
func newRebackFixture(t *testing.T, backend hazards.StoreBackend) *rebackFixture {
	t.Helper()
	f := &rebackFixture{doer: &rebackDoer{}, roads: &rebackRoads{}, weather: &rebackWeather{}}
	ct := caltrans.NewFeedParser()
	ct.HTTPClient = offlineDoer{}
	f.svc = hazards.NewServiceWithAPIs(rebackConfig(), f.roads, f.weather, ct, nil, backend).
		WithClients(
			usgs.NewClientWithHTTPDoer("https://usgs.test", f.doer),
			calfire.NewClientWithHTTPDoer("https://calfire.test", f.doer),
			wfigs.NewClientWithHTTPDoer("https://wfigs.test", f.doer),
			caloes.NewClientWithHTTPDoer("https://caloes.test", f.doer),
		)
	return f
}

// assertNoLiveEventFetch fails if any event-backed layer touched a live feed.
func (f *rebackFixture) assertNoLiveEventFetch(t *testing.T) {
	t.Helper()
	require.Zero(t, f.doer.count(), "store-backed layers must not fetch live upstreams")
	require.Zero(t, atomic.LoadInt32(&f.roads.incidentCalls), "road_incident must not consult the live incident feed")
	require.Zero(t, atomic.LoadInt32(&f.weather.alertCalls), "weather_alert must not consult the live alert feed")
}

// newRebackStore opens a temp store seeded with the source rows the tests
// attribute events to (events.source_id is a foreign key into sources).
func newRebackStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.SeedSources(context.Background(), []store.SourceSeed{
		{ID: "caloes", Name: "Cal OES", PollInterval: 2 * time.Minute},
		{ID: "usgs", Name: "USGS", PollInterval: 5 * time.Minute},
	}))
	return st
}

// evacEvent is an ACTIVE evacuation-order event pinned to one place. No
// geometry: place scoping flows through the explicit place_ids preset, which
// UpsertEvent writes into event_places.
func evacEvent(zone, placeID string) *gridv1.Event {
	return &gridv1.Event{
		Id:         "evac:" + zone,
		Layer:      gridv1.Layer_EVACUATION,
		Category:   "order",
		Severity:   gridv1.Severity_SEVERE,
		Status:     gridv1.EventStatus_ACTIVE,
		Headline:   "Evacuation Order — " + zone,
		AreaLabel:  zone,
		PlaceIds:   []string{placeID},
		Provenance: &gridv1.Provenance{SourceId: "caloes", SourceName: "Cal OES"},
		Detail: &gridv1.Event_Evacuation{Evacuation: &gridv1.EvacuationDetail{
			ZoneId: zone, Level: "ORDER", EventType: "Fire", County: "Calaveras",
		}},
	}
}

// situationOut decodes the summary fields under test. ActiveEvacuations is
// *int so JSON null (the fail-loud "unknown") is distinguishable from 0.
type situationOut struct {
	Summary struct {
		ActiveEvacuations *int   `json:"active_evacuations"`
		EvacuationStatus  string `json:"evacuation_status"`
	} `json:"summary"`
	Layers []struct {
		Layer        string `json:"layer"`
		SourceStatus string `json:"source_status"`
		FeatureCount int    `json:"feature_count"`
	} `json:"layers"`
}

func getSituation(t *testing.T, svc *hazards.Service, area string) situationOut {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, hazards.SituationPrefix+area, nil)
	req = req.WithContext(logging.EnsureLogger(req.Context()))
	rr := httptest.NewRecorder()
	svc.ServeSituation(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out situationOut
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	return out
}

type layerOut struct {
	Features []map[string]any `json:"features"`
	Metadata struct {
		SourceStatus     string `json:"source_status"`
		LastSourceUpdate string `json:"last_source_update"`
		Attribution      string `json:"attribution"`
		SourceURL        string `json:"source_url"`
	} `json:"metadata"`
}

func getLayer(t *testing.T, svc *hazards.Service, area, layer string) layerOut {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, hazards.HandlerPrefix+area+"/"+layer+".geojson", nil)
	req = req.WithContext(logging.EnsureLogger(req.Context()))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out layerOut
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	return out
}

// --- the tests ---

// (a) Cal OES down but the store holds active zones from an earlier good
// poll: the layer and the situation serve the stored zones as STALE — richer
// than the old 5m TTL cache, and never a fabricated clear state.
func TestReback_EvacUnavailableWithStoredZones_ServesStaleCount(t *testing.T) {
	ctx := context.Background()
	st := newRebackStore(t)
	for _, ev := range []*gridv1.Event{evacEvent("CAL-E-046", "area:one"), evacEvent("CAL-E-047", "area:one")} {
		_, err := st.UpsertEvent(ctx, ev)
		require.NoError(t, err)
	}
	// The source row has never succeeded, so a failed poll drives it straight
	// to UNAVAILABLE — the harshest health state, with data still in the store.
	require.NoError(t, st.RecordAttempt(ctx, "caloes", errors.New("caloes: 502")))

	f := newRebackFixture(t, &storeBackedGrid{st: st})

	sit := getSituation(t, f.svc, "one")
	require.Equal(t, "STALE", sit.Summary.EvacuationStatus)
	require.NotNil(t, sit.Summary.ActiveEvacuations, "stored zones must surface a count, not null")
	require.Equal(t, 2, *sit.Summary.ActiveEvacuations)

	lo := getLayer(t, f.svc, "one", hazards.LayerEvacuation)
	require.Equal(t, "STALE", lo.Metadata.SourceStatus)
	require.Len(t, lo.Features, 2)
	// The life-safety framing survives every state: Genasys link + caveat.
	require.Equal(t, caloes.SourceURL, lo.Metadata.SourceURL)
	require.NotEmpty(t, lo.Metadata.Attribution)

	f.assertNoLiveEventFetch(t)
}

// (b) Cal OES down and the store has nothing (never succeeded): the invariant
// "an error never becomes a 0" — situation reports null + UNAVAILABLE.
func TestReback_EvacNeverSucceeded_NullAndUnavailable(t *testing.T) {
	ctx := context.Background()
	st := newRebackStore(t)
	require.NoError(t, st.RecordAttempt(ctx, "caloes", errors.New("caloes: connection refused")))

	f := newRebackFixture(t, &storeBackedGrid{st: st})

	sit := getSituation(t, f.svc, "one")
	require.Equal(t, "UNAVAILABLE", sit.Summary.EvacuationStatus)
	require.Nil(t, sit.Summary.ActiveEvacuations, "an error must render as null (unknown), never 0")

	lo := getLayer(t, f.svc, "one", hazards.LayerEvacuation)
	require.Equal(t, "UNAVAILABLE", lo.Metadata.SourceStatus)
	require.Empty(t, lo.Features)
	require.Equal(t, caloes.SourceURL, lo.Metadata.SourceURL, "Genasys link must survive UNAVAILABLE")

	f.assertNoLiveEventFetch(t)
}

// (c) Cal OES healthy with zero active zones: the caveated confirmed-empty —
// 0 + OK, deliberately distinguishable from (b)'s null + UNAVAILABLE.
func TestReback_EvacOKZeroEvents_ZeroAndOK(t *testing.T) {
	ctx := context.Background()
	st := newRebackStore(t)
	require.NoError(t, st.RecordAttempt(ctx, "caloes", nil))

	f := newRebackFixture(t, &storeBackedGrid{st: st})

	sit := getSituation(t, f.svc, "one")
	require.Equal(t, "OK", sit.Summary.EvacuationStatus)
	require.NotNil(t, sit.Summary.ActiveEvacuations, "a clean empty is a confirmed 0, not unknown")
	require.Equal(t, 0, *sit.Summary.ActiveEvacuations)

	lo := getLayer(t, f.svc, "one", hazards.LayerEvacuation)
	require.Equal(t, "OK", lo.Metadata.SourceStatus)
	require.Empty(t, lo.Features)

	f.assertNoLiveEventFetch(t)
}

// (d) Events attach to places (event_places), and the layer query is scoped
// by "area:{id}": area two must not inherit area one's zones.
func TestReback_AreaScoping_SecondAreaDoesNotInheritEvents(t *testing.T) {
	ctx := context.Background()
	st := newRebackStore(t)
	_, err := st.UpsertEvent(ctx, evacEvent("CAL-E-046", "area:one"))
	require.NoError(t, err)
	require.NoError(t, st.RecordAttempt(ctx, "caloes", nil))

	f := newRebackFixture(t, &storeBackedGrid{st: st})

	one := getLayer(t, f.svc, "one", hazards.LayerEvacuation)
	require.Len(t, one.Features, 1)
	require.Equal(t, "evac:CAL-E-046", one.Features[0]["properties"].(map[string]any)["id"])

	two := getLayer(t, f.svc, "two", hazards.LayerEvacuation)
	require.Empty(t, two.Features, "area two must not inherit area one's events")
	require.Equal(t, "OK", two.Metadata.SourceStatus, "healthy source + no zones in this area is a clean empty")

	sitTwo := getSituation(t, f.svc, "two")
	require.NotNil(t, sitTwo.Summary.ActiveEvacuations)
	require.Equal(t, 0, *sitTwo.Summary.ActiveEvacuations)

	f.assertNoLiveEventFetch(t)
}

// (e) With a backend present, every event-backed layer serves from it and the
// live builders are never invoked: no upstream HTTP, no roads/weather API
// event calls, and the backend sees exactly the "area:{id}"-scoped queries.
func TestReback_BackendBypassesLiveBuilders(t *testing.T) {
	backend := &recordingBackend{feats: map[string][]hazards.Feature{
		hazards.LayerEarthquake: {{
			Type: "Feature",
			Properties: hazards.Properties{
				ID: "usgs:nc123", Layer: hazards.LayerEarthquake, Kind: "Earthquake",
				Severity: "MODERATE", SeverityRank: 2, Headline: "M4.2 — 10km NE of Murphys, CA",
				Source: hazards.Source{ID: "usgs", Name: "USGS"},
			},
		}},
	}}
	f := newRebackFixture(t, backend)

	eventLayers := []string{
		hazards.LayerWildfire, hazards.LayerEvacuation, hazards.LayerWeatherAlert,
		hazards.LayerEarthquake, hazards.LayerRoadIncident,
	}
	for _, layer := range eventLayers {
		lo := getLayer(t, f.svc, "one", layer)
		require.Equal(t, "OK", lo.Metadata.SourceStatus, layer)
	}

	// The backend's canned earthquake feature is what got served.
	eq := getLayer(t, f.svc, "one", hazards.LayerEarthquake)
	require.Len(t, eq.Features, 1)
	require.Equal(t, "usgs:nc123", eq.Features[0]["properties"].(map[string]any)["id"])

	// Every event layer hit the backend with the area's place id...
	got := backend.recorded()
	for _, layer := range eventLayers {
		require.Contains(t, got, "area:one/"+layer)
	}
	// ...and nothing leaked to a live builder.
	f.assertNoLiveEventFetch(t)

	// The situation fan-out also stays store-backed for event layers.
	sit := getSituation(t, f.svc, "one")
	require.Equal(t, "OK", sit.Summary.EvacuationStatus)
	f.assertNoLiveEventFetch(t)
}
