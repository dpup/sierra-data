package ingest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/usgs"
)

const quakeFixture = `{
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

func TestEarthquakePoll(t *testing.T) {
	doer := &fakeDoer{resp: quakeFixture}
	n := NewEarthquakeNormalizer(testConfig(), usgs.NewClientWithHTTPDoer("https://usgs.test", doer))
	assert.Equal(t, []string{"usgs"}, n.SourceIDs())

	res, err := n.Poll(testCtx())
	require.NoError(t, err)
	require.Len(t, res.Events, 2)
	assert.Nil(t, res.PerSource)

	// The query covers the union bbox of the configured areas.
	assert.Contains(t, doer.lastURL, "minlatitude=37.7")
	assert.Contains(t, doer.lastURL, "maxlatitude=38.5")
	assert.Contains(t, doer.lastURL, "minlongitude=-120.9")
	assert.Contains(t, doer.lastURL, "maxlongitude=-119.2")
	assert.Contains(t, doer.lastURL, "minmagnitude=2.5")

	ev := res.Events[0]
	assert.Equal(t, "usgs:nc75095123", ev.Id)
	assert.Equal(t, gridv1.Layer_EARTHQUAKE, ev.Layer)
	assert.Equal(t, "M4.2 — 10km NE of Murphys, CA", ev.Headline) // shipped format, exact
	assert.Equal(t, "earthquake", ev.Category)
	assert.Equal(t, gridv1.Severity_MODERATE, ev.Severity)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.Status)
	assert.Equal(t, "10km NE of Murphys, CA", ev.AreaLabel)
	assert.Equal(t, "https://earthquake.usgs.gov/earthquakes/eventpage/nc75095123", ev.CanonicalUrl)

	require.NotNil(t, ev.Geometry)
	assert.Equal(t, 38.2, ev.Geometry.Centroid.Lat)
	assert.Equal(t, -120.45, ev.Geometry.Centroid.Lng)
	assert.Equal(t, 38.2, ev.Geometry.Bbox.MinLat)
	assert.Equal(t, 38.2, ev.Geometry.Bbox.MaxLat)

	d := ev.GetEarthquake()
	require.NotNil(t, d)
	assert.Equal(t, 4.2, d.Magnitude)
	assert.Equal(t, 7.6, d.DepthKm)
	assert.Equal(t, int32(37), d.Felt)
	assert.Equal(t, "https://earthquake.usgs.gov/earthquakes/eventpage/nc75095123", d.Url)

	require.NotNil(t, ev.Provenance)
	assert.Equal(t, "usgs", ev.Provenance.SourceId)
	assert.Equal(t, "USGS", ev.Provenance.SourceName)
	assert.Equal(t, "U.S. Geological Survey", ev.Provenance.Attribution)

	require.NotNil(t, ev.Effective)
	assert.Equal(t, time.UnixMilli(1782400000000).UTC(), ev.Effective.AsTime())
	require.NotNil(t, ev.ObservedAt)
	assert.Equal(t, time.UnixMilli(1782400500000).UTC(), ev.ObservedAt.AsTime())

	// Second quake: unsafe URL scrubbed, observed_at falls back to event time.
	ev2 := res.Events[1]
	assert.Equal(t, "usgs:nc75095124", ev2.Id)
	assert.Equal(t, gridv1.Severity_MINOR, ev2.Severity)
	assert.Empty(t, ev2.CanonicalUrl)
	assert.Empty(t, ev2.GetEarthquake().Url)
	require.NotNil(t, ev2.ObservedAt)
	assert.Equal(t, ev2.Effective.AsTime(), ev2.ObservedAt.AsTime())
}

func TestEarthquakePollError(t *testing.T) {
	n := NewEarthquakeNormalizer(testConfig(), usgs.NewClientWithHTTPDoer("https://usgs.test", &fakeDoer{err: assert.AnError}))
	_, err := n.Poll(testCtx())
	assert.Error(t, err)
}
