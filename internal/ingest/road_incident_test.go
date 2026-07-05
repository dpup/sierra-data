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
}

func (f *fakeRoadsAPI) ListIncidents(ctx context.Context, req *api.ListIncidentsRequest) (*api.ListIncidentsResponse, error) {
	f.calls = append(f.calls, req.GetArea())
	if err := f.errs[req.GetArea()]; err != nil {
		return nil, err
	}
	return f.byArea[req.GetArea()], nil
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
		Metadata:            map[string]string{"duration": "several hours", "emergency_services": "on scene"},
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

	res, err := n.Poll(testCtx())
	require.NoError(t, err)
	assert.Nil(t, res.PerSource)
	assert.Equal(t, []string{"mother-lode", "high-country"}, roads.calls)
	// Locationless skipped; the duplicate across areas collapses.
	assert.ElementsMatch(t, []string{"chp:250916ST0066", "chp:closure-hwy-4-avery"}, eventIDs(res.Events))

	ev := eventByID(t, res.Events, "chp:250916ST0066")
	assert.Equal(t, gridv1.Layer_ROAD_INCIDENT, ev.Layer)
	assert.Equal(t, "Vehicle fire blocking the right lane", ev.Headline) // shipped: the description
	assert.Equal(t, "Vehicle fire on Hwy 4", ev.Summary)
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
	assert.Equal(t, "250916ST0066", d.LogNumber)
	assert.Equal(t, "incident", d.IncidentType)
	assert.Equal(t, "Hwy 4 at Avery", d.LocationDescription)
	assert.Equal(t, "severe", d.Impact)
	assert.Equal(t, "several hours", d.Duration)
	assert.Equal(t, "Vehicle fire on Hwy 4", d.CondensedSummary)
	assert.Equal(t, "on scene", d.Metadata["emergency_services"])

	// AI-enhanced (impact set) => Enhancement provenance from config.
	require.NotNil(t, ev.Enhancement)
	assert.Equal(t, "gpt-5-mini", ev.Enhancement.Model)
	assert.Equal(t, []string{"summary", "description", "impact"}, ev.Enhancement.Fields)

	// Lane closures attribute to "caltrans"; not enhanced => no Enhancement.
	cl := eventByID(t, res.Events, "chp:closure-hwy-4-avery")
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

	res, err := n.Poll(testCtx())
	require.NoError(t, err)
	// Both source rows degrade together — they share the per-area calls.
	require.NotNil(t, res.PerSource)
	assert.Error(t, res.PerSource["chp"])
	assert.Error(t, res.PerSource["caltrans"])
	assert.ElementsMatch(t, []string{"chp:250916ST0066"}, eventIDs(res.Events))
}

func TestRoadIncidentPoll_AllAreasFail(t *testing.T) {
	roads := &fakeRoadsAPI{errs: map[string]error{
		"mother-lode":  assert.AnError,
		"high-country": assert.AnError,
	}}
	n := NewRoadIncidentNormalizer(testConfig(), roads)
	_, err := n.Poll(testCtx())
	assert.Error(t, err)
}

func TestImpactSlug(t *testing.T) {
	assert.Equal(t, "", impactSlug(api.AlertImpact_ALERT_IMPACT_UNSPECIFIED))
	assert.Equal(t, "none", impactSlug(api.AlertImpact_IMPACT_NONE))
	assert.Equal(t, "light", impactSlug(api.AlertImpact_IMPACT_LIGHT))
	assert.Equal(t, "moderate", impactSlug(api.AlertImpact_IMPACT_MODERATE))
	assert.Equal(t, "severe", impactSlug(api.AlertImpact_IMPACT_SEVERE))
}
