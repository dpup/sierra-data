package geojson

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOverlapFraction(t *testing.T) {
	county := mustParse(t, string(BboxPolygonGeoJSON(38.0, -120.5, 38.5, -120.0)))
	// Shares the county's east edge, drawn 0.002° (~200 m) inside it.
	sliver := mustParse(t, string(BboxPolygonGeoJSON(38.0, -120.002, 38.5, -119.5)))
	half := mustParse(t, string(BboxPolygonGeoJSON(38.0, -120.25, 38.5, -119.75)))
	inside := mustParse(t, string(BboxPolygonGeoJSON(38.1, -120.4, 38.2, -120.3)))
	apart := mustParse(t, string(BboxPolygonGeoJSON(39.0, -120.5, 39.5, -120.0)))

	assert.True(t, Intersects(county, sliver), "Intersects counts the sliver")
	assert.Less(t, OverlapFraction(county, sliver), 0.02)
	assert.InDelta(t, 0.5, OverlapFraction(county, half), 0.05)
	assert.InDelta(t, 1.0, OverlapFraction(county, inside), 0.05, "of the smaller shape")
	assert.Zero(t, OverlapFraction(county, apart))
}
