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

// LocationForecasts returns each configured location's short-range fire-weather
// forecast, keyed by location id. It is ADDITIVE and FAIL-SOFT: it never returns
// an error — a per-location failure omits that location (serving stale where the
// cache still has it), so conditions and the fire_weather layer degrade rather
// than break. Returns nil when forecasts are disabled or no NWS client is wired.
func (s *WeatherService) LocationForecasts(ctx context.Context) map[string]*nws.Forecast {
	if s.nwsClient == nil || !s.config.Weather.Forecast.Enabled {
		return nil
	}
	horizon := time.Duration(s.forecastHorizonHours()) * time.Hour
	out := make(map[string]*nws.Forecast)
	for _, loc := range s.config.Weather.Locations {
		if f := s.locationForecast(ctx, loc, horizon); f != nil {
			out[loc.ID] = f
		}
	}
	return out
}

// locationForecast serves a location's forecast cache-first (fresh), refreshing
// from NWS on miss/stale and serving last-good within the very-stale bound on
// failure (mirrors the NWS-alerts pattern in weather_nws.go).
func (s *WeatherService) locationForecast(ctx context.Context, loc config.WeatherLocation, horizon time.Duration) *nws.Forecast {
	key := "nws:forecast:" + loc.ID
	var cached nws.Forecast
	if found, _ := s.cache.Get(key, &cached); found && !s.cache.IsStale(key) {
		return &cached
	}
	gridURL := s.locationGridURL(ctx, loc)
	if gridURL == "" {
		return s.staleForecast(key)
	}
	f, err := s.nwsClient.GetGridForecast(ctx, gridURL, horizon)
	if err != nil {
		logging.Errorw(ctx, "NWS forecast fetch failed; serving stale if available",
			"location_id", loc.ID, "error", err)
		return s.staleForecast(key)
	}
	if len(f.Points) == 0 {
		// Never cache an empty forecast (it would replay as a false "no data");
		// serve stale instead so a later good fetch fills in.
		return s.staleForecast(key)
	}
	if err := s.cache.Set(key, f, s.forecastRefreshInterval(), "nws_forecast"); err != nil {
		logging.Errorw(ctx, "Failed to cache forecast", "location_id", loc.ID, "error", err)
	}
	return f
}

// staleForecast returns the last-good forecast for a key if present and within
// the very-stale bound, else nil (omit — never fabricate).
func (s *WeatherService) staleForecast(key string) *nws.Forecast {
	var stale nws.Forecast
	if _, found, _ := s.cache.GetWithMetadata(key, &stale); found && !s.cache.IsVeryStale(key) {
		return &stale
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
