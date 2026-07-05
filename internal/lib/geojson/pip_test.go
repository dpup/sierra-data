package geojson

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPointInGeometry_PolygonWithHole(t *testing.T) {
	g := mustParse(t, polygonWithHole)

	assert.True(t, PointInGeometry(38.1, -120.9, g), "in shell, away from hole")
	assert.True(t, PointInGeometry(38.5, -120.9, g), "in shell, west of hole at hole latitude")
	assert.False(t, PointInGeometry(38.5, -120.5, g), "inside the hole is outside the polygon")
	assert.False(t, PointInGeometry(37.5, -120.5, g), "south of shell")
}

// TestPointInGeometry_BboxFastPath: a far-away point is rejected by the bbox
// check (same result as a full ring walk, cheaper).
func TestPointInGeometry_BboxFastPath(t *testing.T) {
	g := mustParse(t, polygonWithHole)
	assert.False(t, PointInGeometry(45, -100, g))
}

func TestPointInGeometry_MultiPolygonParts(t *testing.T) {
	g := mustParse(t, multiPolygon)

	assert.True(t, PointInGeometry(38.15, -120.85, g), "inside part A")
	assert.True(t, PointInGeometry(38.85, -120.15, g), "inside part B")
	// Inside the overall bbox but between the parts — exercises the ray cast,
	// not the bbox fast path.
	assert.False(t, PointInGeometry(38.5, -120.5, g), "gap between parts")
}

// TestPointInGeometry_NonAreal: only Polygon/MultiPolygon can contain a point.
func TestPointInGeometry_NonAreal(t *testing.T) {
	assert.False(t, PointInGeometry(38.2, -120.3, nil))
	assert.False(t, PointInGeometry(38.2, -120.3,
		mustParse(t, `{"type":"Point","coordinates":[-120.3,38.2]}`)))
	assert.False(t, PointInGeometry(38.0, -120.5,
		mustParse(t, `{"type":"LineString","coordinates":[[-120.5,38.0],[-120.0,38.5]]}`)))
	assert.False(t, PointInGeometry(38.0, -120.5,
		mustParse(t, `{"type":"MultiPoint","coordinates":[[-120.5,38.0],[-120.0,38.5]]}`)))
	assert.False(t, PointInGeometry(38.0, -120.5,
		mustParse(t, `{"type":"MultiLineString","coordinates":[[[-120.5,38.0],[-120.0,38.5]]]}`)))
}

// TestPointInGeometry_BoundaryConvention pins the documented half-open
// boundary behavior for an axis-aligned square (lat 0..10, lng 0..10):
// min-lat/min-lng edges and the min corner count as inside; max-lat/max-lng
// edges and the max corner count as outside. This is the ray-cast convention,
// not a contract — callers must treat on-boundary results as unspecified and
// buffer the geometry if they need edge tolerance.
func TestPointInGeometry_BoundaryConvention(t *testing.T) {
	g := mustParse(t, `{"type":"Polygon","coordinates":[[[0,0],[10,0],[10,10],[0,10],[0,0]]]}`)

	assert.True(t, PointInGeometry(5, 5, g), "interior")
	assert.True(t, PointInGeometry(0, 5, g), "min-lat (bottom) edge")
	assert.True(t, PointInGeometry(5, 0, g), "min-lng (left) edge")
	assert.True(t, PointInGeometry(0, 0, g), "min corner vertex")
	assert.False(t, PointInGeometry(10, 5, g), "max-lat (top) edge")
	assert.False(t, PointInGeometry(5, 10, g), "max-lng (right) edge")
	assert.False(t, PointInGeometry(10, 10, g), "max corner vertex")
}

// TestPointInGeometry_UnclosedRing: rings missing the duplicate closing vertex
// are treated as implicitly closed (last->first wraparound edge).
func TestPointInGeometry_UnclosedRing(t *testing.T) {
	closed := mustParse(t, `{"type":"Polygon","coordinates":[[[0,0],[10,0],[10,10],[0,10],[0,0]]]}`)
	open := mustParse(t, `{"type":"Polygon","coordinates":[[[0,0],[10,0],[10,10],[0,10]]]}`)

	for _, pt := range [][2]float64{{5, 5}, {0, 5}, {5, 0}, {10, 5}, {5, 10}, {11, 5}} {
		assert.Equal(t,
			PointInGeometry(pt[0], pt[1], closed),
			PointInGeometry(pt[0], pt[1], open),
			"closed and unclosed rings must agree at (%v, %v)", pt[0], pt[1])
	}
	assert.True(t, PointInGeometry(5, 5, open))
}

// TestPointInGeometry_ConcaveShape: a C-shaped (concave) polygon — the notch
// is outside even though it is inside the bbox, and the bbox-center centroid
// falls in the notch (why Centroid is documented as display-only).
func TestPointInGeometry_ConcaveShape(t *testing.T) {
	// C-shape opening east: outer 0..10 square minus an east-side notch
	// lat 3..7 / lng 4..10.
	g := mustParse(t, `{"type":"Polygon","coordinates":[[
		[0,0],[10,0],[10,3],[4,3],[4,7],[10,7],[10,10],[0,10],[0,0]
	]]}`)

	assert.True(t, PointInGeometry(1.5, 5, g), "bottom arm")
	assert.True(t, PointInGeometry(8.5, 5, g), "top arm")
	assert.True(t, PointInGeometry(5, 2, g), "spine")
	assert.False(t, PointInGeometry(5, 7, g), "notch is inside bbox but outside")

	lat, lng := g.Centroid()
	assert.False(t, PointInGeometry(lat, lng, g), "bbox-center centroid lands in the notch")
}
