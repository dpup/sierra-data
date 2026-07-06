package ingest

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/calfire"
	"github.com/dpup/sierra-data/internal/clients/wfigs"
	"github.com/dpup/sierra-data/internal/store"
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

// scriptedPrior is a canned Prior for normalizer tests (the scheduler builds
// the real one from the store).
type scriptedPrior struct {
	events []*gridv1.Event
}

func (p *scriptedPrior) ByID(id string) *gridv1.Event {
	for _, ev := range p.events {
		if ev.GetId() == id {
			return ev
		}
	}
	return nil
}

func (p *scriptedPrior) ForSource(sourceID string) []*gridv1.Event {
	var out []*gridv1.Event
	for _, ev := range p.events {
		if ev.GetProvenance().GetSourceId() == sourceID {
			out = append(out, ev)
		}
	}
	return out
}

// priorWildfireEvent builds a minimal stored wildfire event for scripting a
// Prior: id + source row + a point centroid (enough for continuity picks) +
// has_perimeter detail.
func priorWildfireEvent(id, sourceID string, lat, lng float64, hasPerimeter bool) *gridv1.Event {
	ev := NewEvent(id, gridv1.Layer_WILDFIRE, gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE, "prior "+id)
	ev.Provenance = NewProvenance(sourceID, sourceID, "", "")
	ev.Geometry = &gridv1.Geometry{Centroid: &gridv1.LatLng{Lat: lat, Lng: lng}}
	ev.Detail = &gridv1.Event_Wildfire{Wildfire: &gridv1.WildfireDetail{HasPerimeter: hasPerimeter}}
	return ev
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

	res, err := n.Poll(testCtx(), nil)
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
	// CAL FIRE down: adoption is uncomputable, so with an empty prior NO
	// standalone events are minted (a perimeter normally adopted by a calfire
	// incident must not surface as a duplicate wfigs:* event); calfire is
	// flagged in PerSource.
	n := newWildfireNormalizer(&fakeDoer{err: assert.AnError}, &fakeDoer{resp: wfigsFixture})
	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.NotNil(t, res.PerSource)
	assert.Error(t, res.PerSource["calfire"])
	assert.NotContains(t, res.PerSource, "wfigs")
	assert.Empty(t, res.Events, "no prior standalone ids => nothing to safely emit while CAL FIRE is down")

	// WFIGS down: incident points still flow, wfigs flagged.
	n = newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{err: assert.AnError})
	res, err = n.Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Error(t, res.PerSource["wfigs"])
	assert.ElementsMatch(t, []string{"calfire:abc-123", "calfire:ambiguous"}, eventIDs(res.Events))
	assert.False(t, eventByID(t, res.Events, "calfire:abc-123").GetWildfire().HasPerimeter)
}

// CAL FIRE down + WFIGS up: only standalone ids the store already tracks are
// re-emitted (fresh perimeter data); perimeters that are normally adopted by
// calfire:* incidents must NOT be minted as new wfigs:* duplicates.
func TestWildfirePoll_CalfireDown_EmitsOnlyPriorStandaloneIDs(t *testing.T) {
	prior := &scriptedPrior{events: []*gridv1.Event{
		priorWildfireEvent("wfigs:lonely", "wfigs", 38.05, -119.85, true),
		// The adopted fire's event lives under the calfire namespace; its
		// perimeter ("Salt Springs") must not re-surface as wfigs:saltsprings.
		priorWildfireEvent("calfire:abc-123", "calfire", 38.2, -120.4, true),
	}}
	n := newWildfireNormalizer(&fakeDoer{err: assert.AnError}, &fakeDoer{resp: wfigsFixture})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Error(t, res.PerSource["calfire"])
	assert.NotContains(t, res.PerSource, "wfigs")

	assert.ElementsMatch(t, []string{"wfigs:lonely"}, eventIDs(res.Events),
		"only prior wfigs standalones survive; saltsprings/ambiguous must not be minted")

	// The surviving standalone carries CURRENT perimeter data, not the prior's.
	lonely := eventByID(t, res.Events, "wfigs:lonely")
	assert.Equal(t, "Lonely — 250 ac, 10% contained", lonely.Headline)
	assert.True(t, lonely.GetWildfire().HasPerimeter)
}

// WFIGS down + CAL FIRE up: an incident whose stored version holds a perimeter
// keeps that geometry + has_perimeter (no false "perimeter gone" revision);
// scalar fields still update from CAL FIRE.
func TestWildfirePoll_WfigsDown_CarriesPriorPerimeterForward(t *testing.T) {
	priorGeom, err := geometryFromTyped("Polygon",
		[]byte(`[[[-120.45,38.15],[-120.35,38.15],[-120.35,38.25],[-120.45,38.25],[-120.45,38.15]]]`))
	require.NoError(t, err)
	withPerim := priorWildfireEvent("calfire:abc-123", "calfire", 38.2, -120.4, true)
	withPerim.Geometry = priorGeom
	// Prior for the second incident exists but never had a perimeter: no
	// carry-forward, it stays a point.
	noPerim := priorWildfireEvent("calfire:ambiguous", "calfire", 38.0, -120.2, false)
	prior := &scriptedPrior{events: []*gridv1.Event{withPerim, noPerim}}

	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{err: assert.AnError})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Error(t, res.PerSource["wfigs"])

	salt := eventByID(t, res.Events, "calfire:abc-123")
	assert.True(t, salt.GetWildfire().HasPerimeter, "prior perimeter must be carried forward while WFIGS is down")
	require.NotNil(t, salt.Geometry)
	assert.Equal(t, priorGeom.Geojson, salt.Geometry.Geojson, "prior polygon geometry carried forward verbatim")
	// Scalar fields still come from the current CAL FIRE record.
	assert.Equal(t, "Salt Springs Fire — 1200 ac, 35% contained", salt.Headline)
	assert.Equal(t, 1200.0, salt.GetWildfire().Acres)

	amb := eventByID(t, res.Events, "calfire:ambiguous")
	assert.False(t, amb.GetWildfire().HasPerimeter)
	var geom struct {
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(amb.Geometry.Geojson, &geom))
	assert.Equal(t, "Point", geom.Type)
}

// The carry-forward's whole point is revision hygiene, so prove it in the
// store's own terms: with CAL FIRE content unchanged, a WFIGS outage tick
// must produce an event that is CONTENT-HASH-EQUAL to the healthy tick's —
// UpsertEvent would write no revision. Without the carry-forward, geometry
// downgrades to a point and has_perimeter flips false: a false "perimeter
// gone" revision (and a second false revision when WFIGS recovers).
func TestWildfirePoll_WfigsDown_HashEqualNoFalseRevision(t *testing.T) {
	// Tick 1: both feeds healthy; the incident adopts its perimeter.
	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: wfigsFixture})
	res1, err := n.Poll(testCtx(), &scriptedPrior{})
	require.NoError(t, err)
	healthy := eventByID(t, res1.Events, "calfire:abc-123")
	require.True(t, healthy.GetWildfire().HasPerimeter, "sanity: healthy tick adopts the perimeter")

	// Tick 2: identical CAL FIRE data, WFIGS down; prior is tick 1's set (what
	// the store would hold).
	n = newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{err: assert.AnError})
	res2, err := n.Poll(testCtx(), &scriptedPrior{events: res1.Events})
	require.NoError(t, err)
	carried := eventByID(t, res2.Events, "calfire:abc-123")

	assert.Equal(t, store.ContentHash(healthy), store.ContentHash(carried),
		"unchanged CAL FIRE content across a WFIGS outage must be hash-equal: no false revision")
}

// Control for the carry-forward: when WFIGS is HEALTHY and the perimeter is
// genuinely gone from the feed, the downgrade to point + has_perimeter=false
// is a real revision and must still happen.
func TestWildfirePoll_WfigsHealthy_PerimeterGoneIsRealDowngrade(t *testing.T) {
	withPerim := priorWildfireEvent("calfire:abc-123", "calfire", 38.2, -120.4, true)
	prior := &scriptedPrior{events: []*gridv1.Event{withPerim}}

	// WFIGS responds cleanly but no longer includes the Salt Springs perimeter.
	const wfigsNoSalt = `{"features": []}`
	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: wfigsNoSalt})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Nil(t, res.PerSource)

	salt := eventByID(t, res.Events, "calfire:abc-123")
	assert.False(t, salt.GetWildfire().HasPerimeter, "healthy WFIGS with no perimeter is a genuine downgrade")
	var geom struct {
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(salt.Geometry.Geojson, &geom))
	assert.Equal(t, "Point", geom.Type)
}

// wfigsPerimeter renders one WFIGS feature; box is [minLng, minLat, maxLng,
// maxLat].
func wfigsPerimeter(name string, box [4]float64) string {
	return fmt.Sprintf(`{
	  "properties": {"poly_IncidentName": %q, "attr_IncidentSize": 40.0, "attr_PercentContained": 20, "attr_FireCause": "Unknown"},
	  "geometry": {"type": "Polygon", "coordinates": [[[%[2]f,%[3]f],[%[4]f,%[3]f],[%[4]f,%[5]f],[%[2]f,%[5]f],[%[2]f,%[3]f]]]}
	}`, name, box[0], box[1], box[2], box[3])
}

// Standalone id continuity: when a name is down to exactly ONE candidate, the
// survivor keeps the id it already holds in the store instead of always being
// reassigned the bare id (which would splice two fires' histories together).
func TestWildfireStandaloneIDContinuity(t *testing.T) {
	// One "Ambiguous" perimeter remains: the NORTH one (centroid lat 38.4).
	northOnly := `{"features": [` + wfigsPerimeter("Ambiguous", [4]float64{-120.3, 38.35, -120.2, 38.45}) + `]}`
	const noIncidents = `[]`

	t.Run("survivor keeps its suffixed id", func(t *testing.T) {
		// Last tick there were two: bare (south) disappeared, -2 (north)
		// survived. Prior now only tracks the suffixed id.
		prior := &scriptedPrior{events: []*gridv1.Event{
			priorWildfireEvent("wfigs:ambiguous-2", "wfigs", 38.4, -120.25, true),
		}}
		n := newWildfireNormalizer(&fakeDoer{resp: noIncidents}, &fakeDoer{resp: northOnly})
		res, err := n.Poll(testCtx(), prior)
		require.NoError(t, err)
		assert.Equal(t, []string{"wfigs:ambiguous-2"}, eventIDs(res.Events),
			"single candidate must reuse the one suffixed prior id, not be renamed to the bare id")
	})

	t.Run("prior bare id is kept", func(t *testing.T) {
		prior := &scriptedPrior{events: []*gridv1.Event{
			priorWildfireEvent("wfigs:ambiguous", "wfigs", 38.4, -120.25, true),
		}}
		n := newWildfireNormalizer(&fakeDoer{resp: noIncidents}, &fakeDoer{resp: northOnly})
		res, err := n.Poll(testCtx(), prior)
		require.NoError(t, err)
		assert.Equal(t, []string{"wfigs:ambiguous"}, eventIDs(res.Events))
	})

	t.Run("no prior ids mints the bare id", func(t *testing.T) {
		n := newWildfireNormalizer(&fakeDoer{resp: noIncidents}, &fakeDoer{resp: northOnly})
		res, err := n.Poll(testCtx(), &scriptedPrior{})
		require.NoError(t, err)
		assert.Equal(t, []string{"wfigs:ambiguous"}, eventIDs(res.Events))
	})

	t.Run("residual edge picks nearest-centroid prior id", func(t *testing.T) {
		// Both prior ids still active, one candidate left: the survivor is
		// whichever prior fire is spatially nearest — here the north one, which
		// held the suffixed id.
		prior := &scriptedPrior{events: []*gridv1.Event{
			priorWildfireEvent("wfigs:ambiguous", "wfigs", 38.0, -120.25, true),   // south
			priorWildfireEvent("wfigs:ambiguous-2", "wfigs", 38.4, -120.25, true), // north
		}}
		n := newWildfireNormalizer(&fakeDoer{resp: noIncidents}, &fakeDoer{resp: northOnly})
		res, err := n.Poll(testCtx(), prior)
		require.NoError(t, err)
		assert.Equal(t, []string{"wfigs:ambiguous-2"}, eventIDs(res.Events),
			"north candidate must keep the north fire's id, not adopt the south fire's bare id")
	})

	t.Run("unrelated prior names do not affect the id", func(t *testing.T) {
		prior := &scriptedPrior{events: []*gridv1.Event{
			priorWildfireEvent("wfigs:ambiguous2", "wfigs", 38.4, -120.25, true), // different norm name
		}}
		n := newWildfireNormalizer(&fakeDoer{resp: noIncidents}, &fakeDoer{resp: northOnly})
		res, err := n.Poll(testCtx(), prior)
		require.NoError(t, err)
		assert.Equal(t, []string{"wfigs:ambiguous"}, eventIDs(res.Events))
	})
}

func TestWildfirePoll_BothFail(t *testing.T) {
	n := newWildfireNormalizer(&fakeDoer{err: assert.AnError}, &fakeDoer{err: assert.AnError})
	_, err := n.Poll(testCtx(), nil)
	assert.Error(t, err)
}
