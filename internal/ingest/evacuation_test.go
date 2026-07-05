package ingest

import (
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

	res, err := n.Poll(testCtx())
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

func TestEvacuationPollError(t *testing.T) {
	// A Cal OES failure must be a hard error (UNAVAILABLE upstream), never an
	// empty all-clear.
	n := NewEvacuationNormalizer(testConfig(), caloes.NewClientWithHTTPDoer("https://caloes.test", &fakeDoer{err: assert.AnError}))
	_, err := n.Poll(testCtx())
	assert.Error(t, err)
}

func TestHumanEvacLevel(t *testing.T) {
	assert.Equal(t, "Order", humanEvacLevel("ORDER"))
	assert.Equal(t, "Warning", humanEvacLevel("WARNING"))
	assert.Equal(t, "Advisory", humanEvacLevel("ADVISORY"))
	assert.Equal(t, "Shelter In Place", humanEvacLevel("SHELTER_IN_PLACE"))
}
