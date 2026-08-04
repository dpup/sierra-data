package geojson

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A ~0.1° box near 38.2N — roughly 11 km on a side, the scale of a real
// perimeter/coverage polygon in the service area.
const nearBoxA = `{"type":"Polygon","coordinates":[[[-120.5,38.2],[-120.4,38.2],[-120.4,38.3],[-120.5,38.3],[-120.5,38.2]]]}`

func TestWithinDistance_OverlapAndContainment(t *testing.T) {
	a := mustParse(t, nearBoxA)
	// Overlapping box: distance 0 regardless of the buffer.
	overlap := mustParse(t, `{"type":"Polygon","coordinates":[[[-120.45,38.25],[-120.35,38.25],[-120.35,38.35],[-120.45,38.35],[-120.45,38.25]]]}`)
	assert.True(t, WithinDistance(a, overlap, 1))

	// A point inside the polygon — Intersects is false for a point, so this
	// exercises the vertex-inside path.
	inside := mustParse(t, `{"type":"Point","coordinates":[-120.45,38.25]}`)
	assert.True(t, WithinDistance(a, inside, 1))
	assert.True(t, WithinDistance(inside, a, 1))
}

func TestWithinDistance_SeparatedByKnownGap(t *testing.T) {
	a := mustParse(t, nearBoxA)
	// Due east of a's eastern edge (-120.4) by 0.1° of longitude. At 38.25N a
	// degree of longitude is ~87.4 km, so the gap is ~8.7 km.
	east := mustParse(t, `{"type":"Polygon","coordinates":[[[-120.3,38.2],[-120.2,38.2],[-120.2,38.3],[-120.3,38.3],[-120.3,38.2]]]}`)

	assert.False(t, WithinDistance(a, east, 5000), "~8.7 km apart is outside a 5 km buffer")
	assert.True(t, WithinDistance(a, east, 10000), "~8.7 km apart is inside a 10 km buffer")
	// Symmetric.
	assert.Equal(t, WithinDistance(a, east, 10000), WithinDistance(east, a, 10000))
}

func TestWithinDistance_PointNearPolygon(t *testing.T) {
	a := mustParse(t, nearBoxA)
	// ~0.05° north of the top edge (38.3) ≈ 5.6 km.
	pt := mustParse(t, `{"type":"Point","coordinates":[-120.45,38.35]}`)
	assert.False(t, WithinDistance(a, pt, 3000))
	assert.True(t, WithinDistance(a, pt, 8000))
	assert.True(t, WithinDistance(pt, a, 8000))
}

func TestWithinDistance_PointToPoint(t *testing.T) {
	// Neither geometry has edges — the vertex-to-vertex fallback. 0.1° of
	// latitude ≈ 11.1 km.
	a := mustParse(t, `{"type":"Point","coordinates":[-120.4,38.2]}`)
	b := mustParse(t, `{"type":"Point","coordinates":[-120.4,38.3]}`)
	assert.False(t, WithinDistance(a, b, 5000))
	assert.True(t, WithinDistance(a, b, 12000))
}

func TestWithinDistance_FarApartRejectedByBbox(t *testing.T) {
	a := mustParse(t, nearBoxA)
	far := mustParse(t, `{"type":"Polygon","coordinates":[[[-118.0,36.0],[-117.9,36.0],[-117.9,36.1],[-118.0,36.1],[-118.0,36.0]]]}`)
	assert.False(t, WithinDistance(a, far, 20000))
}

func TestWithinDistance_MultiPolygonAndLineString(t *testing.T) {
	a := mustParse(t, nearBoxA)
	// One far part, one part ~8.7 km east — the near part must win.
	multi := mustParse(t, `{"type":"MultiPolygon","coordinates":[
		[[[-118.0,36.0],[-117.9,36.0],[-117.9,36.1],[-118.0,36.1],[-118.0,36.0]]],
		[[[-120.3,38.2],[-120.2,38.2],[-120.2,38.3],[-120.3,38.3],[-120.3,38.2]]]]}`)
	assert.True(t, WithinDistance(a, multi, 10000))
	assert.False(t, WithinDistance(a, multi, 5000))

	// A corridor LineString running ~8.7 km east of the box.
	line := mustParse(t, `{"type":"LineString","coordinates":[[-120.3,38.1],[-120.3,38.4]]}`)
	assert.True(t, WithinDistance(a, line, 10000))
	assert.False(t, WithinDistance(a, line, 5000))
}

func TestWithinDistance_NilAndNonPositive(t *testing.T) {
	a := mustParse(t, nearBoxA)
	assert.False(t, WithinDistance(nil, a, 1000))
	assert.False(t, WithinDistance(a, nil, 1000))
	assert.False(t, WithinDistance(a, a, 0), "a non-positive buffer disables the test entirely")
	assert.False(t, WithinDistance(a, a, -1))
}
