package services

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/prefab/errors"
	"github.com/dpup/prefab/logging"
)

// PeriodicRefreshService simulates regular API requests to maintain cache warmth
// Replaces complex CacheWarmer with simple periodic calls to existing refresh logic
type PeriodicRefreshService struct {
	roadsService *RoadsService
	config       *config.Config

	// Background refresh control
	stopChan chan struct{}
	running  bool
}

// NewPeriodicRefreshService creates a new periodic refresh service
func NewPeriodicRefreshService(roadsService *RoadsService, config *config.Config) *PeriodicRefreshService {
	return &PeriodicRefreshService{
		roadsService: roadsService,
		config:       config,
		stopChan:     make(chan struct{}),
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

// Stop gracefully stops the periodic refresh
func (p *PeriodicRefreshService) Stop() {
	if !p.running {
		return
	}

	p.running = false
	close(p.stopChan)
	logging.Info(context.Background(), "Stopped periodic refresh service")
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

	// Call the road service refresh method directly
	roads, err := p.roadsService.refreshRoadData(refreshCtx)
	if err != nil {
		logging.Errorw(ctx, "Periodic refresh: failed to refresh road data", "error", err)
		return
	}

	// Cache the refreshed data
	cacheKey := "roads:all"
	if err := p.roadsService.cache.Set(cacheKey, roads, p.config.Roads.RefreshInterval, "roads"); err != nil {
		logging.Errorw(ctx, "Periodic refresh: failed to cache roads", "error", err)
	} else {
		logging.Infow(ctx, "Periodic refresh: successfully cached roads", "road_count", len(roads))
	}
}

// IsRunning returns whether periodic refresh is active
func (p *PeriodicRefreshService) IsRunning() bool {
	return p.running
}
