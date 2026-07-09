package geojson

import "testing"

func TestPointNearLine(t *testing.T) {
	// LineStringGeoJSON takes {lat, lng} pairs (see places.Seed). Angels Camp -> Murphys.
	g, err := Parse(LineStringGeoJSON([][2]float64{{38.0678, -120.5402}, {38.1377, -120.4561}}))
	if err != nil {
		t.Fatal(err)
	}

	// Chord midpoint is on the line -> within any positive buffer.
	if !PointNearLine(38.10275, -120.49815, g, 1500) {
		t.Error("midpoint of the chord should be within 1500m of the line")
	}
	// ~35 km west -> outside the buffer.
	if PointNearLine(38.10275, -120.90, g, 1500) {
		t.Error("a point ~35km away should not be within 1500m")
	}
	// Zero/negative buffer never matches.
	if PointNearLine(38.10275, -120.49815, g, 0) {
		t.Error("a zero buffer should not match")
	}

	// Non-line geometries return false (they use PointInGeometry instead).
	pt, err := Parse([]byte(`{"type":"Point","coordinates":[-120.5,38.1]}`))
	if err != nil {
		t.Fatal(err)
	}
	if PointNearLine(38.1, -120.5, pt, 1500) {
		t.Error("a Point geometry must return false from PointNearLine")
	}
}
