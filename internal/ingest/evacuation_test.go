package ingest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/caloes"
)

const evacFixture = `{
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
        "STATUS": "Prepare to leave",
        "EVENT_TYPE": "Fire",
        "PUBLIC_INFO": "Be ready."
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

func TestEvacuationPoll(t *testing.T) {
	doer := &fakeDoer{resp: evacFixture}
	n := NewEvacuationNormalizer(testConfig(), caloes.NewClientWithHTTPDoer("https://caloes.test", doer))
	assert.Equal(t, []string{"caloes"}, n.SourceIDs())

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Nil(t, res.PerSource)
	// Zone B ("... Lifted") is inactive and dropped.
	assert.ElementsMatch(t, []string{"evac:CAL-E-046", "evac:TUO-E-101", "evac:TUO-E-102"}, eventIDs(res.Events))

	order := eventByID(t, res.Events, "evac:CAL-E-046")
	assert.Equal(t, gridv1.Layer_EVACUATION, order.Layer)
	assert.Equal(t, "Evacuation Order — Zone A", order.Headline) // shipped format, exact
	assert.Equal(t, "order", order.Category)
	assert.Equal(t, gridv1.Severity_EXTREME, order.Severity)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, order.Status)
	// Life-safety: directive text carried verbatim, never paraphrased.
	assert.Equal(t, "Leave now via Hwy 4. Do not delay.", order.Description)
	assert.Equal(t, "Zone A", order.AreaLabel)
	require.NotNil(t, order.ObservedAt)
	assert.Equal(t, time.UnixMilli(1782400000000).UTC(), order.ObservedAt.AsTime())

	d := order.GetEvacuation()
	require.NotNil(t, d)
	assert.Equal(t, "CAL-E-046", d.ZoneId)
	assert.Equal(t, "ORDER", d.Level)
	assert.Equal(t, "Fire", d.EventType)
	assert.Equal(t, "Calaveras", d.County)

	require.NotNil(t, order.Geometry)
	assert.InDelta(t, 38.15, order.Geometry.Centroid.Lat, 1e-9)
	assert.InDelta(t, -120.35, order.Geometry.Centroid.Lng, 1e-9)
	assert.InDelta(t, 38.1, order.Geometry.Bbox.MinLat, 1e-9)
	assert.InDelta(t, 38.2, order.Geometry.Bbox.MaxLat, 1e-9)

	require.NotNil(t, order.Provenance)
	assert.Equal(t, "caloes", order.Provenance.SourceId)
	assert.Equal(t, "Cal OES", order.Provenance.SourceName)
	assert.Equal(t, "Cal OES — reference only", order.Provenance.Attribution)
	assert.Equal(t, caloes.SourceURL, order.Provenance.SourceUrl)

	// Unrecognized active status conservatively classifies as WARNING.
	warn := eventByID(t, res.Events, "evac:TUO-E-101")
	assert.Equal(t, "WARNING", warn.GetEvacuation().Level)
	assert.Equal(t, "warning", warn.Category)
	assert.Equal(t, gridv1.Severity_SEVERE, warn.Severity)
	assert.Equal(t, "Evacuation Warning — Zone C", warn.Headline)

	sip := eventByID(t, res.Events, "evac:TUO-E-102")
	assert.Equal(t, "SHELTER_IN_PLACE", sip.GetEvacuation().Level)
	assert.Equal(t, "shelter_in_place", sip.Category)
	assert.Equal(t, gridv1.Severity_SEVERE, sip.Severity)
	assert.Equal(t, "Evacuation Shelter In Place — Zone D", sip.Headline)
}

// evacFeature renders one Cal OES feature; geometry is a small square at
// (lat, lng) unless rawGeometry overrides it.
func evacFeature(zoneID, zoneName, county, status, publicInfo string, lat, lng float64) string {
	return fmt.Sprintf(`{
	  "properties": {"ZONE_ID": %q, "ZONE_NAME": %q, "COUNTY": %q, "STATUS": %q, "PUBLIC_INFO": %q},
	  "geometry": {"type": "Polygon", "coordinates": [[[%[6]f,%[7]f],[%[8]f,%[7]f],[%[8]f,%[9]f],[%[6]f,%[9]f],[%[6]f,%[7]f]]]}
	}`, zoneID, zoneName, county, status, publicInfo, lng, lat, lng+0.1, lat+0.1)
}

func evacCollection(features ...string) string {
	return `{"type": "FeatureCollection", "features": [` + strings.Join(features, ",") + `]}`
}

func pollEvac(t *testing.T, fixture string) *PollResult {
	t.Helper()
	n := NewEvacuationNormalizer(testConfig(), caloes.NewClientWithHTTPDoer("https://caloes.test", &fakeDoer{resp: fixture}))
	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	return res
}

// A present row whose STATUS is blank is missing data, NOT an all-clear:
// dropping it would let the resolve sweep fabricate a lifted zone. It takes
// the conservative WARNING default; only explicit inactive keywords drop.
func TestEvacuationPoll_BlankStatusKeptAsWarning(t *testing.T) {
	res := pollEvac(t, evacCollection(
		evacFeature("CAL-E-050", "Zone Blank", "Calaveras", "", "Await instructions.", 38.1, -120.4),
		evacFeature("CAL-E-051", "Zone Space", "Calaveras", "   ", "", 38.1, -120.2),
		evacFeature("CAL-E-052", "Zone Lifted", "Calaveras", "Evacuation Order Lifted", "All clear.", 38.1, -120.6),
	))
	// Blank and whitespace-only survive; the explicit "Lifted" drops.
	assert.ElementsMatch(t, []string{"evac:CAL-E-050", "evac:CAL-E-051"}, eventIDs(res.Events))

	blank := eventByID(t, res.Events, "evac:CAL-E-050")
	assert.Equal(t, "WARNING", blank.GetEvacuation().Level)
	assert.Equal(t, gridv1.Severity_SEVERE, blank.Severity)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, blank.Status)
	assert.Equal(t, "Evacuation Warning — Zone Blank", blank.Headline)
	assert.Equal(t, "Await instructions.", blank.Description)
}

// Two concurrent events on ONE zone collapse to a single event keeping the
// higher-severity level (ORDER > SHELTER_IN_PLACE > WARNING > ADVISORY),
// instead of two same-id events overwriting each other every poll.
func TestEvacuationPoll_ConcurrentEventsOneZoneKeepHigherSeverity(t *testing.T) {
	res := pollEvac(t, evacCollection(
		evacFeature("CAL-E-046", "Zone A", "Calaveras", "Evacuation Warning", "Be ready.", 38.1, -120.4),
		evacFeature("CAL-E-046", "Zone A", "Calaveras", "Evacuation Order", "Leave now.", 38.1, -120.4),
	))
	require.Len(t, res.Events, 1, "one zone => one event, whatever the upstream duplication")
	ev := res.Events[0]
	assert.Equal(t, "evac:CAL-E-046", ev.Id)
	assert.Equal(t, "ORDER", ev.GetEvacuation().Level)
	assert.Equal(t, gridv1.Severity_EXTREME, ev.Severity)
	assert.Equal(t, "Leave now.", ev.Description, "the winning event's directive text is kept")

	// Order-independence: the higher severity wins regardless of feed order.
	res = pollEvac(t, evacCollection(
		evacFeature("CAL-E-046", "Zone A", "Calaveras", "Evacuation Order", "Leave now.", 38.1, -120.4),
		evacFeature("CAL-E-046", "Zone A", "Calaveras", "Evacuation Warning", "Be ready.", 38.1, -120.4),
	))
	require.Len(t, res.Events, 1)
	assert.Equal(t, "ORDER", res.Events[0].GetEvacuation().Level)
}

// Rows with BOTH identifiers blank must not collapse to the single id
// "evac:" — they get a synthetic content id from county + geometry bytes,
// stable across polls and distinct per zone.
func TestEvacuationPoll_BlankIdentityGetsSyntheticID(t *testing.T) {
	fixture := evacCollection(
		evacFeature("", "", "Calaveras", "Evacuation Order", "Leave.", 38.1, -120.4),
		evacFeature("", "", "Tuolumne", "Evacuation Warning", "Ready.", 38.0, -120.1),
	)
	res := pollEvac(t, fixture)
	require.Len(t, res.Events, 2)
	id0, id1 := res.Events[0].Id, res.Events[1].Id
	assert.NotEqual(t, id0, id1, "distinct unnamed zones must keep distinct ids")
	for _, id := range []string{id0, id1} {
		assert.Regexp(t, `^evac:zone-[0-9a-f]{8}$`, id)
	}

	// Deterministic across polls: same feed bytes => same ids.
	res2 := pollEvac(t, fixture)
	assert.ElementsMatch(t, []string{id0, id1}, eventIDs(res2.Events))
}

// Residual collision: two DIFFERENT zones that still collide on an id (same
// name, no zone id) are suffixed deterministically by (county, centroid)
// order rather than overwriting each other.
func TestEvacuationPoll_ResidualCollisionSuffixed(t *testing.T) {
	fixture := evacCollection(
		// Feed order is reversed relative to county sort order to prove the
		// suffix assignment is deterministic, not first-come.
		evacFeature("", "Twin Zone", "Tuolumne", "Evacuation Warning", "", 38.0, -120.1),
		evacFeature("", "Twin Zone", "Calaveras", "Evacuation Order", "", 38.1, -120.4),
	)
	res := pollEvac(t, fixture)
	require.Len(t, res.Events, 2)
	assert.ElementsMatch(t, []string{"evac:Twin Zone", "evac:Twin Zone-2"}, eventIDs(res.Events))
	// Calaveras < Tuolumne: the Calaveras zone keeps the bare id.
	bare := eventByID(t, res.Events, "evac:Twin Zone")
	assert.Equal(t, "Calaveras", bare.GetEvacuation().County)
	suffixed := eventByID(t, res.Events, "evac:Twin Zone-2")
	assert.Equal(t, "Tuolumne", suffixed.GetEvacuation().County)
}

// An active zone with unparseable geometry must stay visible to place-scoped
// reads: with no geometry there are no geometric place matches, so it is
// preset onto every configured hazard-area place (conservative
// over-attachment; the store unions presets with geometric matches).
func TestEvacuationPoll_BadGeometryAttachesEveryArea(t *testing.T) {
	res := pollEvac(t, `{"type": "FeatureCollection", "features": [{
	  "properties": {"ZONE_ID": "CAL-E-060", "ZONE_NAME": "Zone Broken", "COUNTY": "Calaveras", "STATUS": "Evacuation Order", "PUBLIC_INFO": "Leave now."},
	  "geometry": {"type": "Polygon", "coordinates": "garbage"}
	}]}`)
	require.Len(t, res.Events, 1)
	ev := res.Events[0]
	assert.Equal(t, "evac:CAL-E-060", ev.Id)
	assert.Nil(t, ev.Geometry)
	assert.Equal(t, []string{"area:calaveras", "area:tuolumne"}, ev.PlaceIds,
		"geometry-less life-safety event must be attached to every configured area")
	assert.Equal(t, gridv1.Severity_EXTREME, ev.Severity)
}

func TestEvacLevelRank(t *testing.T) {
	assert.Greater(t, evacLevelRank("ORDER"), evacLevelRank("SHELTER_IN_PLACE"))
	assert.Greater(t, evacLevelRank("SHELTER_IN_PLACE"), evacLevelRank("WARNING"))
	assert.Greater(t, evacLevelRank("WARNING"), evacLevelRank("ADVISORY"))
	assert.Greater(t, evacLevelRank("ADVISORY"), evacLevelRank(""))
}

func TestEvacuationPollError(t *testing.T) {
	// A Cal OES failure must be a hard error (UNAVAILABLE upstream), never an
	// empty all-clear.
	n := NewEvacuationNormalizer(testConfig(), caloes.NewClientWithHTTPDoer("https://caloes.test", &fakeDoer{err: assert.AnError}))
	_, err := n.Poll(testCtx(), nil)
	assert.Error(t, err)
}

func TestHumanEvacLevel(t *testing.T) {
	assert.Equal(t, "Order", humanEvacLevel("ORDER"))
	assert.Equal(t, "Warning", humanEvacLevel("WARNING"))
	assert.Equal(t, "Advisory", humanEvacLevel("ADVISORY"))
	assert.Equal(t, "Shelter In Place", humanEvacLevel("SHELTER_IN_PLACE"))
}

func eventByCounty(events []*gridv1.Event, county string) *gridv1.Event {
	for _, ev := range events {
		if ev.GetEvacuation().GetCounty() == county {
			return ev
		}
	}
	return nil
}

// A surviving zone must KEEP its suffixed id across polls when its colliding
// sibling is lifted — never flip to the bare id, which the resolve sweep would
// read as the old id disappearing and turn into a spurious RESOLVED all-clear
// for a zone that is still actively evacuating.
func TestEvacuationPoll_CollisionContinuityAcrossPolls(t *testing.T) {
	// Poll 1: two zones share id "evac:Twin Zone" -> Calaveras keeps the bare id
	// (county sort), Tuolumne gets the -2 suffix.
	res1 := pollEvac(t, evacCollection(
		evacFeature("", "Twin Zone", "Tuolumne", "Evacuation Warning", "", 38.0, -120.1),
		evacFeature("", "Twin Zone", "Calaveras", "Evacuation Order", "", 38.1, -120.4),
	))
	require.Len(t, res1.Events, 2)
	require.Equal(t, "evac:Twin Zone-2", eventByCounty(res1.Events, "Tuolumne").GetId())
	prior := &scriptedPrior{events: res1.Events}

	// Poll 2: the Calaveras zone is lifted; only Tuolumne remains (now the sole
	// candidate for the base id). It must stay evac:Twin Zone-2, not become the
	// bare evac:Twin Zone.
	n := NewEvacuationNormalizer(testConfig(), caloes.NewClientWithHTTPDoer("https://caloes.test",
		&fakeDoer{resp: evacCollection(
			evacFeature("", "Twin Zone", "Tuolumne", "Evacuation Warning", "", 38.0, -120.1),
		)}))
	res2, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	require.Len(t, res2.Events, 1)
	assert.Equal(t, "evac:Twin Zone-2", res2.Events[0].Id,
		"survivor must keep its suffixed id, not flip to bare (which fabricates an all-clear)")
}
