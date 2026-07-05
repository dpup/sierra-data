package services

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dpup/prefab/logging"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/cache"
	"github.com/dpup/info.ersn.net/server/internal/clients/caltrans"
	"github.com/dpup/info.ersn.net/server/internal/clients/google"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/lib/alerts"
	"github.com/dpup/info.ersn.net/server/internal/lib/geo"
	"github.com/dpup/info.ersn.net/server/internal/lib/routing"
)

// RoadsService implements the gRPC RoadsService
// Implementation per tasks.md T016 and data-model.md Route entity
type RoadsService struct {
	api.UnimplementedRoadsServiceServer
	googleClient   *google.Client
	caltransClient *caltrans.FeedParser
	cache          *cache.Cache
	config         *config.Config
	alertEnhancer  alerts.AlertEnhancer
	routeMatcher   routing.RouteMatcher
	geoUtils       geo.GeoUtils
	contentHasher  *alerts.ContentHasher

	// Per-feed outcome of the most recent incident refresh attempt
	// (chp-only.kml / lcs2way.kml). refreshIncidents keeps serving the
	// surviving feed when only one dies, so this is the only signal that
	// distinguishes an upstream failure from a genuinely empty feed —
	// without it a consumer would read a dead feed as an all-clear.
	// Guarded by incidentFeedMu; see IncidentFeedHealth in incidents.go.
	incidentFeedMu      sync.Mutex
	incidentFeedChpErr  error
	incidentFeedLaneErr error
	incidentFeedAt      time.Time
}

// trafficData holds traffic information for a road
type trafficData struct {
	DurationMins    int32
	DistanceKm      int32
	CongestionLevel string
	DelayMins       int32
}

// googleRouteCache holds cached Google Routes API responses to reduce API usage
type googleRouteCache struct {
	DurationMins    int32     `json:"duration_mins"`
	DistanceKm      int32     `json:"distance_km"`
	CongestionLevel string    `json:"congestion_level"`
	DelayMins       int32     `json:"delay_mins"`
	Polyline        string    `json:"polyline"`
	CachedAt        time.Time `json:"cached_at"`
}

// NewRoadsService creates a new RoadsService
func NewRoadsService(googleClient *google.Client, caltransClient *caltrans.FeedParser, cache *cache.Cache, config *config.Config, alertEnhancer alerts.AlertEnhancer) *RoadsService {
	return &RoadsService{
		googleClient:   googleClient,
		caltransClient: caltransClient,
		cache:          cache,
		config:         config,
		alertEnhancer:  alertEnhancer,
		routeMatcher:   routing.NewRouteMatcher(),
		geoUtils:       geo.NewGeoUtils(),
		contentHasher:  alerts.NewContentHasher(),
	}
}

// ListRoads implements the gRPC method defined in contracts/roads.proto line 12-17
// Returns cached data with timestamp, relying on periodic background refresh to update data
func (s *RoadsService) ListRoads(ctx context.Context, req *api.ListRoadsRequest) (*api.ListRoadsResponse, error) {
	logging.Info(ctx, "ListRoads called")

	// Get cached roads (serve whatever we have, even if stale)
	var cachedRoads []*api.Road
	cacheKey := "roads:all"

	entry, found, err := s.cache.GetWithMetadata(cacheKey, &cachedRoads)
	if err != nil {
		logging.Errorw(ctx, "Cache error", "error", err, "cache_key", cacheKey)
	}

	// If we have cached data (fresh or stale), return it with timestamp
	if found {
		var lastUpdated *timestamppb.Timestamp
		if entry != nil {
			lastUpdated = timestamppb.New(entry.CreatedAt)
		}

		isStale := s.cache.IsStale(cacheKey)
		isVeryStale := s.cache.IsVeryStale(cacheKey)

		var staleness string
		if !isStale {
			staleness = "fresh"
		} else if !isVeryStale {
			staleness = "stale"
		} else {
			staleness = "very_stale"
		}

		logging.Infow(ctx, "Returning cached roads",
			"road_count", len(cachedRoads),
			"staleness", staleness,
			"last_updated", lastUpdated.AsTime().Format(time.RFC3339))

		return &api.ListRoadsResponse{
			Roads:       cachedRoads,
			LastUpdated: lastUpdated,
		}, nil
	}

	// No cached data available - perform synchronous refresh as fallback
	logging.Info(ctx, "No cached data available - performing fallback refresh")
	roads, err := s.refreshRoadData(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh road data and no cached data available: %w", err)
	}

	// Cache the refreshed data
	if err := s.cache.Set(cacheKey, roads, s.config.Roads.RefreshInterval, "roads"); err != nil {
		logging.Errorw(ctx, "Failed to cache roads", "error", err)
	}

	return &api.ListRoadsResponse{
		Roads:       roads,
		LastUpdated: timestamppb.Now(),
	}, nil
}

// GetRoad implements the gRPC method for retrieving a specific road
func (s *RoadsService) GetRoad(ctx context.Context, req *api.GetRoadRequest) (*api.GetRoadResponse, error) {
	logging.Infow(ctx, "GetRoad called", "road_id", req.RoadId)

	// Get all roads (will use cache if available)
	listResp, err := s.ListRoads(ctx, &api.ListRoadsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to get roads: %w", err)
	}

	// Find the requested road
	for _, road := range listResp.Roads {
		if road.Id == req.RoadId {
			return &api.GetRoadResponse{
				Road:        road,
				LastUpdated: listResp.LastUpdated,
			}, nil
		}
	}

	return nil, status.Errorf(codes.NotFound, "road not found: %s", req.RoadId)
}

// GetProcessingMetrics implements the gRPC method for processing metrics.
//
// Metrics collection is not implemented yet. Rather than shipping a misleading
// all-zeros payload that looks like real telemetry, return Unimplemented (HTTP
// 501) so consumers can tell the difference. Replace this with real counters
// (instrument the refresh pipeline) when metrics are needed.
func (s *RoadsService) GetProcessingMetrics(ctx context.Context, req *api.GetProcessingMetricsRequest) (*api.ProcessingMetrics, error) {
	logging.Info(ctx, "GetProcessingMetrics called (not implemented)")
	return nil, status.Error(codes.Unimplemented, "processing metrics are not yet implemented")
}

// refreshRoadData fetches fresh data from all external sources
func (s *RoadsService) refreshRoadData(ctx context.Context) ([]*api.Road, error) {
	// Fetch Caltrans data once for all roads
	laneClosures, _ := s.caltransClient.ParseLaneClosures(ctx)
	chpIncidents, _ := s.caltransClient.ParseCHPIncidents(ctx)
	allIncidents := append(laneClosures, chpIncidents...)

	// Fetch chain control data once for all roads
	chainControls, err := s.caltransClient.ParseChainControlsDetailed(ctx)
	if err != nil {
		logging.Errorw(ctx, "Failed to get chain controls", "error", err)
		chainControls = nil
	}

	// Fetch road conditions from roads.dot.ca.gov for each unique highway
	roadConditionsByHighway := s.fetchRoadConditions(ctx)

	logging.Infow(ctx, "Retrieved Caltrans incidents for all roads",
		"lane_closures", len(laneClosures),
		"chp_incidents", len(chpIncidents),
		"chain_controls", len(chainControls),
		"road_conditions_highways", len(roadConditionsByHighway))

	// Build routes and collect traffic data for all monitored roads
	var allRoutes []routing.Route
	var roadRouteMap = make(map[string]routing.Route) // Map road ID to route
	var trafficDataMap = make(map[string]trafficData) // Map road ID to traffic data

	for _, monitoredRoad := range s.config.Roads.MonitoredRoads {
		// Get traffic data and Google polyline for this road
		durationMins, distanceKm, congestionLevel, delayMins, googlePolyline, err := s.getTrafficDataWithPolyline(ctx, monitoredRoad)
		if err != nil {
			logging.Errorw(ctx, "Failed to get traffic data for route building", "road_id", monitoredRoad.ID, "error", err)
			// Use defaults for missing traffic data
			durationMins = 0
			distanceKm = 0
			congestionLevel = "unknown"
			delayMins = 0
			googlePolyline = "" // Will use fallback polyline
		}

		// Store traffic data for later use
		trafficDataMap[monitoredRoad.ID] = trafficData{
			DurationMins:    durationMins,
			DistanceKm:      distanceKm,
			CongestionLevel: congestionLevel,
			DelayMins:       delayMins,
		}

		route := s.buildRouteFromMonitoredRoad(ctx, monitoredRoad, googlePolyline)
		allRoutes = append(allRoutes, route)
		roadRouteMap[monitoredRoad.ID] = route
	}

	// Process alerts globally across all routes for deduplication
	alertsByRoute, err := s.processGlobalAlerts(ctx, allIncidents, allRoutes)
	if err != nil {
		return nil, fmt.Errorf("failed to process global alerts: %w", err)
	}

	// Build roads with their respective alerts and traffic data
	var roads []*api.Road
	for _, monitoredRoad := range s.config.Roads.MonitoredRoads {
		route := roadRouteMap[monitoredRoad.ID]
		routeAlerts := alertsByRoute[route.ID]
		traffic := trafficDataMap[monitoredRoad.ID]

		// Get road conditions for this road's highway
		hwNum := extractHighwayNumber(monitoredRoad.Name)
		var roadConditions []caltrans.RoadCondition
		if hwNum != "" {
			roadConditions = roadConditionsByHighway[hwNum]
		}

		road, err := s.buildRoadFromRouteAndAlerts(ctx, monitoredRoad, route, routeAlerts, traffic, chainControls, roadConditions)
		if err != nil {
			logging.Errorw(ctx, "Failed to build road", "road_id", monitoredRoad.ID, "error", err)
			continue
		}
		roads = append(roads, road)
	}

	if len(roads) == 0 {
		return nil, fmt.Errorf("no roads could be processed")
	}

	return roads, nil
}

// buildRouteFromMonitoredRoad creates a routing.Route from config with polyline
func (s *RoadsService) buildRouteFromMonitoredRoad(ctx context.Context, monitoredRoad config.MonitoredRoad, googlePolyline string) routing.Route {
	// Create route definition for classification using actual Google polyline if available
	var routePolyline geo.Polyline
	if googlePolyline != "" {
		// Decode Google polyline to get actual route points
		decodedPoints, err := s.geoUtils.DecodePolyline(googlePolyline)
		if err != nil {
			logging.Errorw(ctx, "Failed to decode Google polyline", "road_id", monitoredRoad.ID, "error", err)
			// Fall back to simple 2-point polyline
			routePolyline = geo.Polyline{Points: []geo.Point{
				{Latitude: monitoredRoad.Origin.Latitude, Longitude: monitoredRoad.Origin.Longitude},
				{Latitude: monitoredRoad.Destination.Latitude, Longitude: monitoredRoad.Destination.Longitude},
			}}
		} else {
			routePolyline = geo.Polyline{Points: decodedPoints}
		}
	} else {
		// Use simple 2-point polyline as fallback
		routePolyline = geo.Polyline{Points: []geo.Point{
			{Latitude: monitoredRoad.Origin.Latitude, Longitude: monitoredRoad.Origin.Longitude},
			{Latitude: monitoredRoad.Destination.Latitude, Longitude: monitoredRoad.Destination.Longitude},
		}}
	}

	return routing.Route{
		ID:          monitoredRoad.ID,
		Name:        monitoredRoad.Name,
		Section:     monitoredRoad.Section,
		Origin:      geo.Point{Latitude: monitoredRoad.Origin.Latitude, Longitude: monitoredRoad.Origin.Longitude},
		Destination: geo.Point{Latitude: monitoredRoad.Destination.Latitude, Longitude: monitoredRoad.Destination.Longitude},
		Polyline:    routePolyline,
		MaxDistance: 5000, // Default 5 kilometers
	}
}

// processGlobalAlerts classifies alerts across all routes and applies deduplication
func (s *RoadsService) processGlobalAlerts(ctx context.Context, allIncidents []caltrans.CaltransIncident, allRoutes []routing.Route) (map[string][]routing.ClassifiedAlert, error) {
	// Convert Caltrans incidents to unclassified alerts
	var unclassifiedAlerts []routing.UnclassifiedAlert
	for _, incident := range allIncidents {
		unclassifiedAlert := routing.UnclassifiedAlert{
			ID:          fmt.Sprintf("%s_%d", incident.Name, incident.LastFetched.Unix()),
			Title:       incident.Name,
			Location:    geo.Point{Latitude: incident.Coordinates.Latitude, Longitude: incident.Coordinates.Longitude},
			Description: incident.DescriptionText,
			Type:        s.mapCaltransTypeToString(incident.FeedType),
			StyleUrl:    incident.StyleUrl,
		}

		// Add affected polyline if available
		if incident.AffectedArea != nil {
			geoPolyline := geo.Polyline{Points: make([]geo.Point, len(incident.AffectedArea.Points))}
			for i, point := range incident.AffectedArea.Points {
				geoPolyline.Points[i] = geo.Point{Latitude: point.Latitude, Longitude: point.Longitude}
			}
			unclassifiedAlert.AffectedPolyline = &geoPolyline
		}

		unclassifiedAlerts = append(unclassifiedAlerts, unclassifiedAlert)
	}

	// Classify each alert against all routes to find the best classification
	var globalClassifications []globalAlertClassification

	for _, unclassifiedAlert := range unclassifiedAlerts {
		for _, route := range allRoutes {
			classifiedAlert, err := s.routeMatcher.ClassifyAlert(ctx, unclassifiedAlert, []routing.Route{route})
			if err != nil {
				logging.Errorw(ctx, "Error classifying alert",
					"alert_id", unclassifiedAlert.ID,
					"route_id", route.ID,
					"error", err)
				continue
			}

			// Only include relevant alerts (ON_ROUTE and NEARBY)
			if classifiedAlert.Classification != routing.Distant {
				globalClassifications = append(globalClassifications, globalAlertClassification{
					AlertID:         unclassifiedAlert.ID,
					RouteID:         route.ID,
					ClassifiedAlert: classifiedAlert,
				})
			}
		}
	}

	// Apply deduplication: if an alert is ON_ROUTE for any road, remove it from NEARBY for others
	return s.deduplicateAlerts(ctx, globalClassifications), nil
}

// globalAlertClassification represents an alert's classification for a specific route
type globalAlertClassification struct {
	AlertID         string
	RouteID         string
	ClassifiedAlert routing.ClassifiedAlert
}

// deduplicateAlerts applies the deduplication logic
func (s *RoadsService) deduplicateAlerts(ctx context.Context, classifications []globalAlertClassification) map[string][]routing.ClassifiedAlert {
	// Track which alerts are ON_ROUTE for any road
	onRouteAlerts := make(map[string]bool)
	for _, classification := range classifications {
		if classification.ClassifiedAlert.Classification == routing.OnRoute {
			onRouteAlerts[classification.AlertID] = true
		}
	}

	// Build final alert assignments, filtering out NEARBY alerts that are ON_ROUTE elsewhere
	alertsByRoute := make(map[string][]routing.ClassifiedAlert)

	for _, classification := range classifications {
		alertID := classification.AlertID
		routeID := classification.RouteID

		// If this alert is ON_ROUTE somewhere and this is a NEARBY classification, skip it
		if onRouteAlerts[alertID] && classification.ClassifiedAlert.Classification == routing.Nearby {
			logging.Infow(ctx, "Deduplicating alert: removing NEARBY classification (alert is ON_ROUTE elsewhere)",
				"alert_id", alertID,
				"route_id", routeID,
				"alert_title", classification.ClassifiedAlert.Title)
			continue
		}

		// Add this alert to the route
		alertsByRoute[routeID] = append(alertsByRoute[routeID], classification.ClassifiedAlert)
	}

	return alertsByRoute
}

// buildRoadFromRouteAndAlerts builds a complete road from route info and classified alerts
func (s *RoadsService) buildRoadFromRouteAndAlerts(ctx context.Context, monitoredRoad config.MonitoredRoad, route routing.Route, classifiedAlerts []routing.ClassifiedAlert, traffic trafficData, chainControls []caltrans.ChainControlData, roadConditions []caltrans.RoadCondition) (*api.Road, error) {
	// Use the pre-fetched traffic data
	durationMins := traffic.DurationMins
	distanceKm := traffic.DistanceKm
	congestionLevel := traffic.CongestionLevel
	delayMins := traffic.DelayMins

	// Process the classified alerts for this route
	roadStatus := api.RoadStatus_OPEN
	chainControl := api.ChainControlStatus_NONE
	var statusExplanation string
	var enhancedAlerts []*api.RoadAlert
	var chainControlInfo *api.ChainControlInfo

	for _, classifiedAlert := range classifiedAlerts {
		// Convert to API road alert and get enhanced data
		alert, enhanced, err := s.buildEnhancedRoadAlert(ctx, classifiedAlert, monitoredRoad)
		if err != nil {
			logging.Errorw(ctx, "Error building enhanced alert",
				"alert_title", classifiedAlert.Title,
				"error", err)
			continue
		}

		enhancedAlerts = append(enhancedAlerts, alert)

		// Update road status based on AI analysis (only for ON_ROUTE alerts)
		if classifiedAlert.Classification == routing.OnRoute && enhanced != nil {
			// Use AI-determined road status
			switch enhanced.StructuredDescription.RoadStatus {
			case "closed":
				roadStatus = api.RoadStatus_CLOSED
				if enhanced.StructuredDescription.RestrictionDetails != "" {
					statusExplanation = enhanced.StructuredDescription.RestrictionDetails
				}
			case "restricted":
				if roadStatus != api.RoadStatus_CLOSED { // Don't downgrade from closed
					roadStatus = api.RoadStatus_RESTRICTED
					if statusExplanation == "" { // Keep first/most relevant explanation
						statusExplanation = enhanced.StructuredDescription.RestrictionDetails
					}
				}
			}

			// Update chain control if specified
			switch enhanced.StructuredDescription.ChainStatus {
			case "r1", "r2":
				chainControl = api.ChainControlStatus_REQUIRED
			case "active_unspecified":
				if chainControl == api.ChainControlStatus_NONE { // Don't downgrade from specific R1/R2
					chainControl = api.ChainControlStatus_ADVISED
				}
			}
		}
	}

	// Apply road conditions from roads.dot.ca.gov (closures, chain controls)
	s.applyRoadConditions(ctx, monitoredRoad, roadConditions, &roadStatus, &chainControl, &statusExplanation, &enhancedAlerts)

	// Find chain control info for this route
	chainControlInfo = s.findChainControlForRoute(ctx, route, chainControls)
	if chainControlInfo != nil {
		chainControl = api.ChainControlStatus_REQUIRED
		logging.Infow(ctx, "Chain control found for route",
			"road_id", route.ID,
			"level", chainControlInfo.Level,
			"location", chainControlInfo.LocationName)
	}

	// Convert congestion level to enum
	congestionEnum := s.mapCongestionLevel(congestionLevel)

	return &api.Road{
		Id:                monitoredRoad.ID,
		Name:              monitoredRoad.Name,
		Section:           monitoredRoad.Section,
		Status:            roadStatus,
		StatusExplanation: statusExplanation,
		DurationMinutes:   durationMins,
		DistanceKm:        distanceKm,
		CongestionLevel:   congestionEnum,
		DelayMinutes:      delayMins,
		ChainControl:      chainControl,
		Alerts:            enhancedAlerts,
		ChainControlInfo:  chainControlInfo,
	}, nil
}

// processMonitoredRoad processes a single road with all data sources
func (s *RoadsService) processMonitoredRoad(ctx context.Context, monitoredRoad config.MonitoredRoad) (*api.Road, error) {
	logging.Infow(ctx, "Processing road", "road_id", monitoredRoad.ID, "name", monitoredRoad.Name)

	// Get traffic data and route geometry from Google Routes
	durationMins, distanceKm, congestionLevel, delayMins, googlePolyline, err := s.getTrafficDataWithPolyline(ctx, monitoredRoad)
	if err != nil {
		logging.Errorw(ctx, "Failed to get traffic data", "road_id", monitoredRoad.ID, "error", err)
		// Use defaults for missing traffic data
		durationMins = 0
		distanceKm = 0
		congestionLevel = "unknown"
		delayMins = 0
		googlePolyline = "" // Will fall back to simple 2-point polyline
	}

	// Get Caltrans data for road status and chain control using actual route geometry
	roadStatus, chainControl, alerts, statusExplanation, chainControlInfo, err := s.getCaltransDataWithRouteGeometry(ctx, monitoredRoad, googlePolyline)
	if err != nil {
		logging.Errorw(ctx, "Failed to get Caltrans data", "road_id", monitoredRoad.ID, "error", err)
		// Use defaults
		roadStatus = "open"
		chainControl = "none"
		alerts = nil
		statusExplanation = ""
		chainControlInfo = nil
	}

	// Build road object (internal fields like origin, destination, polylines kept internal)
	road := &api.Road{
		Id:                monitoredRoad.ID,
		Name:              monitoredRoad.Name,
		Section:           monitoredRoad.Section,
		Status:            s.mapRoadStatus(roadStatus),
		DurationMinutes:   durationMins,
		DistanceKm:        distanceKm,
		CongestionLevel:   s.mapCongestionLevel(congestionLevel),
		DelayMinutes:      delayMins,
		ChainControl:      s.mapChainControlStatus(chainControl),
		Alerts:            alerts,
		StatusExplanation: statusExplanation,
		ChainControlInfo:  chainControlInfo,
	}

	return road, nil
}

// getTrafficDataWithPolyline fetches traffic data and route geometry from Google Routes API
// Implements dedicated caching to reduce API calls and stay within 10k monthly limit
func (s *RoadsService) getTrafficDataWithPolyline(ctx context.Context, monitoredRoad config.MonitoredRoad) (int32, int32, string, int32, string, error) {
	if s.config.GoogleRoutes.APIKey == "" {
		return 0, 0, "unknown", 0, "", fmt.Errorf("google Routes API key not configured")
	}

	// Check Google Routes-specific cache first (separate from main road cache)
	googleCacheKey := fmt.Sprintf("google_routes_%s", monitoredRoad.ID)
	var routeCache googleRouteCache
	if found, err := s.cache.Get(googleCacheKey, &routeCache); err == nil && found {
		logging.Infow(ctx, "Using cached Google Routes data", "road_id", monitoredRoad.ID, "cached_at", routeCache.CachedAt)
		return routeCache.DurationMins, routeCache.DistanceKm, routeCache.CongestionLevel, routeCache.DelayMins, routeCache.Polyline, nil
	}

	// Cache miss - call Google Routes API
	logging.Infow(ctx, "Calling Google Routes API", "road_id", monitoredRoad.ID)
	roadData, err := s.googleClient.ComputeRoutes(ctx,
		monitoredRoad.Origin.ToProto(),
		monitoredRoad.Destination.ToProto())
	if err != nil {
		return 0, 0, "unknown", 0, "", fmt.Errorf("failed to compute routes: %w", err)
	}

	// Calculate real delay from Google's traffic-aware vs baseline durations
	delaySeconds := roadData.DurationSeconds - roadData.StaticDurationSeconds
	if delaySeconds < 0 {
		delaySeconds = 0 // Shouldn't happen, but safety check
	}

	// Determine congestion level based on actual delay minutes
	delayMins := int32(delaySeconds / 60)
	congestionLevel := s.classifyCongestionByDelay(delayMins)

	// Convert to user-friendly units
	durationMins := int32(roadData.DurationSeconds / 60)
	distanceKm := int32(roadData.DistanceMeters / 1000)

	// Cache the Google Routes data with longer TTL to reduce API calls
	cache := googleRouteCache{
		DurationMins:    durationMins,
		DistanceKm:      distanceKm,
		CongestionLevel: congestionLevel,
		DelayMins:       delayMins,
		Polyline:        roadData.Polyline,
		CachedAt:        time.Now(),
	}

	// Cache Google Routes data for 45 minutes. With the 15m refresh loop this
	// yields ~1 API call per road every 45 min (~32/day/road, ~3.9k/month for the
	// 4 monitored roads) - comfortably under the Compute Routes Pro free tier of
	// 5,000/month. Traffic data this old is fine for these rural highways.
	if err := s.cache.Set(googleCacheKey, cache, 45*time.Minute, "google_routes"); err != nil {
		logging.Errorw(ctx, "Failed to cache Google Routes data", "error", err, "road_id", monitoredRoad.ID)
	}

	return durationMins, distanceKm, congestionLevel, delayMins, roadData.Polyline, nil
}

// classifyCongestionByDelay determines congestion level based on actual delay minutes
func (s *RoadsService) classifyCongestionByDelay(delayMins int32) string {
	switch {
	case delayMins >= 20:
		return "severe" // 20+ minutes delay
	case delayMins >= 10:
		return "heavy" // 10-19 minutes delay
	case delayMins >= 5:
		return "moderate" // 5-9 minutes delay
	case delayMins >= 2:
		return "light" // 2-4 minutes delay
	default:
		return "clear" // 0-1 minutes delay
	}
}

// mapRoadStatus converts string status to RoadStatus enum
func (s *RoadsService) mapRoadStatus(status string) api.RoadStatus {
	switch status {
	case "open":
		return api.RoadStatus_OPEN
	case "closed":
		return api.RoadStatus_CLOSED
	case "restricted":
		return api.RoadStatus_RESTRICTED
	case "maintenance":
		return api.RoadStatus_MAINTENANCE
	default:
		return api.RoadStatus_ROAD_STATUS_UNSPECIFIED
	}
}

// mapCongestionLevel converts string congestion level to CongestionLevel enum
func (s *RoadsService) mapCongestionLevel(level string) api.CongestionLevel {
	switch level {
	case "clear":
		return api.CongestionLevel_CLEAR
	case "light":
		return api.CongestionLevel_LIGHT
	case "moderate":
		return api.CongestionLevel_MODERATE
	case "heavy":
		return api.CongestionLevel_HEAVY
	case "severe":
		return api.CongestionLevel_SEVERE
	default:
		return api.CongestionLevel_CONGESTION_LEVEL_UNSPECIFIED
	}
}

// mapChainControlStatus converts string chain control to ChainControlStatus enum
func (s *RoadsService) mapChainControlStatus(status string) api.ChainControlStatus {
	switch status {
	case "none":
		return api.ChainControlStatus_NONE
	case "advised":
		return api.ChainControlStatus_ADVISED
	case "required":
		return api.ChainControlStatus_REQUIRED
	case "prohibited":
		return api.ChainControlStatus_PROHIBITED
	default:
		return api.ChainControlStatus_CHAIN_CONTROL_UNSPECIFIED
	}
}

// Removed duplicate analyzeCongestionLevel - now combined above

// getCaltransDataWithRouteGeometry fetches road status, chain control, and alerts using actual route geometry
// Returns: roadStatus, chainControlStatus, alerts, statusExplanation, chainControlInfo, error
func (s *RoadsService) getCaltransDataWithRouteGeometry(ctx context.Context, monitoredRoad config.MonitoredRoad, googlePolyline string) (string, string, []*api.RoadAlert, string, *api.ChainControlInfo, error) {
	// Create route definition for classification using actual Google polyline if available
	var routePolyline geo.Polyline
	if googlePolyline != "" {
		// Decode Google polyline to get actual route points
		decodedPoints, err := s.geoUtils.DecodePolyline(googlePolyline)
		if err != nil {
			logging.Errorw(ctx, "Failed to decode Google polyline", "road_id", monitoredRoad.ID, "error", err)
			// Fall back to simple 2-point polyline
			routePolyline = geo.Polyline{Points: []geo.Point{
				{Latitude: monitoredRoad.Origin.Latitude, Longitude: monitoredRoad.Origin.Longitude},
				{Latitude: monitoredRoad.Destination.Latitude, Longitude: monitoredRoad.Destination.Longitude},
			}}
		} else {
			routePolyline = geo.Polyline{Points: decodedPoints}
		}
	} else {
		// Use simple 2-point polyline as fallback
		routePolyline = geo.Polyline{Points: []geo.Point{
			{Latitude: monitoredRoad.Origin.Latitude, Longitude: monitoredRoad.Origin.Longitude},
			{Latitude: monitoredRoad.Destination.Latitude, Longitude: monitoredRoad.Destination.Longitude},
		}}
	}

	route := routing.Route{
		ID:          monitoredRoad.ID,
		Name:        monitoredRoad.Name,
		Section:     monitoredRoad.Section,
		Origin:      geo.Point{Latitude: monitoredRoad.Origin.Latitude, Longitude: monitoredRoad.Origin.Longitude},
		Destination: geo.Point{Latitude: monitoredRoad.Destination.Latitude, Longitude: monitoredRoad.Destination.Longitude},
		Polyline:    routePolyline,
		MaxDistance: 5000, // Default 5 kilometers
	}

	return s.processCaltransDataWithRoute(ctx, route, monitoredRoad)
}

// processCaltransDataWithRoute handles the actual Caltrans data processing with route information
// Returns: roadStatus, chainControlStatus, alerts, statusExplanation, chainControlInfo, error
func (s *RoadsService) processCaltransDataWithRoute(ctx context.Context, route routing.Route, monitoredRoad config.MonitoredRoad) (string, string, []*api.RoadAlert, string, *api.ChainControlInfo, error) {

	// Get all incidents from Caltrans (no geographic pre-filtering)
	laneClosures, _ := s.caltransClient.ParseLaneClosures(ctx)
	chpIncidents, _ := s.caltransClient.ParseCHPIncidents(ctx)

	// Get chain control data
	chainControls, err := s.caltransClient.ParseChainControlsDetailed(ctx)
	if err != nil {
		logging.Errorw(ctx, "Failed to get chain controls", "error", err)
		chainControls = nil
	}

	logging.Infow(ctx, "Retrieved Caltrans incidents",
		"road_id", route.ID,
		"lane_closures", len(laneClosures),
		"chp_incidents", len(chpIncidents),
		"chain_controls", len(chainControls))

	// Combine all incidents
	allIncidents := append(laneClosures, chpIncidents...)

	// Convert Caltrans incidents to unclassified alerts
	var unclassifiedAlerts []routing.UnclassifiedAlert
	for _, incident := range allIncidents {
		unclassifiedAlert := routing.UnclassifiedAlert{
			ID:          fmt.Sprintf("%s_%d", incident.Name, incident.LastFetched.Unix()),
			Title:       incident.Name, // Use actual Caltrans title (e.g., "CHP Incident 250911GG0206")
			Location:    geo.Point{Latitude: incident.Coordinates.Latitude, Longitude: incident.Coordinates.Longitude},
			Description: incident.DescriptionText,
			Type:        s.mapCaltransTypeToString(incident.FeedType),
			StyleUrl:    incident.StyleUrl,
		}

		// Add affected polyline if available
		if incident.AffectedArea != nil {
			geoPolyline := geo.Polyline{Points: make([]geo.Point, len(incident.AffectedArea.Points))}
			for i, point := range incident.AffectedArea.Points {
				geoPolyline.Points[i] = geo.Point{Latitude: point.Latitude, Longitude: point.Longitude}
			}
			unclassifiedAlert.AffectedPolyline = &geoPolyline
		}

		unclassifiedAlerts = append(unclassifiedAlerts, unclassifiedAlert)
	}

	// Classify alerts using route-aware matching
	var classifiedAlerts []routing.ClassifiedAlert
	for _, unclassifiedAlert := range unclassifiedAlerts {
		classifiedAlert, err := s.routeMatcher.ClassifyAlert(ctx, unclassifiedAlert, []routing.Route{route})
		if err != nil {
			logging.Errorw(ctx, "Error classifying alert",
				"alert_id", unclassifiedAlert.ID,
				"alert_title", unclassifiedAlert.Title,
				"error", err)
			continue
		}

		logging.Infow(ctx, "Classified alert",
			"alert_title", unclassifiedAlert.Title,
			"classification", string(classifiedAlert.Classification),
			"distance_to_route", classifiedAlert.DistanceToRoute,
			"lat", unclassifiedAlert.Location.Latitude,
			"lon", unclassifiedAlert.Location.Longitude)

		classifiedAlerts = append(classifiedAlerts, classifiedAlert)
	}

	logging.Infow(ctx, "Alert classification complete",
		"road_id", route.ID,
		"total_incidents", len(allIncidents),
		"classified_alerts", len(classifiedAlerts))

	// Process classified alerts with AI-enhanced road status determination
	roadStatus := api.RoadStatus_OPEN
	chainControl := api.ChainControlStatus_NONE
	var statusExplanation string
	var enhancedAlerts []*api.RoadAlert

	for _, classifiedAlert := range classifiedAlerts {
		// Only include ON_ROUTE and NEARBY alerts
		if classifiedAlert.Classification == routing.Distant {
			logging.Infow(ctx, "Skipping distant alert",
				"alert_title", classifiedAlert.Title,
				"classification", "DISTANT")
			continue
		}

		// Convert to API road alert and get enhanced data
		alert, enhanced, err := s.buildEnhancedRoadAlert(ctx, classifiedAlert, monitoredRoad)
		if err != nil {
			logging.Errorw(ctx, "Error building enhanced alert",
				"alert_title", classifiedAlert.Title,
				"error", err)
			continue
		}

		enhancedAlerts = append(enhancedAlerts, alert)

		// Update road status based on AI analysis (only for ON_ROUTE alerts)
		if classifiedAlert.Classification == routing.OnRoute && enhanced != nil {
			// Use AI-determined road status
			switch enhanced.StructuredDescription.RoadStatus {
			case "closed":
				roadStatus = api.RoadStatus_CLOSED
				if enhanced.StructuredDescription.RestrictionDetails != "" {
					statusExplanation = enhanced.StructuredDescription.RestrictionDetails
				}
			case "restricted":
				if roadStatus != api.RoadStatus_CLOSED { // Don't downgrade from closed
					roadStatus = api.RoadStatus_RESTRICTED
					if statusExplanation == "" { // Keep first/most relevant explanation
						statusExplanation = enhanced.StructuredDescription.RestrictionDetails
					}
				}
			}

			// Update chain control based on AI analysis
			switch enhanced.StructuredDescription.ChainStatus {
			case "r2":
				chainControl = api.ChainControlStatus_REQUIRED
			case "r1":
				if chainControl != api.ChainControlStatus_REQUIRED {
					chainControl = api.ChainControlStatus_REQUIRED // Both R1 and R2 map to REQUIRED for now
				}
			case "active_unspecified":
				if chainControl == api.ChainControlStatus_NONE {
					chainControl = api.ChainControlStatus_ADVISED
				}
			}
		} else if classifiedAlert.Classification == routing.OnRoute {
			// Fallback logic if AI enhancement failed
			if classifiedAlert.Type == "closure" || classifiedAlert.Type == "construction" {
				if roadStatus == api.RoadStatus_OPEN {
					roadStatus = api.RoadStatus_RESTRICTED
					statusExplanation = "Road construction or lane closure in effect"
				}
			}
		}
	}

	// Find chain control info for this route
	chainControlInfo := s.findChainControlForRoute(ctx, route, chainControls)
	if chainControlInfo != nil {
		chainControl = api.ChainControlStatus_REQUIRED
		logging.Infow(ctx, "Chain control found for route",
			"road_id", route.ID,
			"level", chainControlInfo.Level,
			"location", chainControlInfo.LocationName)
	}

	// Convert status enums back to strings for now (maintain compatibility)
	roadStatusStr := s.roadStatusToString(roadStatus)
	chainControlStr := s.chainControlToString(chainControl)

	return roadStatusStr, chainControlStr, enhancedAlerts, statusExplanation, chainControlInfo, nil
}

// findChainControlForRoute finds the closest chain control point that applies to this route
func (s *RoadsService) findChainControlForRoute(ctx context.Context, route routing.Route, chainControls []caltrans.ChainControlData) *api.ChainControlInfo {
	if len(chainControls) == 0 {
		return nil
	}

	// Extract highway name from route (e.g., "Hwy 4" -> "4", "Highway 89" -> "89")
	routeHighway := extractHighwayNumber(route.Name)
	if routeHighway == "" {
		return nil
	}

	var bestMatch *caltrans.ChainControlData
	bestDistance := float64(10000) // 10km max distance

	for i, cc := range chainControls {
		// Check if this chain control is for the same highway
		ccHighway := extractHighwayNumber(cc.Highway)
		if ccHighway != routeHighway {
			continue
		}

		// Check if chain control point is near the route
		if cc.Coordinates == nil {
			continue
		}

		ccPoint := geo.Point{Latitude: cc.Coordinates.Latitude, Longitude: cc.Coordinates.Longitude}

		// Check distance to route polyline
		for _, routePoint := range route.Polyline.Points {
			distance, err := s.geoUtils.DistanceFromCoords(
				ccPoint.Latitude, ccPoint.Longitude,
				routePoint.Latitude, routePoint.Longitude,
			)
			if err != nil {
				continue
			}

			// If chain control is within 5km of route and closer than current best
			if distance < 5000 && distance < bestDistance {
				bestDistance = distance
				bestMatch = &chainControls[i]
			}
		}
	}

	if bestMatch == nil {
		return nil
	}

	// Convert to API ChainControlInfo
	return &api.ChainControlInfo{
		Level:         s.mapChainControlLevel(bestMatch.Level),
		LocationName:  bestMatch.LocationName,
		Latitude:      bestMatch.Coordinates.Latitude,
		Longitude:     bestMatch.Coordinates.Longitude,
		EffectiveTime: parseRFC3339Timestamp(bestMatch.EffectiveTime),
		Direction:     bestMatch.Direction,
		Description:   bestMatch.Description,
	}
}

// parseRFC3339Timestamp converts an RFC3339 string to a protobuf Timestamp,
// returning nil if the string is empty or unparseable (Caltrans feeds are not
// always well-formed).
func parseRFC3339Timestamp(s string) *timestamppb.Timestamp {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return timestamppb.New(t)
}

// extractHighwayNumber extracts the highway number from various formats
// "Hwy 4" -> "4", "Highway 89" -> "89", "US 50" -> "50", "I-80" -> "80"
func extractHighwayNumber(name string) string {
	patterns := []string{
		`(?i)(?:Hwy|Highway|Route)\s*(\d+)`,
		`(?i)(?:US|I)-?\s*(\d+)`,
		`(?i)^(\d+)$`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		if match := re.FindStringSubmatch(name); len(match) > 1 {
			return match[1]
		}
	}
	return ""
}

// mapChainControlLevel converts string level to ChainControlLevel enum
func (s *RoadsService) mapChainControlLevel(level string) api.ChainControlLevel {
	switch level {
	case "R1":
		return api.ChainControlLevel_CHAIN_CONTROL_LEVEL_R1
	case "R2":
		return api.ChainControlLevel_CHAIN_CONTROL_LEVEL_R2
	case "R3":
		return api.ChainControlLevel_CHAIN_CONTROL_LEVEL_R3
	default:
		return api.ChainControlLevel_CHAIN_CONTROL_LEVEL_UNSPECIFIED
	}
}

// mapCaltransTypeToString converts Caltrans feed type to string
func (s *RoadsService) mapCaltransTypeToString(feedType caltrans.CaltransFeedType) string {
	switch feedType {
	case caltrans.CHAIN_CONTROL:
		return "weather"
	case caltrans.LANE_CLOSURE:
		return "closure"
	case caltrans.CHP_INCIDENT:
		return "incident"
	default:
		return "unknown"
	}
}

// buildEnhancedRoadAlert creates an enhanced API road alert from classified alert
func (s *RoadsService) buildEnhancedRoadAlert(ctx context.Context, classifiedAlert routing.ClassifiedAlert, monitoredRoad config.MonitoredRoad) (*api.RoadAlert, *alerts.EnhancedAlert, error) {
	// Build base alert (polylines kept internal for processing)
	alertType := s.mapStringToAlertType(classifiedAlert.Type)
	alert := &api.RoadAlert{
		Id:                    logNumberFromText(classifiedAlert.Title, classifiedAlert.Description), // Stable id; matches Incident.id
		Type:                  alertType,
		Severity:              api.AlertSeverity_WARNING, // Default, will be updated after AI enhancement
		Classification:        s.mapRoutingToAPIClassification(classifiedAlert.Classification),
		Title:                 classifiedAlert.Title,       // Use real Caltrans title (e.g., "CHP Incident 250911GG0206")
		Description:           classifiedAlert.Description, // Will be enhanced below
		StartTime:             nil,                         // Will be set from AI enhancement or fallback to current time
		EndTime:               nil,
		LastUpdated:           nil, // Will be set from AI enhancement or fallback to current time
		Location:              &api.Coordinates{Latitude: classifiedAlert.Location.Latitude, Longitude: classifiedAlert.Location.Longitude},
		DistanceToRouteMeters: classifiedAlert.DistanceToRoute, // Distance for client rendering
		Metadata:              make(map[string]string),
	}

	var enhancedData *alerts.EnhancedAlert

	// Enhance with AI if available
	if s.alertEnhancer != nil {
		enhanced, err := s.EnhanceAlertWithAI(ctx, classifiedAlert)
		if err != nil {
			logging.Errorw(ctx, "Alert enhancement failed, using original", "error", err)
		} else {
			enhancedData = enhanced
			// Update alert with enhanced data at top level
			alert.Description = enhanced.StructuredDescription.Details
			alert.CondensedSummary = enhanced.CondensedSummary
			alert.LocationDescription = enhanced.StructuredDescription.Location.Description
			alert.Impact = mapAlertImpact(enhanced.StructuredDescription.Impact)

			// Parse time_reported if provided - use for StartTime
			if enhanced.StructuredDescription.TimeReported != "" {
				if timeReported, err := time.Parse(time.RFC3339, enhanced.StructuredDescription.TimeReported); err == nil {
					alert.TimeReported = timestamppb.New(timeReported)
					alert.StartTime = timestamppb.New(timeReported) // Use time_reported as StartTime
				}
			}

			// Parse last_update if provided - use for LastUpdated
			if enhanced.StructuredDescription.LastUpdate != "" {
				if lastUpdate, err := time.Parse(time.RFC3339, enhanced.StructuredDescription.LastUpdate); err == nil {
					alert.LastUpdated = timestamppb.New(lastUpdate)
				}
			}

			// Update severity based on AI-enhanced impact and description
			alert.Severity = s.determineAlertSeverity(
				classifiedAlert.Classification,
				enhanced.StructuredDescription.Impact,
				alertType,
				enhanced.StructuredDescription.Details,
			)

			// Reserve metadata only for AI's additional_info
			for key, value := range enhanced.StructuredDescription.AdditionalInfo {
				alert.Metadata[key] = value
			}
		}
	}

	return alert, enhancedData, nil
}

// EnhanceAlertWithAI uses the alert enhancer to improve alert descriptions with integrated caching
// Made public for testing
func (s *RoadsService) EnhanceAlertWithAI(ctx context.Context, classifiedAlert routing.ClassifiedAlert) (*alerts.EnhancedAlert, error) {
	rawAlert := alerts.RawAlert{
		ID:          classifiedAlert.ID,
		Title:       classifiedAlert.Title,
		Description: classifiedAlert.Description,
		Location:    fmt.Sprintf("%s (%.4f, %.4f)", classifiedAlert.Title, classifiedAlert.Location.Latitude, classifiedAlert.Location.Longitude),
		StyleUrl:    classifiedAlert.StyleUrl,
		Timestamp:   time.Now(),
	}
	enhanced, _, err := s.enhanceRawAlert(ctx, rawAlert, true)
	return enhanced, err
}

// errEnhancementBudget signals that a cache-miss enhancement was skipped
// because the caller's per-refresh OpenAI budget was exhausted.
var errEnhancementBudget = errors.New("enhancement budget exhausted for this refresh")

// enhanceRawAlert runs the content-hash → cache → OpenAI → cache pipeline for
// a raw alert. Shared by per-road alerts and the region-wide incidents feed.
// calledAPI reports whether an OpenAI call was actually made (false on cache
// hits), letting callers budget API usage. When allowAPICall is false a cache
// miss returns errEnhancementBudget instead of calling OpenAI.
func (s *RoadsService) enhanceRawAlert(ctx context.Context, rawAlert alerts.RawAlert, allowAPICall bool) (enhancedAlert *alerts.EnhancedAlert, calledAPI bool, err error) {
	// Generate content hash for cache key
	contentHash := s.contentHasher.HashRawAlert(rawAlert)

	// Check cache first
	var cachedAlert alerts.EnhancedAlert
	key := fmt.Sprintf("enhanced_alert:%s", contentHash)
	if found, err := s.cache.Get(key, &cachedAlert); err == nil && found {
		logging.Infow(ctx, "Cache hit for alert content hash", "hash", contentHash[:8])
		return &cachedAlert, false, nil
	}

	if !allowAPICall {
		return nil, false, errEnhancementBudget
	}

	logging.Infow(ctx, "Cache miss for alert content hash - calling OpenAI", "hash", contentHash[:8])

	// Cache miss - call OpenAI enhancement
	enhanced, err := s.alertEnhancer.EnhanceAlert(ctx, rawAlert)
	if err != nil {
		logging.Errorw(ctx, "OpenAI enhancement failed", "hash", contentHash[:8], "error", err)
		return nil, true, err
	}

	// Cache the result with 24 hour TTL to prevent duplicate OpenAI calls
	ttl := 24 * time.Hour
	if err := s.cache.SetEnhancedAlert(contentHash, enhanced, ttl); err != nil {
		logging.Errorw(ctx, "Failed to cache enhanced alert", "error", err)
		// Don't fail the request if caching fails
	} else {
		logging.Infow(ctx, "Cached enhanced alert for 24h", "hash", contentHash[:8])
	}

	return &enhanced, true, nil
}

// mapAlertImpact maps the AI enhancer's impact string to the AlertImpact enum.
func mapAlertImpact(impact string) api.AlertImpact {
	switch strings.ToLower(strings.TrimSpace(impact)) {
	case "none":
		return api.AlertImpact_IMPACT_NONE
	case "light":
		return api.AlertImpact_IMPACT_LIGHT
	case "moderate":
		return api.AlertImpact_IMPACT_MODERATE
	case "severe":
		return api.AlertImpact_IMPACT_SEVERE
	default:
		return api.AlertImpact_ALERT_IMPACT_UNSPECIFIED
	}
}

// Helper mapping functions
func (s *RoadsService) mapStringToAlertType(typeStr string) api.AlertType {
	switch typeStr {
	case "closure":
		return api.AlertType_CLOSURE
	case "construction":
		return api.AlertType_CONSTRUCTION
	case "incident":
		return api.AlertType_INCIDENT
	case "weather":
		return api.AlertType_WEATHER
	default:
		return api.AlertType_ALERT_TYPE_UNSPECIFIED
	}
}

func (s *RoadsService) determineAlertSeverity(classification routing.AlertClassification, impact string, alertType api.AlertType, description string) api.AlertSeverity {
	// Base severity on impact level first, then adjust by classification
	var baseSeverity api.AlertSeverity

	switch impact {
	case "severe":
		baseSeverity = api.AlertSeverity_CRITICAL
	case "moderate":
		baseSeverity = api.AlertSeverity_WARNING
	case "light":
		baseSeverity = api.AlertSeverity_WARNING
	case "none":
		baseSeverity = api.AlertSeverity_INFO
	default:
		// Fallback based on alert type and description
		if alertType == api.AlertType_CLOSURE {
			baseSeverity = api.AlertSeverity_CRITICAL
		} else if strings.Contains(strings.ToLower(description), "assistance") ||
			strings.Contains(strings.ToLower(description), "maintenance") {
			baseSeverity = api.AlertSeverity_INFO
		} else {
			baseSeverity = api.AlertSeverity_WARNING
		}
	}

	// Downgrade if distant (far from route)
	if classification == routing.Distant {
		baseSeverity = api.AlertSeverity_INFO
	}

	return baseSeverity
}

func (s *RoadsService) mapRoutingToAPIClassification(classification routing.AlertClassification) api.AlertClassification {
	switch classification {
	case routing.OnRoute:
		return api.AlertClassification_ON_ROUTE
	case routing.Nearby:
		return api.AlertClassification_NEARBY
	case routing.Distant:
		return api.AlertClassification_DISTANT
	default:
		return api.AlertClassification_ALERT_CLASSIFICATION_UNSPECIFIED
	}
}

func (s *RoadsService) roadStatusToString(status api.RoadStatus) string {
	switch status {
	case api.RoadStatus_OPEN:
		return "open"
	case api.RoadStatus_RESTRICTED:
		return "restricted"
	case api.RoadStatus_CLOSED:
		return "closed"
	default:
		return "open"
	}
}

func (s *RoadsService) chainControlToString(chainControl api.ChainControlStatus) string {
	switch chainControl {
	case api.ChainControlStatus_NONE:
		return "none"
	case api.ChainControlStatus_ADVISED:
		return "advised"
	case api.ChainControlStatus_REQUIRED:
		return "required"
	case api.ChainControlStatus_PROHIBITED:
		return "prohibited"
	default:
		return "none"
	}
}

// fetchRoadConditions fetches road conditions for each unique highway in the monitored roads
func (s *RoadsService) fetchRoadConditions(ctx context.Context) map[string][]caltrans.RoadCondition {
	result := make(map[string][]caltrans.RoadCondition)

	// Collect unique highway numbers
	seen := make(map[string]bool)
	for _, road := range s.config.Roads.MonitoredRoads {
		hwNum := extractHighwayNumber(road.Name)
		if hwNum == "" || seen[hwNum] {
			continue
		}
		seen[hwNum] = true

		conditions, err := s.caltransClient.ParseRoadConditions(ctx, hwNum)
		if err != nil {
			logging.Errorw(ctx, "Failed to fetch road conditions",
				"highway", hwNum, "error", err)
			continue
		}

		if len(conditions) > 0 {
			result[hwNum] = conditions
			logging.Infow(ctx, "Fetched road conditions",
				"highway", hwNum,
				"conditions", len(conditions))
		}
	}

	return result
}

// applyRoadConditions processes road conditions from roads.dot.ca.gov and updates road status
func (s *RoadsService) applyRoadConditions(ctx context.Context, monitoredRoad config.MonitoredRoad, conditions []caltrans.RoadCondition, roadStatus *api.RoadStatus, chainControl *api.ChainControlStatus, statusExplanation *string, alerts *[]*api.RoadAlert) {
	if len(conditions) == 0 {
		return
	}

	for _, condition := range conditions {
		// Road conditions from roads.dot.ca.gov are route-wide text advisories with
		// no geometry — the feed returns every condition on the highway statewide.
		// Attach one to a segment only when its text actually matches that segment's
		// section/keywords; otherwise the same SR-X condition (e.g. a closure at the
		// Delta) is duplicated onto every monitored segment of that highway. The
		// geolocated KML incident feed still provides spatially-accurate alerts.
		if !caltrans.MatchConditionToSegment(condition, monitoredRoad.Section, monitoredRoad.LocationKeywords) {
			continue
		}

		logging.Infow(ctx, "Processing on-route road condition",
			"road_id", monitoredRoad.ID,
			"type", condition.Type,
			"description", condition.Description)

		alert := s.buildRoadConditionAlert(condition, api.AlertClassification_ON_ROUTE)
		*alerts = append(*alerts, alert)

		switch condition.Type {
		case caltrans.CONDITION_CLOSURE:
			*roadStatus = api.RoadStatus_CLOSED
			*statusExplanation = condition.Description
			logging.Infow(ctx, "Road condition: CLOSED",
				"road_id", monitoredRoad.ID,
				"reason", condition.Reason,
				"description", condition.Description)

		case caltrans.CONDITION_CHAIN:
			if *chainControl != api.ChainControlStatus_REQUIRED {
				*chainControl = api.ChainControlStatus_REQUIRED
			}
			logging.Infow(ctx, "Road condition: chains required",
				"road_id", monitoredRoad.ID,
				"description", condition.Description)

		case caltrans.CONDITION_RESTRICTION:
			if *roadStatus != api.RoadStatus_CLOSED {
				*roadStatus = api.RoadStatus_RESTRICTED
				if *statusExplanation == "" {
					*statusExplanation = condition.Description
				}
			}
		}
	}
}

// buildRoadConditionAlert creates a RoadAlert from a road condition
func (s *RoadsService) buildRoadConditionAlert(condition caltrans.RoadCondition, classification api.AlertClassification) *api.RoadAlert {
	alertType := api.AlertType_CLOSURE
	severity := api.AlertSeverity_CRITICAL

	switch condition.Type {
	case caltrans.CONDITION_CLOSURE:
		alertType = api.AlertType_CLOSURE
		severity = api.AlertSeverity_CRITICAL
	case caltrans.CONDITION_CHAIN:
		alertType = api.AlertType_WEATHER
		severity = api.AlertSeverity_WARNING
	case caltrans.CONDITION_RESTRICTION:
		alertType = api.AlertType_CLOSURE
		severity = api.AlertSeverity_WARNING
	case caltrans.CONDITION_INFO:
		alertType = api.AlertType_ALERT_TYPE_UNSPECIFIED
		severity = api.AlertSeverity_INFO
	}

	return &api.RoadAlert{
		Type:           alertType,
		Severity:       severity,
		Classification: classification,
		Title:          fmt.Sprintf("SR %s Road Condition", condition.Highway),
		Description:    condition.Description,
		Metadata: map[string]string{
			"source": "roads.dot.ca.gov",
			"reason": condition.Reason,
		},
	}
}

// Legacy mapping functions - kept for compatibility but replaced by route-aware classification

// TODO: Re-enable for winter chain control processing
// extractChainControlStatus determines chain control requirements from description
// func (s *RoadsService) extractChainControlStatus(description string) string {
// 	lowerDesc := strings.ToLower(description)
//
// 	if strings.Contains(lowerDesc, "chain control required") ||
// 	   strings.Contains(lowerDesc, "chains required") {
// 		return "required"
// 	}
// 	if strings.Contains(lowerDesc, "chain control advised") ||
// 	   strings.Contains(lowerDesc, "chains advised") {
// 		return "advised"
// 	}
// 	if strings.Contains(lowerDesc, "chain control in effect") {
// 		return "required"
// 	}
//
// 	return "none"
// }
