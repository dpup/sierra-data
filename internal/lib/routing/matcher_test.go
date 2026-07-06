package routing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/lib/geo"
)

// Contract tests for route-matcher library
// These tests define the interface before implementation exists
// They MUST FAIL initially to satisfy TDD RED-GREEN-Refactor cycle

func TestRouteMatcher_ClassifyAlert(t *testing.T) {
	matcher := NewRouteMatcher()
	ctx := context.Background()

	// Define Highway 4 test route
	hwy4Route := Route{
		ID:          "hwy4-angels-murphys",
		Name:        "Hwy 4",
		Section:     "Angels Camp to Murphys",
		Origin:      geo.Point{Latitude: 38.0675, Longitude: -120.5436},
		Destination: geo.Point{Latitude: 38.1391, Longitude: -120.4561},
		Polyline: geo.Polyline{
			EncodedPolyline: "_p~iF~ps|U_ulLnnqC_mqNvxq`@",
			Points: []geo.Point{
				{Latitude: 38.0675, Longitude: -120.5436}, // Angels Camp
				{Latitude: 38.1391, Longitude: -120.4561}, // Murphys
			},
		},
		MaxDistance: 16093.4, // 10 miles in meters
	}

	routes := []Route{hwy4Route}

	// Test ON_ROUTE classification (point very close to route)
	onRouteAlert := UnclassifiedAlert{
		ID:          "test-001",
		Location:    geo.Point{Latitude: 38.0675, Longitude: -120.5436}, // At Angels Camp
		Description: "Lane closure on Highway 4",
		Type:        "closure",
	}

	classified, err := matcher.ClassifyAlert(ctx, onRouteAlert, routes)
	require.NoError(t, err)
	assert.Equal(t, OnRoute, classified.Classification)
	assert.Contains(t, classified.RouteIDs, "hwy4-angels-murphys")
	assert.Less(t, classified.DistanceToRoute, 100.0, "ON_ROUTE should be < 100m from route")

	// Test NEARBY classification (within threshold but not on route)
	nearbyAlert := UnclassifiedAlert{
		ID:          "test-002",
		Location:    geo.Point{Latitude: 38.0800, Longitude: -120.5200}, // ~2 miles from route
		Description: "Incident on side road near Angels Camp",
		Type:        "incident",
	}

	classified, err = matcher.ClassifyAlert(ctx, nearbyAlert, routes)
	require.NoError(t, err)
	assert.Equal(t, Nearby, classified.Classification)
	assert.Contains(t, classified.RouteIDs, "hwy4-angels-murphys")
	assert.Greater(t, classified.DistanceToRoute, 100.0, "NEARBY should be > 100m from route")
	assert.Less(t, classified.DistanceToRoute, 16093.4, "NEARBY should be < 10 miles from route")

	// Test DISTANT classification (beyond threshold)
	distantAlert := UnclassifiedAlert{
		ID:          "test-003",
		Location:    geo.Point{Latitude: 37.5000, Longitude: -121.0000}, // Far from route
		Description: "Incident far from Highway 4",
		Type:        "incident",
	}

	classified, err = matcher.ClassifyAlert(ctx, distantAlert, routes)
	require.NoError(t, err)
	assert.Equal(t, Distant, classified.Classification)
	assert.Empty(t, classified.RouteIDs, "DISTANT should not be associated with any routes")
	assert.Greater(t, classified.DistanceToRoute, 16093.4, "DISTANT should be > 10 miles from route")
}

func TestRouteMatcher_PolylineBasedClassification(t *testing.T) {
	matcher := NewRouteMatcher()
	ctx := context.Background()

	// Test route
	hwy4Route := Route{
		ID:   "hwy4-angels-murphys",
		Name: "Hwy 4",
		Polyline: geo.Polyline{
			Points: []geo.Point{
				{Latitude: 38.0675, Longitude: -120.5436}, // Angels Camp
				{Latitude: 38.1391, Longitude: -120.4561}, // Murphys
			},
		},
		MaxDistance: 16093.4,
	}

	routes := []Route{hwy4Route}

	// Test closure with polyline that overlaps route (> 10% overlap = ON_ROUTE)
	closureAlert := UnclassifiedAlert{
		ID:          "test-closure-001",
		Type:        "closure",
		Description: "Lane closure on Highway 4 between Angels Camp and Murphys",
		AffectedPolyline: &geo.Polyline{
			Points: []geo.Point{
				{Latitude: 38.0800, Longitude: -120.5300}, // Overlapping section
				{Latitude: 38.1200, Longitude: -120.4700}, // Overlapping section
			},
		},
	}

	classified, err := matcher.ClassifyAlert(ctx, closureAlert, routes)
	require.NoError(t, err)

	// Should be classified based on polyline overlap percentage
	assert.NotEqual(t, Distant, classified.Classification, "Overlapping closure should not be DISTANT")
}

func TestRouteMatcher_MultiRouteIncident(t *testing.T) {
	matcher := NewRouteMatcher()
	ctx := context.Background()

	// Define two intersecting routes
	hwy4Route := Route{
		ID:   "hwy4-angels-murphys",
		Name: "Hwy 4",
		Polyline: geo.Polyline{
			Points: []geo.Point{
				{Latitude: 38.0675, Longitude: -120.5436},
				{Latitude: 38.1391, Longitude: -120.4561},
			},
		},
		MaxDistance: 16093.4,
	}

	hwy49Route := Route{
		ID:   "hwy49-angels-camp",
		Name: "Hwy 49",
		Polyline: geo.Polyline{
			Points: []geo.Point{
				{Latitude: 38.0675, Longitude: -120.5436}, // Same start as Hwy 4 (Angels Camp)
				{Latitude: 38.0500, Longitude: -120.5600},
			},
		},
		MaxDistance: 16093.4,
	}

	routes := []Route{hwy4Route, hwy49Route}

	// Incident at intersection of both routes
	intersectionAlert := UnclassifiedAlert{
		ID:          "test-multi-001",
		Location:    geo.Point{Latitude: 38.0675, Longitude: -120.5436}, // Angels Camp intersection
		Description: "Multi-vehicle accident at intersection",
		Type:        "incident",
	}

	classified, err := matcher.ClassifyAlert(ctx, intersectionAlert, routes)
	require.NoError(t, err)

	// Should be ON_ROUTE for both routes
	assert.Equal(t, OnRoute, classified.Classification)
	assert.Len(t, classified.RouteIDs, 2, "Should affect both intersecting routes")
	assert.Contains(t, classified.RouteIDs, "hwy4-angels-murphys")
	assert.Contains(t, classified.RouteIDs, "hwy49-angels-camp")
}

func TestRouteMatcher_GetRouteAlerts(t *testing.T) {
	matcher := NewRouteMatcher()
	ctx := context.Background()

	// Create classified alerts
	alerts := []ClassifiedAlert{
		{
			UnclassifiedAlert: UnclassifiedAlert{ID: "alert-001", Type: "closure"},
			Classification:    OnRoute,
			RouteIDs:          []string{"hwy4-angels-murphys"},
		},
		{
			UnclassifiedAlert: UnclassifiedAlert{ID: "alert-002", Type: "incident"},
			Classification:    Nearby,
			RouteIDs:          []string{"hwy4-angels-murphys"},
		},
		{
			UnclassifiedAlert: UnclassifiedAlert{ID: "alert-003", Type: "incident"},
			Classification:    OnRoute,
			RouteIDs:          []string{"hwy49-angels-camp"},
		},
		{
			UnclassifiedAlert: UnclassifiedAlert{ID: "alert-004", Type: "incident"},
			Classification:    Distant,
			RouteIDs:          []string{}, // No routes
		},
	}

	// Get alerts for specific route
	routeAlerts, err := matcher.GetRouteAlerts(ctx, "hwy4-angels-murphys", alerts)
	require.NoError(t, err)

	assert.Len(t, routeAlerts, 2, "Should return 2 alerts for hwy4-angels-murphys")

	// Verify ON_ROUTE alerts come first (prioritization)
	assert.Equal(t, OnRoute, routeAlerts[0].Classification, "ON_ROUTE alerts should be prioritized")
	assert.Equal(t, "alert-001", routeAlerts[0].ID)
}

func TestRouteMatcher_UpdateRouteGeometry(t *testing.T) {
	matcher := NewRouteMatcher()
	ctx := context.Background()

	routeID := "hwy4-angels-murphys"
	newPolyline := geo.Polyline{
		EncodedPolyline: "updated_polyline_encoding",
		Points: []geo.Point{
			{Latitude: 38.0675, Longitude: -120.5436},
			{Latitude: 38.1000, Longitude: -120.5000}, // Different intermediate point
			{Latitude: 38.1391, Longitude: -120.4561},
		},
	}

	err := matcher.UpdateRouteGeometry(ctx, routeID, newPolyline)
	assert.NoError(t, err, "Should successfully update route geometry")
}

func TestRouteMatcher_ConfigurableThresholds(t *testing.T) {
	matcher := NewRouteMatcher()
	ctx := context.Background()

	// Test route with different distance threshold
	customRoute := Route{
		ID:   "test-route",
		Name: "Test Route",
		Polyline: geo.Polyline{
			Points: []geo.Point{
				{Latitude: 38.0000, Longitude: -120.0000},
				{Latitude: 38.0100, Longitude: -120.0100},
			},
		},
		MaxDistance: 8046.7, // 5 miles instead of default 10 miles
	}

	routes := []Route{customRoute}

	// Alert that would be NEARBY at 10 miles but DISTANT at 5 miles
	alert := UnclassifiedAlert{
		ID:          "test-threshold",
		Location:    geo.Point{Latitude: 38.1000, Longitude: -120.1000}, // Further away, ~10+ miles
		Description: "Test threshold configuration",
		Type:        "incident",
	}

	classified, err := matcher.ClassifyAlert(ctx, alert, routes)
	require.NoError(t, err)

	// Should be DISTANT due to 5-mile threshold
	assert.Equal(t, Distant, classified.Classification, "Should respect custom threshold")
}

func TestRouteMatcher_ErrorHandling(t *testing.T) {
	matcher := NewRouteMatcher()
	ctx := context.Background()

	// Test with empty routes slice
	alert := UnclassifiedAlert{
		ID:          "test-error",
		Location:    geo.Point{Latitude: 38.0000, Longitude: -120.0000},
		Description: "Test error handling",
		Type:        "incident",
	}

	classified, err := matcher.ClassifyAlert(ctx, alert, []Route{})
	require.NoError(t, err)
	assert.Equal(t, Distant, classified.Classification, "Should classify as DISTANT when no routes")

	// Test with invalid route geometry
	invalidRoute := Route{
		ID:   "invalid-route",
		Name: "Invalid Route",
		Polyline: geo.Polyline{
			Points: []geo.Point{}, // Empty points slice
		},
		MaxDistance: 16093.4,
	}

	_, err = matcher.ClassifyAlert(ctx, alert, []Route{invalidRoute})
	assert.Error(t, err, "Should return error for invalid route geometry")
}

// Performance test
func BenchmarkRouteMatcher_ClassifyAlert(b *testing.B) {
	matcher := NewRouteMatcher()
	ctx := context.Background()

	route := Route{
		ID:   "benchmark-route",
		Name: "Benchmark Route",
		Polyline: geo.Polyline{
			Points: []geo.Point{
				{Latitude: 38.0675, Longitude: -120.5436},
				{Latitude: 38.1391, Longitude: -120.4561},
			},
		},
		MaxDistance: 16093.4,
	}

	routes := []Route{route}
	alert := UnclassifiedAlert{
		ID:          "benchmark-alert",
		Location:    geo.Point{Latitude: 38.1000, Longitude: -120.5000},
		Description: "Benchmark test alert",
		Type:        "incident",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = matcher.ClassifyAlert(ctx, alert, routes)
	}
}
