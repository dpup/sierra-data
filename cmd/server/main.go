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
	"github.com/dpup/sierra-data/internal/clients/calfireburn"
	"github.com/dpup/sierra-data/internal/clients/caloes"
	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/clients/census"
	"github.com/dpup/sierra-data/internal/clients/firis"
	"github.com/dpup/sierra-data/internal/clients/google"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/clients/nws"
	"github.com/dpup/sierra-data/internal/clients/pge"
	"github.com/dpup/sierra-data/internal/clients/usgs"
	"github.com/dpup/sierra-data/internal/clients/weather"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/gridapi"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/ingest"
	"github.com/dpup/sierra-data/internal/ingestapi"
	"github.com/dpup/sierra-data/internal/lib/alerts"
	"github.com/dpup/sierra-data/internal/lib/geojson"
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
	periodicRefresh := services.NewPeriodicRefreshService(roadsService, weatherService, appConfig)
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
	gridStore, err := store.Open(appConfig.Grid.DBPath,
		store.WithJournalMode(appConfig.Grid.JournalMode),
		store.WithCacheSizeMB(appConfig.Grid.CacheSizeMB),
		store.WithMaxOpenConns(appConfig.Grid.MaxOpenConns),
		store.WithLockTimeout(appConfig.Grid.LockTimeout),
		store.WithWildfireProximity(appConfig.Grid.Wildfire.PlaceBuffer()))
	if err != nil {
		logging.Errorw(ctx, "Failed to open grid store", "path", appConfig.Grid.DBPath, "error", err)
		log.Fatalf("Failed to open grid store: %v", err)
	}
	defer gridStore.Close()

	// Pragmas are per-connection, so this log line is the only place the store's
	// effective configuration is observable from outside the process.
	if st, err := gridStore.Settings(); err == nil {
		logging.Infow(ctx, "Grid store opened",
			"path", appConfig.Grid.DBPath, "journalMode", st.JournalMode,
			"synchronous", st.Synchronous, "cacheSizeKB", st.CacheSizeKB,
			"maxOpenConns", st.MaxOpenConns)
	}

	if err := gridStore.SeedSources(ctx, gridSourceSeeds(appConfig)); err != nil {
		logging.Errorw(ctx, "Failed to seed grid sources", "error", err)
		log.Fatalf("Failed to seed grid sources: %v", err)
	}
	retireOrphanedSources(ctx, gridStore)
	if err := places.Seed(ctx, gridStore, appConfig); err != nil {
		logging.Errorw(ctx, "Failed to seed grid places", "error", err)
		log.Fatalf("Failed to seed grid places: %v", err)
	}
	// Refresh index statistics after seeding. Without them the place-scoped
	// event query walks every attachment a place has ever had (see
	// store.Analyze). Fail-soft and timed: stale stats make queries slow, but a
	// failure here must never keep the service from starting, and the duration
	// is worth watching because this runs before the listener opens.
	analyzeStart := time.Now()
	if err := gridStore.Analyze(ctx); err != nil {
		logging.Warnw(ctx, "Failed to refresh grid store statistics; place-scoped queries may be slow",
			"error", err)
	} else {
		logging.Infow(ctx, "Refreshed grid store statistics", "duration", time.Since(analyzeStart))
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
		{Normalizer: ingest.NewWildfireNormalizer(appConfig, calfire.NewClient(), firis.NewClient()), Interval: gridPollInterval(appConfig, "calfire", "firis")},
		{Normalizer: ingest.NewEvacuationNormalizer(appConfig, caloes.NewClient()), Interval: gridPollInterval(appConfig, "caloes")},
		{Normalizer: ingest.NewWeatherAlertNormalizer(appConfig, weatherService), Interval: gridPollInterval(appConfig, "nws")},
		{Normalizer: ingest.NewRoadIncidentNormalizer(appConfig, roadsService), Interval: gridPollInterval(appConfig, "chp", "caltrans")},
		{Normalizer: ingest.NewPowerNormalizer(appConfig, pge.NewClient()), Interval: gridPollInterval(appConfig, "pge", "psps")},
	}

	// Burn status (optional): per-county residential burning status from two
	// independent authorities — the county air district's burn line and CAL
	// FIRE's seasonal suspension. Ambient state, polled a couple of times a day.
	if len(appConfig.Grid.Burn.Counties) > 0 {
		// The burn-day facet is PUSHED (see internal/ingestapi): the normalizer
		// reads the staging table the push endpoint writes, rather than fetching
		// an upstream. CAL FIRE is still a real fetch.
		pollers = append(pollers, ingest.PollerSpec{
			Normalizer: ingest.NewBurnStatusNormalizer(appConfig, calfireburn.NewClient(), gridStore, storePlaceIndex{gridStore}),
			Interval:   gridPollInterval(appConfig, "burnline", "calfire-burn"),
		})
	}

	// MeshCore mesh-node presence (optional): a long-lived MQTT subscriber to
	// community bridges accumulates node state; the NetworkNormalizer serves a
	// snapshot on the scheduler's tick. Enabled only when configured with brokers.
	if appConfig.Grid.Meshcore.Enabled && len(appConfig.Grid.Meshcore.Brokers) > 0 {
		meshcoreReg := meshcore.NewRegistry(meshcoreClientConfig(appConfig))
		// Rehydrate presence from the persisted store BEFORE connecting, so a
		// deploy doesn't drop the whole mesh to "unknown" (and let the sweep expire
		// the slow SIERRA repeaters) until every node re-adverts.
		seedMeshRegistry(ctx, meshcoreReg, gridStore)
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

	// Pushed burn-day readings. Disabled (404) unless a token is configured, so a
	// deployment without a credential cannot expose an unauthenticated write path
	// by omission.
	// The endpoint accepts a reading only for a CONFIGURED line id, so the
	// staging table can never accumulate rows no poller will read.
	burnLineIDs := make([]string, 0, len(appConfig.Grid.Burn.Lines))
	for _, l := range appConfig.Grid.Burn.Lines {
		burnLineIDs = append(burnLineIDs, l.ID)
	}
	burnLineHandler := ingestapi.NewBurnLineHandler(gridStore, appConfig.Grid.Burn.IngestToken, burnLineIDs)
	if burnLineHandler.Enabled() {
		logging.Info(ctx, "Burn-line push endpoint enabled at /ingest/burn-line")
	} else {
		logging.Info(ctx, "Burn-line push endpoint disabled (no grid.burn.ingestToken configured)")
	}

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
		// The service's ONLY write surface: pushed burn-day readings. Bearer-token
		// authenticated, 404s when no token is configured, and never writes events
		// (it stages a row the ingest scheduler picks up). See internal/ingestapi.
		prefab.WithHTTPHandlerFunc("/ingest/burn-line", burnLineHandler.ServeHTTP),
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

// retiredSourceIDs are source ids no poller declares anymore. The disappearance
// sweep only runs for a live poller's SourceIDs(), so ACTIVE events left behind by
// a removed/renamed source (e.g. the standalone "wfigs:" perimeters after the
// wfigs→firis swap) would otherwise persist ACTIVE forever — stale duplicates on
// the fire map beside the fresh "firis:" events. Retire them once at boot.
// Entries may be dropped after a deploy has drained them (the transition is
// idempotent — once expired, ActiveEventsBySource returns none).
var retiredSourceIDs = []string{"wfigs"}

// retireOrphanedSources expires any ACTIVE/SCHEDULED events still attached to a
// retired source (a proper EXPIRED revision, history preserved) and then removes
// the defunct source registry row so /api/v1/sources doesn't list it forever.
// Best effort: a failure logs and continues — it must never block startup.
// Idempotent: once drained, ActiveEventsBySource returns none and the delete is a
// no-op.
func retireOrphanedSources(ctx context.Context, st *store.Store) {
	for _, src := range retiredSourceIDs {
		evs, err := st.ActiveEventsBySource(ctx, src)
		if err != nil {
			logging.Warnw(ctx, "Could not scan retired source for orphaned events", "source", src, "error", err)
			continue
		}
		if len(evs) > 0 {
			ids := make([]string, len(evs))
			for i, e := range evs {
				ids[i] = e.Event.GetId()
			}
			if err := st.TransitionEvents(ctx, ids, gridv1.EventStatus_EXPIRED, time.Now()); err != nil {
				logging.Warnw(ctx, "Could not retire orphaned events for retired source", "source", src, "count", len(ids), "error", err)
				continue
			}
			logging.Infow(ctx, "Retired orphaned events for a removed source", "source", src, "count", len(ids))
		}
		if err := st.DeleteSource(ctx, src); err != nil {
			logging.Warnw(ctx, "Could not remove retired source registry row", "source", src, "error", err)
		}
	}
}

// gridSourceInfo is the static registry of source display names, attributions
// and upstream homepages.
//
// These describe THE FEED, which is a different question from an event's
// provenance, and conflating the two is what made the sources board read as
// duplicated. An event's Provenance credits every source that contributed to
// THAT event — a `calfire:` incident carrying an adopted FIRIS perimeter is
// legitimately attributed "CAL FIRE / FIRIS" — whereas the row below describes
// only the CAL FIRE active-incidents feed, which is not FIRIS (that has its own
// row). The two must agree on WHO a source is; they need not carry the same
// string, and where they differ the divergence is deliberate.
//
// Names disambiguate feeds that share an upstream. calfire/firis, pge/psps and
// chp/caltrans are each one poller covering two source rows, so every other cell
// on the board — poll interval, thresholds, last success to the second — is
// identical within a pair; the name is the only thing that can tell them apart.
// Keep the organization recognizable and put the feed's subject in parentheses.
var gridSourceInfo = map[string]struct{ name, attribution, homepage string }{
	"usgs":     {"USGS Earthquakes", "U.S. Geological Survey", "https://earthquake.usgs.gov/earthquakes/map/"},
	"calfire":  {"CAL FIRE (active incidents)", "CAL FIRE", "https://incidents.fire.ca.gov"},
	"firis":    {"FIRIS (fire perimeters)", "CAL FIRE / FIRIS / NIFC", firisProgramURL},
	"caloes":   {"Cal OES (evacuation zones)", "Cal OES — reference only", caloes.SourceURL},
	"nws":      {"National Weather Service (Sacramento)", "NOAA / National Weather Service", "https://www.weather.gov/sto"},
	"chp":      {"CHP (traffic incidents)", "quickmap.dot.ca.gov", quickMapURL},
	"caltrans": {"Caltrans (chain control + lane closures)", "quickmap.dot.ca.gov", quickMapURL},
	"meshcore": {"MeshCore Mesh", "MeshCore community mesh", "https://map.meshcore.io"},
	"pge":      {"PG&E (electric outages)", "Pacific Gas and Electric", pge.OutageMapURL},
	"psps":     {"PG&E (public safety power shutoffs)", "Pacific Gas and Electric", pge.PSPSUpdatesURL},
	// Two authorities gate a legal burn and both are carried; see
	// internal/ingest/burn_status.go. The burn line row covers every configured
	// county's line, so its name stays generic.
	"burnline":     {"County burn line (burn day)", "County air district burn information line", burnDayHomepage},
	"calfire-burn": {"CAL FIRE (burn permit suspension)", "CAL FIRE", calfireBurnHomepage},
}

// Homepages used by more than one source row, or that have no constant of their
// own in a client package. Human-facing pages — the site links the source name
// at them — not the machine endpoints the clients fetch.
const (
	quickMapURL = "https://quickmap.dot.ca.gov/"
	// FIRIS data reaches us through an ArcGIS feature service with no landing
	// page; Cal OES runs the program, so its page is the honest upstream.
	firisProgramURL = "https://www.caloes.ca.gov/office-of-the-director/operations/response-operations/fire-rescue/firis/"
	// The public page for the burn-day readings we consume. The AUTHORITY is the
	// county's recorded phone line, which has no URL — each event carries the
	// number in burn_status.burnLinePhone so a reader can always reach it.
	burnDayHomepage = "https://burnday.ersn.net"
	// CAL FIRE's own burn status table.
	calfireBurnHomepage = "https://burnpermit.fire.ca.gov/current-burn-status"
)

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
	graceCeil := mc.GraceCeil
	if graceCeil <= 0 {
		graceCeil = 72 * time.Hour
	}
	// RetainFor (in-memory node retention) is anchored to the presence ceiling, NOT
	// the source's expireAfter: expireAfter is now the short disappearance-sweep
	// safety net, while a node must live in memory for its whole cadence-derived
	// presence window (up to GraceCeil). NewRegistry defaults the remaining knobs.
	return meshcore.Config{
		Brokers:               brokers,
		RequireValidSignature: mc.RequireValidSignature,
		RetainFor:             graceCeil,
		SpamFloor:             spamFloor,
		CadenceK:              mc.CadenceK,
		GraceFloor:            mc.GraceFloor,
		GraceCeil:             graceCeil,
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

// seedMeshRegistry rehydrates the MeshCore Registry from the persisted store so
// node presence survives a restart. It seeds every ACTIVE/SCHEDULED network node
// (identity + location + last-seen) and replays each node's recent advert times
// (from the observation store) to reconstruct its cadence, so the per-node
// presence window is right immediately after boot. Best-effort: a store read
// failure logs and leaves the Registry to refill from live adverts (the prior
// behavior), never blocks startup.
func seedMeshRegistry(ctx context.Context, reg *meshcore.Registry, st *store.Store) {
	events, err := st.ActiveEventsBySource(ctx, "meshcore")
	if err != nil {
		logging.Warnw(ctx, "MeshCore: presence rehydration skipped — loading active nodes failed", "error", err)
		return
	}
	if len(events) == 0 {
		return
	}
	heard, err := st.MeshNodeHeardTimes(ctx, 40)
	if err != nil {
		logging.Warnw(ctx, "MeshCore: cadence rehydration skipped — reading heard times failed", "error", err)
		heard = nil
	}
	seeds := make([]meshcore.SeedNode, 0, len(events))
	for _, se := range events {
		d := se.Event.GetMesh()
		pk := d.GetPublicKey()
		if pk == "" {
			continue
		}
		sn := meshcore.SeedNode{
			PubKey:     pk,
			Role:       d.GetNodeType(),
			Name:       d.GetName(),
			HeardTimes: heard[pk],
			LastHeard:  se.LastSeenAt,
		}
		if c := se.Event.GetGeometry().GetCentroid(); c != nil {
			sn.HasLocation = true
			sn.Lat, sn.Lng = c.GetLat(), c.GetLng()
		}
		if sn.LastHeard.IsZero() {
			if ts := se.Event.GetObservedAt(); ts != nil {
				sn.LastHeard = ts.AsTime()
			}
		}
		seeds = append(seeds, sn)
	}
	reg.Seed(seeds)
	logging.Infow(ctx, "MeshCore: rehydrated node presence from store", "nodes", len(seeds))
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
			HomepageURL:   info.homepage,
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

// storePlaceIndex resolves a county's place ids from the seeded place directory,
// satisfying ingest.PlaceIndex.
type storePlaceIndex struct{ st *store.Store }

// PlaceIDsForCounty returns the county place plus every place the county's
// status is true of: towns (parented to their containing county by the seeder)
// and any AREA whose coverage overlaps it.
//
// This is what makes a burn status attach without geometry. A query for a town
// ("?place=arnold") must see its county's burn status — the status is true of
// the whole county by definition, so resolving membership once at ingest is
// both cheaper and more accurate than writing an 18-32 KB county polygon into
// every event and every revision of it.
//
// AREAs need the geometric test because they are deliberately NOT parented to a
// county: the coverage footprint spans several (that is why AREA is its own
// place kind). Without this the deployment's own place — the one
// /places/ebbetts-pass/summary reports on — would show an empty burn domain
// while both its counties had a status.
//
// CORRIDORs are excluded: a corridor is a road, and burn status is a fact about
// land, not about the highway crossing it.
func (s storePlaceIndex) PlaceIDsForCounty(ctx context.Context, countySlug string) ([]string, error) {
	county, err := s.st.GetPlace(ctx, countySlug)
	if err != nil {
		return nil, err
	}
	ids := []string{county.GetId()}

	var countyGeom *geojson.Geom
	if raw := county.GetGeometry().GetGeojson(); len(raw) > 0 {
		countyGeom, _ = geojson.Parse(raw)
	}

	all, err := s.st.ListPlaces(ctx, gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED, "")
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.GetId() == county.GetId() {
			continue
		}
		switch {
		case p.GetParentId() == county.GetId():
			ids = append(ids, p.GetId())
		case p.GetKind() == gridv1.PlaceKind_AREA && countyGeom != nil:
			raw := p.GetGeometry().GetGeojson()
			if len(raw) == 0 {
				continue
			}
			g, err := geojson.Parse(raw)
			if err != nil {
				continue
			}
			if geojson.Intersects(countyGeom, g) {
				ids = append(ids, p.GetId())
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}
