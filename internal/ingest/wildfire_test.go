package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/calfire"
	"github.com/dpup/sierra-data/internal/clients/firis"
	"github.com/dpup/sierra-data/internal/config"
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

// The FIRIS combo feed (f=geojson): properties are incident_name / area_acres /
// mission / poly_DateCurrent / source / displayStatus (no containment or cause).
// The northern "Ambiguous" perimeter is deliberately listed FIRST so the test
// proves collision suffixes follow centroid (lat, lng) order, not slice order.
// Salt Springs carries two successive CAL FIRE Intel flights (same fire, same
// place, different poly_DateCurrent) to exercise the dedup: only the latest
// survives to be adopted.
const firisFixture = `{
  "features": [
    {
      "properties": {"incident_name": "Ambiguous Fire", "area_acres": 40.0, "poly_DateCurrent": 1000, "source": "FIRIS", "displayStatus": "Active"},
      "geometry": {"type": "Polygon", "coordinates": [[[-120.3,38.35],[-120.2,38.35],[-120.2,38.45],[-120.3,38.45],[-120.3,38.35]]]}
    },
    {
      "properties": {"incident_name": "Ambiguous", "area_acres": 30.0, "poly_DateCurrent": 1000, "source": "FIRIS", "displayStatus": "Active"},
      "geometry": {"type": "Polygon", "coordinates": [[[-120.3,37.95],[-120.2,37.95],[-120.2,38.05],[-120.3,38.05],[-120.3,37.95]]]}
    },
    {
      "properties": {"incident_name": "Salt Springs", "incident_number": "ss-uuid", "area_acres": 1150.0, "poly_DateCurrent": 1000, "source": "CAL FIRE INTEL FLIGHT DATA", "displayStatus": "Active"},
      "geometry": {"type": "Polygon", "coordinates": [[[-120.35,38.05],[-120.25,38.05],[-120.25,38.15],[-120.35,38.15],[-120.35,38.05]]]}
    },
    {
      "properties": {"incident_name": "Salt Springs", "incident_number": "ss-uuid", "area_acres": 1180.0, "poly_DateCurrent": 5000, "source": "CAL FIRE INTEL FLIGHT DATA", "displayStatus": "Active"},
      "geometry": {"type": "Polygon", "coordinates": [[[-120.45,38.15],[-120.35,38.15],[-120.35,38.25],[-120.45,38.25],[-120.45,38.15]]]}
    },
    {
      "properties": {"incident_name": "Lonely", "area_acres": 250.4, "poly_DateCurrent": 1000, "source": "FIRIS", "displayStatus": "Active"},
      "geometry": {"type": "Polygon", "coordinates": [[[-119.9,38.0],[-119.8,38.0],[-119.8,38.1],[-119.9,38.1],[-119.9,38.0]]]}
    }
  ]
}`

func newWildfireNormalizer(cfDoer, fcDoer *fakeDoer) *WildfireNormalizer {
	return NewWildfireNormalizer(
		testConfig(),
		calfire.NewClientWithHTTPDoer("https://calfire.test", cfDoer),
		firis.NewClientWithHTTPDoer("https://firis.test", fcDoer),
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
	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: firisFixture})
	assert.Equal(t, []string{"calfire", "firis"}, n.SourceIDs())

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Nil(t, res.PerSource)

	// Salt Springs joins; Far Away (out of bounds) + No Coords are dropped;
	// the two ambiguous perimeters and Lonely are standalone.
	assert.ElementsMatch(t, []string{
		"calfire:abc-123",
		"calfire:ambiguous",
		"firis:ambiguous",
		"firis:ambiguous-2",
		"firis:lonely",
	}, eventIDs(res.Events))

	// Adopted perimeter: polygon geometry + has_perimeter. The two Salt Springs
	// flights collapse to the latest (poly_DateCurrent 5000), whose polygon the
	// incident adopts.
	salt := eventByID(t, res.Events, "calfire:abc-123")
	assert.Equal(t, "Salt Springs Fire — 1200 ac, 35% contained", salt.Headline) // shipped format, exact
	assert.Equal(t, gridv1.Severity_EXTREME, salt.Severity)                      // 1200 ac (NWCG class F) & <50% contained
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
	// The two Salt Springs flights share an incident_number (one fire) but sit at
	// distinct centroids (~38.10 vs ~38.20); the adopted polygon must be the NEWER
	// flight's (poly_DateCurrent 5000, centroid ~38.20), not the older ~38.10 — a
	// real check that "latest wins", not a tautology.
	assert.InDelta(t, 38.20, salt.Geometry.Centroid.Lat, 0.02)
	d := salt.GetWildfire()
	require.NotNil(t, d)
	assert.True(t, d.HasPerimeter)
	assert.Equal(t, 1200.0, d.Acres) // scalar from CAL FIRE, not the perimeter's area
	assert.Equal(t, int32(35), d.Containment)
	assert.Equal(t, "Calaveras", d.County)
	require.NotNil(t, salt.Provenance)
	assert.Equal(t, "calfire", salt.Provenance.SourceId)
	assert.Equal(t, "CAL FIRE", salt.Provenance.SourceName)
	assert.Equal(t, "CAL FIRE / FIRIS", salt.Provenance.Attribution)

	// Ambiguous name: two distinct fires far apart survive dedup as separate
	// clusters, so the incident must NOT adopt either perimeter.
	amb := eventByID(t, res.Events, "calfire:ambiguous")
	assert.Equal(t, "Ambiguous Fire — 10 ac, 90% contained", amb.Headline)
	assert.Equal(t, gridv1.Severity_MODERATE, amb.Severity)
	assert.False(t, amb.GetWildfire().HasPerimeter)
	require.NoError(t, json.Unmarshal(amb.Geometry.Geojson, &geom))
	assert.Equal(t, "Point", geom.Type)

	// Collision suffixes ordered by centroid latitude: the southern perimeter
	// (fixture-second) takes the bare id, the northern one gets -2.
	south := eventByID(t, res.Events, "firis:ambiguous")
	assert.InDelta(t, 38.0, south.Geometry.Centroid.Lat, 1e-9)
	north := eventByID(t, res.Events, "firis:ambiguous-2")
	assert.InDelta(t, 38.4, north.Geometry.Centroid.Lat, 1e-9)

	lonely := eventByID(t, res.Events, "firis:lonely")
	assert.Equal(t, "Lonely — 250 ac", lonely.Headline) // no containment clause (feed has none)
	assert.Equal(t, gridv1.Severity_SEVERE, lonely.Severity)
	ld := lonely.GetWildfire()
	require.NotNil(t, ld)
	assert.True(t, ld.HasPerimeter)
	assert.Equal(t, 250.4, ld.Acres)
	assert.Zero(t, ld.Containment) // combo feed carries no containment for standalones
	require.NotNil(t, lonely.Provenance)
	assert.Equal(t, "firis", lonely.Provenance.SourceId)
	assert.Equal(t, "CAL FIRE / FIRIS", lonely.Provenance.SourceName)
	assert.Equal(t, "CAL FIRE / FIRIS / NIFC", lonely.Provenance.Attribution)
}

// dedupePerimeters is the risk of the FIRIS swap: the combo feed has many rows
// per fire. These exercise the grouping/clustering/pick directly.
func TestDedupePerimeters(t *testing.T) {
	ctx := testCtx()

	t.Run("collapses co-located same-name to the latest poly_DateCurrent", func(t *testing.T) {
		// Two CAL FIRE Intel flights of one fire (same place) + an older FIRIS
		// mission row (name null, mission-derived). All → one candidate, newest wins.
		perims := []firis.Perimeter{
			firisPerim("DOVE", "", "CAL FIRE INTEL FLIGHT DATA", "Active", 5000, 225, 37.96, -120.40),
			firisPerim("DOVE", "", "CAL FIRE INTEL FLIGHT DATA", "Active", 1000, 223, 37.96, -120.40),
			firisPerim("", "CA-TCU-DOVE-N57B", "FIRIS", "Active", 500, 166, 37.97, -120.41),
		}
		out := dedupePerimeters(ctx, perims)
		require.Len(t, out, 1)
		assert.Equal(t, "dove", out[0].norm)
		assert.Equal(t, 225.0, out[0].perim.Acres, "latest poly_DateCurrent wins")
	})

	t.Run("distinct same-name fires far apart stay separate with distinct geometry", func(t *testing.T) {
		perims := []firis.Perimeter{
			firisPerim("OAK", "", "FIRIS", "Active", 1000, 10, 38.0, -120.0),
			firisPerim("OAK", "", "FIRIS", "Active", 1000, 20, 40.0, -122.0),
		}
		out := dedupePerimeters(ctx, perims)
		require.Len(t, out, 2, "two spatial clusters => two candidates (must not merge distinct fires)")
		// The two survivors must keep DISTINCT geometry (no cross-contamination).
		assert.NotEqual(t, out[0].geom.Centroid.Lat, out[1].geom.Centroid.Lat)
	})

	t.Run("different incident_numbers never merge even when co-located", func(t *testing.T) {
		// Two genuinely-distinct same-named fires ~10 km apart (within the centroid
		// threshold) but with different incident_numbers must NOT collapse — else one
		// real perimeter is dropped and both incidents adopt the survivor's polygon.
		perims := []firis.Perimeter{
			firisPerim("MILL", "", "CAL FIRE INTEL FLIGHT DATA", "Active", 1000, 10, 38.00, -120.0),
			firisPerim("MILL", "", "CAL FIRE INTEL FLIGHT DATA", "Active", 1000, 20, 38.09, -120.0),
		}
		perims[0].IncidentNumber = "uuid-A"
		perims[1].IncidentNumber = "uuid-B"
		out := dedupePerimeters(ctx, perims)
		require.Len(t, out, 2, "distinct incident_numbers are authoritatively distinct fires")
	})

	t.Run("same incident_number merges even when centroids drift apart", func(t *testing.T) {
		// One fire's successive flights drift >15 km apart; the shared incident_number
		// keeps them one fire (freshest wins).
		perims := []firis.Perimeter{
			firisPerim("BIG", "", "CAL FIRE INTEL FLIGHT DATA", "Active", 1000, 100, 38.0, -120.0),
			firisPerim("BIG", "", "CAL FIRE INTEL FLIGHT DATA", "Active", 5000, 300, 38.3, -120.0),
		}
		perims[0].IncidentNumber = "uuid-X"
		perims[1].IncidentNumber = "uuid-X"
		out := dedupePerimeters(ctx, perims)
		require.Len(t, out, 1, "a shared incident_number is one fire regardless of centroid drift")
		assert.Equal(t, 300.0, out[0].perim.Acres, "freshest flight wins")
	})

	t.Run("cluster assignment is order-independent (deterministic)", func(t *testing.T) {
		// A 3-point chain on a line (0, ~12 km, ~24 km) whose middle point is within
		// threshold of both ends. Feeding the rows in different orders must yield the
		// SAME number of clusters — an order-dependent count would flap ids across
		// polls (mint/expire phantom -2 events).
		a := firisPerim("CHAIN", "", "FIRIS", "Active", 1000, 10, 38.00, -120.0)
		b := firisPerim("CHAIN", "", "FIRIS", "Active", 1000, 20, 38.11, -120.0)
		c := firisPerim("CHAIN", "", "FIRIS", "Active", 1000, 30, 38.22, -120.0)
		n1 := len(dedupePerimeters(ctx, []firis.Perimeter{a, b, c}))
		n2 := len(dedupePerimeters(ctx, []firis.Perimeter{b, a, c}))
		n3 := len(dedupePerimeters(ctx, []firis.Perimeter{c, b, a}))
		assert.Equal(t, n1, n2, "cluster count must not depend on feed order")
		assert.Equal(t, n1, n3, "cluster count must not depend on feed order")
	})

	t.Run("drops an explicitly Inactive perimeter", func(t *testing.T) {
		perims := []firis.Perimeter{firisPerim("TWIST", "", "FIRIS", "Inactive", 1000, 5, 38.0, -120.0)}
		assert.Empty(t, dedupePerimeters(ctx, perims))
	})

	t.Run("keeps a blank-status perimeter (lenient)", func(t *testing.T) {
		perims := []firis.Perimeter{firisPerim("BLANK", "", "FIRIS", "", 1000, 5, 38.0, -120.0)}
		assert.Len(t, dedupePerimeters(ctx, perims), 1)
	})

	t.Run("drops a nameless + missionless perimeter", func(t *testing.T) {
		perims := []firis.Perimeter{firisPerim("", "", "FIRIS", "Active", 1000, 5, 38.0, -120.0)}
		assert.Empty(t, dedupePerimeters(ctx, perims))
	})

	t.Run("mission-derived name groups with the CAL FIRE Intel row", func(t *testing.T) {
		// A FIRIS mission row (name null) co-located with a NEWER named CAL FIRE
		// Intel row must collapse into one, the Intel row winning on the stamp.
		perims := []firis.Perimeter{
			firisPerim("", "CA-TCU-DOVE-N57B", "FIRIS", "Active", 500, 166, 37.96, -120.40),
			firisPerim("DOVE", "", "CAL FIRE INTEL FLIGHT DATA", "Active", 5000, 225, 37.96, -120.40),
		}
		out := dedupePerimeters(ctx, perims)
		require.Len(t, out, 1)
		assert.Equal(t, 225.0, out[0].perim.Acres)
	})

	t.Run("equal timestamps tiebreak to source priority", func(t *testing.T) {
		perims := []firis.Perimeter{
			firisPerim("PINE", "", "WFIGS", "Active", 1000, 50, 39.0, -121.0),
			firisPerim("PINE", "", "CAL FIRE INTEL FLIGHT DATA", "Active", 1000, 40, 39.0, -121.0),
		}
		out := dedupePerimeters(ctx, perims)
		require.Len(t, out, 1)
		assert.Equal(t, 40.0, out[0].perim.Acres, "CAL FIRE Intel outranks WFIGS on an equal stamp")
	})

	t.Run("full tie falls through to larger acreage", func(t *testing.T) {
		// Same time, both Active, same source → the acres tiebreak (the last
		// determinism guard) decides.
		perims := []firis.Perimeter{
			firisPerim("ELK", "", "FIRIS", "Active", 1000, 30, 39.0, -121.0),
			firisPerim("ELK", "", "FIRIS", "Active", 1000, 80, 39.0, -121.0),
		}
		out := dedupePerimeters(ctx, perims)
		require.Len(t, out, 1)
		assert.Equal(t, 80.0, out[0].perim.Acres, "larger acreage wins a full tie")
	})

	t.Run("Active outranks blank status on an equal stamp", func(t *testing.T) {
		// Blank status is kept (lenient) but loses the Active-status tiebreak.
		perims := []firis.Perimeter{
			firisPerim("ASH", "", "FIRIS", "", 1000, 99, 39.0, -121.0),
			firisPerim("ASH", "", "FIRIS", "Active", 1000, 10, 39.0, -121.0),
		}
		out := dedupePerimeters(ctx, perims)
		require.Len(t, out, 1)
		assert.Equal(t, 10.0, out[0].perim.Acres, "the Active row wins over a blank-status row at equal time")
	})
}

func TestNameFromMission(t *testing.T) {
	cases := map[string]string{
		"CA-TCU-DOVE-N57B":        "DOVE",
		"CA-FKU-PARAMOUNT":        "PARAMOUNT",
		"CA-RRU-SPRINGS-N50X":     "SPRINGS",
		"CA-RRU-SAN-ANDREAS-N50X": "SAN ANDREAS", // hyphenated name, flight id stripped
		"CA-TCU-DOVE":             "DOVE",        // no flight id
		"CA-TCU-N57B":             "",            // unit + flight only, no fire name → not the plane's tail
		"":                        "",
		"garbage":                 "",
		"CA-ONLY":                 "", // too few tokens
		"XX-TCU-DOVE-N57B":        "", // not a CA mission
	}
	for mission, want := range cases {
		if got := nameFromMission(mission); got != want {
			t.Errorf("nameFromMission(%q) = %q, want %q", mission, got, want)
		}
	}
}

func TestWildfirePoll_PartialFailure(t *testing.T) {
	// CAL FIRE down: adoption is uncomputable, so with an empty prior NO
	// standalone events are minted (a perimeter normally adopted by a calfire
	// incident must not surface as a duplicate firis:* event); calfire is
	// flagged in PerSource.
	n := newWildfireNormalizer(&fakeDoer{err: assert.AnError}, &fakeDoer{resp: firisFixture})
	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.NotNil(t, res.PerSource)
	assert.Error(t, res.PerSource["calfire"])
	assert.NotContains(t, res.PerSource, "firis")
	assert.Empty(t, res.Events, "no prior standalone ids => nothing to safely emit while CAL FIRE is down")

	// FIRIS down: incident points still flow, firis flagged.
	n = newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{err: assert.AnError})
	res, err = n.Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Error(t, res.PerSource["firis"])
	assert.ElementsMatch(t, []string{"calfire:abc-123", "calfire:ambiguous"}, eventIDs(res.Events))
	assert.False(t, eventByID(t, res.Events, "calfire:abc-123").GetWildfire().HasPerimeter)
}

// CAL FIRE down + FIRIS up: only standalone ids the store already tracks are
// re-emitted (fresh perimeter data); perimeters that are normally adopted by
// calfire:* incidents must NOT be minted as new firis:* duplicates.
func TestWildfirePoll_CalfireDown_EmitsOnlyPriorStandaloneIDs(t *testing.T) {
	prior := &scriptedPrior{events: []*gridv1.Event{
		priorWildfireEvent("firis:lonely", "firis", 38.05, -119.85, true),
		// The adopted fire's event lives under the calfire namespace; its
		// perimeter ("Salt Springs") must not re-surface as firis:saltsprings.
		priorWildfireEvent("calfire:abc-123", "calfire", 38.2, -120.4, true),
	}}
	n := newWildfireNormalizer(&fakeDoer{err: assert.AnError}, &fakeDoer{resp: firisFixture})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Error(t, res.PerSource["calfire"])
	assert.NotContains(t, res.PerSource, "firis")

	assert.ElementsMatch(t, []string{"firis:lonely"}, eventIDs(res.Events),
		"only prior firis standalones survive; saltsprings/ambiguous must not be minted")

	// The surviving standalone carries CURRENT perimeter data, not the prior's.
	lonely := eventByID(t, res.Events, "firis:lonely")
	assert.Equal(t, "Lonely — 250 ac", lonely.Headline)
	assert.True(t, lonely.GetWildfire().HasPerimeter)
}

// FIRIS down + CAL FIRE up: an incident whose stored version holds a perimeter
// keeps that geometry + has_perimeter (no false "perimeter gone" revision);
// scalar fields still update from CAL FIRE.
func TestWildfirePoll_FirisDown_CarriesPriorPerimeterForward(t *testing.T) {
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
	assert.Error(t, res.PerSource["firis"])

	salt := eventByID(t, res.Events, "calfire:abc-123")
	assert.True(t, salt.GetWildfire().HasPerimeter, "prior perimeter must be carried forward while FIRIS is down")
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
// store's own terms: with CAL FIRE content unchanged, a FIRIS outage tick
// must produce an event that is CONTENT-HASH-EQUAL to the healthy tick's —
// UpsertEvent would write no revision. Without the carry-forward, geometry
// downgrades to a point and has_perimeter flips false: a false "perimeter
// gone" revision (and a second false revision when FIRIS recovers).
func TestWildfirePoll_FirisDown_HashEqualNoFalseRevision(t *testing.T) {
	// Tick 1: both feeds healthy; the incident adopts its perimeter.
	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: firisFixture})
	res1, err := n.Poll(testCtx(), &scriptedPrior{})
	require.NoError(t, err)
	healthy := eventByID(t, res1.Events, "calfire:abc-123")
	require.True(t, healthy.GetWildfire().HasPerimeter, "sanity: healthy tick adopts the perimeter")

	// Tick 2: identical CAL FIRE data, FIRIS down; prior is tick 1's set (what
	// the store would hold).
	n = newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{err: assert.AnError})
	res2, err := n.Poll(testCtx(), &scriptedPrior{events: res1.Events})
	require.NoError(t, err)
	carried := eventByID(t, res2.Events, "calfire:abc-123")

	assert.Equal(t, store.ContentHash(healthy), store.ContentHash(carried),
		"unchanged CAL FIRE content across a FIRIS outage must be hash-equal: no false revision")
}

// Control for the carry-forward: when FIRIS is HEALTHY and returns OTHER
// perimeters but genuinely no longer includes this fire's, the downgrade to
// point + has_perimeter=false is a real revision and must still happen. The feed
// must be NON-empty — a wholesale-empty response is treated as non-authoritative
// (see the carry-forward test below).
func TestWildfirePoll_FirisHealthy_PerimeterGoneIsRealDowngrade(t *testing.T) {
	withPerim := priorWildfireEvent("calfire:abc-123", "calfire", 38.2, -120.4, true)
	prior := &scriptedPrior{events: []*gridv1.Event{withPerim}}

	// FIRIS responds cleanly WITH some other perimeter but no longer includes
	// Salt Springs → the feed is authoritative and Salt Springs downgrades.
	firisNoSalt := `{"features": [` + firisFeature("Lonely", [4]float64{-119.9, 38.0, -119.8, 38.1}) + `]}`
	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: firisNoSalt})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Nil(t, res.PerSource)

	salt := eventByID(t, res.Events, "calfire:abc-123")
	assert.False(t, salt.GetWildfire().HasPerimeter, "a non-empty feed that omits this fire is a genuine downgrade")
	var geom struct {
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(salt.Geometry.Geojson, &geom))
	assert.Equal(t, "Point", geom.Type)
}

// A clean-but-wholesale-EMPTY FIRIS response (HTTP 200, zero features — a common
// transient ArcGIS glitch) must NOT downgrade every adopted fire to a point +
// write a false "perimeter gone" revision across the whole map. It is treated as
// non-authoritative: the prior perimeter is carried forward, exactly like a hard
// outage. Contrast with the control above (a non-empty feed that omits one fire IS
// authoritative).
func TestWildfirePoll_FirisWholesaleEmpty_CarriesPriorPerimeterForward(t *testing.T) {
	priorGeom, err := geometryFromTyped("Polygon",
		[]byte(`[[[-120.45,38.15],[-120.35,38.15],[-120.35,38.25],[-120.45,38.25],[-120.45,38.15]]]`))
	require.NoError(t, err)
	withPerim := priorWildfireEvent("calfire:abc-123", "calfire", 38.2, -120.4, true)
	withPerim.Geometry = priorGeom
	prior := &scriptedPrior{events: []*gridv1.Event{withPerim}}

	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: `{"features": []}`})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Nil(t, res.PerSource, "an empty-but-successful fetch is not a source failure")

	salt := eventByID(t, res.Events, "calfire:abc-123")
	assert.True(t, salt.GetWildfire().HasPerimeter, "wholesale-empty must carry the prior perimeter forward, not downgrade")
	assert.Equal(t, priorGeom.Geojson, salt.Geometry.Geojson, "prior polygon carried forward verbatim")
}

// firisPerim builds one combo-feed perimeter as a small square around (lat, lng).
func firisPerim(name, mission, source, status string, dateMs int64, acres, lat, lng float64) firis.Perimeter {
	coords := fmt.Sprintf(`[[[%f,%f],[%f,%f],[%f,%f],[%f,%f],[%f,%f]]]`,
		lng-0.01, lat-0.01, lng+0.01, lat-0.01, lng+0.01, lat+0.01, lng-0.01, lat+0.01, lng-0.01, lat-0.01)
	var dc time.Time
	if dateMs != 0 {
		dc = time.UnixMilli(dateMs).UTC()
	}
	return firis.Perimeter{
		IncidentName: name, Mission: mission, Source: source, Status: status,
		Acres: acres, DateCurrent: dc,
		GeometryType: "Polygon", GeometryCoords: json.RawMessage(coords),
	}
}

// firisFeature renders one FIRIS feature; box is [minLng, minLat, maxLng, maxLat].
func firisFeature(name string, box [4]float64) string {
	return fmt.Sprintf(`{
	  "properties": {"incident_name": %q, "area_acres": 40.0, "poly_DateCurrent": 1000, "source": "FIRIS", "displayStatus": "Active"},
	  "geometry": {"type": "Polygon", "coordinates": [[[%[2]f,%[3]f],[%[4]f,%[3]f],[%[4]f,%[5]f],[%[2]f,%[5]f],[%[2]f,%[3]f]]]}
	}`, name, box[0], box[1], box[2], box[3])
}

// Standalone id continuity: when a name is down to exactly ONE candidate, the
// survivor keeps the id it already holds in the store instead of always being
// reassigned the bare id (which would splice two fires' histories together).
func TestWildfireStandaloneIDContinuity(t *testing.T) {
	// One "Ambiguous" perimeter remains: the NORTH one (centroid lat 38.4).
	northOnly := `{"features": [` + firisFeature("Ambiguous", [4]float64{-120.3, 38.35, -120.2, 38.45}) + `]}`
	const noIncidents = `[]`

	t.Run("survivor keeps its suffixed id", func(t *testing.T) {
		// Last tick there were two: bare (south) disappeared, -2 (north)
		// survived. Prior now only tracks the suffixed id.
		prior := &scriptedPrior{events: []*gridv1.Event{
			priorWildfireEvent("firis:ambiguous-2", "firis", 38.4, -120.25, true),
		}}
		n := newWildfireNormalizer(&fakeDoer{resp: noIncidents}, &fakeDoer{resp: northOnly})
		res, err := n.Poll(testCtx(), prior)
		require.NoError(t, err)
		assert.Equal(t, []string{"firis:ambiguous-2"}, eventIDs(res.Events),
			"single candidate must reuse the one suffixed prior id, not be renamed to the bare id")
	})

	t.Run("prior bare id is kept", func(t *testing.T) {
		prior := &scriptedPrior{events: []*gridv1.Event{
			priorWildfireEvent("firis:ambiguous", "firis", 38.4, -120.25, true),
		}}
		n := newWildfireNormalizer(&fakeDoer{resp: noIncidents}, &fakeDoer{resp: northOnly})
		res, err := n.Poll(testCtx(), prior)
		require.NoError(t, err)
		assert.Equal(t, []string{"firis:ambiguous"}, eventIDs(res.Events))
	})

	t.Run("no prior ids mints the bare id", func(t *testing.T) {
		n := newWildfireNormalizer(&fakeDoer{resp: noIncidents}, &fakeDoer{resp: northOnly})
		res, err := n.Poll(testCtx(), &scriptedPrior{})
		require.NoError(t, err)
		assert.Equal(t, []string{"firis:ambiguous"}, eventIDs(res.Events))
	})

	t.Run("residual edge picks nearest-centroid prior id", func(t *testing.T) {
		// Both prior ids still active, one candidate left: the survivor is
		// whichever prior fire is spatially nearest — here the north one, which
		// held the suffixed id.
		prior := &scriptedPrior{events: []*gridv1.Event{
			priorWildfireEvent("firis:ambiguous", "firis", 38.0, -120.25, true),   // south
			priorWildfireEvent("firis:ambiguous-2", "firis", 38.4, -120.25, true), // north
		}}
		n := newWildfireNormalizer(&fakeDoer{resp: noIncidents}, &fakeDoer{resp: northOnly})
		res, err := n.Poll(testCtx(), prior)
		require.NoError(t, err)
		assert.Equal(t, []string{"firis:ambiguous-2"}, eventIDs(res.Events),
			"north candidate must keep the north fire's id, not adopt the south fire's bare id")
	})

	t.Run("unrelated prior names do not affect the id", func(t *testing.T) {
		prior := &scriptedPrior{events: []*gridv1.Event{
			priorWildfireEvent("firis:ambiguous2", "firis", 38.4, -120.25, true), // different norm name
		}}
		n := newWildfireNormalizer(&fakeDoer{resp: noIncidents}, &fakeDoer{resp: northOnly})
		res, err := n.Poll(testCtx(), prior)
		require.NoError(t, err)
		assert.Equal(t, []string{"firis:ambiguous"}, eventIDs(res.Events))
	})
}

func TestWildfirePoll_BothFail(t *testing.T) {
	n := newWildfireNormalizer(&fakeDoer{err: assert.AnError}, &fakeDoer{err: assert.AnError})
	_, err := n.Poll(testCtx(), nil)
	assert.Error(t, err)
}

// firisEditDoer answers the FIRIS metadata endpoint (?f=json, no returnGeometry)
// with a fixed dataLastEditDate and the feature query with a fixture, counting
// how many times the expensive perimeter query runs.
type firisEditDoer struct {
	editMillis int64
	perimResp  string
	failQuery  bool // when true, the returnGeometry feature query errors (HTTP 500)
	queries    int
}

func (d *firisEditDoer) Do(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.RawQuery, "returnGeometry=true") {
		d.queries++
		if d.failQuery {
			return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("boom")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(d.perimResp)), Header: make(http.Header)}, nil
	}
	body := fmt.Sprintf(`{"editingInfo":{"dataLastEditDate":%d}}`, d.editMillis)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func newGatingNormalizer(fc *firisEditDoer) *WildfireNormalizer {
	return NewWildfireNormalizer(
		testConfig(),
		calfire.NewClientWithHTTPDoer("https://calfire.test", &fakeDoer{resp: calfireFixture}),
		firis.NewClientWithHTTPDoer("https://firis.test/query", fc),
	)
}

// The exact scenario the gating targets: the cheap metadata check succeeds but the
// expensive feature /query fails (429/500). It must fail loud (PerSource[firis]
// set → sweep skipped) and NOT advance the stamp, so the next tick retries.
func TestWildfirePerimeterQueryErrorFailsLoud(t *testing.T) {
	fc := &firisEditDoer{editMillis: 1000, perimResp: firisFixture}
	n := newGatingNormalizer(fc)

	// First poll establishes a cached set (stamp 1000).
	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Nil(t, res.PerSource)
	require.Equal(t, 1, fc.queries)

	// Stamp advances → gate attempts a refetch, but the feature query now fails.
	fc.editMillis = 2000
	fc.failQuery = true
	res, err = n.Poll(testCtx(), nil)
	require.NoError(t, err) // calfire is up → no hard Poll error
	require.Error(t, res.PerSource["firis"], "failed feature query must flag the source (sweep skipped)")
	assert.Equal(t, 2, fc.queries)

	// Self-heal: the failed fetch must NOT have advanced the stamp, so the next
	// tick refetches (stamp 2000 still != cached 1000) and succeeds.
	fc.failQuery = false
	res, err = n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Nil(t, res.PerSource)
	assert.Equal(t, 3, fc.queries, "unadvanced stamp → next tick refetches")
}

// The unchanged-stamp path serves the cache AS A SUCCESS the sweep acts on, so it
// must reproduce the fresh event set (a regression to nil/empty would keep the
// query count at 1 and silently blank every standalone perimeter).
func TestWildfireCachedServeMatchesFreshFetch(t *testing.T) {
	fc := &firisEditDoer{editMillis: 1000, perimResp: firisFixture}
	n := newGatingNormalizer(fc)

	res1, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, fc.queries)

	res2, err := n.Poll(testCtx(), nil) // unchanged stamp → served from cache
	require.NoError(t, err)
	require.Equal(t, 1, fc.queries, "served from cache, no new query")
	assert.ElementsMatch(t, eventIDs(res1.Events), eventIDs(res2.Events),
		"cached serve must reproduce the fresh event set, not silently blank perimeters")
	lonely := eventByID(t, res2.Events, "firis:lonely")
	assert.True(t, lonely.GetWildfire().GetHasPerimeter(), "a standalone perimeter must survive the cached serve")
}

// The safety valve: even with an unchanged stamp, a cache aged past maxPerimCacheAge
// forces a refetch, so a stalled/CDN-pinned dataLastEditDate can't freeze the map.
func TestWildfirePerimeterCacheMaxAge(t *testing.T) {
	fc := &firisEditDoer{editMillis: 1000, perimResp: firisFixture}
	n := newGatingNormalizer(fc)
	base := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	clk := base
	n.now = func() time.Time { return clk }

	_, err := n.Poll(testCtx(), nil) // fetch #1 at base
	require.NoError(t, err)
	require.Equal(t, 1, fc.queries)

	clk = base.Add(maxPerimCacheAge - time.Minute) // still fresh → cache served
	_, err = n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, fc.queries)

	clk = base.Add(maxPerimCacheAge + time.Minute) // past the valve → forced refetch
	_, err = n.Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Equal(t, 2, fc.queries, "stale cache past maxPerimCacheAge forces a refetch despite an unchanged stamp")
}

func TestWildfireGatesPerimeterFetchOnLastEdit(t *testing.T) {
	fc := &firisEditDoer{editMillis: 1000, perimResp: firisFixture}
	n := newGatingNormalizer(fc)

	// First poll fetches perimeters (no cache yet).
	_, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, fc.queries)

	// Unchanged dataLastEditDate → the expensive query is skipped; last-good served.
	_, err = n.Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, fc.queries, "unchanged lastEdit must not re-query perimeters")

	// Advancing the stamp triggers exactly one refetch.
	fc.editMillis = 2000
	_, err = n.Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Equal(t, 2, fc.queries)
}

// --- Widened fire geography (grid.wildfire.marginDegrees) -------------------
//
// Fire is the only layer with its own geography: wider than the hazards union
// every other poller uses, and in-scope by PERIMETER as well as by CAL FIRE's
// single origin point.

func TestWildfireScope_WidensHazardUnionByMargin(t *testing.T) {
	cfg := testConfig() // hazard union (37.7, -120.9)..(38.5, -119.2)

	// Unset margin falls back to the default rather than collapsing to the bare
	// union — an omitted key must never silently narrow fire coverage.
	scope, ok := wildfireScope(cfg)
	require.True(t, ok)
	assert.InDelta(t, 37.7-config.DefaultWildfireMarginDegrees, scope.MinLatitude, 1e-9)
	assert.InDelta(t, 38.5+config.DefaultWildfireMarginDegrees, scope.MaxLatitude, 1e-9)
	assert.InDelta(t, -120.9-config.DefaultWildfireMarginDegrees, scope.MinLongitude, 1e-9)
	assert.InDelta(t, -119.2+config.DefaultWildfireMarginDegrees, scope.MaxLongitude, 1e-9)

	// An explicit margin is honored.
	cfg.Grid.Wildfire.MarginDegrees = 1.25
	scope, ok = wildfireScope(cfg)
	require.True(t, ok)
	assert.InDelta(t, 36.45, scope.MinLatitude, 1e-9)
	assert.InDelta(t, 39.75, scope.MaxLatitude, 1e-9)

	// The fire box must be strictly wider than the CHP/Caltrans incident box —
	// the whole point of giving this layer its own geography.
	chp := cfg.Roads.IncidentAreas[0].Bounds
	assert.Less(t, scope.MinLatitude, chp.MinLatitude)
	assert.Greater(t, scope.MaxLatitude, chp.MaxLatitude)
	assert.Less(t, scope.MinLongitude, chp.MinLongitude)
	assert.Greater(t, scope.MaxLongitude, chp.MaxLongitude)

	// No hazard areas is not an empty scope — it's a hard error at the caller.
	_, ok = wildfireScope(&config.Config{})
	assert.False(t, ok)
}

func TestWildfirePoll_PerimeterQueryUsesWidenedScope(t *testing.T) {
	fc := &fakeDoer{resp: firisFixture}
	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, fc)
	_, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)

	// The ArcGIS envelope is minLng,minLat,maxLng,maxLat of the WIDENED box
	// (union -120.9/37.7/-119.2/38.5, default margin 0.5), URL-encoded.
	assert.Contains(t, fc.lastURL, "geometry="+url.QueryEscape("-121.4,37.2,-118.7,39"))
}

// The edge case that motivated all of this: CAL FIRE reports ONE origin point
// per incident, and it can sit outside the box while the fire's perimeter burns
// into it. Dropping the incident would leave only the bare standalone perimeter
// — no acreage, no containment, no incident URL.
func TestWildfirePoll_IncidentOutsidePointButPerimeterReachesIn(t *testing.T) {
	// Origin at 39.9N, ~0.9° north of the widened box's 39.0 ceiling.
	const calfireEdge = `[{
	  "UniqueId": "edge-1", "Name": "Ridge Fire", "County": "Amador",
	  "AcresBurned": 8000.0, "PercentContained": 5.0,
	  "Latitude": 39.9, "Longitude": -120.4, "IsActive": true
	}]`
	// Its perimeter is a long north-south polygon whose southern end (38.9)
	// reaches inside the widened box.
	const firisEdge = `{"features":[{
	  "properties": {"incident_name": "Ridge Fire", "area_acres": 7900.0, "poly_DateCurrent": 1000, "source": "FIRIS", "displayStatus": "Active"},
	  "geometry": {"type":"Polygon","coordinates":[[[-120.45,38.9],[-120.35,38.9],[-120.35,40.0],[-120.45,40.0],[-120.45,38.9]]]}
	}]}`

	n := newWildfireNormalizer(&fakeDoer{resp: calfireEdge}, &fakeDoer{resp: firisEdge})
	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)

	// The incident is kept and adopts the polygon — NOT dropped in favour of a
	// standalone firis: event.
	assert.Equal(t, []string{"calfire:edge-1"}, eventIDs(res.Events))
	ev := eventByID(t, res.Events, "calfire:edge-1")
	assert.True(t, ev.GetWildfire().HasPerimeter)
	assert.Equal(t, 8000.0, ev.GetWildfire().Acres, "the CAL FIRE scalars are exactly what a bare perimeter would lose")
	assert.Equal(t, int32(5), ev.GetWildfire().Containment)
}

// A fire whose point AND perimeter are both far outside stays out — the widened
// box is wider, not unbounded.
func TestWildfirePoll_IncidentFullyOutsideStaysDropped(t *testing.T) {
	const calfireFar = `[{
	  "UniqueId": "far-1", "Name": "Shasta Fire", "AcresBurned": 100.0,
	  "Latitude": 40.9, "Longitude": -122.0, "IsActive": true
	}]`
	const firisFar = `{"features":[{
	  "properties": {"incident_name": "Shasta Fire", "area_acres": 90.0, "poly_DateCurrent": 1000, "source": "FIRIS", "displayStatus": "Active"},
	  "geometry": {"type":"Polygon","coordinates":[[[-122.05,40.85],[-121.95,40.85],[-121.95,40.95],[-122.05,40.95],[-122.05,40.85]]]}
	}]}`

	n := newWildfireNormalizer(&fakeDoer{resp: calfireFar}, &fakeDoer{resp: firisFar})
	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	// Only the orphan perimeter (which the real ArcGIS envelope would not have
	// returned) — no calfire: event.
	assert.NotContains(t, eventIDs(res.Events), "calfire:far-1")
}

// Scope must be STABLE across a FIRIS outage. A perimeter-only in-scope fire
// that silently left Events would be RESOLVED by the disappearance sweep — a
// fabricated all-clear on a life-safety layer.
func TestWildfirePoll_PerimeterOnlyScopeSurvivesFirisOutage(t *testing.T) {
	const calfireEdge = `[{
	  "UniqueId": "edge-1", "Name": "Ridge Fire", "AcresBurned": 8000.0,
	  "PercentContained": 5.0, "Latitude": 39.9, "Longitude": -120.4, "IsActive": true
	}]`
	const firisEdge = `{"features":[{
	  "properties": {"incident_name": "Ridge Fire", "area_acres": 7900.0, "poly_DateCurrent": 1000, "source": "FIRIS", "displayStatus": "Active"},
	  "geometry": {"type":"Polygon","coordinates":[[[-120.45,38.9],[-120.35,38.9],[-120.35,40.0],[-120.45,40.0],[-120.45,38.9]]]}
	}]}`

	// Tick 1: healthy, in scope via the perimeter.
	n := newWildfireNormalizer(&fakeDoer{resp: calfireEdge}, &fakeDoer{resp: firisEdge})
	res1, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	healthy := eventByID(t, res1.Events, "calfire:edge-1")

	// Tick 2: FIRIS down. The carried-forward polygon must keep it in scope AND
	// hash-equal, so the sweep neither resolves it nor writes a false revision.
	n = newWildfireNormalizer(&fakeDoer{resp: calfireEdge}, &fakeDoer{err: assert.AnError})
	res2, err := n.Poll(testCtx(), &scriptedPrior{events: res1.Events})
	require.NoError(t, err)
	carried := eventByID(t, res2.Events, "calfire:edge-1")
	assert.True(t, carried.GetWildfire().HasPerimeter)
	assert.Equal(t, store.ContentHash(healthy), store.ContentHash(carried))
}

// --- Supersession: a standalone perimeter absorbed by a CAL FIRE incident ---
//
// While no incident claims a perimeter it is emitted as firis:<name>. Once one
// does, that id stops being emitted — and because firis is an `expire` source,
// the sweep alone would hold the orphan ACTIVE for the full 24h grace, drawing
// the same fire twice for a day. Naming the successor ends it immediately.

func TestWildfireSupersedesAdoptedStandalone(t *testing.T) {
	// The store already holds Salt Springs as a standalone perimeter (the state
	// after a tick where CAL FIRE had not yet listed the incident, or before the
	// scope widened). "firis:lonely" stays standalone and must be untouched.
	prior := &scriptedPrior{events: []*gridv1.Event{
		priorWildfireEvent("firis:saltsprings", "firis", 38.2, -120.4, true),
		priorWildfireEvent("firis:lonely", "firis", 38.05, -119.85, true),
	}}

	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: firisFixture})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)

	// The incident adopted the perimeter, so the standalone id is gone from
	// Events — and named as superseded rather than left to the grace.
	assert.NotContains(t, eventIDs(res.Events), "firis:saltsprings")
	assert.Contains(t, eventIDs(res.Events), "calfire:abc-123")
	assert.Equal(t, []string{"firis:saltsprings"}, res.Superseded)

	// The genuinely-standalone fire is still emitted and NOT superseded.
	assert.Contains(t, eventIDs(res.Events), "firis:lonely")
}

func TestWildfireSupersedesNothingWithoutAPriorStandalone(t *testing.T) {
	// Same adoption, but the store never held the standalone: nothing to retire.
	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: firisFixture})
	res, err := n.Poll(testCtx(), &scriptedPrior{})
	require.NoError(t, err)
	assert.Empty(t, res.Superseded)
}

// Supersession must rest on positive evidence only. With FIRIS down, adoption
// is uncomputable — the standalone's absence proves nothing, so it keeps its
// grace.
func TestWildfireSupersedesNothingWhileFirisDown(t *testing.T) {
	prior := &scriptedPrior{events: []*gridv1.Event{
		priorWildfireEvent("firis:saltsprings", "firis", 38.2, -120.4, true),
	}}
	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{err: assert.AnError})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	require.Error(t, res.PerSource["firis"])
	assert.Empty(t, res.Superseded)
}

// Same for CAL FIRE down: no incidents, so nothing adopts, so nothing is
// superseded (the standalone is in fact still emitted).
func TestWildfireSupersedesNothingWhileCalfireDown(t *testing.T) {
	prior := &scriptedPrior{events: []*gridv1.Event{
		priorWildfireEvent("firis:saltsprings", "firis", 38.2, -120.4, true),
	}}
	n := newWildfireNormalizer(&fakeDoer{err: assert.AnError}, &fakeDoer{resp: firisFixture})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	require.Error(t, res.PerSource["calfire"])
	assert.Empty(t, res.Superseded)
	assert.Contains(t, eventIDs(res.Events), "firis:saltsprings", "still standalone while CAL FIRE is down")
}

// An ambiguous name (two distinct same-named fires) blocks adoption, so both
// perimeters stay standalone and neither is superseded.
func TestWildfireSupersedesNothingWhenAmbiguous(t *testing.T) {
	prior := &scriptedPrior{events: []*gridv1.Event{
		priorWildfireEvent("firis:ambiguous", "firis", 38.0, -120.25, true),
	}}
	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: firisFixture})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.NotContains(t, res.Superseded, "firis:ambiguous")
	assert.Contains(t, eventIDs(res.Events), "firis:ambiguous")
}

// Precision: when a name had TWO clusters and only the surviving one is
// adopted, the sibling that genuinely dropped out of the feed keeps its grace —
// its absence is still ambiguous. Only the adopted id is named.
func TestWildfireSupersedesOnlyTheAdoptedSibling(t *testing.T) {
	// Prior holds both "ambiguous" clusters as standalones. This tick's FIRIS
	// feed carries only the SOUTHERN one (38.0), which the CAL FIRE "Ambiguous
	// Fire" incident (38.0, -120.2) then adopts unambiguously.
	const firisOneCluster = `{"features":[{
	  "properties": {"incident_name": "Ambiguous Fire", "area_acres": 30.0, "poly_DateCurrent": 1000, "source": "FIRIS", "displayStatus": "Active"},
	  "geometry": {"type":"Polygon","coordinates":[[[-120.3,37.95],[-120.2,37.95],[-120.2,38.05],[-120.3,38.05],[-120.3,37.95]]]}
	}]}`
	prior := &scriptedPrior{events: []*gridv1.Event{
		priorWildfireEvent("firis:ambiguous", "firis", 38.0, -120.25, true),   // southern
		priorWildfireEvent("firis:ambiguous-2", "firis", 38.4, -120.25, true), // northern, gone from the feed
	}}

	n := newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: firisOneCluster})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)

	assert.Equal(t, []string{"firis:ambiguous"}, res.Superseded,
		"only the adopted id is proven gone; the sibling merely vanished and keeps its grace")
	assert.NotContains(t, eventIDs(res.Events), "firis:ambiguous-2")
}
