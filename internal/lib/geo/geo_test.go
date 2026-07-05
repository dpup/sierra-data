package geo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Contract tests for geo-utils library
// These tests define the interface before implementation exists
// They MUST FAIL initially to satisfy TDD RED-GREEN-Refactor cycle

func TestGeoUtils_PointToPoint(t *testing.T) {
	// Highway 4 test coordinates: Angels Camp to Murphys (real route)
	angelscamp := Point{Latitude: 38.0675, Longitude: -120.5436}
	murphys := Point{Latitude: 38.1391, Longitude: -120.4561}

	geoUtils := NewGeoUtils()

	// Test great-circle distance calculation
	distance, err := geoUtils.PointToPoint(angelscamp, murphys)
	require.NoError(t, err)

	// Expected distance ~11.0 km between Angels Camp and Murphys (actual great-circle distance)
	assert.InDelta(t, 11046, distance, 100, "Distance should be approximately 11.0km")

	// Test error cases
	invalidPoint := Point{Latitude: 200, Longitude: -300} // Invalid coordinates
	_, err = geoUtils.PointToPoint(angelscamp, invalidPoint)
	assert.Error(t, err, "Should return error for invalid coordinates")
}

func TestGeoUtils_PointToPolyline(t *testing.T) {
	geoUtils := NewGeoUtils()

	// Test point near Highway 4 route
	testPoint := Point{Latitude: 38.1000, Longitude: -120.5000}

	// Example Highway 4 polyline (simplified)
	routePolyline := Polyline{
		EncodedPolyline: "_p~iF~ps|U_ulLnnqC_mqNvxq`@",
		Points: []Point{
			{Latitude: 38.0675, Longitude: -120.5436}, // Angels Camp
			{Latitude: 38.1391, Longitude: -120.4561}, // Murphys
		},
	}

	distance, err := geoUtils.PointToPolyline(testPoint, routePolyline)
	require.NoError(t, err)
	assert.Greater(t, distance, 0.0, "Distance should be positive")
	assert.Less(t, distance, 50000.0, "Distance should be reasonable (< 50km)")

	// Test point directly on route (should be very close to 0)
	onRoutePoint := Point{Latitude: 38.0675, Longitude: -120.5436}
	distance, err = geoUtils.PointToPolyline(onRoutePoint, routePolyline)
	require.NoError(t, err)
	assert.Less(t, distance, 100.0, "Point on route should be < 100m from polyline")
}

func TestGeoUtils_PolylinesOverlap(t *testing.T) {
	geoUtils := NewGeoUtils()

	// Test overlapping polylines (road closure on route)
	routePolyline := Polyline{
		EncodedPolyline: "_p~iF~ps|U_ulLnnqC_mqNvxq`@",
		Points: []Point{
			{Latitude: 38.0675, Longitude: -120.5436}, // Angels Camp
			{Latitude: 38.1391, Longitude: -120.4561}, // Murphys
		},
	}

	// Closure polyline that overlaps with route
	closurePolyline := Polyline{
		EncodedPolyline: "overlap_test_polyline",
		Points: []Point{
			{Latitude: 38.1000, Longitude: -120.5100}, // Overlapping section
			{Latitude: 38.1200, Longitude: -120.4800}, // Overlapping section
		},
	}

	thresholdMeters := 50.0
	overlaps, segments, err := geoUtils.PolylinesOverlap(routePolyline, closurePolyline, thresholdMeters)
	require.NoError(t, err)

	// This should be determined by the actual geometric overlap
	assert.IsType(t, bool(false), overlaps)
	assert.IsType(t, []OverlapSegment{}, segments)
}

func TestGeoUtils_PolylineOverlapPercentage(t *testing.T) {
	geoUtils := NewGeoUtils()

	routePolyline := Polyline{
		EncodedPolyline: "_p~iF~ps|U_ulLnnqC_mqNvxq`@",
		Points: []Point{
			{Latitude: 38.0675, Longitude: -120.5436},
			{Latitude: 38.1391, Longitude: -120.4561},
		},
	}

	closurePolyline := Polyline{
		Points: []Point{
			{Latitude: 38.1000, Longitude: -120.5100},
			{Latitude: 38.1200, Longitude: -120.4800},
		},
	}

	thresholdMeters := 50.0
	percentage, err := geoUtils.PolylineOverlapPercentage(routePolyline, closurePolyline, thresholdMeters)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, percentage, 0.0, "Percentage should be >= 0")
	assert.LessOrEqual(t, percentage, 100.0, "Percentage should be <= 100")
}

func TestGeoUtils_DecodePolyline(t *testing.T) {
	geoUtils := NewGeoUtils()

	// Test valid Google polyline encoding
	encodedPolyline := "_p~iF~ps|U_ulLnnqC_mqNvxq`@"

	points, err := geoUtils.DecodePolyline(encodedPolyline)
	require.NoError(t, err)
	assert.Greater(t, len(points), 0, "Should decode to at least one point")

	// Validate decoded points have reasonable coordinates
	for _, point := range points {
		assert.GreaterOrEqual(t, point.Latitude, -90.0)
		assert.LessOrEqual(t, point.Latitude, 90.0)
		assert.GreaterOrEqual(t, point.Longitude, -180.0)
		assert.LessOrEqual(t, point.Longitude, 180.0)
	}

	// Test invalid polyline
	_, err = geoUtils.DecodePolyline("invalid_polyline_data")
	assert.Error(t, err, "Should return error for invalid polyline")
}

func TestGeoUtils_ClosestPointOnPolyline(t *testing.T) {
	geoUtils := NewGeoUtils()

	testPoint := Point{Latitude: 38.1000, Longitude: -120.5000}
	routePolyline := Polyline{
		Points: []Point{
			{Latitude: 38.0675, Longitude: -120.5436},
			{Latitude: 38.1391, Longitude: -120.4561},
		},
	}

	closestPoint, err := geoUtils.ClosestPointOnPolyline(testPoint, routePolyline)
	require.NoError(t, err)

	// Closest point should have valid coordinates
	assert.GreaterOrEqual(t, closestPoint.Latitude, -90.0)
	assert.LessOrEqual(t, closestPoint.Latitude, 90.0)
	assert.GreaterOrEqual(t, closestPoint.Longitude, -180.0)
	assert.LessOrEqual(t, closestPoint.Longitude, 180.0)
}

// Test edge cases and validation
func TestGeoUtils_EdgeCases(t *testing.T) {
	geoUtils := NewGeoUtils()

	// Test empty polyline
	emptyPolyline := Polyline{Points: []Point{}}
	testPoint := Point{Latitude: 38.0675, Longitude: -120.5436}

	_, err := geoUtils.PointToPolyline(testPoint, emptyPolyline)
	assert.Error(t, err, "Should return error for empty polyline")

	// Test same point (distance should be 0)
	distance, err := geoUtils.PointToPoint(testPoint, testPoint)
	require.NoError(t, err)
	assert.Equal(t, 0.0, distance, "Distance from point to itself should be 0")
}

// Test polyline overlap detection with different waypoint frequencies
func TestGeoUtils_PolylineOverlapWithDifferentFrequencies(t *testing.T) {
	geoUtils := NewGeoUtils()

	// Simulate Google Routes polyline with high frequency waypoints (every ~100m)
	googleRoute := Polyline{Points: []Point{
		{Latitude: 38.0675, Longitude: -120.5436}, // Angels Camp
		{Latitude: 38.0725, Longitude: -120.5386}, // 100m intervals
		{Latitude: 38.0775, Longitude: -120.5336},
		{Latitude: 38.0825, Longitude: -120.5286},
		{Latitude: 38.0875, Longitude: -120.5236},
		{Latitude: 38.0925, Longitude: -120.5186},
		{Latitude: 38.0975, Longitude: -120.5136},
		{Latitude: 38.1025, Longitude: -120.5086},
		{Latitude: 38.1075, Longitude: -120.5036},
		{Latitude: 38.1125, Longitude: -120.4986},
		{Latitude: 38.1175, Longitude: -120.4936},
		{Latitude: 38.1225, Longitude: -120.4886},
		{Latitude: 38.1275, Longitude: -120.4836},
		{Latitude: 38.1325, Longitude: -120.4786},
		{Latitude: 38.1391, Longitude: -120.4561}, // Murphys
	}}

	// Simulate Caltrans closure with low frequency waypoints (every ~500m)
	caltransClosure := Polyline{Points: []Point{
		{Latitude: 38.0700, Longitude: -120.5400}, // Slightly offset from route
		{Latitude: 38.0950, Longitude: -120.5150}, // Large segments
		{Latitude: 38.1200, Longitude: -120.4900},
		{Latitude: 38.1350, Longitude: -120.4750},
	}}

	// Test: Should detect overlap despite different waypoint frequencies
	hasOverlap, segments, err := geoUtils.PolylinesOverlap(googleRoute, caltransClosure, 100.0)
	require.NoError(t, err)
	assert.True(t, hasOverlap, "Should detect overlap between polylines with different waypoint frequencies")
	assert.Greater(t, len(segments), 0, "Should find overlap segments")

	// Test: Parallel roads that are too far apart
	distantRoute := Polyline{Points: []Point{
		{Latitude: 38.0675, Longitude: -120.4000}, // 1km+ away
		{Latitude: 38.1391, Longitude: -120.3500},
	}}

	hasOverlap, _, err = geoUtils.PolylinesOverlap(googleRoute, distantRoute, 100.0)
	require.NoError(t, err)
	assert.False(t, hasOverlap, "Should not detect overlap for distant parallel routes")
}

// Test overlap percentage calculation accuracy
func TestGeoUtils_PolylineOverlapPercentageAccuracy(t *testing.T) {
	geoUtils := NewGeoUtils()

	// Route from Angels Camp to Murphys
	mainRoute := Polyline{Points: []Point{
		{Latitude: 38.0675, Longitude: -120.5436},
		{Latitude: 38.1391, Longitude: -120.4561},
	}}

	// Closure covering exactly half the route
	halfClosure := Polyline{Points: []Point{
		{Latitude: 38.0675, Longitude: -120.5436}, // Same start
		{Latitude: 38.1033, Longitude: -120.4999}, // Midpoint
	}}

	percentage, err := geoUtils.PolylineOverlapPercentage(mainRoute, halfClosure, 50.0)
	require.NoError(t, err)

	// Should be approximately 50% (allowing for some calculation variance)
	assert.InDelta(t, 50.0, percentage, 10.0, "Half-closure should be approximately 50% overlap")

	// Test: No overlap case
	noOverlapRoute := Polyline{Points: []Point{
		{Latitude: 39.0000, Longitude: -121.0000}, // Completely different area
		{Latitude: 39.1000, Longitude: -121.1000},
	}}

	percentage, err = geoUtils.PolylineOverlapPercentage(mainRoute, noOverlapRoute, 50.0)
	require.NoError(t, err)
	assert.Equal(t, 0.0, percentage, "No overlap should return 0%")
}
