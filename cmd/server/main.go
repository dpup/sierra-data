package main

import (
	"context"
	"log"
	"net/http"
	"sort"
	"time"
	_ "time/tzdata" // Embed the IANA tz database so America/Los_Angeles resolves in minimal containers

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"
	"github.com/dpup/prefab/plugins/etag"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/cache"
	"github.com/dpup/sierra-data/internal/clients/calfire"
	"github.com/dpup/sierra-data/internal/clients/caloes"
	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/clients/census"
	"github.com/dpup/sierra-data/internal/clients/google"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
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

	// Tell prefab's config validator that the app's own config namespaces are
	// known, so it doesn't warn about every roads/weather/grid/... key as a
	// "potential typo". Prefab's own server.* keys stay validated, so real typos
	// there still surface. Must run before prefab.New (which validates on build).
	registerAppConfigKeys()

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
	// health, and the place directory — the /api/v1 foundation.
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
	pollers := []ingest.PollerSpec{
		{Normalizer: ingest.NewEarthquakeNormalizer(appConfig, usgs.NewClient()), Interval: gridPollInterval(appConfig, "usgs")},
		{Normalizer: ingest.NewWildfireNormalizer(appConfig, calfire.NewClient(), wfigs.NewClient()), Interval: gridPollInterval(appConfig, "calfire", "wfigs")},
		{Normalizer: ingest.NewEvacuationNormalizer(appConfig, caloes.NewClient()), Interval: gridPollInterval(appConfig, "caloes")},
		{Normalizer: ingest.NewWeatherAlertNormalizer(appConfig, weatherService), Interval: gridPollInterval(appConfig, "nws")},
		{Normalizer: ingest.NewRoadIncidentNormalizer(appConfig, roadsService), Interval: gridPollInterval(appConfig, "chp", "caltrans")},
	}

	// MeshCore mesh-node presence (optional): a long-lived MQTT subscriber to
	// community bridges accumulates node state; the NetworkNormalizer serves a
	// snapshot on the scheduler's tick. Enabled only when configured with brokers.
	if appConfig.Grid.Meshcore.Enabled && len(appConfig.Grid.Meshcore.Brokers) > 0 {
		meshcoreReg := meshcore.NewRegistry(meshcoreClientConfig(appConfig))
		if err := meshcoreReg.Connect(ctx); err != nil {
			logging.Errorw(ctx, "Failed to start MeshCore subscriber", "error", err)
		} else {
			defer meshcoreReg.Close()
			logging.Infow(ctx, "MeshCore subscriber started", "brokers", len(appConfig.Grid.Meshcore.Brokers))
			pollers = append(pollers, ingest.PollerSpec{
				Normalizer: ingest.NewNetworkNormalizer(appConfig, meshcoreReg),
				Interval:   gridPollInterval(appConfig, "meshcore"),
			})
		}
	}

	scheduler := ingest.NewScheduler(gridStore, ingest.SchedulerConfig{
		Pollers:         pollers,
		Tuning:          appConfig.Grid.Sources,
		Enhancer:        nwsEnhancer,
		EnhancerModel:   model,
		BudgetPerTick:   appConfig.Grid.Enhancement.BudgetPerTick,
		MeshMaintenance: meshMaintenanceConfig(appConfig),
	})
	scheduler.Start(ctx)

	// Grid API service: backs the /api/v1 GridService RPCs and the hand-built
	// summary + .geojson gateway routes (the census geocoder powers
	// /api/v1/places:resolve?address=).
	censusClient := census.NewClient()
	gridapiService := gridapi.NewService(gridStore, weatherService, censusClient, appConfig, hazardsService)

	// MCP endpoint (docs/mcp-design.md): read-only tools for LLM agents over
	// Streamable HTTP. The tools call the /api/v1 surface in-process against the
	// gRPC-Gateway mux, which only exists after prefab.New wires the gateway — so
	// MCP holds a deferred handler we point at that mux below.
	gatewayMux := &deferredHandler{}
	mcpHandler := mcp.NewHandler(gatewayMux)

	// GridService: the proto-defined /api/v1 entity/query surface over
	// gRPC-Gateway (docs/grpc-gateway-migration-plan.md). Gateway annotations
	// mount under /api/, which Prefab already serves.
	gridServer := gridapi.NewGridServer(gridapiService)

	// Prefab server. gRPC + gateway serve the /api/v1 GridService; MCP and the
	// static site are plain HTTP handlers.
	server := prefab.New(
		prefab.WithContext(ctx),
		prefab.WithGRPCReflection(),
		// Cache-Control: public, max-age=30 on every (read-only) GridService
		// response — the freshness lifetime that complements the ETag revalidation
		// below. Restores what the hand-built /v1 handlers set before the migration.
		prefab.WithGRPCInterceptor(gridapi.CacheControlInterceptor(30)),
		// Conditional GET (ETag/If-None-Match -> 304) for RPCs that call
		// etag.Guard — event detail (revision), the event/history lists
		// (DataVersion + filters), and places (per-process nonce) — so a match
		// skips the expensive load. The hand-built .geojson route bypasses the
		// gRPC interceptor and keeps its own body-hash ETag. (GetPlaceSummary is
		// not yet guarded.)
		prefab.WithPlugin(etag.Plugin()),
		prefab.WithGRPCService(&gridv1.GridService_ServiceDesc, gridServer),
		prefab.WithGRPCGateway(func(ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error {
			if err := gridv1.RegisterGridServiceHandlerFromEndpoint(ctx, mux, endpoint, opts); err != nil {
				return err
			}
			// Mount the hand-built .geojson endpoint on the same mux (summary is
			// now the GetPlaceSummary RPC).
			if err := gridServer.RegisterGatewayRoutes(mux); err != nil {
				return err
			}
			// Point MCP at the fully-wired gateway so its tools query /api/v1
			// in-process (same mux prefab serves at /api/).
			gatewayMux.h = mux
			return nil
		}),
		prefab.WithHTTPHandlerFunc("/mcp", mcpHandler.ServeHTTP),
		// Publish the generated OpenAPI spec for /api/v1 (protoc-gen-openapiv2).
		// Exact path, so it wins over the gateway's /api/ subtree mount.
		prefab.WithHTTPHandlerFunc("/api/openapi.json", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(gridv1.OpenAPISpec)
		}),
		prefab.WithHTTPHandlerFunc("/", siteHandler),
	)

	logging.Info(ctx, "Server initialization complete, starting HTTP services")

	// Start the server (blocks until shutdown)
	if err := server.Start(); err != nil {
		logging.Errorw(ctx, "Server failed", "error", err)
		log.Fatalf("Server failed: %v", err)
	}
}

// deferredHandler is an http.Handler whose delegate is set after construction.
// MCP needs the gRPC-Gateway mux, but that mux only exists once prefab.New wires
// the gateway (in the WithGRPCGateway callback) — later than mcp.NewHandler runs.
// Until the delegate is set, requests fail loud rather than silently 404.
type deferredHandler struct{ h http.Handler }

func (d *deferredHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d.h == nil {
		http.Error(w, "gateway not ready", http.StatusServiceUnavailable)
		return
	}
	d.h.ServeHTTP(w, r)
}

// gridSourceInfo is the static registry of source display names and
// attributions. It must match the normalizers' provenance constants (which
// in turn match the shipped hazards Source blocks) so /api/v1/sources and event
// provenance agree.
var gridSourceInfo = map[string]struct{ name, attribution string }{
	"usgs":     {"USGS", "U.S. Geological Survey"},
	"calfire":  {"CAL FIRE", "CAL FIRE / WFIGS"},
	"wfigs":    {"NIFC WFIGS", "NIFC / WFIGS"},
	"caloes":   {"Cal OES", "Cal OES — reference only"},
	"nws":      {"National Weather Service", "NOAA / National Weather Service"},
	"chp":      {"CHP / Caltrans", "quickmap.dot.ca.gov"},
	"caltrans": {"Caltrans", "quickmap.dot.ca.gov"},
	"meshcore": {"MeshCore Mesh", "MeshCore community mesh"},
}

// registerAppConfigKeys registers the app's top-level config namespaces with
// prefab's key validator. They live in prefab.yaml but aren't prefab's own keys,
// so without this prefab logs every one as an unknown key on startup. Registered
// as namespace prefixes — prefab's HasRegisteredPrefix allows any key beneath a
// registered namespace, so one entry per namespace covers all nested keys.
func registerAppConfigKeys() {
	for _, ns := range []string{
		"grid", "roads", "weather", "hazards",
		"openai", "openweather", "googleRoutes", "google_routes",
	} {
		prefab.RegisterConfigKey(prefab.ConfigKeyInfo{
			Key:         ns,
			Description: "The Grid application config namespace (see internal/config)",
			Type:        "object",
		})
	}
}

// meshcoreClientConfig maps the grid.meshcore config onto the meshcore client
// Config. RetainFor is anchored to the source's expireAfter so the store's
// lifecycle — not the in-memory buffer — decides when a silent node is gone.
func meshcoreClientConfig(cfg *config.Config) meshcore.Config {
	mc := cfg.Grid.Meshcore
	brokers := make([]meshcore.Broker, 0, len(mc.Brokers))
	for _, b := range mc.Brokers {
		// Per-broker creds win; otherwise fall back to the shared subscriber
		// credentials (env-injected via PF__GRID__MESHCORE__USERNAME/PASSWORD).
		user, pass := b.Username, b.Password
		if user == "" {
			user = mc.Username
		}
		if pass == "" {
			pass = mc.Password
		}
		brokers = append(brokers, meshcore.Broker{
			URL:      b.URL,
			ClientID: b.ClientID,
			Username: user,
			Password: pass,
			Topics:   b.Topics,
			QoS:      b.QoS,
		})
	}
	// SpamFloor guards the relay-observation store from a fast-adverting node;
	// default 30s when unset (0 in yaml is treated as the default, not "disable" —
	// direct Config construction in tests can still disable it with a negative).
	spamFloor := mc.SpamFloor
	if spamFloor <= 0 {
		spamFloor = 30 * time.Second
	}
	return meshcore.Config{
		Brokers:               brokers,
		RequireValidSignature: mc.RequireValidSignature,
		RetainFor:             cfg.Grid.Sources["meshcore"].ExpireAfter,
		SpamFloor:             spamFloor,
	}
}

// meshMaintenanceConfig maps the grid.meshcore config onto the scheduler's
// relay-topology maintenance tick (compaction + prune). A disabled meshcore
// source returns a zero config (Interval 0), which turns the tick off entirely.
// Cadence/retention default when unset (docs/mesh-topology-design.md §10).
func meshMaintenanceConfig(cfg *config.Config) ingest.MeshMaintenance {
	mc := cfg.Grid.Meshcore
	if !mc.Enabled || len(mc.Brokers) == 0 {
		return ingest.MeshMaintenance{}
	}
	interval := mc.CompactionInterval
	if interval <= 0 {
		interval = time.Hour
	}
	obsRetention := mc.ObservationRetention
	if obsRetention <= 0 {
		obsRetention = 48 * time.Hour
	}
	rollupRetention := mc.RollupRetention
	if rollupRetention <= 0 {
		rollupRetention = 2 * 365 * 24 * time.Hour
	}
	return ingest.MeshMaintenance{
		Interval:             interval,
		ObservationRetention: obsRetention,
		RollupRetention:      rollupRetention,
	}
}

// gridSourceSeeds builds the source registry rows: ids + tuning from
// grid.sources config, names/attributions from the static registry (a
// config id without a registry entry is seeded with its id as the name so it
// still shows up in /api/v1/sources rather than failing silently).
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
