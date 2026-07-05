package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/dpup/prefab/logging"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/cache"
	"github.com/dpup/info.ersn.net/server/internal/clients/nws"
	"github.com/dpup/info.ersn.net/server/internal/clients/weather"
	"github.com/dpup/info.ersn.net/server/internal/config"
)

// WeatherService implements the gRPC WeatherService
// Implementation per tasks.md T017 and data-model.md WeatherData entity
type WeatherService struct {
	api.UnimplementedWeatherServiceServer
	weatherClient *weather.Client
	nwsClient     *nws.Client
	cache         *cache.Cache
	config        *config.Config
}

// NewWeatherService creates a new WeatherService
func NewWeatherService(weatherClient *weather.Client, nwsClient *nws.Client, cache *cache.Cache, config *config.Config) *WeatherService {
	return &WeatherService{
		weatherClient: weatherClient,
		nwsClient:     nwsClient,
		cache:         cache,
		config:        config,
	}
}

// ListWeather implements the gRPC method defined in contracts/weather.proto lines 12-17
func (s *WeatherService) ListWeather(ctx context.Context, req *api.ListWeatherRequest) (*api.ListWeatherResponse, error) {
	logging.Info(ctx, "ListWeather called")

	// Try to get cached weather data first
	var cachedWeatherData []*api.WeatherData
	cacheKey := "weather:all"

	found, err := s.cache.Get(cacheKey, &cachedWeatherData)
	if err != nil {
		logging.Errorw(ctx, "Cache error", "error", err, "cache_key", cacheKey)
	}

	if found && !s.cache.IsStale(cacheKey) {
		logging.Infow(ctx, "Returning cached weather data", "location_count", len(cachedWeatherData))

		// Get cache metadata for last_updated timestamp
		entry, _, _ := s.cache.GetWithMetadata(cacheKey, nil)
		var lastUpdated *timestamppb.Timestamp
		if entry != nil {
			lastUpdated = timestamppb.New(entry.CreatedAt)
		}

		return &api.ListWeatherResponse{
			WeatherData: cachedWeatherData,
			LastUpdated: lastUpdated,
			FireWeather: s.computeRegionFireWeather(ctx),
		}, nil
	}

	// Cache miss or stale - refresh from external API
	logging.Info(ctx, "Refreshing weather data from OpenWeatherMap API")
	weatherData, err := s.refreshWeatherData(ctx)
	if err != nil {
		// Refresh failed - serve stale data if it's within the very-stale bound.
		// Get filters stale entries, so re-read with GetWithMetadata (which
		// returns them and lets the caller decide).
		var staleData []*api.WeatherData
		entry, foundStale, _ := s.cache.GetWithMetadata(cacheKey, &staleData)
		if foundStale && len(staleData) > 0 && !s.cache.IsVeryStale(cacheKey) {
			logging.Errorw(ctx, "Refresh failed, returning stale cached weather data", "error", err)
			return &api.ListWeatherResponse{
				WeatherData: staleData,
				LastUpdated: timestamppb.New(entry.CreatedAt),
				FireWeather: s.computeRegionFireWeather(ctx),
			}, nil
		}
		return nil, fmt.Errorf("failed to refresh weather data: %w", err)
	}

	// Cache the refreshed data
	if err := s.cache.Set(cacheKey, weatherData, s.config.Weather.RefreshInterval, "weather"); err != nil {
		logging.Errorw(ctx, "Failed to cache weather data", "error", err)
	}

	return &api.ListWeatherResponse{
		WeatherData: weatherData,
		LastUpdated: timestamppb.Now(),
		FireWeather: s.computeRegionFireWeather(ctx),
	}, nil
}

// GetLocationWeather implements the gRPC method for retrieving weather for a specific location
func (s *WeatherService) GetLocationWeather(ctx context.Context, req *api.GetLocationWeatherRequest) (*api.GetLocationWeatherResponse, error) {
	logging.Infow(ctx, "GetLocationWeather called", "location_id", req.LocationId)

	// Get all weather data (will use cache if available)
	listResp, err := s.ListWeather(ctx, &api.ListWeatherRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to get weather data: %w", err)
	}

	// Find the requested location
	for _, weatherData := range listResp.WeatherData {
		if weatherData.LocationId == req.LocationId {
			return &api.GetLocationWeatherResponse{
				WeatherData: weatherData,
				LastUpdated: listResp.LastUpdated,
				FireWeather: listResp.FireWeather,
			}, nil
		}
	}

	return nil, status.Errorf(codes.NotFound, "location not found: %s", req.LocationId)
}

// ListWeatherAlerts implements the gRPC method for retrieving weather alerts
func (s *WeatherService) ListWeatherAlerts(ctx context.Context, req *api.ListWeatherAlertsRequest) (*api.ListWeatherAlertsResponse, error) {
	logging.Info(ctx, "ListWeatherAlerts called")

	// Try to get cached alerts first
	var cachedAlerts []*api.WeatherAlert
	cacheKey := "weather:alerts"

	found, err := s.cache.Get(cacheKey, &cachedAlerts)
	if err != nil {
		logging.Errorw(ctx, "Cache error", "error", err, "cache_key", cacheKey)
	}

	if found && !s.cache.IsStale(cacheKey) {
		logging.Infow(ctx, "Returning cached weather alerts", "alert_count", len(cachedAlerts))

		entry, _, _ := s.cache.GetWithMetadata(cacheKey, nil)
		var lastUpdated *timestamppb.Timestamp
		if entry != nil {
			lastUpdated = timestamppb.New(entry.CreatedAt)
		}

		return &api.ListWeatherAlertsResponse{
			Alerts:      filterAlertsByZones(cachedAlerts, req.Zones),
			LastUpdated: lastUpdated,
		}, nil
	}

	// Cache miss or stale - refresh alerts from external API
	logging.Info(ctx, "Refreshing weather alerts (NWS zone alerts)")
	alerts, err := s.refreshWeatherAlerts(ctx)
	if err != nil {
		// Refresh failed - serve stale alerts if within the very-stale bound.
		// Get filters stale entries, so re-read with GetWithMetadata.
		var staleAlerts []*api.WeatherAlert
		entry, foundStale, _ := s.cache.GetWithMetadata(cacheKey, &staleAlerts)
		if foundStale && !s.cache.IsVeryStale(cacheKey) {
			logging.Errorw(ctx, "Refresh failed, returning stale cached alerts", "error", err)
			return &api.ListWeatherAlertsResponse{
				Alerts:      filterAlertsByZones(staleAlerts, req.Zones),
				LastUpdated: timestamppb.New(entry.CreatedAt),
			}, nil
		}
		return nil, fmt.Errorf("failed to refresh weather alerts: %w", err)
	}

	// Cache the refreshed alerts
	if err := s.cache.Set(cacheKey, alerts, s.config.Weather.RefreshInterval, "weather_alerts"); err != nil {
		logging.Errorw(ctx, "Failed to cache weather alerts", "error", err)
	}

	return &api.ListWeatherAlertsResponse{
		Alerts:      filterAlertsByZones(alerts, req.Zones),
		LastUpdated: timestamppb.Now(),
	}, nil
}

// refreshWeatherData fetches fresh weather data from OpenWeatherMap for all configured locations
func (s *WeatherService) refreshWeatherData(ctx context.Context) ([]*api.WeatherData, error) {
	var weatherDataList []*api.WeatherData

	// Get existing cached data to preserve on per-location failures. The cache
	// entry is stale by the time a refresh runs (that's what triggered it), so
	// read via GetWithMetadata — Get filters stale entries out.
	var existingData []*api.WeatherData
	existingDataMap := make(map[string]*api.WeatherData)
	cacheKey := "weather:all"
	if _, found, _ := s.cache.GetWithMetadata(cacheKey, &existingData); found {
		for _, wd := range existingData {
			existingDataMap[wd.LocationId] = wd
		}
	}

	logging.Infow(ctx, "Starting weather refresh", "location_count", len(s.config.Weather.Locations))

	// Process each configured location
	for i, location := range s.config.Weather.Locations {
		logging.Infow(ctx, "Processing weather location", "index", i, "location_id", location.ID, "location_name", location.Name)

		weatherData, err := s.processWeatherLocation(ctx, location)
		if err != nil {
			logging.Errorw(ctx, "Failed to process weather for location",
				"location_id", location.ID,
				"location_name", location.Name,
				"error", err)
			// Try to preserve existing cached data for this location
			if existing, ok := existingDataMap[location.ID]; ok {
				logging.Infow(ctx, "Preserving stale weather data for location", "location_id", location.ID)
				weatherDataList = append(weatherDataList, existing)
			}
			continue
		}
		weatherDataList = append(weatherDataList, weatherData)
		logging.Infow(ctx, "Successfully processed weather location", "location_id", location.ID)
	}

	logging.Infow(ctx, "Weather refresh complete",
		"total_locations", len(s.config.Weather.Locations),
		"successful_locations", len(weatherDataList))

	if len(weatherDataList) == 0 {
		return nil, fmt.Errorf("no weather data could be processed")
	}

	return weatherDataList, nil
}

// processWeatherLocation fetches weather data for a single location.
// Current conditions come from OpenWeatherMap; per-location alerts are the NWS
// alerts active in the location's configured forecast zone. (OpenWeather One
// Call alerts were dropped deliberately — for US locations they are relabeled
// NWS data, and the One Call 3.0 endpoint's 1,000 calls/day free cap was being
// exceeded. Don't reintroduce per-location One Call fetches.)
func (s *WeatherService) processWeatherLocation(ctx context.Context, location config.WeatherLocation) (*api.WeatherData, error) {
	logging.Infow(ctx, "Processing weather for location", "location_id", location.ID)

	if s.config.OpenWeather.APIKey == "" {
		return nil, fmt.Errorf("OpenWeatherMap API key not configured")
	}

	// Get current weather data
	weatherData, err := s.weatherClient.GetCurrentWeather(ctx, location.ToProto())
	if err != nil {
		return nil, fmt.Errorf("failed to get current weather: %w", err)
	}

	// Set location ID and name from config
	weatherData.LocationId = location.ID
	weatherData.LocationName = location.Name

	// Attach the NWS alerts for this location's forecast zone. The shared NWS
	// fetch is cached, so this is free per additional location; a transient NWS
	// failure leaves alerts empty rather than failing current conditions.
	weatherData.Alerts = s.nwsAlertsForZone(ctx, location.Zone)

	return weatherData, nil
}

// refreshWeatherAlerts builds the weather-alerts list from authoritative NWS
// zone alerts (issue #4). A NWS failure propagates as an error — callers fall
// back to their stale cache or fail loud — rather than caching an empty list
// that would read as "no alerts".
func (s *WeatherService) refreshWeatherAlerts(ctx context.Context) ([]*api.WeatherAlert, error) {
	nwsAlerts, err := s.fetchNWSAlerts(ctx)
	if err != nil {
		return nil, err
	}
	return nwsAlertsToProto(nwsAlerts), nil
}

// filterAlertsByZones constrains zone-scoped (NWS) alerts to the requested
// forecast zones. Non-NWS alerts (e.g. OpenWeatherMap, which are location-based
// rather than zone-based) are NOT zone-scoped and always pass through, so the
// filter narrows NWS coverage without silently dropping the other source.
// When no zones are requested the input list is returned unchanged. Requested
// zones may be comma-separated or repeated.
func filterAlertsByZones(alerts []*api.WeatherAlert, zones []string) []*api.WeatherAlert {
	zoneSet := make(map[string]bool)
	for _, z := range zones {
		for _, part := range strings.Split(z, ",") {
			zc := strings.ToUpper(strings.TrimSpace(part))
			if zc != "" {
				zoneSet[zc] = true
			}
		}
	}
	if len(zoneSet) == 0 {
		return alerts
	}

	var out []*api.WeatherAlert
	for _, a := range alerts {
		// Only NWS alerts are zone-scoped; everything else is unaffected.
		if a.Source != api.AlertSource_NWS {
			out = append(out, a)
			continue
		}
		for _, z := range a.Zones {
			if zoneSet[strings.ToUpper(strings.TrimSpace(z))] {
				out = append(out, a)
				break
			}
		}
	}
	return out
}
