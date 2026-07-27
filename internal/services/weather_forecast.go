package services

import (
	"context"
	"time"

	"github.com/dpup/prefab/logging"

	"github.com/dpup/sierra-data/internal/clients/nws"
	"github.com/dpup/sierra-data/internal/config"
)

const (
	// gridURLTTL caches the static /points → forecastGridData mapping for a location.
	// The cache is in-memory (reset on restart), so this is effectively "forever".
	gridURLTTL = 30 * 24 * time.Hour
	// forecast defaults when weather.forecast leaves them unset.
	defaultForecastRefresh = time.Hour
	defaultForecastHorizon = 48
)

// RefreshForecasts fetches every configured location's forecast from NWS and
// warms the cache. It runs on the BACKGROUND refresher (periodic_refresh.go),
// never on the request path — so /api/v1/conditions and the fire_weather layer
// only ever read the cache and never fan out live NWS fetches. No-op when
// disabled / unwired. Fail-soft per location: a failure logs and leaves the
// last-good entry in place.
func (s *WeatherService) RefreshForecasts(ctx context.Context) {
	if s.nwsClient == nil || !s.config.Weather.Forecast.Enabled {
		return
	}
	horizon := time.Duration(s.forecastHorizonHours()) * time.Hour
	for _, loc := range s.config.Weather.Locations {
		s.refreshLocationForecast(ctx, loc, horizon)
	}
}

// refreshLocationForecast fetches one location and updates the cache. On a 404
// (NWS re-tiled the grid so the cached URL is dead) it invalidates the cached
// gridpoint URL and re-resolves once, so a moved gridpoint self-heals within a
// refresh cycle rather than being wedged until the gridURL TTL.
func (s *WeatherService) refreshLocationForecast(ctx context.Context, loc config.WeatherLocation, horizon time.Duration) {
	key := "nws:forecast:" + loc.ID
	// Skip while still fresh — Get returns found only for non-stale entries, so a
	// forecast warmed within its refreshInterval isn't re-fetched even when the
	// background refresher ticks faster (it shares the roads cadence).
	var fresh nws.Forecast
	if found, _ := s.cache.Get(key, &fresh); found {
		return
	}
	gridURL := s.locationGridURL(ctx, loc)
	if gridURL == "" {
		return
	}
	f, err := s.nwsClient.GetGridForecast(ctx, gridURL, horizon)
	if nws.IsNotFound(err) {
		s.cache.Delete("nws:gridurl:" + loc.ID)
		if gridURL = s.locationGridURL(ctx, loc); gridURL != "" {
			f, err = s.nwsClient.GetGridForecast(ctx, gridURL, horizon)
		}
	}
	if err != nil {
		logging.Errorw(ctx, "NWS forecast refresh failed; keeping last-good",
			"location_id", loc.ID, "error", err)
		return
	}
	if len(f.Points) == 0 {
		// Never cache an empty forecast (it would replay as a false "no data");
		// keep the last-good entry so a later good fetch fills in.
		logging.Warnw(ctx, "NWS forecast returned no points; keeping last-good", "location_id", loc.ID)
		return
	}
	if err := s.cache.Set(key, f, s.forecastRefreshInterval(), "nws_forecast"); err != nil {
		logging.Errorw(ctx, "Failed to cache forecast", "location_id", loc.ID, "error", err)
	}
}

// LocationForecasts READS the warmed per-location forecasts from the cache (fresh,
// or last-good within the very-stale bound) WITHOUT fetching — the request path
// never touches NWS (RefreshForecasts warms it in the background). It is additive
// and fail-soft: a location with no warm entry is simply omitted. Returns nil when
// forecasts are disabled. (ctx is unused — kept for the WeatherAPI interface.)
func (s *WeatherService) LocationForecasts(_ context.Context) map[string]*nws.Forecast {
	if !s.config.Weather.Forecast.Enabled {
		return nil
	}
	out := make(map[string]*nws.Forecast)
	for _, loc := range s.config.Weather.Locations {
		if f := s.readForecast("nws:forecast:" + loc.ID); f != nil {
			out[loc.ID] = f
		}
	}
	return out
}

// readForecast returns a cached forecast — fresh, or last-good within the
// very-stale bound — without any upstream fetch, or nil.
func (s *WeatherService) readForecast(key string) *nws.Forecast {
	var f nws.Forecast
	if found, _ := s.cache.Get(key, &f); found {
		cp := f
		return &cp
	}
	var stale nws.Forecast
	if _, found, _ := s.cache.GetWithMetadata(key, &stale); found && !s.cache.IsVeryStale(key) {
		cp := stale
		return &cp
	}
	return nil
}

// locationGridURL resolves and caches the static /points → forecastGridData URL.
func (s *WeatherService) locationGridURL(ctx context.Context, loc config.WeatherLocation) string {
	key := "nws:gridurl:" + loc.ID
	var url string
	if found, _ := s.cache.Get(key, &url); found && url != "" {
		return url
	}
	resolved, err := s.nwsClient.ResolveForecastURL(ctx, loc.Coordinates.Latitude, loc.Coordinates.Longitude)
	if err != nil {
		logging.Errorw(ctx, "NWS gridpoint resolve failed", "location_id", loc.ID, "error", err)
		// The mapping is static, so a previously-resolved URL is still valid even
		// if the cache marks it stale — use it rather than losing the forecast.
		var prev string
		if _, found, _ := s.cache.GetWithMetadata(key, &prev); found && prev != "" {
			return prev
		}
		return ""
	}
	if err := s.cache.Set(key, resolved, gridURLTTL, "nws_gridurl"); err != nil {
		logging.Errorw(ctx, "Failed to cache gridpoint URL", "location_id", loc.ID, "error", err)
	}
	return resolved
}

func (s *WeatherService) forecastRefreshInterval() time.Duration {
	if d := s.config.Weather.Forecast.RefreshInterval; d > 0 {
		return d
	}
	return defaultForecastRefresh
}

func (s *WeatherService) forecastHorizonHours() int {
	if h := s.config.Weather.Forecast.HorizonHours; h > 0 {
		return h
	}
	return defaultForecastHorizon
}
