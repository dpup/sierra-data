package ingest

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/calfire"
	"github.com/dpup/info.ersn.net/server/internal/clients/wfigs"
)

const calfireFixture = `[
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

// The northern "Ambiguous" perimeter is deliberately listed FIRST so the test
// proves collision suffixes follow centroid (lat, lng) order, not slice order.
const wfigsFixture = `{
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
      "geometry": {"type": "Polygon", "coordinates": [[[-119.9,38.0],[-119.8,38.0],[-119.8,38.1],[-119.9,38.1],[-119.9,38.0]]]}
    }
  ]
}`

func newWildfireNormalizer(cfDoer, wfDoer *fakeDoer) *WildfireNormalizer {
	return NewWildfireNormalizer(
		testConfig(),
		calfire.NewClientWithHTTPDoer("https://calfire.test", cfDoer),
		wfigs.NewClientWithHTTPDoer("https://wfigs.test", wfDoer),
	)
}

func eventByID(t *testing.T, events []*gridv1.Event, id string) *gridv1.Event {
	t.Helper()
	for _, ev := range events {
		if ev.Id == id {
			return ev
		}
	}
	t.Fatalf("no event with id %q", id)
	return nil
}

func eventIDs(events []*gridv1.Event) []string {
	ids := make([]string, len(events))
	for i, ev := range events {
		ids[i] = ev.Id
	}
	return ids
}

func TestWildfirePoll_JoinAndStandalone(t *testing.T) {
	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: wfigsFixture})
	assert.Equal(t, []string{"calfire", "wfigs"}, n.SourceIDs())

	res, err := n.Poll(testCtx())
	require.NoError(t, err)
	assert.Nil(t, res.PerSource)

	// Salt Springs joins; Far Away (out of bounds) + No Coords are dropped;
	// the two ambiguous perimeters and Lonely are standalone.
	assert.ElementsMatch(t, []string{
		"calfire:abc-123",
		"calfire:ambiguous",
		"wfigs:ambiguous",
		"wfigs:ambiguous-2",
		"wfigs:lonely",
	}, eventIDs(res.Events))

	// Adopted perimeter: polygon geometry + has_perimeter.
	salt := eventByID(t, res.Events, "calfire:abc-123")
	assert.Equal(t, "Salt Springs Fire — 1200 ac, 35% contained", salt.Headline) // shipped format, exact
	assert.Equal(t, gridv1.Severity_SEVERE, salt.Severity)                       // <50% contained
	assert.Equal(t, gridv1.EventStatus_ACTIVE, salt.Status)
	assert.Equal(t, "wildfire", salt.Category)
	assert.Equal(t, "near Hwy 4", salt.AreaLabel)
	assert.Equal(t, "https://www.fire.ca.gov/incidents/salt-springs", salt.CanonicalUrl)
	require.NotNil(t, salt.Effective)
	require.NotNil(t, salt.ObservedAt)
	require.NotNil(t, salt.Geometry)
	var geom struct {
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(salt.Geometry.Geojson, &geom))
	assert.Equal(t, "Polygon", geom.Type)
	assert.InDelta(t, 38.2, salt.Geometry.Centroid.Lat, 1e-9)
	d := salt.GetWildfire()
	require.NotNil(t, d)
	assert.True(t, d.HasPerimeter)
	assert.Equal(t, 1200.0, d.Acres)
	assert.Equal(t, int32(35), d.Containment)
	assert.Equal(t, "Calaveras", d.County)
	require.NotNil(t, salt.Provenance)
	assert.Equal(t, "calfire", salt.Provenance.SourceId)
	assert.Equal(t, "CAL FIRE", salt.Provenance.SourceName)
	assert.Equal(t, "CAL FIRE / WFIGS", salt.Provenance.Attribution)

	// Ambiguous name: the incident must NOT adopt either perimeter.
	amb := eventByID(t, res.Events, "calfire:ambiguous")
	assert.Equal(t, "Ambiguous Fire — 10 ac, 90% contained", amb.Headline)
	assert.Equal(t, gridv1.Severity_MODERATE, amb.Severity)
	assert.False(t, amb.GetWildfire().HasPerimeter)
	require.NoError(t, json.Unmarshal(amb.Geometry.Geojson, &geom))
	assert.Equal(t, "Point", geom.Type)

	// Collision suffixes ordered by centroid latitude: the southern perimeter
	// (fixture-second) takes the bare id, the northern one gets -2.
	south := eventByID(t, res.Events, "wfigs:ambiguous")
	assert.InDelta(t, 38.0, south.Geometry.Centroid.Lat, 1e-9)
	north := eventByID(t, res.Events, "wfigs:ambiguous-2")
	assert.InDelta(t, 38.4, north.Geometry.Centroid.Lat, 1e-9)

	lonely := eventByID(t, res.Events, "wfigs:lonely")
	assert.Equal(t, "Lonely — 250 ac, 10% contained", lonely.Headline)
	assert.Equal(t, gridv1.Severity_SEVERE, lonely.Severity)
	ld := lonely.GetWildfire()
	require.NotNil(t, ld)
	assert.True(t, ld.HasPerimeter)
	assert.Equal(t, "Human", ld.Cause)
	require.NotNil(t, lonely.Provenance)
	assert.Equal(t, "wfigs", lonely.Provenance.SourceId)
	assert.Equal(t, "NIFC WFIGS", lonely.Provenance.SourceName)
	assert.Equal(t, "NIFC / WFIGS", lonely.Provenance.Attribution)
}

func TestWildfirePoll_PartialFailure(t *testing.T) {
	// CAL FIRE down: perimeter events still flow, calfire flagged in PerSource.
	n := newWildfireNormalizer(&fakeDoer{err: assert.AnError}, &fakeDoer{resp: wfigsFixture})
	res, err := n.Poll(testCtx())
	require.NoError(t, err)
	require.NotNil(t, res.PerSource)
	assert.Error(t, res.PerSource["calfire"])
	assert.NotContains(t, res.PerSource, "wfigs")
	// With no incidents to adopt them, all four perimeters are standalone.
	assert.ElementsMatch(t, []string{
		"wfigs:ambiguous", "wfigs:ambiguous-2", "wfigs:lonely", "wfigs:saltsprings",
	}, eventIDs(res.Events))

	// WFIGS down: incident points still flow, wfigs flagged.
	n = newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{err: assert.AnError})
	res, err = n.Poll(testCtx())
	require.NoError(t, err)
	assert.Error(t, res.PerSource["wfigs"])
	assert.ElementsMatch(t, []string{"calfire:abc-123", "calfire:ambiguous"}, eventIDs(res.Events))
	assert.False(t, eventByID(t, res.Events, "calfire:abc-123").GetWildfire().HasPerimeter)
}

func TestWildfirePoll_BothFail(t *testing.T) {
	n := newWildfireNormalizer(&fakeDoer{err: assert.AnError}, &fakeDoer{err: assert.AnError})
	_, err := n.Poll(testCtx())
	assert.Error(t, err)
}
