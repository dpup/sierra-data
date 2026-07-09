package main

import (
	"context"
	"log"
	"sort"
	"time"
	_ "time/tzdata" // Embed the IANA tz database so America/Los_Angeles resolves in minimal containers

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/cache"
	"github.com/dpup/sierra-data/internal/clients/calfire"
	"github.com/dpup/sierra-data/internal/clients/caloes"
	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/clients/census"
	"github.com/dpup/sierra-data/internal/clients/google"
	"github.com/dpup/sierra-data/internal/clients/nws"
	"github.com/dpup/sierra-data/internal/clients/usgs"
	"github.com/dpup/sierra-data/internal/clients/weather"
	"github.com/dpup/sierra-data/internal/clients/wfigs"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/gridapi"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/ingest"
	"github.com/dpup/sierra-data/internal/lib/alerts"
	"github.com/dpup/sierra-data/internal/mcp"
	"github.com/dpup/sierra-data/internal/places"
	"github.com/dpup/sierra-data/internal/services"
	"github.com/dpup/sierra-data/internal/store"
)

func main() {
	// Initialize structured logging
	logger := logging.NewProdLogger()
	ctx := logging.With(context.Background(), logger)

	logging.Info(ctx, "Starting The Grid (S.I.E.R.R.A data service)")

	// Load configuration using Prefab's config system
	appConfig := config.LoadConfig()

	// Initialize cache. Periodic cleanup evicts very-stale entries so keys that
	// are never overwritten (content-hash AI-enhancement entries) don't
	// accumulate forever.
	cacheInstance := cache.NewCache()
	cacheInstance.StartPeriodicCleanup(ctx, time.Hour)

	// Initialize external API clients using top-level client configurations
	googleClient := google.NewClient(appConfig.GoogleRoutes.APIKey)
	caltransClient := caltrans.NewFeedParser()
	weatherClient := weather.NewClient(appConfig.OpenWeather.APIKey)
	nwsClient := nws.NewClient(appConfig.Weather.NWS.UserAgent)

	// Initialize OpenAI enhancer with caching (required for service)
	if appConfig.OpenAI.APIKey == "" {
		logging.Error(ctx, "OpenAI API key is required in configuration for incident enhancement")
		log.Fatal("OpenAI API key is required in configuration for incident enhancement")
	}

	model := appConfig.OpenAI.Model

	// Create OpenAI enhancer (caching is integrated directly in services).
	// Weather alerts are NWS-sourced with authoritative wording and are not
	// AI-enhanced; only road alerts use the enhancer.
	alertEnhancer := alerts.NewAlertEnhancer(appConfig.OpenAI.APIKey, model)

	logging.Infow(ctx, "OpenAI enhancement enabled", "model", model, "caching", "content-based")

	// Initialize gRPC services
	roadsService := services.NewRoadsService(googleClient, caltransClient, cacheInstance, appConfig, alertEnhancer)
	weatherService := services.NewWeatherService(weatherClient, nwsClient, cacheInstance, appConfig)

	logging.Infow(ctx, "Live Data API Server starting",
		"roads_monitored", len(appConfig.Roads.MonitoredRoads),
		"weather_locations", len(appConfig.Weather.Locations))

	// Start periodic refresh to maintain cache warmth (replaces complex cache warmer)
	periodicRefresh := services.NewPeriodicRefreshService(roadsService, appConfig)
	if err := periodicRefresh.StartPeriodicRefresh(ctx); err != nil {
		logging.Errorw(ctx, "Failed to start periodic refresh", "error", err)
	}

	// Grid event store + ingest scheduler (docs/v2-implementation-plan.md):
	// normalized hazard events persisted with revision history, per-source
	// health, and the place directory — the /v1 foundation.
	if appConfig.Grid.DBPath == "" {
		logging.Error(ctx, "grid.dbPath is required (default ./data/grid.db in prefab.yaml)")
		log.Fatal("grid.dbPath is required (default ./data/grid.db in prefab.yaml)")
	}
	gridStore, err := store.Open(appConfig.Grid.DBPath, store.WithJournalMode(appConfig.Grid.JournalMode))
	if err != nil {
		logging.Errorw(ctx, "Failed to open grid store", "path", appConfig.Grid.DBPath, "error", err)
		log.Fatalf("Failed to open grid store: %v", err)
	}
	defer gridStore.Close()

	if err := gridStore.SeedSources(ctx, gridSourceSeeds(appConfig)); err != nil {
		logging.Errorw(ctx, "Failed to seed grid sources", "error", err)
		log.Fatalf("Failed to seed grid sources: %v", err)
	}
	if err := places.Seed(ctx, gridStore, appConfig); err != nil {
		logging.Errorw(ctx, "Failed to seed grid places", "error", err)
		log.Fatalf("Failed to seed grid places: %v", err)
	}

	// Hazard condition-layer projector: gridapi calls BuildLayer for the live
	// condition layers (road_segment, chain_control, fire_weather). The event
	// layers are projected from the grid store by gridapi itself.
	hazardsService := hazards.NewServiceWithAPIs(appConfig, roadsService, weatherService, caltransClient, cacheInstance)

	// NWS weather-alert enhancement is optional: nil when disabled or keyless
	// (the scheduler then serves raw alerts — enhancement never gates ingest).
	var nwsEnhancer ingest.NWSEnhancer
	if appConfig.Grid.Enhancement.Enabled {
		nwsEnhancer = ingest.NewNWSEnhancer(appConfig.OpenAI)
	}

	// One poller per upstream scope; weather_alert and road_incident reuse the
	// services' cached, budgeted pipelines (plan decision 6).
	scheduler := ingest.NewScheduler(gridStore, ingest.SchedulerConfig{
		Pollers: []ingest.PollerSpec{
			{Normalizer: ingest.NewEarthquakeNormalizer(appConfig, usgs.NewClient()), Interval: gridPollInterval(appConfig, "usgs")},
			{Normalizer: ingest.NewWildfireNormalizer(appConfig, calfire.NewClient(), wfigs.NewClient()), Interval: gridPollInterval(appConfig, "calfire", "wfigs")},
			{Normalizer: ingest.NewEvacuationNormalizer(appConfig, caloes.NewClient()), Interval: gridPollInterval(appConfig, "caloes")},
			{Normalizer: ingest.NewWeatherAlertNormalizer(appConfig, weatherService), Interval: gridPollInterval(appConfig, "nws")},
			{Normalizer: ingest.NewRoadIncidentNormalizer(appConfig, roadsService), Interval: gridPollInterval(appConfig, "chp", "caltrans")},
		},
		Tuning:        appConfig.Grid.Sources,
		Enhancer:      nwsEnhancer,
		EnhancerModel: model,
		BudgetPerTick: appConfig.Grid.Enhancement.BudgetPerTick,
	})
	scheduler.Start(ctx)

	// /v1 entity + map API over the grid store (hand-built handlers; the
	// census geocoder backs /v1/places/resolve?address=). Mounted at /v1/ —
	// longest-prefix wins, so /api/ (gateway) and / (site) are unaffected.
	censusClient := census.NewClient()
	gridapiService := gridapi.NewService(gridStore, roadsService, weatherService, censusClient, appConfig, hazardsService)

	// MCP endpoint (docs/mcp-design.md): read-only tools for LLM agents over
	// Streamable HTTP, adapting the /v1 surface in-process.
	mcpHandler := mcp.NewHandler(gridapiService)

	// GridService: the proto-defined /api/v1 entity/query surface over
	// gRPC-Gateway (docs/grpc-gateway-migration-plan.md). Being ported endpoint
	// by endpoint; the hand-built /v1 handlers remain until each RPC reaches
	// parity. Gateway annotations mount under /api/, which Prefab already serves.
	gridServer := gridapi.NewGridServer(gridapiService)

	// Prefab server. gRPC + gateway serve the /api/v1 GridService; the hand-built
	// /v1 grid API, MCP, and the static site are plain HTTP handlers.
	server := prefab.New(
		prefab.WithContext(ctx),
		prefab.WithGRPCReflection(),
		// TODO(grpc-gateway-migration §4): enable once prefab ships WithETag —
		// conditional GET (ETag/If-None-Match->304) across gateway + .geojson.
		// prefab.WithETag(),
		prefab.WithGRPCService(&gridv1.GridService_ServiceDesc, gridServer),
		prefab.WithGRPCGateway(func(ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error {
			if err := gridv1.RegisterGridServiceHandlerFromEndpoint(ctx, mux, endpoint, opts); err != nil {
				return err
			}
			// Mount the hand-built summary + .geojson endpoints on the same mux.
			return gridServer.RegisterGatewayRoutes(mux)
		}),
		prefab.WithHTTPHandler(gridapi.HandlerPrefix, gridapiService),
		prefab.WithHTTPHandlerFunc("/mcp", mcpHandler.ServeHTTP),
		prefab.WithHTTPHandlerFunc("/", siteHandler),
	)

	logging.Info(ctx, "Server initialization complete, starting HTTP services")

	// Start the server (blocks until shutdown)
	if err := server.Start(); err != nil {
		logging.Errorw(ctx, "Server failed", "error", err)
		log.Fatalf("Server failed: %v", err)
	}
}

// gridSourceInfo is the static registry of source display names and
// attributions. It must match the normalizers' provenance constants (which
// in turn match the shipped hazards Source blocks) so /v1/sources and event
// provenance agree.
var gridSourceInfo = map[string]struct{ name, attribution string }{
	"usgs":     {"USGS", "U.S. Geological Survey"},
	"calfire":  {"CAL FIRE", "CAL FIRE / WFIGS"},
	"wfigs":    {"NIFC WFIGS", "NIFC / WFIGS"},
	"caloes":   {"Cal OES", "Cal OES — reference only"},
	"nws":      {"National Weather Service", "NOAA / National Weather Service"},
	"chp":      {"CHP / Caltrans", "quickmap.dot.ca.gov"},
	"caltrans": {"Caltrans", "quickmap.dot.ca.gov"},
}

// gridSourceSeeds builds the source registry rows: ids + tuning from
// grid.sources config, names/attributions from the static registry (a
// config id without a registry entry is seeded with its id as the name so it
// still shows up in /v1/sources rather than failing silently).
func gridSourceSeeds(cfg *config.Config) []store.SourceSeed {
	ids := make([]string, 0, len(cfg.Grid.Sources))
	for id := range cfg.Grid.Sources {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic seeding order

	seeds := make([]store.SourceSeed, 0, len(ids))
	for _, id := range ids {
		tuning := cfg.Grid.Sources[id]
		info, ok := gridSourceInfo[id]
		if !ok {
			info.name = id
		}
		seeds = append(seeds, store.SourceSeed{
			ID:            id,
			Name:          info.name,
			Attribution:   info.attribution,
			PollInterval:  tuning.PollInterval,
			StaleAfter:    tuning.StaleAfter,
			ExpireAfter:   tuning.ExpireAfter,
			Disappearance: tuning.Disappearance,
		})
	}
	return seeds
}

// gridPollInterval is a poller's cadence: the fastest configured interval
// among the sources it covers (a poller may span several source rows),
// defaulting to 5m when none is configured.
func gridPollInterval(cfg *config.Config, sourceIDs ...string) time.Duration {
	var best time.Duration
	for _, id := range sourceIDs {
		if t, ok := cfg.Grid.Sources[id]; ok && t.PollInterval > 0 {
			if best == 0 || t.PollInterval < best {
				best = t.PollInterval
			}
		}
	}
	if best == 0 {
		return 5 * time.Minute
	}
	return best
}
