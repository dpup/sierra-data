package gridapi

// Shared test scaffolding: a seeded temp store (real internal/places seed +
// hand-upserted events) behind a Service with tiny fakes for the roads,
// weather and census dependencies. All offline — no network, no real APIs.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/places"
	"github.com/dpup/info.ersn.net/server/internal/store"
)

// base is the fixed reference time all seeded events hang off.
var base = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// testConfig mirrors the prefab.yaml shapes the API consumes: two overlapping
// hazard areas (calaveras matches the prod slug; high-country covers only the
// northeast), two weather towns, two monitored roads.
func testConfig() *config.Config {
	return &config.Config{
		Hazards: config.HazardsConfig{Areas: []config.HazardArea{
			{
				ID:   "calaveras",
				Name: "Calaveras",
				Bounds: config.GeoBounds{
					MinLatitude: 37.8, MaxLatitude: 38.55,
					MinLongitude: -120.9, MaxLongitude: -120.0,
				},
				ScannerFeeds: []config.ScannerFeed{
					{FeedID: "13524", ChannelLabel: "Sheriff / CAL FIRE Dispatch", Agency: "Calaveras SO"},
					{FeedID: "28469", ChannelLabel: "Fire / USFS", Agency: "CAL FIRE / USFS"},
				},
			},
			{
				ID:   "high-country",
				Name: "High Country",
				Bounds: config.GeoBounds{
					MinLatitude: 38.2, MaxLatitude: 38.8,
					MinLongitude: -120.4, MaxLongitude: -119.5,
				},
				ScannerFeeds: []config.ScannerFeed{
					{FeedID: "28469", ChannelLabel: "Fire / USFS", Agency: "CAL FIRE / USFS"},
					{FeedID: "90001", ChannelLabel: "Alpine Ops"}, // no agency: key must be omitted
				},
			},
		}},
		Weather: config.WeatherConfig{Locations: []config.WeatherLocation{
			// Arnold is inside both areas' bounds; Murphys only inside calaveras.
			{ID: "arnold", Name: "Arnold", Coordinates: config.Coordinates{Latitude: 38.2552, Longitude: -120.3512}},
			{ID: "murphys", Name: "Murphys", Coordinates: config.Coordinates{Latitude: 38.1377, Longitude: -120.4610}},
		}},
		Roads: config.RoadsConfig{MonitoredRoads: []config.MonitoredRoad{
			// hwy-4's destination is inside high-country; hwy-108 is entirely
			// south of it. Both are inside calaveras' box.
			{ID: "hwy-4", Name: "Hwy 4", Section: "Angels Camp to Arnold",
				Origin:      config.Coordinates{Latitude: 38.07, Longitude: -120.55},
				Destination: config.Coordinates{Latitude: 38.25, Longitude: -120.35}},
			{ID: "hwy-108", Name: "Hwy 108", Section: "Sonora to Twain Harte",
				Origin:      config.Coordinates{Latitude: 37.98, Longitude: -120.38},
				Destination: config.Coordinates{Latitude: 38.10, Longitude: -119.90}},
		}},
	}
}

// fakeRoads / fakeWeather / fakeCensus are the narrow-interface stubs.
type fakeRoads struct {
	resp *api.ListRoadsResponse
	err  error
}

func (f *fakeRoads) ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error) {
	return f.resp, f.err
}

type fakeWeather struct {
	resp *api.ListWeatherResponse
	err  error
}

func (f *fakeWeather) ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error) {
	return f.resp, f.err
}

type fakeCensus struct {
	lat, lng float64
	matched  string
	err      error
}

func (f *fakeCensus) Geocode(context.Context, string) (float64, float64, string, error) {
	if f.err != nil {
		return 0, 0, "", f.err
	}
	return f.lat, f.lng, f.matched, nil
}

func roadsResp() *api.ListRoadsResponse {
	return &api.ListRoadsResponse{
		Roads: []*api.Road{
			{Id: "hwy-4", Name: "Hwy 4", Status: api.RoadStatus_OPEN},
			{Id: "hwy-108", Name: "Hwy 108", Status: api.RoadStatus_RESTRICTED, StatusExplanation: "One-way traffic control"},
		},
		LastUpdated: timestamppb.New(base),
	}
}

func weatherResp() *api.ListWeatherResponse {
	return &api.ListWeatherResponse{
		WeatherData: []*api.WeatherData{
			{
				LocationId: "arnold", LocationName: "Arnold", WeatherMain: "Clear",
				TemperatureCelsius: 28,
				Alerts:             []*api.WeatherAlert{{Id: "nws-1", Event: "Red Flag Warning"}},
			},
			{
				LocationId: "murphys", LocationName: "Murphys", WeatherMain: "Clouds",
				TemperatureCelsius: 31,
				Alerts:             []*api.WeatherAlert{{Id: "nws-1", Event: "Red Flag Warning"}},
			},
		},
		LastUpdated: timestamppb.New(base),
		FireWeather: &api.FireWeather{State: api.FireWeatherState_RED_FLAG, SourceEvent: "Red Flag Warning"},
	}
}

// pointGeom builds the Geometry for an internal (lat, lng) point.
func pointGeom(lat, lng float64) *gridv1.Geometry {
	return &gridv1.Geometry{
		Geojson:  []byte(`{"type":"Point","coordinates":[` + jsonFloat(lng) + `,` + jsonFloat(lat) + `]}`),
		Bbox:     &gridv1.BoundingBox{MinLat: lat, MinLng: lng, MaxLat: lat, MaxLng: lng},
		Centroid: &gridv1.LatLng{Lat: lat, Lng: lng},
	}
}

func jsonFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// seedEvents writes the fixture events. Geometry placement (see testConfig):
//
//	usgs:q1    EARTHQUAKE    MODERATE ACTIVE    (38.4, -120.1)  in calaveras + high-country; upserted twice (2 revisions)
//	calfire:f1 WILDFIRE      SEVERE   ACTIVE    (38.0, -120.5)  in calaveras only
//	wx:a1      WEATHER_ALERT MODERATE SCHEDULED no geometry; preset place_ids [area:calaveras]
//	chp:i1     ROAD_INCIDENT MINOR    RESOLVED  (38.05, -120.55) in calaveras only
//
// Default-status queries therefore return q1, f1, a1 in canonical order
// (severity desc, observed_at desc): f1 (SEVERE), q1 (base+3h), a1 (base-1h).
func seedEvents(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()

	// events.source_id has a FK to sources(id): the registry rows must exist
	// before any event lands (exactly the boot order main.go follows).
	require.NoError(t, st.SeedSources(ctx, []store.SourceSeed{
		{ID: "usgs", Name: "USGS", Attribution: "USGS Earthquake Hazards Program", PollInterval: 5 * time.Minute},
		{ID: "calfire", Name: "CAL FIRE", PollInterval: 5 * time.Minute},
		{ID: "nws", Name: "National Weather Service", PollInterval: 5 * time.Minute},
		{ID: "chp", Name: "CHP / Caltrans", PollInterval: 5 * time.Minute},
	}))

	quake := &gridv1.Event{
		Id: "usgs:q1", Layer: gridv1.Layer_EARTHQUAKE,
		Severity: gridv1.Severity_MODERATE, Status: gridv1.EventStatus_ACTIVE,
		Headline:   "M4.2 earthquake near Arnold",
		Geometry:   pointGeom(38.4, -120.1),
		ObservedAt: timestamppb.New(base.Add(2 * time.Hour)),
		Provenance: &gridv1.Provenance{SourceId: "usgs"},
	}
	_, err := st.UpsertEvent(ctx, quake)
	require.NoError(t, err)
	// Revise: changed headline + newer observed_at => revision 2.
	quake.Headline = "M4.4 earthquake near Arnold (revised)"
	quake.ObservedAt = timestamppb.New(base.Add(3 * time.Hour))
	res, err := st.UpsertEvent(ctx, quake)
	require.NoError(t, err)
	require.Equal(t, uint32(2), res.Revision)

	_, err = st.UpsertEvent(ctx, &gridv1.Event{
		Id: "calfire:f1", Layer: gridv1.Layer_WILDFIRE,
		Severity: gridv1.Severity_SEVERE, Status: gridv1.EventStatus_ACTIVE,
		Headline:   "Salt Fire — 120 acres, 10% contained",
		Geometry:   pointGeom(38.0, -120.5),
		ObservedAt: timestamppb.New(base),
		Provenance: &gridv1.Provenance{SourceId: "calfire"},
	})
	require.NoError(t, err)

	_, err = st.UpsertEvent(ctx, &gridv1.Event{
		Id: "wx:a1", Layer: gridv1.Layer_WEATHER_ALERT,
		Severity: gridv1.Severity_MODERATE, Status: gridv1.EventStatus_SCHEDULED,
		Headline:   "Winter Weather Advisory",
		PlaceIds:   []string{"area:calaveras"}, // zone-carrying alert: preset, no geometry
		ObservedAt: timestamppb.New(base.Add(-1 * time.Hour)),
		Provenance: &gridv1.Provenance{SourceId: "nws"},
	})
	require.NoError(t, err)

	_, err = st.UpsertEvent(ctx, &gridv1.Event{
		Id: "chp:i1", Layer: gridv1.Layer_ROAD_INCIDENT,
		Severity: gridv1.Severity_MINOR, Status: gridv1.EventStatus_RESOLVED,
		Headline:   "Vehicle off roadway (cleared)",
		Geometry:   pointGeom(38.05, -120.55),
		ObservedAt: timestamppb.New(base.Add(-2 * time.Hour)),
		Provenance: &gridv1.Provenance{SourceId: "chp"},
		Enhancement: &gridv1.Enhancement{
			Model:    "gpt-5-mini",
			Fields:   []string{"headline", "summary", "impact"},
			Request:  "Parse this traffic incident report...",
			Response: `{"details":"Vehicle off roadway","impact":"light"}`,
		},
	})
	require.NoError(t, err)
}

// newTestService builds a Service over a fresh temp store seeded with the
// place directory (real internal/places seed: 2 areas + 8 embedded counties
// + 2 towns + 2 corridors) and the fixture events. Tests may swap the fake
// dependency fields before issuing requests.
func newTestService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	cfg := testConfig()
	require.NoError(t, places.Seed(context.Background(), st, cfg))
	seedEvents(t, st)

	svc := NewService(st, &fakeRoads{resp: roadsResp()}, &fakeWeather{resp: weatherResp()}, &fakeCensus{}, cfg, nil)
	svc.Now = func() time.Time { return base }
	return svc
}

// get issues a GET through the router and returns the recorder.
func get(t *testing.T, s *Service, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	require.Zero(t, len(hdr)%2, "hdr must be key/value pairs")
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// decode unmarshals a JSON response body.
func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), v),
		"body: %s", rec.Body.String())
}

// statusBody is the google.rpc.Status protojson error shape.
type statusBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// requireStatus asserts an error response: HTTP code + Status body shape.
func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, httpCode, grpcCode int) statusBody {
	t.Helper()
	require.Equal(t, httpCode, rec.Code, "body: %s", rec.Body.String())
	var sb statusBody
	decode(t, rec, &sb)
	require.Equal(t, grpcCode, sb.Code)
	require.NotEmpty(t, sb.Message)
	return sb
}

// eventIDs decodes an EventList body into its ordered event ids.
func eventIDs(t *testing.T, rec *httptest.ResponseRecorder) (ids []string, nextToken string) {
	t.Helper()
	var out struct {
		Events []struct {
			ID string `json:"id"`
		} `json:"events"`
		NextPageToken string `json:"next_page_token"`
	}
	decode(t, rec, &out)
	for _, e := range out.Events {
		ids = append(ids, e.ID)
	}
	return ids, out.NextPageToken
}
