package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	api "github.com/dpup/info.ersn.net/server/api/v1"
)

type fakeRoadsAPI struct {
	byArea map[string]*api.ListIncidentsResponse
	errs   map[string]error
	calls  []string
	// Per-feed health reported by IncidentFeedHealth (services keeps serving
	// the surviving feed when only one KML feed fails).
	feedChpErr  error
	feedLaneErr error
}

func (f *fakeRoadsAPI) ListIncidents(ctx context.Context, req *api.ListIncidentsRequest) (*api.ListIncidentsResponse, error) {
	f.calls = append(f.calls, req.GetArea())
	if err := f.errs[req.GetArea()]; err != nil {
		return nil, err
	}
	return f.byArea[req.GetArea()], nil
}

func (f *fakeRoadsAPI) IncidentFeedHealth() (chpErr, laneErr error, at time.Time) {
	return f.feedChpErr, f.feedLaneErr, time.Now()
}

func testIncidents() (*api.Incident, *api.Incident, *api.Incident) {
	started := timestamppb.New(time.Date(2026, 7, 4, 6, 24, 0, 0, time.UTC))
	updated := timestamppb.New(time.Date(2026, 7, 4, 7, 0, 0, 0, time.UTC))
	enhanced := &api.Incident{
		Id:                  "250916ST0066",
		Type:                api.AlertType_INCIDENT,
		Severity:            api.AlertSeverity_CRITICAL,
		Location:            &api.Coordinates{Latitude: 38.2, Longitude: -120.35},
		LocationDescription: "Hwy 4 at Avery",
		Description:         "Vehicle fire blocking the right lane",
		LogNumber:           "250916ST0066",
		Started:             started,
		LastUpdated:         updated,
		CondensedSummary:    "Vehicle fire on Hwy 4",
		Impact:              api.AlertImpact_IMPACT_SEVERE,
		Metadata: map[string]string{
			"duration":           "several hours",
			"emergency_services": "on scene",
			"style_url":          "#chp",       // internal KML artifact — must be stripped
			"source":             "CHP report", // duplicates provenance — must be stripped
		},
	}
	closure := &api.Incident{
		Id:                  "closure-hwy-4-avery",
		Type:                api.AlertType_CLOSURE,
		Severity:            api.AlertSeverity_WARNING,
		Location:            &api.Coordinates{Latitude: 38.25, Longitude: -120.3},
		LocationDescription: "Hwy 4 EB near Avery",
		Description:         "One-way traffic control for utility work",
	}
	locationless := &api.Incident{
		Id:          "no-location",
		Type:        api.AlertType_INCIDENT,
		Description: "Geometry-only placemark",
	}
	return enhanced, closure, locationless
}

func TestRoadIncidentPoll(t *testing.T) {
	enhanced, closure, locationless := testIncidents()
	roads := &fakeRoadsAPI{byArea: map[string]*api.ListIncidentsResponse{
		"mother-lode":  {Incidents: []*api.Incident{enhanced, closure, locationless}},
		"high-country": {Incidents: []*api.Incident{enhanced}}, // overlap: dedupe by id
	}}
	n := NewRoadIncidentNormalizer(testConfig(), roads)
	assert.Equal(t, []string{"chp", "caltrans"}, n.SourceIDs())

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Nil(t, res.PerSource)
	assert.Equal(t, []string{"mother-lode", "high-country"}, roads.calls)
	// Locationless skipped; the duplicate across areas collapses.
	assert.ElementsMatch(t, []string{"chp:250916ST0066", "chp:closure-hwy-4-avery"}, eventIDs(res.Events))

	ev := eventByID(t, res.Events, "chp:250916ST0066")
	assert.Equal(t, gridv1.Layer_ROAD_INCIDENT, ev.Layer)
	// Enhanced incident: the short condensed_summary is the headline; the long
	// detail text is the description. summary stays empty for road incidents —
	// there is no distinct middle tier (grid model §3).
	assert.Equal(t, "Vehicle fire on Hwy 4", ev.Headline)
	assert.Empty(t, ev.Summary)
	assert.Equal(t, "Vehicle fire blocking the right lane", ev.Description)
	assert.Equal(t, "incident", ev.Category)
	assert.Equal(t, gridv1.Severity_SEVERE, ev.Severity)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.Status)
	assert.Equal(t, "Hwy 4 at Avery", ev.AreaLabel)
	require.NotNil(t, ev.Geometry)
	assert.Equal(t, 38.2, ev.Geometry.Centroid.Lat)
	assert.Equal(t, -120.35, ev.Geometry.Centroid.Lng)
	assert.Equal(t, enhanced.Started.AsTime(), ev.Effective.AsTime())
	assert.Equal(t, enhanced.LastUpdated.AsTime(), ev.ObservedAt.AsTime())

	// CHP dispatch incidents attribute to the "chp" source row.
	require.NotNil(t, ev.Provenance)
	assert.Equal(t, "chp", ev.Provenance.SourceId)
	assert.Equal(t, "CHP / Caltrans", ev.Provenance.SourceName)
	assert.Equal(t, "quickmap.dot.ca.gov", ev.Provenance.Attribution)

	d := ev.GetRoadIncident()
	require.NotNil(t, d)
	// Detail carries only kind-specific fields; incident type / location / short
	// line live in the envelope (category / area_label / headline, asserted above).
	assert.Equal(t, "250916ST0066", d.LogNumber)
	assert.Equal(t, "severe", d.Impact)
	assert.Equal(t, "several hours", d.Duration)
	assert.Equal(t, "on scene", d.Metadata["emergency_services"])
	// Internal/redundant metadata keys are stripped from the public map;
	// duration is promoted to the typed field, source duplicates provenance.
	for _, k := range []string{"style_url", "source", "duration"} {
		_, present := d.Metadata[k]
		assert.False(t, present, "internal metadata key %q must be stripped", k)
	}

	// AI-enhanced (impact set) => Enhancement provenance from config.
	require.NotNil(t, ev.Enhancement)
	assert.Equal(t, "gpt-5-mini", ev.Enhancement.Model)
	assert.Equal(t, []string{"summary", "description", "impact"}, ev.Enhancement.Fields)

	// Lane closures attribute to "caltrans"; not enhanced => no Enhancement.
	cl := eventByID(t, res.Events, "chp:closure-hwy-4-avery")
	// Unenhanced incident (no condensed_summary): the detail text is the only
	// text, so it stays the headline and there is no separate summary/description.
	assert.Equal(t, "One-way traffic control for utility work", cl.Headline)
	assert.Empty(t, cl.Summary)
	assert.Empty(t, cl.Description)
	assert.Equal(t, "closure", cl.Category)
	assert.Equal(t, gridv1.Severity_MODERATE, cl.Severity)
	assert.Equal(t, "caltrans", cl.Provenance.SourceId)
	assert.Equal(t, "Caltrans", cl.Provenance.SourceName)
	assert.Equal(t, "quickmap.dot.ca.gov", cl.Provenance.Attribution)
	assert.Nil(t, cl.Enhancement)
	assert.Empty(t, cl.GetRoadIncident().Impact)
	assert.Empty(t, cl.GetRoadIncident().Duration)
	assert.Nil(t, cl.Effective, "lane closures carry no dispatch time")
}

func TestRoadIncidentPoll_PartialFailure(t *testing.T) {
	enhanced, _, _ := testIncidents()
	roads := &fakeRoadsAPI{
		byArea: map[string]*api.ListIncidentsResponse{
			"mother-lode": {Incidents: []*api.Incident{enhanced}},
		},
		errs: map[string]error{"high-country": assert.AnError},
	}
	n := NewRoadIncidentNormalizer(testConfig(), roads)

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	// Both source rows degrade together — they share the per-area calls.
	require.NotNil(t, res.PerSource)
	assert.Error(t, res.PerSource["chp"])
	assert.Error(t, res.PerSource["caltrans"])
	assert.ElementsMatch(t, []string{"chp:250916ST0066"}, eventIDs(res.Events))
}

func TestRoadIncidentPoll_AllAreasFail(t *testing.T) {
	// Every area failing (which in the service co-occurs with both KML feeds
	// being down) stays a hard error, as before.
	roads := &fakeRoadsAPI{
		errs: map[string]error{
			"mother-lode":  assert.AnError,
			"high-country": assert.AnError,
		},
		feedChpErr:  assert.AnError,
		feedLaneErr: assert.AnError,
	}
	n := NewRoadIncidentNormalizer(testConfig(), roads)
	_, err := n.Poll(testCtx(), nil)
	assert.Error(t, err)
}

// A single dead KML feed is invisible at the ListIncidents level (the service
// serves the survivor), but the sweep must not resolve the dead feed's
// events: its source row must carry the error while the surviving feed's
// events keep flowing and its sweep stays live.
func TestRoadIncidentPoll_SingleFeedDown(t *testing.T) {
	_, closure, _ := testIncidents()
	roads := &fakeRoadsAPI{
		byArea: map[string]*api.ListIncidentsResponse{
			"mother-lode":  {Incidents: []*api.Incident{closure}},
			"high-country": {},
		},
		feedChpErr: assert.AnError, // chp-only.kml down; lane closures healthy
	}
	n := NewRoadIncidentNormalizer(testConfig(), roads)

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err, "one dead feed must degrade, not hard-fail the poll")
	require.NotNil(t, res.PerSource)
	assert.Error(t, res.PerSource["chp"], "the dead feed's source row must carry the error (sweep suppressed)")
	assert.NotContains(t, res.PerSource, "caltrans", "the healthy feed still sweeps")
	// The surviving feed's closures still process.
	assert.ElementsMatch(t, []string{"chp:closure-hwy-4-avery"}, eventIDs(res.Events))

	// Symmetric: lane-closure feed down, CHP healthy.
	enhanced, _, _ := testIncidents()
	roads = &fakeRoadsAPI{
		byArea: map[string]*api.ListIncidentsResponse{
			"mother-lode":  {Incidents: []*api.Incident{enhanced}},
			"high-country": {},
		},
		feedLaneErr: assert.AnError,
	}
	res, err = NewRoadIncidentNormalizer(testConfig(), roads).Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Error(t, res.PerSource["caltrans"])
	assert.NotContains(t, res.PerSource, "chp")
	assert.ElementsMatch(t, []string{"chp:250916ST0066"}, eventIDs(res.Events))
}

// All areas failing with only ONE feed down is still a degraded poll, not a
// hard error: both source rows carry the area error so nothing sweeps, but
// health stays per-source.
func TestRoadIncidentPoll_AllAreasFailOneFeedDownDegrades(t *testing.T) {
	roads := &fakeRoadsAPI{
		errs: map[string]error{
			"mother-lode":  assert.AnError,
			"high-country": assert.AnError,
		},
		feedChpErr: assert.AnError,
	}
	res, err := NewRoadIncidentNormalizer(testConfig(), roads).Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Error(t, res.PerSource["chp"])
	assert.Error(t, res.PerSource["caltrans"])
	assert.Empty(t, res.Events)
}

// priorRoadIncidentEvent builds a stored, AI-enhanced road incident event for
// scripting a Prior.
func priorRoadIncidentEvent(id string) *gridv1.Event {
	ev := NewEvent(id, gridv1.Layer_ROAD_INCIDENT, gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE,
		"Enhanced: vehicle fire blocking the right lane near Avery")
	ev.Summary = "Vehicle fire on Hwy 4"
	ev.Provenance = NewProvenance("chp", "CHP / Caltrans", "quickmap.dot.ca.gov", "")
	ev.Enhancement = &gridv1.Enhancement{Model: "gpt-5-mini", Fields: enhancedFields}
	return ev
}

// After a restart the AI cache is empty and incidents beyond the enhancement
// budget arrive with impact UNSPECIFIED. When the store already holds an
// enhanced version, the prior event is carried forward verbatim (hash-equal
// => no spurious raw revision + re-enhancement revision pair).
func TestRoadIncidentPoll_UnenhancedIncomingKeepsPriorEnhanced(t *testing.T) {
	raw := &api.Incident{
		Id:                  "250916ST0066",
		Type:                api.AlertType_INCIDENT,
		Severity:            api.AlertSeverity_CRITICAL,
		Location:            &api.Coordinates{Latitude: 38.2, Longitude: -120.35},
		LocationDescription: "Hwy 4 at Avery",
		Description:         "VEH FIRE RHS", // raw feed text, not yet AI-processed
		LogNumber:           "250916ST0066",
		Impact:              api.AlertImpact_ALERT_IMPACT_UNSPECIFIED,
	}
	roads := &fakeRoadsAPI{byArea: map[string]*api.ListIncidentsResponse{
		"mother-lode":  {Incidents: []*api.Incident{raw}},
		"high-country": {},
	}}
	prior := &scriptedPrior{events: []*gridv1.Event{priorRoadIncidentEvent("chp:250916ST0066")}}

	res, err := NewRoadIncidentNormalizer(testConfig(), roads).Poll(testCtx(), prior)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	ev := res.Events[0]
	assert.Equal(t, "chp:250916ST0066", ev.Id)
	assert.Equal(t, "Enhanced: vehicle fire blocking the right lane near Avery", ev.Headline,
		"the stored enhanced event must be carried forward verbatim, not clobbered by the raw copy")
	assert.Equal(t, "Vehicle fire on Hwy 4", ev.Summary)
	require.NotNil(t, ev.Enhancement)
}

// Controls for the carry-forward: it must apply ONLY when the incoming copy
// is unenhanced AND the prior is enhanced.
func TestRoadIncidentPoll_CarryForwardControls(t *testing.T) {
	t.Run("genuinely new incident emits raw immediately", func(t *testing.T) {
		raw := &api.Incident{
			Id:          "NEW123",
			Type:        api.AlertType_INCIDENT,
			Location:    &api.Coordinates{Latitude: 38.2, Longitude: -120.35},
			Description: "TRFC COLLISION",
		}
		roads := &fakeRoadsAPI{byArea: map[string]*api.ListIncidentsResponse{
			"mother-lode":  {Incidents: []*api.Incident{raw}},
			"high-country": {},
		}}
		res, err := NewRoadIncidentNormalizer(testConfig(), roads).Poll(testCtx(), &scriptedPrior{})
		require.NoError(t, err)
		require.Len(t, res.Events, 1)
		assert.Equal(t, "TRFC COLLISION", res.Events[0].Headline, "availability first: new incidents are never held back")
		assert.Nil(t, res.Events[0].Enhancement)
	})

	t.Run("enhanced incoming supersedes the prior", func(t *testing.T) {
		enhanced, _, _ := testIncidents()
		roads := &fakeRoadsAPI{byArea: map[string]*api.ListIncidentsResponse{
			"mother-lode":  {Incidents: []*api.Incident{enhanced}},
			"high-country": {},
		}}
		prior := &scriptedPrior{events: []*gridv1.Event{priorRoadIncidentEvent("chp:250916ST0066")}}
		res, err := NewRoadIncidentNormalizer(testConfig(), roads).Poll(testCtx(), prior)
		require.NoError(t, err)
		require.Len(t, res.Events, 1)
		assert.Equal(t, "Vehicle fire on Hwy 4", res.Events[0].Headline,
			"a freshly enhanced incoming copy must replace the stored version")
	})

	t.Run("unenhanced prior does not suppress raw updates", func(t *testing.T) {
		raw := &api.Incident{
			Id:          "250916ST0066",
			Type:        api.AlertType_INCIDENT,
			Location:    &api.Coordinates{Latitude: 38.2, Longitude: -120.35},
			Description: "VEH FIRE RHS [UPDATED]",
		}
		roads := &fakeRoadsAPI{byArea: map[string]*api.ListIncidentsResponse{
			"mother-lode":  {Incidents: []*api.Incident{raw}},
			"high-country": {},
		}}
		priorRaw := priorRoadIncidentEvent("chp:250916ST0066")
		priorRaw.Enhancement = nil // stored copy was itself raw
		priorRaw.Headline = "VEH FIRE RHS"
		prior := &scriptedPrior{events: []*gridv1.Event{priorRaw}}
		res, err := NewRoadIncidentNormalizer(testConfig(), roads).Poll(testCtx(), prior)
		require.NoError(t, err)
		require.Len(t, res.Events, 1)
		assert.Equal(t, "VEH FIRE RHS [UPDATED]", res.Events[0].Headline,
			"raw-over-raw carries the newest feed text (new detail lines must flow)")
	})
}

func TestImpactSlug(t *testing.T) {
	assert.Equal(t, "", impactSlug(api.AlertImpact_ALERT_IMPACT_UNSPECIFIED))
	assert.Equal(t, "none", impactSlug(api.AlertImpact_IMPACT_NONE))
	assert.Equal(t, "light", impactSlug(api.AlertImpact_IMPACT_LIGHT))
	assert.Equal(t, "moderate", impactSlug(api.AlertImpact_IMPACT_MODERATE))
	assert.Equal(t, "severe", impactSlug(api.AlertImpact_IMPACT_SEVERE))
}
