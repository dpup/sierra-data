package geojson

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// circle is a densely sampled ring, the shape of a real NWS zone edge.
func circle(lat, lng, r float64, n int) []Position {
	ring := make([]Position, 0, n+1)
	for i := 0; i < n; i++ {
		a := 2 * math.Pi * float64(i) / float64(n)
		ring = append(ring, Position{lng + r*math.Cos(a), lat + r*math.Sin(a)})
	}
	return append(ring, ring[0])
}

func TestSimplifiedMultiPolygon(t *testing.T) {
	a := &Geom{Type: "Polygon", Rings: [][]Position{circle(38.2, -120.4, 0.2, 2000)}}
	b := &Geom{Type: "Polygon", Rings: [][]Position{circle(38.6, -120.0, 0.2, 2000)}}
	line := &Geom{Type: "LineString", Points: []Position{{-120, 38}, {-119, 39}}}

	raw := SimplifiedMultiPolygon(0.005, a, line, b)
	require.NotNil(t, raw)
	g, err := Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "MultiPolygon", g.Type)
	require.Len(t, g.Polygons, 2, "areal parts only, in order")
	n := len(g.Polygons[0][0])
	assert.Less(t, n, 200, "2000 vertices simplify to far fewer")
	assert.Greater(t, n, 10, "and still describe the shape")
	assert.Equal(t, g.Polygons[0][0][0], g.Polygons[0][0][n-1], "rings stay closed")
	assert.True(t, PointInGeometry(38.2, -120.4, g))
	assert.False(t, PointInGeometry(38.4, -120.6, g))

	assert.Equal(t, raw, SimplifiedMultiPolygon(0.005, a, line, b), "deterministic")
	assert.Nil(t, SimplifiedMultiPolygon(0.005, line))
	assert.True(t, json.Valid(raw))
}

// A peak on one side of a narrow strip, under a shallow bump on the other:
// Douglas-Peucker keeps the peak and drops the bump, so the plain result
// crosses itself. The repaired ring takes back the bump and does not.
func TestSimplifiedRingDoesNotCrossItself(t *testing.T) {
	ring := []Position{
		{0.45, 0}, {0.5, 0.006}, {0.55, 0}, {1, 0},
		{1, 0.005}, {0.56, 0.005}, {0.56, 0.0085}, {0.44, 0.0085}, {0.44, 0.005}, {0, 0.005},
		{0, 0}, {0.45, 0},
	}
	plain := pick(ring, keptIndices(dpKeep(ring, 0.005)))
	require.True(t, ringSelfCrosses(plain), "precondition: plain Douglas-Peucker crosses")

	got := simplifyRingValid(ring, 0.005)
	assert.False(t, ringSelfCrosses(got))
	assert.Less(t, len(got), len(ring), "still simplified")
	assert.Equal(t, got[0], got[len(got)-1], "closed")
}
