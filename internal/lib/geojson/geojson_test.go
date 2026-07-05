package geojson

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustParse(t *testing.T, raw string) *Geom {
	t.Helper()
	g, err := Parse([]byte(raw))
	require.NoError(t, err)
	return g
}

// polygonWithHole: shell lng -121..-120 / lat 38..39, hole lng -120.6..-120.4 /
// lat 38.4..38.6. Shared by parse, bbox, and PIP tests.
const polygonWithHole = `{"type":"Polygon","coordinates":[
	[[-121,38],[-120,38],[-120,39],[-121,39],[-121,38]],
	[[-120.6,38.4],[-120.4,38.4],[-120.4,38.6],[-120.6,38.6],[-120.6,38.4]]
]}`

// multiPolygon: two disjoint squares — part A lng -121..-120.7 / lat 38..38.3,
// part B lng -120.3..-120 / lat 38.7..39.
const multiPolygon = `{"type":"MultiPolygon","coordinates":[
	[[[-121,38],[-120.7,38],[-120.7,38.3],[-121,38.3],[-121,38]]],
	[[[-120.3,38.7],[-120,38.7],[-120,39],[-120.3,39],[-120.3,38.7]]]
]}`

func TestParse_Point(t *testing.T) {
	g := mustParse(t, `{"type":"Point","coordinates":[-120.3,38.2]}`)
	assert.Equal(t, "Point", g.Type)
	assert.Equal(t, Position{-120.3, 38.2}, g.Point)
	assert.Equal(t, 38.2, g.Point.Lat())
	assert.Equal(t, -120.3, g.Point.Lng())
}

// TestParse_ElevationDropped: RFC 7946 allows a third (elevation) element;
// Parse keeps only lng/lat.
func TestParse_ElevationDropped(t *testing.T) {
	g := mustParse(t, `{"type":"Point","coordinates":[-120.3,38.2,750.5]}`)
	assert.Equal(t, Position{-120.3, 38.2}, g.Point)
}

func TestParse_LineString(t *testing.T) {
	g := mustParse(t, `{"type":"LineString","coordinates":[[-120.5,38.0],[-120.0,38.5]]}`)
	assert.Equal(t, "LineString", g.Type)
	require.Len(t, g.Points, 2)
	assert.Equal(t, Position{-120.5, 38.0}, g.Points[0])
}

func TestParse_PolygonWithHole(t *testing.T) {
	g := mustParse(t, polygonWithHole)
	assert.Equal(t, "Polygon", g.Type)
	require.Len(t, g.Rings, 2)
	assert.Len(t, g.Rings[0], 5) // closed shell
	assert.Len(t, g.Rings[1], 5) // closed hole
}

func TestParse_MultiPolygon(t *testing.T) {
	g := mustParse(t, multiPolygon)
	assert.Equal(t, "MultiPolygon", g.Type)
	require.Len(t, g.Polygons, 2)
	require.Len(t, g.Polygons[0], 1)
	assert.Len(t, g.Polygons[0][0], 5)
}

// TestParse_ToleratedTypes: MultiPoint and MultiLineString parse (so upstream
// feeds don't hard-fail) even though PIP treats them as non-containing.
func TestParse_ToleratedTypes(t *testing.T) {
	mp := mustParse(t, `{"type":"MultiPoint","coordinates":[[-120.5,38.0],[-120.0,38.5]]}`)
	assert.Equal(t, "MultiPoint", mp.Type)
	assert.Len(t, mp.Points, 2)

	mls := mustParse(t, `{"type":"MultiLineString","coordinates":[[[-120.5,38.0],[-120.0,38.5]],[[-121,39],[-120.9,39.1]]]}`)
	assert.Equal(t, "MultiLineString", mls.Type)
	assert.Len(t, mls.Rings, 2)
}

func TestParse_Errors(t *testing.T) {
	cases := map[string]string{
		"invalid json":         `{"type":"Point"`,
		"unknown type":         `{"type":"Feature","coordinates":[0,0]}`,
		"geometry collection":  `{"type":"GeometryCollection","geometries":[]}`,
		"missing coordinates":  `{"type":"Point"}`,
		"null coordinates":     `{"type":"Polygon","coordinates":null}`,
		"short position":       `{"type":"Point","coordinates":[-120.3]}`,
		"one-point linestring": `{"type":"LineString","coordinates":[[-120.3,38.2]]}`,
		"empty polygon":        `{"type":"Polygon","coordinates":[]}`,
		"two-point ring":       `{"type":"Polygon","coordinates":[[[-121,38],[-120,39]]]}`,
		"empty multipolygon":   `{"type":"MultiPolygon","coordinates":[]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(raw))
			assert.Error(t, err)
		})
	}
}

func TestBbox_PerType(t *testing.T) {
	tests := []struct {
		name                           string
		raw                            string
		minLat, minLng, maxLat, maxLng float64
	}{
		{"point (degenerate)", `{"type":"Point","coordinates":[-120.3,38.2]}`, 38.2, -120.3, 38.2, -120.3},
		{"linestring", `{"type":"LineString","coordinates":[[-120.5,38.0],[-120.0,38.5]]}`, 38.0, -120.5, 38.5, -120.0},
		{"multipoint", `{"type":"MultiPoint","coordinates":[[-120.5,38.0],[-120.0,38.5]]}`, 38.0, -120.5, 38.5, -120.0},
		{"multilinestring", `{"type":"MultiLineString","coordinates":[[[-120.5,38.0],[-120.0,38.5]],[[-121,39],[-120.9,39.1]]]}`, 38.0, -121, 39.1, -120.0},
		// Hole is interior — bbox comes from the shell alone.
		{"polygon with hole", polygonWithHole, 38, -121, 39, -120},
		// Bbox spans both disjoint parts.
		{"multipolygon", multiPolygon, 38, -121, 39, -120},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			minLat, minLng, maxLat, maxLng := mustParse(t, tt.raw).Bbox()
			assert.Equal(t, tt.minLat, minLat, "minLat")
			assert.Equal(t, tt.minLng, minLng, "minLng")
			assert.Equal(t, tt.maxLat, maxLat, "maxLat")
			assert.Equal(t, tt.maxLng, maxLng, "maxLng")
		})
	}
}

func TestCentroid_BboxCenter(t *testing.T) {
	lat, lng := mustParse(t, polygonWithHole).Centroid()
	assert.Equal(t, 38.5, lat)
	assert.Equal(t, -120.5, lng)

	// A point is its own centroid.
	lat, lng = mustParse(t, `{"type":"Point","coordinates":[-120.3,38.2]}`).Centroid()
	assert.Equal(t, 38.2, lat)
	assert.Equal(t, -120.3, lng)

	// MultiPolygon: bbox center of both parts — may fall in the gap between
	// them (documented: indexing/display anchor, not an area centroid).
	lat, lng = mustParse(t, multiPolygon).Centroid()
	assert.Equal(t, 38.5, lat)
	assert.Equal(t, -120.5, lng)
	assert.False(t, PointInGeometry(lat, lng, mustParse(t, multiPolygon)))
}

// TestPointGeoJSON_WireOrder: output coordinates are [lng, lat] (RFC 7946),
// the inverse of the (lat, lng) arguments.
func TestPointGeoJSON_WireOrder(t *testing.T) {
	b := PointGeoJSON(38.2, -120.3)
	assert.JSONEq(t, `{"type":"Point","coordinates":[-120.3,38.2]}`, string(b))
}

// TestEncoders_FiveDecimalTrim: coordinates are trimmed to 5 decimals (~1.1 m),
// matching internal/hazards/geojson.go.
func TestEncoders_FiveDecimalTrim(t *testing.T) {
	g, err := Parse(PointGeoJSON(38.123456789, -120.987654321))
	require.NoError(t, err)
	assert.Equal(t, Position{-120.98765, 38.12346}, g.Point)
}

func TestPointGeoJSON_RoundTrip(t *testing.T) {
	g, err := Parse(PointGeoJSON(38.2, -120.3))
	require.NoError(t, err)
	assert.Equal(t, "Point", g.Type)
	lat, lng := g.Centroid()
	assert.Equal(t, 38.2, lat)
	assert.Equal(t, -120.3, lng)
}

func TestLineStringGeoJSON_RoundTrip(t *testing.T) {
	b := LineStringGeoJSON([][2]float64{{38.0, -120.5}, {38.5, -120.0}}) // (lat, lng) pairs
	g, err := Parse(b)
	require.NoError(t, err)
	assert.Equal(t, "LineString", g.Type)
	require.Len(t, g.Points, 2)
	assert.Equal(t, Position{-120.5, 38.0}, g.Points[0])

	minLat, minLng, maxLat, maxLng := g.Bbox()
	assert.Equal(t, []float64{38.0, -120.5, 38.5, -120.0}, []float64{minLat, minLng, maxLat, maxLng})
}

// TestLineStringGeoJSON_TooShort: fewer than two points is an invalid
// LineString — encoder returns nil (the package's "no geometry").
func TestLineStringGeoJSON_TooShort(t *testing.T) {
	assert.Nil(t, LineStringGeoJSON(nil))
	assert.Nil(t, LineStringGeoJSON([][2]float64{{38.0, -120.5}}))
}

func TestBboxPolygonGeoJSON_RoundTrip(t *testing.T) {
	b := BboxPolygonGeoJSON(38, -121, 39, -120)
	g, err := Parse(b)
	require.NoError(t, err)
	assert.Equal(t, "Polygon", g.Type)
	require.Len(t, g.Rings, 1)
	require.Len(t, g.Rings[0], 5)
	assert.Equal(t, g.Rings[0][0], g.Rings[0][4], "ring must be closed")

	minLat, minLng, maxLat, maxLng := g.Bbox()
	assert.Equal(t, []float64{38, -121, 39, -120}, []float64{minLat, minLng, maxLat, maxLng})

	assert.True(t, PointInGeometry(38.5, -120.5, g), "bbox center must be inside the bbox polygon")
	assert.False(t, PointInGeometry(37.9, -120.5, g))
}

// TestBboxPolygonGeoJSON_RingOrientation: exterior ring is counterclockwise
// per the RFC 7946 right-hand rule.
func TestBboxPolygonGeoJSON_RingOrientation(t *testing.T) {
	var geom struct {
		Coordinates [][][2]float64 `json:"coordinates"`
	}
	require.NoError(t, json.Unmarshal(BboxPolygonGeoJSON(38, -121, 39, -120), &geom))
	ring := geom.Coordinates[0]
	// Shoelace sum > 0 in [lng, lat] space means counterclockwise.
	area := 0.0
	for i := 0; i < len(ring)-1; i++ {
		area += ring[i][0]*ring[i+1][1] - ring[i+1][0]*ring[i][1]
	}
	assert.Positive(t, area)
}

func TestBboxIntersects(t *testing.T) {
	// Reference box: lat 38..39, lng -121..-120.
	tests := []struct {
		name                           string
		minLat, minLng, maxLat, maxLng float64
		want                           bool
	}{
		{"overlapping", 38.5, -120.5, 39.5, -119.5, true},
		{"contained", 38.4, -120.6, 38.6, -120.4, true},
		{"containing", 37, -122, 40, -119, true},
		{"identical", 38, -121, 39, -120, true},
		{"touching edge (inclusive)", 39, -121, 40, -120, true},
		{"touching corner (inclusive)", 39, -120, 40, -119, true},
		{"disjoint in lat", 39.1, -121, 40, -120, false},
		{"disjoint in lng", 38, -119.9, 39, -119, false},
		{"fully disjoint", 45, -100, 46, -99, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BboxIntersects(38, -121, 39, -120, tt.minLat, tt.minLng, tt.maxLat, tt.maxLng)
			assert.Equal(t, tt.want, got)
			// Intersection is symmetric.
			flipped := BboxIntersects(tt.minLat, tt.minLng, tt.maxLat, tt.maxLng, 38, -121, 39, -120)
			assert.Equal(t, tt.want, flipped)
		})
	}
}
