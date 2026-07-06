// Store→GeoJSON byte-compat harness (docs/v2-implementation-plan.md T13).
//
// This test is THE GATE for re-backing the /api/v1 hazards event layers onto
// the event store (T14): for every event-backed layer it builds features twice
// from the SAME fixtures —
//
//	(a) through the live hazards.Service builders (the shipped envelope), and
//	(b) through the internal/ingest normalizers → a temp internal/store →
//	    store.QueryEvents → gridapi.ProjectEvents,
//
// — and asserts the two feature sets are JSON-equal, EXCLUDING exactly the
// plan §5 exclusion list. Each exclusion is an explicit normalization step
// below; ANY other difference fails with the exact property path that drifted.
//
// Exclusions with no normalization step here (they are already identical by
// construction — kept as documentation):
//
//	(4) earthquake updated_at — the projection omits it when
//	    observed_at == effective, reproducing the shipped omit-when-zero
//	    behavior (ingest falls observed_at back to the event time when USGS
//	    never revised the record). The residual case (an upstream Updated
//	    stamp exactly equal to the event time) is accepted drift per plan §5.
//	(5) road_incident / wildfire / evacuation / earthquake / weather source
//	    blocks — the projection emits the shipped per-layer CONSTANTS (derived
//	    from the layer, not stored provenance), so they compare exactly; only
//	    URL/name fields that genuinely vary per event flow from the event.
package hazards_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dpup/prefab/logging"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/calfire"
	"github.com/dpup/info.ersn.net/server/internal/clients/caloes"
	"github.com/dpup/info.ersn.net/server/internal/clients/nws"
	"github.com/dpup/info.ersn.net/server/internal/clients/usgs"
	"github.com/dpup/info.ersn.net/server/internal/clients/wfigs"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/gridapi"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
	"github.com/dpup/info.ersn.net/server/internal/ingest"
	"github.com/dpup/info.ersn.net/server/internal/services"
	"github.com/dpup/info.ersn.net/server/internal/store"
)

const compatAreaID = "calaveras"

// compatConfig is one hazard area (so the live builders' per-area scoping and
// ingest's union-bbox scoping cover the identical box) with one incident area.
func compatConfig() *config.Config {
	bounds := config.GeoBounds{MinLatitude: 37.7, MaxLatitude: 38.5, MinLongitude: -120.9, MaxLongitude: -119.2}
	return &config.Config{
		OpenAI: config.OpenAIClient{Model: "gpt-5-mini"},
		Weather: config.WeatherConfig{
			NWS: config.NWSConfig{Zones: []string{"CAZ064", "CAZ065"}},
		},
		Roads: config.RoadsConfig{
			IncidentAreas: []config.IncidentArea{{ID: "mother-lode", Bounds: bounds}},
		},
		Hazards: config.HazardsConfig{
			Areas: []config.HazardArea{{
				ID:           compatAreaID,
				Bounds:       bounds,
				IncidentArea: "mother-lode",
				Zones:        []string{"CAZ064", "CAZ065"},
			}},
		},
	}
}

// --- fixtures (inline, mirroring the internal/ingest client-fixture style) ---

// calfireCompatFixture covers: an incident that adopts a perimeter (Salt
// Springs), an incident whose name is AMBIGUOUS across two perimeters (must
// not adopt either), and two rows both sides must drop (out-of-bounds, no
// coordinates) — proving the scoping parity, not just the field mapping.
const calfireCompatFixture = `[
  {
    "UniqueId": "abc-123",
    "Name": "Salt Springs Fire",
    "County": "Calaveras",
    "Location": "near Hwy 4",
    "AcresBurned": 1200.0,
    "PercentContained": 35.0,
    "Latitude": 38.2,
    "Longitude": -120.4,
    "Started": "2026-07-01T10:00:00Z",
    "Updated": "2026-07-04T08:00:00Z",
    "Url": "https://www.fire.ca.gov/incidents/salt-springs",
    "IsActive": true
  },
  {
    "Name": "Ambiguous Fire",
    "County": "Tuolumne",
    "AcresBurned": 10.0,
    "PercentContained": 90.0,
    "Latitude": 38.0,
    "Longitude": -120.2,
    "IsActive": true
  },
  {
    "Name": "Far Away Fire",
    "AcresBurned": 5.0,
    "PercentContained": 0.0,
    "Latitude": 40.9,
    "Longitude": -122.0,
    "IsActive": true
  },
  {
    "Name": "No Coords Fire",
    "AcresBurned": 5.0,
    "PercentContained": 0.0,
    "IsActive": true
  }
]`

// wfigsCompatFixture: the adopted "Salt Springs" perimeter, two perimeters
// whose names normalize identically ("ambiguous" — emitted standalone by both
// sides), and a standalone MultiPolygon perimeter ("Lonely") with no matching
// incident. Coordinates must survive both paths verbatim.
const wfigsCompatFixture = `{
  "features": [
    {
      "properties": {"poly_IncidentName": "Ambiguous Fire", "attr_IncidentSize": 40.0, "attr_PercentContained": 20, "attr_FireCause": "Unknown"},
      "geometry": {"type": "Polygon", "coordinates": [[[-120.3,38.35],[-120.2,38.35],[-120.2,38.45],[-120.3,38.45],[-120.3,38.35]]]}
    },
    {
      "properties": {"poly_IncidentName": "Ambiguous", "attr_IncidentSize": 30.0, "attr_PercentContained": 60, "attr_FireCause": "Lightning"},
      "geometry": {"type": "Polygon", "coordinates": [[[-120.3,37.95],[-120.2,37.95],[-120.2,38.05],[-120.3,38.05],[-120.3,37.95]]]}
    },
    {
      "properties": {"poly_IncidentName": "Salt Springs", "attr_IncidentSize": 1180.0, "attr_PercentContained": 35, "attr_FireCause": "Undetermined"},
      "geometry": {"type": "Polygon", "coordinates": [[[-120.45,38.15],[-120.35,38.15],[-120.35,38.25],[-120.45,38.25],[-120.45,38.15]]]}
    },
    {
      "properties": {"poly_IncidentName": "Lonely", "attr_IncidentSize": 250.4, "attr_PercentContained": 10, "attr_FireCause": "Human"},
      "geometry": {"type": "MultiPolygon", "coordinates": [[[[-119.9,38.0],[-119.8,38.0],[-119.8,38.1],[-119.9,38.1],[-119.9,38.0]]],[[[-119.95,38.12],[-119.93,38.12],[-119.93,38.14],[-119.95,38.14],[-119.95,38.12]]]]}
    }
  ]
}`

// evacCompatFixture: an Order, an explicitly-lifted zone (dropped by both
// sides), a Warning, and a Shelter in Place. PUBLIC_INFO is life-safety text
// carried verbatim on both paths.
const evacCompatFixture = `{
  "type": "FeatureCollection",
  "features": [
    {
      "properties": {
        "ZONE_ID": "CAL-E-046",
        "ZONE_NAME": "Zone A",
        "COUNTY": "Calaveras",
        "STATUS": "Evacuation Order",
        "EVENT_TYPE": "Fire",
        "PUBLIC_INFO": "Leave now via Hwy 4. Do not delay.",
        "STATEWIDE_LAST_UPDATED": 1782400000000
      },
      "geometry": {"type": "Polygon", "coordinates": [[[-120.4,38.1],[-120.3,38.1],[-120.3,38.2],[-120.4,38.2],[-120.4,38.1]]]}
    },
    {
      "properties": {
        "ZONE_ID": "CAL-E-047",
        "ZONE_NAME": "Zone B",
        "COUNTY": "Calaveras",
        "STATUS": "Evacuation Order Lifted",
        "PUBLIC_INFO": "All clear."
      },
      "geometry": {"type": "Polygon", "coordinates": [[[-120.5,38.1],[-120.4,38.1],[-120.4,38.2],[-120.5,38.2],[-120.5,38.1]]]}
    },
    {
      "properties": {
        "ZONE_ID": "TUO-E-101",
        "ZONE_NAME": "Zone C",
        "COUNTY": "Tuolumne",
        "STATUS": "Evacuation Warning",
        "EVENT_TYPE": "Fire",
        "PUBLIC_INFO": "Be ready to leave.",
        "STATEWIDE_LAST_UPDATED": 1782300000000
      },
      "geometry": {"type": "Polygon", "coordinates": [[[-120.2,38.0],[-120.1,38.0],[-120.1,38.1],[-120.2,38.1],[-120.2,38.0]]]}
    },
    {
      "properties": {
        "ZONE_ID": "TUO-E-102",
        "ZONE_NAME": "Zone D",
        "COUNTY": "Tuolumne",
        "STATUS": "Shelter in Place",
        "PUBLIC_INFO": "Stay indoors."
      },
      "geometry": {"type": "Polygon", "coordinates": [[[-120.0,38.0],[-119.9,38.0],[-119.9,38.1],[-120.0,38.1],[-120.0,38.0]]]}
    }
  ]
}`

// quakeCompatFixture: one revised quake (updated > time => updated_at emitted)
// and one never-revised quake (updated 0 => updated_at omitted, plan §5 item
// 4) whose unsafe URL both paths must scrub.
const quakeCompatFixture = `{
  "type": "FeatureCollection",
  "features": [
    {
      "id": "nc75095123",
      "properties": {
        "mag": 4.2,
        "place": "10km NE of Murphys, CA",
        "time": 1782400000000,
        "updated": 1782400500000,
        "felt": 37,
        "url": "https://earthquake.usgs.gov/earthquakes/eventpage/nc75095123"
      },
      "geometry": { "type": "Point", "coordinates": [-120.45, 38.2, 7.6] }
    },
    {
      "id": "nc75095124",
      "properties": {
        "mag": 2.6,
        "place": "5km SW of Arnold, CA",
        "time": 1782300000000,
        "updated": 0,
        "url": "javascript:alert(1)"
      },
      "geometry": { "type": "Point", "coordinates": [-120.5, 38.1, 3.0] }
    }
  ]
}`

// compatNWSAlerts: an active zoned alert, a not-yet-effective zoned watch
// (SCHEDULED in the store — still served, QueryEvents defaults include it),
// and a zoneless NWS-"Extreme" alert exercising exclusion (3). All zones are
// within the area's zone list so the live builder's zonesMatch scoping keeps
// every alert, matching the unscoped store query.
func compatNWSAlerts(now time.Time) []nws.Alert {
	return []nws.Alert{
		{
			ID:          "urn:oid:2.49.0.1.840.0.abc",
			Event:       "Red Flag Warning",
			Severity:    "Severe",
			Certainty:   "Likely",
			Urgency:     "Expected",
			Headline:    "Red Flag Warning until 8 PM PDT",
			Description: "* WHAT...Gusty winds and low humidity.",
			Instruction: "Avoid outdoor burning.",
			SenderName:  "NWS Sacramento CA",
			AreaDesc:    "West Slope Northern Sierra Nevada",
			Effective:   now.Add(-2 * time.Hour),
			Expires:     now.Add(8 * time.Hour),
			Zones:       []string{"CAZ064"},
		},
		{
			ID:         "urn:oid:2.49.0.1.840.0.def",
			Event:      "Winter Storm Watch",
			Severity:   "Moderate",
			SenderName: "NWS Sacramento CA",
			Effective:  now.Add(6 * time.Hour), // not yet effective
			Zones:      []string{"CAZ065"},
		},
		{
			ID:         "urn:oid:2.49.0.1.840.0.ghi",
			Event:      "Extreme Wind Warning",
			Severity:   "Extreme", // exclusion (3): shipped collapses to SEVERE, store keeps EXTREME
			SenderName: "NWS Sacramento CA",
			Effective:  now.Add(-1 * time.Hour),
			// No zones: unscoped, kept by both sides.
		},
	}
}

// compatIncidents: an AI-enhanced CHP dispatch incident, an unenhanced
// Caltrans lane closure (caltrans provenance in the store, but the shipped
// envelope's one constant source block — exclusion (5)), and a locationless
// placemark both sides must skip. Status is api.IncidentStatus_ACTIVE exactly
// as services/incidents.go stamps every incident — that is what makes the
// shipped envelope's status ("active") equal the projection's lifecycle slug.
func compatIncidents() []*api.Incident {
	return []*api.Incident{
		{
			Id:                  "250916ST0066",
			Type:                api.AlertType_INCIDENT,
			Status:              api.IncidentStatus_ACTIVE,
			Severity:            api.AlertSeverity_CRITICAL,
			Location:            &api.Coordinates{Latitude: 38.2, Longitude: -120.35},
			LocationDescription: "Hwy 4 at Avery",
			Description:         "Vehicle fire blocking the right lane",
			LogNumber:           "250916ST0066",
			Started:             timestamppb.New(time.Date(2026, 7, 4, 6, 24, 0, 0, time.UTC)),
			LastUpdated:         timestamppb.New(time.Date(2026, 7, 4, 7, 0, 0, 0, time.UTC)),
			CondensedSummary:    "Vehicle fire on Hwy 4",
			Impact:              api.AlertImpact_IMPACT_SEVERE,
			Metadata:            map[string]string{"duration": "several hours", "emergency_services": "on scene"},
		},
		{
			Id:                  "closure-hwy-4-avery",
			Type:                api.AlertType_CLOSURE,
			Status:              api.IncidentStatus_ACTIVE,
			Severity:            api.AlertSeverity_WARNING,
			Location:            &api.Coordinates{Latitude: 38.25, Longitude: -120.3},
			LocationDescription: "Hwy 4 EB near Avery",
			Description:         "One-way traffic control for utility work",
		},
		{
			Id:          "no-location",
			Type:        api.AlertType_INCIDENT,
			Status:      api.IncidentStatus_ACTIVE,
			Description: "Geometry-only placemark",
		},
	}
}

// --- fakes ---

// compatDoer serves a canned body for any request — the repo's standard
// HTTPDoer injection, satisfying every client package's HTTPDoer interface.
type compatDoer struct{ resp string }

func (f *compatDoer) Do(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(f.resp)),
		Header:     make(http.Header),
	}, nil
}

// compatRoads implements hazards.RoadsAPI and the roads slice the ingest
// road_incident normalizer consumes (ListIncidents + IncidentFeedHealth), so
// both sides read the IDENTICAL AI-enhanced incident list — exactly the
// production topology, where both consume RoadsService.ListIncidents.
type compatRoads struct{ incidents []*api.Incident }

func (f *compatRoads) ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error) {
	return &api.ListRoadsResponse{}, nil // conditions layer; not under compat test
}

func (f *compatRoads) ListIncidents(_ context.Context, req *api.ListIncidentsRequest) (*api.ListIncidentsResponse, error) {
	if req.GetArea() != "mother-lode" {
		return nil, fmt.Errorf("unexpected incident area %q", req.GetArea())
	}
	return &api.ListIncidentsResponse{Incidents: f.incidents}, nil
}

func (f *compatRoads) IncidentFeedHealth() (chpErr, laneErr error, at time.Time) {
	return nil, nil, time.Now()
}

// compatWeather implements hazards.WeatherAPI and the weather slice the
// ingest weather_alert normalizer consumes (RawNWSAlerts), both fed from one
// nws.Alert fixture list — mirroring production, where ListWeatherAlerts and
// RawNWSAlerts serve the same cached NWS fetch.
type compatWeather struct{ alerts []nws.Alert }

func (f *compatWeather) ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error) {
	return &api.ListWeatherResponse{}, nil // fire_weather is condition-backed; not under compat test
}

func (f *compatWeather) ListWeatherAlerts(context.Context, *api.ListWeatherAlertsRequest) (*api.ListWeatherAlertsResponse, error) {
	return &api.ListWeatherAlertsResponse{Alerts: apiAlertsFromNWS(f.alerts)}, nil
}

func (f *compatWeather) RawNWSAlerts(context.Context) ([]nws.Alert, time.Time, error) {
	return f.alerts, time.Now(), nil
}

// apiAlertsFromNWS is a verbatim mirror of the shipped nws.Alert →
// api.WeatherAlert conversion (services/weather_nws.go nwsAlertsToProto +
// mapNWSSeverity), INCLUDING the Extreme→CRITICAL collapse that exclusion (3)
// documents. The id derivation is the real exported services.NWSAlertID, so
// the shipped side's "wx:"+id stays byte-identical to production.
func apiAlertsFromNWS(alerts []nws.Alert) []*api.WeatherAlert {
	var out []*api.WeatherAlert
	for _, a := range alerts {
		var sev api.AlertSeverity
		switch strings.ToLower(strings.TrimSpace(a.Severity)) {
		case "extreme", "severe":
			sev = api.AlertSeverity_CRITICAL
		case "moderate":
			sev = api.AlertSeverity_WARNING
		case "minor":
			sev = api.AlertSeverity_INFO
		default:
			sev = api.AlertSeverity_ALERT_SEVERITY_UNSPECIFIED
		}
		wa := &api.WeatherAlert{
			Id:          services.NWSAlertID(a),
			SenderName:  a.SenderName,
			Event:       a.Event,
			Headline:    a.Headline,
			Description: a.Description,
			Source:      api.AlertSource_NWS,
			Severity:    sev,
			Zones:       a.Zones,
		}
		if !a.Effective.IsZero() {
			wa.StartTime = timestamppb.New(a.Effective)
		}
		if !a.Expires.IsZero() {
			wa.EndTime = timestamppb.New(a.Expires)
		}
		out = append(out, wa)
	}
	return out
}

// --- the harness ---

func TestStoreProjectionByteCompat(t *testing.T) {
	ctx := logging.EnsureLogger(context.Background())
	cfg := compatConfig()
	// Whole seconds: both paths format timestamps second-granular (RFC 3339
	// without fractions), so sub-second fixture times would be a false drift.
	now := time.Now().UTC().Truncate(time.Second)

	roads := &compatRoads{incidents: compatIncidents()}
	weather := &compatWeather{alerts: compatNWSAlerts(now)}

	// One client per upstream, shared by BOTH sides — same bytes in, so any
	// output difference is projection drift, never fixture skew.
	usgsClient := usgs.NewClientWithHTTPDoer("https://usgs.test", &compatDoer{resp: quakeCompatFixture})
	calfireClient := calfire.NewClientWithHTTPDoer("https://calfire.test", &compatDoer{resp: calfireCompatFixture})
	wfigsClient := wfigs.NewClientWithHTTPDoer("https://wfigs.test", &compatDoer{resp: wfigsCompatFixture})
	caloesClient := caloes.NewClientWithHTTPDoer("https://caloes.test", &compatDoer{resp: evacCompatFixture})

	// (a) the live builders, via the real HTTP surface (nil cache: every
	// request hits the builder; nil caltrans: chain_control is not event-backed).
	shippedSvc := hazards.NewServiceWithAPIs(cfg, roads, weather, nil, nil).
		WithClients(usgsClient, calfireClient, wfigsClient, caloesClient)

	// (b) the same fixtures through ingest → store.
	st, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	defer st.Close()

	// events.source_id references the sources registry: seed the rows every
	// normalizer below attributes to (as main.go's boot seeding does).
	require.NoError(t, st.SeedSources(ctx, []store.SourceSeed{
		{ID: "usgs", Name: "USGS", PollInterval: 5 * time.Minute},
		{ID: "calfire", Name: "CAL FIRE", PollInterval: 5 * time.Minute},
		{ID: "wfigs", Name: "NIFC WFIGS", PollInterval: 5 * time.Minute},
		{ID: "caloes", Name: "Cal OES", PollInterval: 2 * time.Minute},
		{ID: "nws", Name: "National Weather Service", PollInterval: 5 * time.Minute},
		{ID: "chp", Name: "CHP", PollInterval: 5 * time.Minute},
		{ID: "caltrans", Name: "Caltrans", PollInterval: 5 * time.Minute},
	}))

	normalizers := []ingest.Normalizer{
		ingest.NewEarthquakeNormalizer(cfg, usgsClient),
		ingest.NewWildfireNormalizer(cfg, calfireClient, wfigsClient),
		ingest.NewEvacuationNormalizer(cfg, caloesClient),
		ingest.NewWeatherAlertNormalizer(cfg, weather),
		ingest.NewRoadIncidentNormalizer(cfg, roads),
	}
	for _, n := range normalizers {
		res, err := n.Poll(ctx, nil)
		require.NoError(t, err, "fixture poll %v must succeed", n.SourceIDs())
		require.Empty(t, res.PerSource, "fixture poll %v must be fully healthy", n.SourceIDs())
		for _, ev := range res.Events {
			_, err := st.UpsertEvent(ctx, ev)
			require.NoError(t, err, "upsert %s", ev.GetId())
		}
	}

	layers := []struct {
		slug  string
		layer gridv1.Layer
		count int // expected features per side — a guard against trivially-empty passes
	}{
		{hazards.LayerWildfire, gridv1.Layer_WILDFIRE, 5},
		{hazards.LayerEvacuation, gridv1.Layer_EVACUATION, 3},
		{hazards.LayerWeatherAlert, gridv1.Layer_WEATHER_ALERT, 3},
		{hazards.LayerEarthquake, gridv1.Layer_EARTHQUAKE, 2},
		{hazards.LayerRoadIncident, gridv1.Layer_ROAD_INCIDENT, 2},
	}
	for _, lc := range layers {
		t.Run(lc.slug, func(t *testing.T) {
			shipped := shippedFeatures(t, shippedSvc, lc.slug)
			require.Len(t, shipped, lc.count, "shipped %s feature count", lc.slug)

			events, next, err := st.QueryEvents(ctx, store.EventQuery{
				Layers:   []gridv1.Layer{lc.layer},
				PageSize: 200,
				// Statuses default to ACTIVE+SCHEDULED — the live-map read the
				// /api/v1 re-back will serve. RESOLVED/EXPIRED never reach the
				// projection in that path.
			})
			require.NoError(t, err)
			require.Empty(t, next, "fixtures must fit one page")
			projected := featureMaps(t, gridapi.ProjectEvents(lc.slug, events))
			require.Len(t, projected, lc.count, "projected %s feature count", lc.slug)

			compareFeatureSets(t, lc.slug, shipped, projected)
		})
	}
}

// shippedFeatures builds one layer through the real hazards HTTP handler and
// returns its features as decoded JSON. The layer must be OK — a builder
// error would silently gut the comparison.
func shippedFeatures(t *testing.T, svc *hazards.Service, layer string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, hazards.HandlerPrefix+compatAreaID+"/"+layer+".geojson", nil)
	req = req.WithContext(logging.EnsureLogger(req.Context()))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "GET %s: %s", req.URL.Path, rr.Body.String())

	var fc struct {
		Features []json.RawMessage `json:"features"`
		Metadata struct {
			SourceStatus string `json:"source_status"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &fc))
	require.Equal(t, "OK", fc.Metadata.SourceStatus, "layer %s must build cleanly from fixtures", layer)

	out := make([]map[string]any, len(fc.Features))
	for i, raw := range fc.Features {
		require.NoError(t, json.Unmarshal(raw, &out[i]))
	}
	return out
}

// featureMaps canonicalizes projected features through a JSON round-trip so
// both sides compare as decoded JSON (field order and number formatting
// irrelevant; values exact).
func featureMaps(t *testing.T, feats []hazards.Feature) []map[string]any {
	t.Helper()
	out := make([]map[string]any, len(feats))
	for i, f := range feats {
		b, err := json.Marshal(f)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &out[i]))
	}
	return out
}

// compareFeatureSets pairs the two sides' features and fails with the exact
// property path of any drift outside the plan §5 exclusion list.
func compareFeatureSets(t *testing.T, layer string, shipped, projected []map[string]any) {
	t.Helper()
	for _, f := range shipped {
		normalizeFeature(f)
	}
	for _, f := range projected {
		normalizeFeature(f)
	}

	// Pair by (id, headline): ids alone are the pairing key everywhere except
	// wildfire, where exclusion (1) deliberately collapses same-named
	// standalone perimeter ids — their headlines (name/acres/containment)
	// disambiguate.
	shippedBy := byKey(t, "shipped", shipped)
	projectedBy := byKey(t, "projected", projected)
	require.ElementsMatch(t, mapKeys(shippedBy), mapKeys(projectedBy),
		"layer %s: feature identity sets differ (after wfigs-id normalization)", layer)

	for _, key := range mapKeys(shippedBy) {
		sf, pf := shippedBy[key], projectedBy[key]
		allowWeatherExtremeDrift(layer, sf, pf)
		var diffs []string
		diffValues("", sf, pf, &diffs)
		if len(diffs) > 0 {
			t.Errorf("layer %s feature %q drifted from the shipped envelope:\n  %s",
				layer, key, strings.Join(diffs, "\n  "))
		}
	}
}

// normalizeFeature applies the plan §5 exclusions that are field rewrites.
func normalizeFeature(f map[string]any) {
	props, _ := f["properties"].(map[string]any)
	if props == nil {
		return
	}
	// Exclusion (1): standalone WFIGS perimeter ids. Shipped ids carried an
	// unstable slice index ("wfigs:{norm}:{i}"); the store mints stable
	// "wfigs:{norm}[-N]" (CHANGELOG'd id-stability fix). Both collapse to
	// "wfigs:{norm}" for comparison; everything else about the feature must
	// still match exactly.
	if id, ok := props["id"].(string); ok {
		props["id"] = normalizeWfigsID(id)
	}
	// Exclusion (2): source.fetched_at is freshness metadata, not content —
	// deleted from both sides before comparing.
	if src, ok := props["source"].(map[string]any); ok {
		delete(src, "fetched_at")
	}
}

// wfigsIDRe matches both id schemes: normalized names are strictly [a-z0-9],
// so a trailing ":{digits}" (shipped slice index) or "-{digits}" (store
// collision suffix) is unambiguous.
var wfigsIDRe = regexp.MustCompile(`^wfigs:([a-z0-9]+)([:-][0-9]+)?$`)

func normalizeWfigsID(id string) string {
	if m := wfigsIDRe.FindStringSubmatch(id); m != nil {
		return "wfigs:" + m[1]
	}
	return id
}

// allowWeatherExtremeDrift is exclusion (3): the store maps NWS "Extreme" to
// EXTREME where the shipped api-enum path collapsed it to SEVERE (an accuracy
// fix, CHANGELOG'd; rank drift 3→4 allowed). Forgiven ONLY for the exact
// shipped-SEVERE/store-EXTREME pairing on weather_alert — under the two
// mappings that combination can only arise from NWS "Extreme". Any other
// severity disagreement still fails.
func allowWeatherExtremeDrift(layer string, shipped, projected map[string]any) {
	if layer != hazards.LayerWeatherAlert {
		return
	}
	sp, _ := shipped["properties"].(map[string]any)
	pp, _ := projected["properties"].(map[string]any)
	if sp == nil || pp == nil {
		return
	}
	if sp["severity"] == "SEVERE" && pp["severity"] == "EXTREME" {
		sp["severity"] = "EXTREME"
		sp["severity_rank"] = float64(4)
	}
}

// byKey indexes features by their pairing key, rejecting duplicates (a
// duplicate key would let two features shadow each other and hide drift).
func byKey(t *testing.T, side string, feats []map[string]any) map[string]map[string]any {
	t.Helper()
	out := make(map[string]map[string]any, len(feats))
	for _, f := range feats {
		props, _ := f["properties"].(map[string]any)
		require.NotNil(t, props, "%s feature without properties", side)
		id, _ := props["id"].(string)
		headline, _ := props["headline"].(string)
		key := id + " | " + headline
		_, dup := out[key]
		require.False(t, dup, "%s: duplicate pairing key %q", side, key)
		out[key] = f
	}
	return out
}

func mapKeys(m map[string]map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// diffValues walks two decoded-JSON values and records every leaf difference
// as "path: shipped=X projected=Y" — the failure output points at the exact
// property that drifted.
func diffValues(path string, a, b any, out *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: shipped=%s projected=%s", orRoot(path), renderJSON(a), renderJSON(b)))
			return
		}
		for _, k := range unionKeys(av, bv) {
			sub := k
			if path != "" {
				sub = path + "." + k
			}
			aval, aok := av[k]
			bval, bok := bv[k]
			switch {
			case aok && !bok:
				*out = append(*out, fmt.Sprintf("%s: shipped=%s projected=<absent>", sub, renderJSON(aval)))
			case !aok && bok:
				*out = append(*out, fmt.Sprintf("%s: shipped=<absent> projected=%s", sub, renderJSON(bval)))
			default:
				diffValues(sub, aval, bval, out)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			*out = append(*out, fmt.Sprintf("%s: shipped=%s projected=%s", orRoot(path), renderJSON(a), renderJSON(b)))
			return
		}
		for i := range av {
			diffValues(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i], out)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			*out = append(*out, fmt.Sprintf("%s: shipped=%s projected=%s", orRoot(path), renderJSON(a), renderJSON(b)))
		}
	}
}

// unionKeys returns the sorted union of both maps' keys, so keys present on
// only one side still get reported.
func unionKeys(a, b map[string]any) []string {
	set := make(map[string]bool, len(a)+len(b))
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func orRoot(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}

func renderJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(b)
}
