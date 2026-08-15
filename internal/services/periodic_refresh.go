package services

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/dpup/prefab/errors"
	"github.com/dpup/prefab/logging"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/config"
)

// PeriodicRefreshService simulates regular API requests to maintain cache warmth
// Replaces complex CacheWarmer with simple periodic calls to existing refresh logic
type PeriodicRefreshService struct {
	roadsService   *RoadsService
	weatherService *WeatherService // may be nil (forecast warming is optional)
	config         *config.Config

	// Background refresh control
	stopChan chan struct{}
	running  bool
}

// NewPeriodicRefreshService creates a new periodic refresh service. weatherService
// may be nil; when set, its fire-weather forecast cache is warmed so the request
// path never fetches NWS synchronously.
func NewPeriodicRefreshService(roadsService *RoadsService, weatherService *WeatherService, config *config.Config) *PeriodicRefreshService {
	return &PeriodicRefreshService{
		roadsService:   roadsService,
		weatherService: weatherService,
		config:         config,
		stopChan:       make(chan struct{}),
	}
}

// StartPeriodicRefresh begins simulated API requests to maintain cache freshness
// Uses existing refresh intervals from configuration
func (p *PeriodicRefreshService) StartPeriodicRefresh(ctx context.Context) error {
	if p.running {
		return nil // Already running
	}

	p.running = true

	// Use roads refresh interval from config (default 5 minutes)
	interval := p.config.Roads.RefreshInterval

	logging.Infow(ctx, "Starting periodic refresh", "interval", interval)

	// Start background goroutine for periodic refresh
	go func() {
		defer func() {
			// Recover from any panics in the periodic refresh goroutine
			if r := recover(); r != nil {
				err, _ := errors.ParseStack(debug.Stack())
				skipFrames := 3
				numFrames := 5
				logging.Errorw(ctx, "Periodic refresh: recovered from panic",
					"error", r, "error.stack_trace", err.MinimalStack(skipFrames, numFrames))
			}
			// Mark as not running when goroutine exits
			p.running = false
		}()

		p.refreshLoop(ctx, interval)
	}()

	return nil
}

// refreshLoop runs the periodic refresh in background
func (p *PeriodicRefreshService) refreshLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Do initial refresh immediately
	p.refreshCacheData(ctx)

	for {
		select {
		case <-ctx.Done():
			logging.Info(ctx, "Periodic refresh stopping due to context cancellation")
			return
		case <-p.stopChan:
			logging.Info(ctx, "Periodic refresh stopping due to stop signal")
			return
		case <-ticker.C:
			p.refreshCacheData(ctx)
		}
	}
}

// refreshCacheData directly refreshes the cached road data
func (p *PeriodicRefreshService) refreshCacheData(ctx context.Context) {
	logging.Info(ctx, "Periodic refresh: starting data refresh")

	// Create a timeout context for the refresh operation
	// Allow 5 minutes for processing multiple roads sequentially (4 roads × ~30s each + buffer)
	refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Call the road service refresh method directly. A roads failure must NOT
	// abandon the rest of the tick: these are independent upstreams (Google/
	// Caltrans vs OpenWeather/NWS), and letting one bad Caltrans fetch skip the
	// weather warming would put weather back on the request path for a further
	// 15 minutes — the exact cliff the warming below exists to remove.
	if roads, err := p.roadsService.refreshRoadData(refreshCtx); err != nil {
		logging.Errorw(ctx, "Periodic refresh: failed to refresh road data", "error", err)
	} else {
		cacheKey := "roads:all"
		if err := p.roadsService.cache.Set(cacheKey, roads, p.config.Roads.RefreshInterval, "roads"); err != nil {
			logging.Errorw(ctx, "Periodic refresh: failed to cache roads", "error", err)
		} else {
			logging.Infow(ctx, "Periodic refresh: successfully cached roads", "road_count", len(roads))
		}
	}

	// Warm the CURRENT-CONDITIONS cache the same way. ListWeather serves a fresh
	// cache hit as a no-op and, on a miss, does the OpenWeather fan-out and caches
	// under "weather:all" itself — so calling the public method warms it without
	// duplicating cache-key or TTL logic that could drift.
	//
	// Why it needs warming at all: this cache has the same 15m TTL as this
	// refresher, and it was the only major cache left refreshing lazily. Whenever
	// it expired, the next USER REQUEST paid the full 7-location fan-out plus the
	// NWS fire-weather computation — a multi-second cliff landing on whoever
	// happened to ask first, on a 15-minute cadence. Warming moves that cost off
	// the request path onto this goroutine.
	//
	// Budget: this makes the documented worst case the actual case — 7 locations
	// x 96 ticks/day = 672 calls/day to /data/2.5/weather, every day, rather than
	// only on days with steady traffic. That is well inside the free tier's
	// 60 calls/minute, and it buys predictable latency. Adding weather.locations
	// raises it linearly. (Do NOT reach for One Call 3.0 here — separate 1,000/day
	// cap; see the root CLAUDE.md.)
	if p.weatherService != nil {
		wxCtx, wxCancel := context.WithTimeout(ctx, 2*time.Minute)
		if _, err := p.weatherService.ListWeather(wxCtx, &api.ListWeatherRequest{}); err != nil {
			// Log and continue: warming is best-effort, and ListWeather already
			// falls back to servable-stale data on the request path.
			logging.Errorw(ctx, "Periodic refresh: failed to warm weather conditions", "error", err)
		} else {
			logging.Info(ctx, "Periodic refresh: warmed weather conditions")
		}
		wxCancel()
	}

	// Warm the fire-weather forecast cache so /api/v1/conditions + the fire_weather
	// layer read a warm cache instead of fetching NWS on the request path.
	// RefreshForecasts skips still-fresh entries, so it effectively runs on the
	// forecast's own (hourly) cadence even though the refresher ticks faster.
	if p.weatherService != nil {
		fcCtx, fcCancel := context.WithTimeout(ctx, 2*time.Minute)
		p.weatherService.RefreshForecasts(fcCtx)
		fcCancel()
	}
}
