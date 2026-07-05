package routing

import (
	"context"

	"github.com/dpup/info.ersn.net/server/internal/lib/geo"
)

// AlertClassification represents the relationship between an alert and monitored routes
type AlertClassification string

const (
	OnRoute AlertClassification = "on_route" // < 100m from polyline
	Nearby  AlertClassification = "nearby"   // < configured threshold
	Distant AlertClassification = "distant"  // > threshold (filtered out)
)

// Route represents a monitored route segment with geometry for precise alert matching
type Route struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Section     string       `json:"section"`
	Origin      geo.Point    `json:"origin"`
	Destination geo.Point    `json:"destination"`
	Polyline    geo.Polyline `json:"polyline"`
	MaxDistance float64      `json:"max_distance"` // Distance threshold for "nearby" classification (meters)
}

// UnclassifiedAlert represents an alert before route classification
type UnclassifiedAlert struct {
	ID               string        `json:"id"`
	Title            string        `json:"title"` // Original Caltrans title (e.g., "CHP Incident 250911GG0206")
	Location         geo.Point     `json:"location"`
	Description      string        `json:"description"`
	Type             string        `json:"type"`
	StyleUrl         string        `json:"style_url,omitempty"`         // KML style indicating closure type
	AffectedPolyline *geo.Polyline `json:"affected_polyline,omitempty"` // For closures/construction
}

// ClassifiedAlert represents an alert after route classification
type ClassifiedAlert struct {
	UnclassifiedAlert
	Classification  AlertClassification `json:"classification"`
	RouteIDs        []string            `json:"route_ids"`
	DistanceToRoute float64             `json:"distance_to_route"`
}

// RouteMatcher interface defines alert classification against route geometry
type RouteMatcher interface {
	// Classify single alert against all routes
	ClassifyAlert(ctx context.Context, alert UnclassifiedAlert, routes []Route) (ClassifiedAlert, error)

	// Get alerts for specific route
	GetRouteAlerts(ctx context.Context, routeID string, alerts []ClassifiedAlert) ([]ClassifiedAlert, error)

	// Update route geometry when Google Routes data refreshes
	UpdateRouteGeometry(ctx context.Context, routeID string, newPolyline geo.Polyline) error
}

// NewRouteMatcher is implemented in matcher.go
