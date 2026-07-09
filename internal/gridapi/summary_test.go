package gridapi

// Tests for GET /v1/places/{place}/summary: the ComputeMode rule table (every
// plan §2.4 branch), and handler tests against the seeded temp store — the
// evacuation null-vs-0-vs-count invariant (source health flipped via
// store.RecordAttempt), the merged roads/fire domain rollups over a fake
// hazardsBuilder, top_events ordering + cap, and snake_case spot checks on
// the raw JSON. All offline.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/store"
)

// TestComputeMode pins every rule branch of the pure mode function.
func TestComputeMode(t *testing.T) {
	tests := []struct {
		name string
		in   ModeInputs
		want string
	}{
		// QUIET baselines.
		{"nothing at all", ModeInputs{}, ModeQuiet},
		{"moderate active alone", ModeInputs{MaxActiveSeverity: gridv1.Severity_MODERATE}, ModeQuiet},
		{"minor active alone", ModeInputs{MaxActiveSeverity: gridv1.Severity_MINOR}, ModeQuiet},
		{"fire weather normal", ModeInputs{FireWeatherState: "normal"}, ModeQuiet},

		// ACTIVE: any active evacuation ORDER/WARNING/SHELTER_IN_PLACE.
		{"evac order", ModeInputs{ActiveEvacLevels: []string{"ORDER"}}, ModeActive},
		{"evac warning", ModeInputs{ActiveEvacLevels: []string{"WARNING"}}, ModeActive},
		{"evac shelter in place", ModeInputs{ActiveEvacLevels: []string{"SHELTER_IN_PLACE"}}, ModeActive},
		// Life-safety escalation: an unrecognized active level is never quiet.
		{"evac unknown level escalates", ModeInputs{ActiveEvacLevels: []string{"SOMETHING_NEW"}}, ModeActive},

		// ACTIVE: any active event EXTREME.
		{"extreme active event", ModeInputs{MaxActiveSeverity: gridv1.Severity_EXTREME}, ModeActive},

		// ACTIVE: wildfire SEVERE (unlike other layers' SEVERE).
		{"wildfire severe", ModeInputs{
			MaxActiveSeverity:   gridv1.Severity_SEVERE,
			MaxWildfireSeverity: gridv1.Severity_SEVERE,
		}, ModeActive},

		// WATCH: any active SEVERE (non-wildfire).
		{"non-wildfire severe", ModeInputs{MaxActiveSeverity: gridv1.Severity_SEVERE}, ModeWatch},

		// WATCH: evac ADVISORY — but it never masks a stronger ACTIVE signal.
		{"evac advisory", ModeInputs{ActiveEvacLevels: []string{"ADVISORY"}}, ModeWatch},
		{"advisory plus wildfire severe is still active", ModeInputs{
			ActiveEvacLevels:    []string{"ADVISORY"},
			MaxActiveSeverity:   gridv1.Severity_SEVERE,
			MaxWildfireSeverity: gridv1.Severity_SEVERE,
		}, ModeActive},

		// WATCH: fire_weather elevated / red-flag (case-insensitive).
		{"fire weather elevated", ModeInputs{FireWeatherState: "elevated"}, ModeWatch},
		{"fire weather red-flag", ModeInputs{FireWeatherState: "red-flag"}, ModeWatch},
		{"fire weather RED-FLAG uppercase", ModeInputs{FireWeatherState: "RED-FLAG"}, ModeWatch},

		// WATCH: UNAVAILABLE evac forces mode >= WATCH — unknown is never quiet.
		{"evac unavailable alone", ModeInputs{EvacUnavailable: true}, ModeWatch},

		// WATCH: any layer UNAVAILABLE while another signal >= MODERATE.
		{"layer down with moderate signal", ModeInputs{
			AnyLayerUnavailable: true,
			MaxActiveSeverity:   gridv1.Severity_MODERATE,
		}, ModeWatch},
		{"layer down with minor signal stays quiet", ModeInputs{
			AnyLayerUnavailable: true,
			MaxActiveSeverity:   gridv1.Severity_MINOR,
		}, ModeQuiet},
		{"layer down with no signal stays quiet", ModeInputs{AnyLayerUnavailable: true}, ModeQuiet},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ComputeMode(tc.in))
		})
	}
}

// summaryOut mirrors the plan §2.3 response shape for decoding.
type summaryOut struct {
	Place       string `json:"place"`
	PlaceID     string `json:"placeId"`
	PlaceName   string `json:"placeName"`
	GeneratedAt string `json:"generatedAt"`
	Mode        string `json:"mode"`
	Summary     struct {
		HighestSeverity     string         `json:"highestSeverity"`
		HighestSeverityRank int            `json:"highestSeverityRank"`
		SeverityCounts      map[string]int `json:"severityCounts"`
		TotalActive         int            `json:"totalActive"`
		ActiveEvacuations   *int           `json:"activeEvacuations"`
		EvacuationStatus    string         `json:"evacuationStatus"`
		TopEvents           []struct {
			ID           string `json:"id"`
			Layer        string `json:"layer"`
			Severity     string `json:"severity"`
			SeverityRank int    `json:"severityRank"`
			Headline     string `json:"headline"`
			Source       string `json:"source"`
		} `json:"topEvents"`
	} `json:"summary"`
	Domains []struct {
		Domain          string `json:"domain"`
		Status          string `json:"status"`
		HighestSeverity string `json:"highestSeverity"`
		ActiveCount     int    `json:"activeCount"`
		Headlines       []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			Headline string `json:"headline"`
		} `json:"headlines"`
	} `json:"domains"`
	Sources []struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		LastSuccessAt string `json:"lastSuccessAt"`
	} `json:"sources"`
}

// domain returns the named domain block, failing the test when absent.
func (o *summaryOut) domain(t *testing.T, name string) (d struct {
	Domain          string `json:"domain"`
	Status          string `json:"status"`
	HighestSeverity string `json:"highestSeverity"`
	ActiveCount     int    `json:"activeCount"`
	Headlines       []struct {
		ID       string `json:"id"`
		Severity string `json:"severity"`
		Headline string `json:"headline"`
	} `json:"headlines"`
}) {
	t.Helper()
	for _, dom := range o.Domains {
		if dom.Domain == name {
			return dom
		}
	}
	t.Fatalf("domain %q missing from %+v", name, o.Domains)
	return
}

// condFake is one condition layer's canned BuildLayer result.
type condFake struct {
	features []hazards.Feature
	status   string
}

// fakeCondBuilder fakes the hazardsBuilder seam per layer (the maplayers
// fakeHazards returns one result for all layers; the summary needs three).
type fakeCondBuilder struct {
	byLayer map[string]condFake
}

func (f *fakeCondBuilder) BuildLayer(_ context.Context, _ config.HazardArea, layer string) ([]hazards.Feature, string, time.Time, string, string, bool) {
	c, ok := f.byLayer[layer]
	if !ok {
		return nil, "", time.Time{}, "", "", false
	}
	return c.features, c.status, time.Time{}, "", "", true
}

// condOKBuilder is the all-healthy conditions baseline: empty chain controls,
// empty segments, fire weather in the given state.
func condOKBuilder(fwState string) *fakeCondBuilder {
	var fw []hazards.Feature
	if fwState != "" {
		fw = []hazards.Feature{condFeature("fw:region", hazards.LayerFireWeather, sevForFWState(fwState), "Fire weather: "+fwState,
			&hazards.FireWeatherProps{State: fwState})}
	}
	return &fakeCondBuilder{byLayer: map[string]condFake{
		hazards.LayerChainControl: {status: "OK"},
		hazards.LayerRoadSegment:  {status: "OK"},
		hazards.LayerFireWeather:  {features: fw, status: "OK"},
	}}
}

func sevForFWState(state string) string {
	switch state {
	case "red-flag":
		return "SEVERE"
	case "elevated":
		return "MODERATE"
	default:
		return "INFO"
	}
}

// condFeature builds a condition-layer feature with the envelope fields the
// summary reads (id, severity+rank, headline, optional fire_weather block).
func condFeature(id, layer, severity, headline string, fw *hazards.FireWeatherProps) hazards.Feature {
	rank := map[string]int{"INFO": 0, "MINOR": 1, "MODERATE": 2, "SEVERE": 3, "EXTREME": 4}[severity]
	return hazards.Feature{Type: "Feature", Properties: hazards.Properties{
		ID: id, Layer: layer, Severity: severity, SeverityRank: rank,
		Headline: headline, FireWeather: fw,
	}}
}

// gatewaySummaryJSON mirrors prefab's gRPC-Gateway marshaler (camelCase +
// EmitUnpopulated) so these tests assert the exact wire bytes — including the
// active_evacuations explicit null a nil Int32Value produces.
var gatewaySummaryJSON = protojson.MarshalOptions{UseProtoNames: false, EmitUnpopulated: true}

// getSummaryWith builds the summary proto with a fake condition builder and
// renders it through the gateway marshaler into a recorder (the interface seam
// the concrete GridServer binds to s.Hazards).
func getSummaryWith(t *testing.T, s *Service, hb hazardsBuilder, placeKey string) *httptest.ResponseRecorder {
	t.Helper()
	sum, err := s.buildPlaceSummary(context.Background(), hb, placeKey)
	require.NoError(t, err)
	body, err := gatewaySummaryJSON.Marshal(sum)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	rec.Code = http.StatusOK
	rec.Body.Write(body)
	return rec
}

func decodeSummary(t *testing.T, rec *httptest.ResponseRecorder) summaryOut {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var out summaryOut
	decode(t, rec, &out)
	return out
}

// upsert stores one event, failing on error.
func upsert(t *testing.T, st *store.Store, ev *gridv1.Event) {
	t.Helper()
	_, err := st.UpsertEvent(context.Background(), ev)
	require.NoError(t, err)
}

// evacEvent builds an evacuation event in calaveras. The caloes source row
// must be seeded first (events.source_id FK).
func evacEvent(id, level string, status gridv1.EventStatus) *gridv1.Event {
	sev := gridv1.Severity_EXTREME // ORDER
	if level == "ADVISORY" {
		sev = gridv1.Severity_MODERATE
	}
	return &gridv1.Event{
		Id: id, Layer: gridv1.Layer_EVACUATION, Severity: sev, Status: status,
		Headline:   "Evacuation " + level + " — " + id,
		Geometry:   pointGeom(38.1, -120.4),
		ObservedAt: timestamppb.New(base),
		Provenance: &gridv1.Provenance{SourceId: "caloes"},
		Detail:     &gridv1.Event_Evacuation{Evacuation: &gridv1.EvacuationDetail{ZoneId: id, Level: level}},
	}
}

// TestSummary_Shape: buildPlaceSummary rendered through the gateway marshaler
// (Hazards unwired) — the camelCase wire shape, the explicit evac null, and the
// seeded-fixture rollup (severity summary, top events, domains, source health).
func TestSummary_Shape(t *testing.T) {
	s := newTestService(t) // Hazards nil => condition layers UNAVAILABLE
	rec := getSummaryWith(t, s, nil, "calaveras")
	out := decodeSummary(t, rec)

	// camelCase spot checks on the raw body (the wire shape the site codes against).
	raw := rec.Body.String()
	for _, key := range []string{
		`"placeId"`, `"placeName"`, `"generatedAt"`, `"highestSeverityRank"`,
		`"severityCounts"`, `"totalActive"`, `"activeEvacuations"`,
		`"evacuationStatus"`, `"topEvents"`, `"severityRank"`, `"activeCount"`,
	} {
		assert.Contains(t, raw, key)
	}
	assert.NotContains(t, raw, `"place_id"`, "the surface is camelCase, no snake_case")

	assert.Equal(t, "calaveras", out.Place)
	assert.Equal(t, "area:calaveras", out.PlaceID)
	assert.Equal(t, "Calaveras", out.PlaceName)
	assert.Equal(t, base.Format(time.RFC3339), out.GeneratedAt, "injected clock")
	assert.Equal(t, ModeActive, out.Mode, "active SEVERE wildfire escalates to ACTIVE")

	// Summary block over the fixture events (f1 SEVERE, q1 + a1 MODERATE).
	assert.Equal(t, "SEVERE", out.Summary.HighestSeverity)
	assert.Equal(t, 3, out.Summary.HighestSeverityRank)
	assert.Equal(t, map[string]int{"SEVERE": 1, "MODERATE": 2}, out.Summary.SeverityCounts)
	assert.Equal(t, 3, out.Summary.TotalActive)

	// caloes was never seeded: evac is an explicit null + UNAVAILABLE.
	assert.Nil(t, out.Summary.ActiveEvacuations)
	assert.Equal(t, "UNAVAILABLE", out.Summary.EvacuationStatus)
	assert.Contains(t, raw, `"activeEvacuations":null`, "null must be explicit, not omitted")

	// top_events: severity desc, then observed_at desc; lowercase layer slugs;
	// provenance source ids.
	require.Len(t, out.Summary.TopEvents, 3)
	assert.Equal(t, "calfire:f1", out.Summary.TopEvents[0].ID)
	assert.Equal(t, "WILDFIRE", out.Summary.TopEvents[0].Layer)
	assert.Equal(t, "SEVERE", out.Summary.TopEvents[0].Severity)
	assert.Equal(t, 3, out.Summary.TopEvents[0].SeverityRank)
	assert.Equal(t, "calfire", out.Summary.TopEvents[0].Source)
	assert.Equal(t, "usgs:q1", out.Summary.TopEvents[1].ID)
	assert.Equal(t, "EARTHQUAKE", out.Summary.TopEvents[1].Layer)
	assert.Equal(t, "wx:a1", out.Summary.TopEvents[2].ID)
	assert.Equal(t, "WEATHER_ALERT", out.Summary.TopEvents[2].Layer)

	// Domains in the fixed order, condition-dependent ones UNAVAILABLE (the
	// hazards service is unwired — fail loud, no fabricated OK).
	require.Len(t, out.Domains, 5)
	for i, name := range []string{"fire", "evacuation", "weather", "roads", "seismic"} {
		assert.Equal(t, name, out.Domains[i].Domain)
	}
	fire := out.domain(t, "fire")
	assert.Equal(t, "UNAVAILABLE", fire.Status, "sources never polled + conditions unwired")
	assert.Equal(t, 1, fire.ActiveCount)
	assert.Equal(t, "SEVERE", fire.HighestSeverity)
	roads := out.domain(t, "roads")
	assert.Equal(t, "UNAVAILABLE", roads.Status)
	assert.Equal(t, 0, roads.ActiveCount)
	assert.Equal(t, "INFO", roads.HighestSeverity)
	assert.Empty(t, roads.Headlines)
	assert.Equal(t, 1, out.domain(t, "weather").ActiveCount, "SCHEDULED alert is part of the live set")
	assert.Equal(t, 1, out.domain(t, "seismic").ActiveCount)

	// Sources sidecar: the seeded registry rows (store id order), never polled
	// => UNAVAILABLE with last_success_at omitted.
	require.Len(t, out.Sources, 4)
	var ids []string
	for _, src := range out.Sources {
		ids = append(ids, src.ID)
		assert.Equal(t, "UNAVAILABLE", src.Status, src.ID)
		assert.Empty(t, src.LastSuccessAt, src.ID)
	}
	assert.Equal(t, []string{"calfire", "chp", "nws", "usgs"}, ids)
}

// TestSummary_EvacInvariant: null vs 0 vs count, flipped via RecordAttempt —
// the life-safety contract is "an error never becomes a 0".
func TestSummary_EvacInvariant(t *testing.T) {
	t.Run("caloes down with stored zones serves the stored count as STALE", func(t *testing.T) {
		// The T14 contract (hazards.DegradeStoreStatus): the store IS the
		// last-good cache, so source-down + stored active zones = STALE +
		// the count — the same answer /api/v1/situation gives for this store
		// state, and consistent with the domains block and map layer that
		// serve these very zones. null/UNAVAILABLE would tell the public
		// "unknown" while the same server renders a live ORDER.
		s := newTestService(t)
		seedSource(t, s.Store, "caloes") // FK for the event; still never polled
		upsert(t, s.Store, evacEvent("evac:z1", "ORDER", gridv1.EventStatus_ACTIVE))
		out := decodeSummary(t, getSummaryWith(t, s, condOKBuilder("normal"), "calaveras"))
		require.NotNil(t, out.Summary.ActiveEvacuations, "stored last-good zones must be vouched for, not disowned")
		assert.Equal(t, 1, *out.Summary.ActiveEvacuations)
		assert.Equal(t, "STALE", out.Summary.EvacuationStatus)
		assert.Equal(t, "STALE", out.domain(t, "evacuation").Status,
			"the domain block must agree with the summary block")
		assert.Equal(t, ModeActive, out.Mode, "the stored ORDER still escalates the mode")
	})

	t.Run("caloes OK with no zones is an explicit 0", func(t *testing.T) {
		s := newTestService(t)
		seedSource(t, s.Store, "caloes")
		recordOK(t, s.Store, "caloes")
		out := decodeSummary(t, getSummaryWith(t, s, condOKBuilder("normal"), "calaveras"))
		require.NotNil(t, out.Summary.ActiveEvacuations)
		assert.Equal(t, 0, *out.Summary.ActiveEvacuations, "confirmed-empty is 0, not null")
		assert.Equal(t, "OK", out.Summary.EvacuationStatus)
	})

	t.Run("caloes OK counts ACTIVE zones only", func(t *testing.T) {
		s := newTestService(t)
		seedSource(t, s.Store, "caloes")
		recordOK(t, s.Store, "caloes")
		upsert(t, s.Store, evacEvent("evac:z1", "ORDER", gridv1.EventStatus_ACTIVE))
		upsert(t, s.Store, evacEvent("evac:z2", "WARNING", gridv1.EventStatus_ACTIVE))
		upsert(t, s.Store, evacEvent("evac:z3", "ORDER", gridv1.EventStatus_SCHEDULED))
		out := decodeSummary(t, getSummaryWith(t, s, condOKBuilder("normal"), "calaveras"))
		require.NotNil(t, out.Summary.ActiveEvacuations)
		assert.Equal(t, 2, *out.Summary.ActiveEvacuations, "SCHEDULED zones are not active")
		assert.Equal(t, ModeActive, out.Mode)
		evac := out.domain(t, "evacuation")
		assert.Equal(t, "OK", evac.Status)
		assert.Equal(t, 3, evac.ActiveCount, "the domain reflects the live ACTIVE+SCHEDULED set")
	})

	t.Run("caloes STALE still counts", func(t *testing.T) {
		s := newTestService(t)
		seedSource(t, s.Store, "caloes")
		recordOK(t, s.Store, "caloes")
		upsert(t, s.Store, evacEvent("evac:z1", "ORDER", gridv1.EventStatus_ACTIVE))
		recordErr(t, s.Store, "caloes") // failure within stale_after of the success
		out := decodeSummary(t, getSummaryWith(t, s, condOKBuilder("normal"), "calaveras"))
		require.NotNil(t, out.Summary.ActiveEvacuations)
		assert.Equal(t, 1, *out.Summary.ActiveEvacuations, "last-good data still counts while STALE")
		assert.Equal(t, "STALE", out.Summary.EvacuationStatus)
	})

	t.Run("never-succeeded caloes is null", func(t *testing.T) {
		s := newTestService(t)
		seedSource(t, s.Store, "caloes")
		recordErr(t, s.Store, "caloes")
		rec := getSummaryWith(t, s, condOKBuilder("normal"), "calaveras")
		out := decodeSummary(t, rec)
		assert.Nil(t, out.Summary.ActiveEvacuations)
		// A *int decodes both JSON null and an omitted key to nil, so assert the
		// raw bytes: UNAVAILABLE must be an EXPLICIT null, never an absent field.
		assert.Contains(t, rec.Body.String(), `"activeEvacuations":null`, "null must be explicit, not omitted")
		assert.Equal(t, "UNAVAILABLE", out.Summary.EvacuationStatus)
		assert.NotEqual(t, ModeQuiet, out.Mode, "unknown evac state is never quiet")
	})
}

// allSourcesOK seeds + marks healthy every registry row the summary reads.
func allSourcesOK(t *testing.T, st *store.Store) {
	t.Helper()
	for _, id := range []string{"usgs", "calfire", "nws", "chp"} { // seeded by seedEvents
		recordOK(t, st, id)
	}
	for _, id := range []string{"wfigs", "caltrans", "caloes"} {
		seedSource(t, st, id)
		recordOK(t, st, id)
	}
}

// TestSummary_DomainRollups: the merged roads (road_incident events +
// chain_control/road_segment condition features) and fire (wildfire events +
// fire_weather condition layer) domains, over a fake hazardsBuilder.
func TestSummary_DomainRollups(t *testing.T) {
	s := newTestService(t)
	allSourcesOK(t, s.Store)
	// One ACTIVE road incident event alongside the RESOLVED fixture (which
	// must not appear anywhere).
	upsert(t, s.Store, &gridv1.Event{
		Id: "chp:i2", Layer: gridv1.Layer_ROAD_INCIDENT,
		Severity: gridv1.Severity_MINOR, Status: gridv1.EventStatus_ACTIVE,
		Headline:   "Disabled vehicle blocking shoulder",
		Geometry:   pointGeom(38.06, -120.54),
		ObservedAt: timestamppb.New(base.Add(time.Hour)),
		Provenance: &gridv1.Provenance{SourceId: "chp"},
	})

	hb := &fakeCondBuilder{byLayer: map[string]condFake{
		hazards.LayerChainControl: {status: "OK", features: []hazards.Feature{
			condFeature("chain:hwy-4", hazards.LayerChainControl, "MODERATE", "R2 chains required on Hwy 4", nil),
		}},
		hazards.LayerRoadSegment: {status: "OK", features: []hazards.Feature{
			condFeature("road:hwy-4", hazards.LayerRoadSegment, "INFO", "Hwy 4 clear", nil),
			condFeature("road:hwy-108", hazards.LayerRoadSegment, "INFO", "Hwy 108 clear", nil),
		}},
		hazards.LayerFireWeather: {status: "OK", features: []hazards.Feature{
			condFeature("fw:region", hazards.LayerFireWeather, "SEVERE", "Red Flag Warning",
				&hazards.FireWeatherProps{State: "red-flag"}),
		}},
	}}
	out := decodeSummary(t, getSummaryWith(t, s, hb, "calaveras"))

	// roads: 1 event + a MODERATE chain control + 2 INFO road segments. The two
	// clear (INFO) segments are baseline monitoring, not active hazards, so they
	// drop from active_count and headlines; the event and the chain control
	// remain. Headlines are top-3 by severity (events before conditions on ties).
	roads := out.domain(t, "roads")
	assert.Equal(t, "OK", roads.Status)
	assert.Equal(t, 2, roads.ActiveCount)
	assert.Equal(t, "MODERATE", roads.HighestSeverity)
	require.Len(t, roads.Headlines, 2, "the two clear INFO segments are excluded")
	assert.Equal(t, "chain:hwy-4", roads.Headlines[0].ID)
	assert.Equal(t, "MODERATE", roads.Headlines[0].Severity)
	assert.Equal(t, "chp:i2", roads.Headlines[1].ID)

	// fire: wildfire event + fire_weather banner merged.
	fire := out.domain(t, "fire")
	assert.Equal(t, "OK", fire.Status)
	assert.Equal(t, 2, fire.ActiveCount)
	assert.Equal(t, "SEVERE", fire.HighestSeverity)
	require.Len(t, fire.Headlines, 2)
	assert.Equal(t, "calfire:f1", fire.Headlines[0].ID, "event precedes the condition banner on severity ties")
	assert.Equal(t, "fw:region", fire.Headlines[1].ID)

	// Single-layer domains.
	weather := out.domain(t, "weather")
	assert.Equal(t, "OK", weather.Status)
	assert.Equal(t, 1, weather.ActiveCount)
	assert.Equal(t, "MODERATE", weather.HighestSeverity)
	seismic := out.domain(t, "seismic")
	assert.Equal(t, 1, seismic.ActiveCount)
	require.Len(t, seismic.Headlines, 1)
	assert.Equal(t, "usgs:q1", seismic.Headlines[0].ID)
	evac := out.domain(t, "evacuation")
	assert.Equal(t, "OK", evac.Status)
	assert.Equal(t, 0, evac.ActiveCount)
	assert.Equal(t, "INFO", evac.HighestSeverity)

	// Healthy sources sidecar carries last_success_at.
	require.Len(t, out.Sources, 7)
	for _, src := range out.Sources {
		assert.Equal(t, "OK", src.Status, src.ID)
		assert.NotEmpty(t, src.LastSuccessAt, src.ID)
	}

	// Worst-status wins: a STALE condition layer degrades its domain.
	hb.byLayer[hazards.LayerFireWeather] = condFake{status: "STALE"}
	hb.byLayer[hazards.LayerChainControl] = condFake{status: "UNAVAILABLE"}
	out = decodeSummary(t, getSummaryWith(t, s, hb, "calaveras"))
	assert.Equal(t, "STALE", out.domain(t, "fire").Status)
	assert.Equal(t, "UNAVAILABLE", out.domain(t, "roads").Status)
}

// TestSummary_BaselineConditionsAreNotActive locks the Ebbetts Pass case: on a
// genuinely QUIET area, baseline INFO condition features (OPEN road segments, a
// "normal" fire-weather banner) must NOT count as active or surface as
// headlines, even though the layers are healthy (OK). Regression for the summary
// reporting roads active_count 4 / fire 1 while total_active was 0.
func TestSummary_BaselineConditionsAreNotActive(t *testing.T) {
	s := newTestService(t) // seeds a SEVERE wildfire event (calfire:f1), no road incident
	allSourcesOK(t, s.Store)
	hb := &fakeCondBuilder{byLayer: map[string]condFake{
		hazards.LayerChainControl: {status: "OK"},
		hazards.LayerRoadSegment: {status: "OK", features: []hazards.Feature{
			condFeature("road:hwy-4", hazards.LayerRoadSegment, "INFO", "Hwy 4 clear", nil),
			condFeature("road:hwy-108", hazards.LayerRoadSegment, "INFO", "Hwy 108 clear", nil),
		}},
		hazards.LayerFireWeather: {status: "OK", features: []hazards.Feature{
			condFeature("fw:region", hazards.LayerFireWeather, "INFO", "Fire weather: normal",
				&hazards.FireWeatherProps{State: "normal"}),
		}},
	}}
	out := decodeSummary(t, getSummaryWith(t, s, hb, "calaveras"))

	// roads: no incident, two clear (INFO) segments → nothing active.
	roads := out.domain(t, "roads")
	assert.Equal(t, "OK", roads.Status, "healthy sources stay OK even with nothing active")
	assert.Equal(t, 0, roads.ActiveCount, "clear INFO road segments are baseline, not active")
	assert.Empty(t, roads.Headlines)
	assert.Equal(t, "INFO", roads.HighestSeverity)

	// fire: the seeded SEVERE wildfire event counts; the normal fire-weather
	// banner does not (it is baseline, not a hazard).
	fire := out.domain(t, "fire")
	assert.Equal(t, 1, fire.ActiveCount, "the event counts; the normal fire-weather banner does not")
	assert.Equal(t, "SEVERE", fire.HighestSeverity)
	require.Len(t, fire.Headlines, 1)
	assert.Equal(t, "calfire:f1", fire.Headlines[0].ID, "baseline fw:region banner excluded from headlines")
}

// TestSummary_TopEventsOrderingAndCap: severity_rank desc, then observed_at
// desc, capped at 5.
func TestSummary_TopEventsOrderingAndCap(t *testing.T) {
	s := newTestService(t)
	add := func(id string, sev gridv1.Severity, observed time.Time) {
		upsert(t, s.Store, &gridv1.Event{
			Id: id, Layer: gridv1.Layer_EARTHQUAKE, Severity: sev,
			Status: gridv1.EventStatus_ACTIVE, Headline: id,
			Geometry:   pointGeom(38.3, -120.2),
			ObservedAt: timestamppb.New(observed),
			Provenance: &gridv1.Provenance{SourceId: "usgs"},
		})
	}
	// Fixtures: f1 SEVERE@base, q1 MODERATE@base+3h, a1 MODERATE@base-1h.
	add("usgs:q2", gridv1.Severity_EXTREME, base.Add(-2*time.Hour))
	add("usgs:q3", gridv1.Severity_MODERATE, base.Add(4*time.Hour))
	add("usgs:q4", gridv1.Severity_INFO, base)

	out := decodeSummary(t, getSummaryWith(t, s, condOKBuilder("normal"), "calaveras"))
	assert.Equal(t, 6, out.Summary.TotalActive)
	require.Len(t, out.Summary.TopEvents, 5, "cap 5 — the INFO event falls off")
	var ids []string
	for _, e := range out.Summary.TopEvents {
		ids = append(ids, e.ID)
	}
	// EXTREME first regardless of age, then severity desc / observed_at desc;
	// the INFO event (usgs:q4) is cut by the cap.
	assert.Equal(t, []string{"usgs:q2", "calfire:f1", "usgs:q3", "usgs:q1", "wx:a1"}, ids)
}

// TestSummary_ModeTransitions: the handler end to end through QUIET → WATCH
// escalations that depend on conditions and source health.
func TestSummary_ModeTransitions(t *testing.T) {
	s := newTestService(t)
	allSourcesOK(t, s.Store)
	// Resolve the SEVERE wildfire: remaining ACTIVE signal is q1 (MODERATE);
	// a1 is SCHEDULED and must not escalate the mode.
	require.NoError(t, s.Store.TransitionEvents(context.Background(),
		[]string{"calfire:f1"}, gridv1.EventStatus_RESOLVED, base.Add(4*time.Hour)))

	out := decodeSummary(t, getSummaryWith(t, s, condOKBuilder("normal"), "calaveras"))
	assert.Equal(t, ModeQuiet, out.Mode, "healthy sources + nothing above MODERATE")
	require.NotNil(t, out.Summary.ActiveEvacuations)
	assert.Equal(t, 0, *out.Summary.ActiveEvacuations)

	// Red-flag fire weather alone raises WATCH.
	out = decodeSummary(t, getSummaryWith(t, s, condOKBuilder("red-flag"), "calaveras"))
	assert.Equal(t, ModeWatch, out.Mode)

	// A dark layer while a MODERATE event is active raises WATCH.
	hb := condOKBuilder("normal")
	hb.byLayer[hazards.LayerChainControl] = condFake{status: "UNAVAILABLE"}
	out = decodeSummary(t, getSummaryWith(t, s, hb, "calaveras"))
	assert.Equal(t, ModeWatch, out.Mode)

	// Same dark layer with the MODERATE quake resolved: nothing >= MODERATE
	// is active, so the gap alone stays QUIET.
	require.NoError(t, s.Store.TransitionEvents(context.Background(),
		[]string{"usgs:q1"}, gridv1.EventStatus_RESOLVED, base.Add(5*time.Hour)))
	out = decodeSummary(t, getSummaryWith(t, s, hb, "calaveras"))
	assert.Equal(t, ModeQuiet, out.Mode)
}

// TestSummary_UnknownPlace: the RPC maps an unknown place to gRPC NotFound.
func TestSummary_UnknownPlace(t *testing.T) {
	s := newTestService(t)
	_, err := NewGridServer(s).GetPlaceSummary(context.Background(),
		&gridv1.GetPlaceSummaryRequest{Place: "atlantis"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
	assert.Contains(t, st.Message(), "atlantis")
}
