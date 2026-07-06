package routing

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/dpup/sierra-data/internal/lib/geo"
)

// routeMatcher implements the RouteMatcher interface
type routeMatcher struct {
	geoUtils         geo.GeoUtils
	routeCache       map[string]Route
	cacheMutex       sync.RWMutex
	onRouteThreshold float64 // Distance in meters for ON_ROUTE classification
}

// NewRouteMatcher creates a new RouteMatcher implementation
func NewRouteMatcher() RouteMatcher {
	return &routeMatcher{
		geoUtils:         geo.NewGeoUtils(),
		routeCache:       make(map[string]Route),
		onRouteThreshold: 100.0, // 100 meters default threshold for ON_ROUTE
	}
}

// ClassifyAlert classifies a single alert against all provided routes
func (r *routeMatcher) ClassifyAlert(ctx context.Context, alert UnclassifiedAlert, routes []Route) (ClassifiedAlert, error) {
	if len(routes) == 0 {
		// No routes to classify against - everything is DISTANT
		return ClassifiedAlert{
			UnclassifiedAlert: alert,
			Classification:    Distant,
			RouteIDs:          []string{},
			DistanceToRoute:   999999, // Very large distance
		}, nil
	}

	minDistance := float64(999999)
	var matchingRouteIDs []string
	classification := Distant

	// Check alert against each route
	for _, route := range routes {
		distance, matches, err := r.classifyAlertAgainstRoute(alert, route)
		if err != nil {
			return ClassifiedAlert{}, err
		}

		if matches {
			matchingRouteIDs = append(matchingRouteIDs, route.ID)
		}

		if distance < minDistance {
			minDistance = distance
		}

		// Determine classification based on distance and threshold
		if distance <= r.onRouteThreshold {
			classification = OnRoute
		} else if distance <= route.MaxDistance && classification != OnRoute {
			classification = Nearby
		}
	}

	// If no routes matched, it's distant
	if len(matchingRouteIDs) == 0 {
		classification = Distant
	}

	return ClassifiedAlert{
		UnclassifiedAlert: alert,
		Classification:    classification,
		RouteIDs:          matchingRouteIDs,
		DistanceToRoute:   minDistance,
	}, nil
}

// classifyAlertAgainstRoute determines if an alert matches a specific route
func (r *routeMatcher) classifyAlertAgainstRoute(alert UnclassifiedAlert, route Route) (distance float64, matches bool, err error) {
	// Validate route has valid geometry
	if len(route.Polyline.Points) < 2 {
		return 0, false, errors.New("route must have at least 2 points")
	}

	// Handle different alert types: lane closures may have LineString polylines, incidents are points
	if alert.AffectedPolyline != nil && len(alert.AffectedPolyline.Points) > 1 {
		// Polyline-based classification for lane closures with LineString geometry
		return r.classifyPolylineBasedAlertSimple(alert, route)
	} else {
		// Point-based classification for incidents and single-point closures
		return r.classifyPointBasedAlert(alert, route)
	}
}

// classifyPointBasedAlert handles alerts with single point locations
func (r *routeMatcher) classifyPointBasedAlert(alert UnclassifiedAlert, route Route) (distance float64, matches bool, err error) {
	// Calculate minimum distance from alert point to route polyline
	distance, err = r.geoUtils.PointToPolyline(alert.Location, route.Polyline)
	if err != nil {
		return 0, false, err
	}

	// Determine if it matches based on route's distance threshold
	matches = distance <= route.MaxDistance

	return distance, matches, nil
}

// classifyPolylineBasedAlertSimple handles lane closures with LineString geometry using simple approach
// Instead of complex overlap detection, check if any point along the Caltrans polyline is within threshold distance
func (r *routeMatcher) classifyPolylineBasedAlertSimple(alert UnclassifiedAlert, route Route) (distance float64, matches bool, err error) {
	// Find minimum distance by checking each point in the Caltrans polyline against the Google route
	minDistance := float64(999999)

	for _, alertCoord := range alert.AffectedPolyline.Points {
		// Convert API coordinate to geo.Point
		alertPoint := geo.Point{
			Latitude:  alertCoord.Latitude,
			Longitude: alertCoord.Longitude,
		}

		// Calculate distance from this Caltrans point to the Google route polyline
		dist, err := r.geoUtils.PointToPolyline(alertPoint, route.Polyline)
		if err != nil {
			continue // Skip invalid points
		}

		if dist < minDistance {
			minDistance = dist
		}
	}

	// If no valid points found, return error
	if minDistance == 999999 {
		return 0, false, errors.New("no valid points found in alert polyline")
	}

	// Determine if it matches based on route's distance threshold
	matches = minDistance <= route.MaxDistance

	return minDistance, matches, nil
}

// GetRouteAlerts returns alerts for a specific route, prioritizing ON_ROUTE alerts
func (r *routeMatcher) GetRouteAlerts(ctx context.Context, routeID string, alerts []ClassifiedAlert) ([]ClassifiedAlert, error) {
	var routeAlerts []ClassifiedAlert

	// Filter alerts that affect the specified route
	for _, alert := range alerts {
		for _, affectedRouteID := range alert.RouteIDs {
			if affectedRouteID == routeID {
				routeAlerts = append(routeAlerts, alert)
				break
			}
		}
	}

	// Sort alerts: ON_ROUTE first, then NEARBY, by severity and distance
	sort.Slice(routeAlerts, func(i, j int) bool {
		alertI := routeAlerts[i]
		alertJ := routeAlerts[j]

		// First priority: ON_ROUTE alerts come first
		if alertI.Classification != alertJ.Classification {
			if alertI.Classification == OnRoute {
				return true
			}
			if alertJ.Classification == OnRoute {
				return false
			}
		}

		// Second priority: Sort by distance (closer first)
		if alertI.DistanceToRoute != alertJ.DistanceToRoute {
			return alertI.DistanceToRoute < alertJ.DistanceToRoute
		}

		// Third priority: Sort by alert type (closures first, then incidents)
		typeOrder := map[string]int{
			"closure":      1,
			"construction": 2,
			"incident":     3,
			"weather":      4,
		}

		orderI := typeOrder[alertI.Type]
		orderJ := typeOrder[alertJ.Type]
		if orderI == 0 {
			orderI = 5 // Unknown type
		}
		if orderJ == 0 {
			orderJ = 5 // Unknown type
		}

		return orderI < orderJ
	})

	return routeAlerts, nil
}

// UpdateRouteGeometry updates the geometry of a cached route
func (r *routeMatcher) UpdateRouteGeometry(ctx context.Context, routeID string, newPolyline geo.Polyline) error {
	r.cacheMutex.Lock()
	defer r.cacheMutex.Unlock()

	// Validate the new polyline
	if len(newPolyline.Points) < 2 {
		return errors.New("polyline must have at least 2 points")
	}

	// Check if route exists in cache
	if route, exists := r.routeCache[routeID]; exists {
		// Update the route's polyline
		route.Polyline = newPolyline
		r.routeCache[routeID] = route
	} else {
		// Create a new route entry (this might not be typical, but handles the case)
		newRoute := Route{
			ID:          routeID,
			Polyline:    newPolyline,
			MaxDistance: 5000, // Default 5 kilometers
		}
		r.routeCache[routeID] = newRoute
	}

	return nil
}

// Additional helper methods

// SetOnRouteThreshold allows configuration of the ON_ROUTE distance threshold
func (r *routeMatcher) SetOnRouteThreshold(thresholdMeters float64) {
	r.onRouteThreshold = thresholdMeters
}

// GetOnRouteThreshold returns the current ON_ROUTE threshold
func (r *routeMatcher) GetOnRouteThreshold() float64 {
	return r.onRouteThreshold
}

// CacheRoute stores a route in the internal cache for geometry updates
func (r *routeMatcher) CacheRoute(route Route) {
	r.cacheMutex.Lock()
	defer r.cacheMutex.Unlock()
	r.routeCache[route.ID] = route
}

// GetCachedRoute retrieves a route from the internal cache
func (r *routeMatcher) GetCachedRoute(routeID string) (Route, bool) {
	r.cacheMutex.RLock()
	defer r.cacheMutex.RUnlock()
	route, exists := r.routeCache[routeID]
	return route, exists
}

// ClassifyAlerts processes multiple alerts at once for efficiency
func (r *routeMatcher) ClassifyAlerts(ctx context.Context, alerts []UnclassifiedAlert, routes []Route) ([]ClassifiedAlert, error) {
	var classifiedAlerts []ClassifiedAlert

	for _, alert := range alerts {
		classified, err := r.ClassifyAlert(ctx, alert, routes)
		if err != nil {
			return nil, err
		}
		classifiedAlerts = append(classifiedAlerts, classified)
	}

	return classifiedAlerts, nil
}

// GetRoutesWithinDistance returns routes that have alerts within specified distance
func (r *routeMatcher) GetRoutesWithinDistance(ctx context.Context, point geo.Point, routes []Route, maxDistance float64) ([]Route, error) {
	var matchingRoutes []Route

	for _, route := range routes {
		distance, err := r.geoUtils.PointToPolyline(point, route.Polyline)
		if err != nil {
			continue
		}

		if distance <= maxDistance {
			matchingRoutes = append(matchingRoutes, route)
		}
	}

	return matchingRoutes, nil
}
