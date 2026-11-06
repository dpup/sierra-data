package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

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
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/dpup/sierra-data/internal/clients/firis"
	"github.com/dpup/sierra-data/internal/clients/google"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/clients/nws"
	"github.com/dpup/sierra-data/internal/clients/pge"
	"github.com/dpup/sierra-data/internal/clients/usgs"
	"github.com/dpup/sierra-data/internal/clients/weather"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/gridapi"
	"github.com/dpup/sierra-data/internal/handoff"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/ingest"
	"github.com/dpup/sierra-data/internal/lib/alerts"
	"github.com/dpup/sierra-data/internal/mcp"
	"github.com/dpup/sierra-data/internal/places"
	"github.com/dpup/sierra-data/internal/pushingest"
	"github.com/dpup/sierra-data/internal/services"
	"github.com/dpup/sierra-data/internal/store"
)

// gridApp is the Grid as internal/handoff drives it. prepareGrid builds
// everything that does not touch grid.db, which is all a STANDBY process has;
// Activate opens the store and starts production service; Quiesce undoes
// that, in an order that ends with the store closed and its lock released.
type gridApp struct {
	ctx context.Context // carries the logger; never cancelled
	cfg *config.Config

	// Prepared: no database, no goroutines that matter to a handoff.
	cacheInstance  *cache.Cache
	caltransClient *caltrans.FeedParser
	cwClient       *cwwp2.Client
	roadsService   *services.RoadsService
	weatherService *services.WeatherService
	hazards        *hazards.Service
	nwsEnhancer    ingest.NWSEnhancer
	model          string

	// onFatal is the controller's Fail: the production server stopping after
	// activation is fatal, as it was before handoff (main sets it).
	onFatal func(error)
	// activateHook, when set, runs at named points in Activate (tests inject
	// failures and panics there).
	activateHook func(stage string)

	// Active: set by Activate, torn down by Quiesce.
	mu         sync.Mutex
	gate       *handoff.Gate
	runCancel  context.CancelFunc // ends the scheduler, periodic refresh, cameras, MQTT, the neighbour rebuild
	store      *store.Store
	scheduler  *ingest.Scheduler
	meshReg    *meshcore.Registry
	bg         sync.WaitGroup // background goroutines that read the store
	server     *prefab.Server
	serverDone chan struct{} // closed when prefab's Start returns
	serverErr  error         // its result, valid once serverDone is closed

	quiesced struct{ drained, workers, mesh, bg, store bool }
}

// prepareGrid builds the database-free half of the Grid: config-derived
// clients and the read-path services. Nothing here opens grid.db, and nothing
// here starts upstream polling (that waits for activation, so a standby never
// spends API budget). Invalid configuration fails here, before any handoff.
func prepareGrid(ctx context.Context, appConfig *config.Config) (*gridApp, error) {
	start := time.Now()
	if appConfig.Grid.DBPath == "" {
		return nil, errors.New("grid.dbPath is required (default ./data/grid.db in prefab.yaml)")
	}
	if appConfig.OpenAI.APIKey == "" {
		return nil, errors.New("OpenAI API key is required in configuration for incident enhancement")
	}
	// The push-ingest block is validated now rather than at activation: a bad
	// reporter hash must stop a standby before it is offered as a replacement.
	if _, err := pushingest.NewRegistry(appConfig.Grid.Ingest); err != nil {
		return nil, fmt.Errorf("invalid grid.ingest configuration: %w", err)
	}

	a := &gridApp{ctx: ctx, cfg: appConfig, model: appConfig.OpenAI.Model}

	// Periodic cleanup evicts very-stale entries so keys that are never
	// overwritten (content-hash AI-enhancement entries) don't accumulate
	// forever. In-memory only.
	a.cacheInstance = cache.NewCache()
	a.cacheInstance.StartPeriodicCleanup(ctx, time.Hour)

	googleClient := google.NewClient(appConfig.GoogleRoutes.APIKey)
	a.caltransClient = caltrans.NewFeedParser()
	cwCfg := appConfig.Roads.CaltransFeeds.CWWP2
	a.cwClient = cwwp2.NewClient()
	if cwCfg.BaseURL != "" {
		a.cwClient.BaseURL = cwCfg.BaseURL
	}
	if cwCfg.StaleAfter > 0 {
		a.cwClient.StaleAfter = cwCfg.StaleAfter
	}
	if len(cwCfg.ChainControlDistricts) > 0 {
		a.caltransClient.UseCWWP2ChainControls(a.cwClient, cwCfg.ChainControlDistricts)
	}

	// Create OpenAI enhancer (caching is integrated directly in services).
	// Weather alerts are NWS-sourced with authoritative wording and are not
	// AI-enhanced; only road alerts use the enhancer.
	alertEnhancer := alerts.NewAlertEnhancer(appConfig.OpenAI.APIKey, a.model)
	logging.Infow(ctx, "OpenAI enhancement enabled", "model", a.model, "caching", "content-based")

	a.roadsService = services.NewRoadsService(googleClient, a.caltransClient, a.cacheInstance, appConfig, alertEnhancer)
	// Segment status reads CWWP2 lane closures (set-up windows only) when
	// laneClosureDistricts is set, so it agrees with the road_incident layer.
	a.roadsService.UseCWWP2LaneClosures(a.cwClient)
	a.weatherService = services.NewWeatherService(weather.NewClient(appConfig.OpenWeather.APIKey),
		nws.NewClient(appConfig.Weather.NWS.UserAgent), a.cacheInstance, appConfig)

	// Hazard condition-layer projector: gridapi calls BuildLayer for the live
	// condition layers (road_segment, chain_control, fire_weather,
	// message_sign). The event layers are projected from the grid store by
	// gridapi itself.
	a.hazards = hazards.NewServiceWithAPIs(appConfig, a.roadsService, a.weatherService, a.caltransClient, a.cacheInstance)
	a.hazards.UseMessageSigns(a.cwClient, cwCfg.MessageSignDistricts)

	// NWS weather-alert enhancement is optional: nil when disabled or keyless
	// (the scheduler then serves raw alerts — enhancement never gates ingest).
	if appConfig.Grid.Enhancement.Enabled {
		a.nwsEnhancer = ingest.NewNWSEnhancer(appConfig.OpenAI)
	}

	logging.Infow(ctx, "Grid prepared (no database opened)",
		"roads_monitored", len(appConfig.Roads.MonitoredRoads),
		"weather_locations", len(appConfig.Weather.Locations),
		"duration", time.Since(start))
	return a, nil
}

// Activate opens grid.db and starts production service. On any error it tears
// down what it started; if that teardown cannot release the database it says
// so (handoff.ErrStillHeld), and the controller keeps the database counted as
// held.
func (a *gridApp) Activate(actx context.Context) (err error) {
	ctx := a.ctx
	// Everything below logs through actx too; carry the process logger on it
	// whatever the caller passed (a logger-less context panics in prefab's
	// logging, which once crash-looped production at the first mesh log line).
	actx = handoff.WithLoggerOf(actx, ctx)
	appConfig := a.cfg
	activateStart := time.Now()

	a.mu.Lock()
	// Fresh per attempt: a gate never reopens, and a retry after a failed
	// activation must not see the previous attempt's (released) resources.
	a.gate = handoff.NewGate()
	a.store, a.scheduler, a.meshReg = nil, nil, nil
	a.quiesced = struct{ drained, workers, mesh, bg, store bool }{}
	runCtx, runCancel := context.WithCancel(ctx)
	a.runCancel = runCancel
	a.mu.Unlock()
	defer func() {
		// A panic is a failed activation like any other: release what was
		// opened rather than leave it running behind a "held" status.
		if r := recover(); r != nil {
			err = fmt.Errorf("panic during activation: %v\n%s", r, debug.Stack())
		}
		if err == nil {
			return
		}
		logging.Warnw(ctx, "Activation failed; releasing what it opened", "error", err)
		if qerr := a.Quiesce(ctx); qerr != nil {
			err = fmt.Errorf("%w (and releasing it failed: %w: %w)", err, handoff.ErrStillHeld, qerr)
		}
	}()

	// Grid event store + ingest scheduler (docs/design/v2-implementation-plan.md):
	// normalized hazard events persisted with revision history, per-source
	// health, and the place directory — the /api/v1 foundation.
	openStart := time.Now()
	gridStore, err := store.Open(appConfig.Grid.DBPath,
		store.WithJournalMode(appConfig.Grid.JournalMode),
		store.WithCacheSizeMB(appConfig.Grid.CacheSizeMB),
		store.WithMaxOpenConns(appConfig.Grid.MaxOpenConns),
		store.WithLockTimeout(appConfig.Grid.LockTimeout),
		// A SIGTERM during the lock wait ends it at once.
		store.WithLockContext(actx),
		store.WithWildfireProximity(appConfig.Grid.Wildfire.PlaceBuffer()))
	if err != nil {
		return fmt.Errorf("open grid store %s: %w", appConfig.Grid.DBPath, err)
	}
	a.mu.Lock()
	a.store = gridStore
	a.mu.Unlock()
	a.hook("store-opened")

	// Pragmas are per-connection, so this log line is the only place the store's
	// effective configuration is observable from outside the process.
	if st, err := gridStore.Settings(); err == nil {
		logging.Infow(ctx, "Grid store opened",
			"path", appConfig.Grid.DBPath, "journalMode", st.JournalMode,
			"synchronous", st.Synchronous, "cacheSizeKB", st.CacheSizeKB,
			"maxOpenConns", st.MaxOpenConns, "duration", time.Since(openStart))
	}
	if err := actx.Err(); err != nil {
		return err
	}

	seedStart := time.Now()
	if err := gridStore.SeedSources(actx, gridSourceSeeds(appConfig)); err != nil {
		return fmt.Errorf("seed grid sources: %w", err)
	}
	retireOrphanedSources(actx, gridStore)
	if err := places.Seed(actx, gridStore, appConfig); err != nil {
		return fmt.Errorf("seed grid places: %w", err)
	}
	logging.Infow(ctx, "Seeded grid sources and places", "duration", time.Since(seedStart))

	// Index statistics. Without them the place-scoped event query walks every
	// attachment a place has ever had (see store.Analyze). They persist in the
	// database file, so only a database that has never been analyzed needs a
	// synchronous ANALYZE here (cheap: it is new or small). Otherwise the
	// previous process's statistics stand, and the scheduler refreshes them
	// shortly after boot: a full ANALYZE measured 59.5s on the EFS store, and
	// running it before the listener opened was most of a deploy's 502 window.
	// Fail-soft either way: stale stats make queries slow, never wrong.
	var statsRefreshFirstAfter time.Duration
	if has, err := gridStore.HasStatistics(actx); err == nil && has {
		statsRefreshFirstAfter = bootStatsRefreshDelay
		logging.Infow(ctx, "Using persisted grid store statistics; refresh deferred",
			"refreshIn", bootStatsRefreshDelay)
	} else {
		analyzeStart := time.Now()
		if err := gridStore.Analyze(actx); err != nil {
			logging.Warnw(ctx, "Failed to refresh grid store statistics; place-scoped queries may be slow",
				"error", err)
		} else {
			logging.Infow(ctx, "Refreshed grid store statistics", "duration", time.Since(analyzeStart))
		}
	}
	if err := actx.Err(); err != nil {
		return err
	}

	// One poller per upstream scope; weather_alert and road_incident reuse the
	// services' cached, budgeted pipelines (plan decision 6).
	roadIncidents := ingest.NewRoadIncidentNormalizer(appConfig, a.roadsService)
	pollers := []ingest.PollerSpec{
		{Normalizer: ingest.NewEarthquakeNormalizer(appConfig, usgs.NewClient()), Interval: gridPollInterval(appConfig, "usgs")},
		{Normalizer: ingest.NewWildfireNormalizer(appConfig, calfire.NewClient(), firis.NewClient()), Interval: gridPollInterval(appConfig, "calfire", "firis")},
		{Normalizer: ingest.NewEvacuationNormalizer(appConfig, caloes.NewClient()), Interval: gridPollInterval(appConfig, "caloes")},
		{Normalizer: ingest.NewWeatherAlertNormalizer(appConfig, a.weatherService), Interval: gridPollInterval(appConfig, "nws")},
		// chp alone, or chp + caltrans when lane closures still come from
		// lcs2way.kml (see RoadIncidentNormalizer.SourceIDs).
		{Normalizer: roadIncidents, Interval: gridPollInterval(appConfig, roadIncidents.SourceIDs()...)},
		{Normalizer: ingest.NewPowerNormalizer(appConfig, pge.NewClient()), Interval: gridPollInterval(appConfig, "pge", "psps")},
	}
	// Caltrans lane closures from CWWP2, scheduled windows included. Its own
	// poller on the caltrans interval rather than the CHP one: the two district
	// files are ~5 MB together, uncompressed, and the portal does not gzip.
	if len(appConfig.Roads.CaltransFeeds.CWWP2.LaneClosureDistricts) > 0 {
		pollers = append(pollers, ingest.PollerSpec{
			Normalizer: ingest.NewLaneClosureNormalizer(appConfig, a.cwClient),
			Interval:   gridPollInterval(appConfig, "caltrans"),
		})
	}

	// Authenticated push ingest (optional): operator-run monitors POST to
	// /api/v1/ingest/{stream}. The block was validated in prepareGrid, so an
	// error here is unexpected rather than a config typo.
	burnLineIDs := make([]string, 0, len(appConfig.Grid.Burn.Lines))
	for _, l := range appConfig.Grid.Burn.Lines {
		burnLineIDs = append(burnLineIDs, l.ID)
	}
	// An accepted burn reading wakes the burn poller, so the event shows it
	// right away instead of on the next hourly tick.
	burnWake, wakeBurn := ingest.NewWake()
	pushRegistry, err := pushingest.NewRegistry(appConfig.Grid.Ingest,
		pushingest.WithBurnLines(gridStore, burnLineIDs),
		pushingest.WithBurnAccepted(wakeBurn))
	if err != nil {
		return fmt.Errorf("invalid grid.ingest configuration: %w", err)
	}
	for _, id := range pushRegistry.MissingTokens() {
		logging.Warnw(ctx, "Push reporter has no token hash; nothing can authenticate as it",
			"reporter", id, "env", "PF__GRID__INGEST__REPORTER_TOKEN_SHA256__"+strings.ToUpper(id))
	}
	// Reporter health is in-memory; without this every deploy reports the
	// once-a-day burn-line reporter as never having reported until its next run.
	if err := pushRegistry.RestoreBurnHealth(actx); err != nil {
		logging.Warnw(ctx, "Could not restore burn reporter health", "error", err)
	}

	// Burn status (optional): per-county residential burning status from two
	// independent authorities — the county air district's burn line and CAL
	// FIRE's seasonal suspension. Ambient state, polled a couple of times a day.
	if len(appConfig.Grid.Burn.Counties) > 0 {
		// The burn-day facet is PUSHED (the `burn.line` stream on
		// /api/v1/ingest): the normalizer reads the staging table the push
		// handler writes, rather than fetching an upstream. CAL FIRE is still a
		// real fetch.
		pollers = append(pollers, ingest.PollerSpec{
			Normalizer: ingest.NewBurnStatusNormalizer(appConfig, calfireburn.NewClient(), gridStore,
				storePlaceIndex{gridStore}, burnReporters(pushRegistry)),
			Interval: gridPollInterval(appConfig, "burnline", "calfire-burn"),
			Wake:     burnWake,
		})
	}

	// Mesh-node presence (optional). TWO possible inputs, and the poller runs if
	// EITHER is configured: the MeshCore MQTT subscriber (community bridges,
	// broadcast adverts) and the push-ingest buffer (operator monitors reading
	// repeater admin interfaces). The NetworkNormalizer merges them into one event
	// per node on the scheduler's tick.
	//
	// The MeshCore registry itself has two doors: the MQTT brokers, and the
	// mesh.packet push stream, through which an operator's companion radio
	// forwards the raw adverts it hears (a companion in Arnold hears SIERRA
	// nodes no MQTT observer does). Either door justifies the registry; only
	// the brokers need a connection.
	var meshcoreReg ingest.MeshRegistry
	mqttEnabled := appConfig.Grid.Meshcore.Enabled && len(appConfig.Grid.Meshcore.Brokers) > 0
	packetReporters := pushRegistry.ReporterIDs(pushingest.MeshPacketStream)
	if mqttEnabled || len(packetReporters) > 0 {
		reg := meshcore.NewRegistry(meshcoreClientConfig(appConfig))
		// Rehydrate presence from the persisted store BEFORE connecting, so a
		// deploy doesn't drop the whole mesh to "unknown" (and let the sweep expire
		// the slow SIERRA repeaters) until every node re-adverts.
		if seedMeshRegistry(actx, reg, gridStore) {
			// The neighbour graph is rebuilt in the background: replaying 48h
			// of receptions measured 14.4s over EFS, and nothing needs it
			// before the listener opens. It reads the store, so it is a
			// background database user: Quiesce cancels it and waits.
			a.bg.Add(1)
			go func() {
				defer a.bg.Done()
				seedMeshNeighbours(runCtx, reg, gridStore)
			}()
		}
		if mqttEnabled {
			if err := reg.Connect(runCtx); err != nil {
				logging.Errorw(ctx, "Failed to start MeshCore subscriber", "error", err)
			} else {
				logging.Infow(ctx, "MeshCore subscriber started", "brokers", len(appConfig.Grid.Meshcore.Brokers))
			}
		}
		a.mu.Lock()
		a.meshReg = reg
		a.mu.Unlock()
		if len(packetReporters) > 0 {
			pushRegistry.SetPacketSink(reg)
			logging.Infow(ctx, "MeshCore packet forwarding enabled", "reporters", packetReporters)
		}
		meshcoreReg = reg
	}
	var meshPush ingest.MeshPushSource
	if pushRegistry.Enabled() {
		meshPush = pushRegistry
		logging.Infow(ctx, "Push ingest enabled",
			"reporters", pushRegistry.ReporterIDs(pushingest.MeshStream),
			"packetReporters", packetReporters)
	}
	if meshcoreReg != nil || meshPush != nil {
		meshSources := []string{"meshcore"}
		seen := map[string]bool{"meshcore": true}
		for _, id := range append(pushRegistry.ReporterIDs(pushingest.MeshStream), packetReporters...) {
			if !seen[id] {
				seen[id] = true
				meshSources = append(meshSources, id)
			}
		}
		pollers = append(pollers, ingest.PollerSpec{
			Normalizer: ingest.NewNetworkNormalizer(appConfig, meshcoreReg, meshPush).
				WithCatalog(func(ctx context.Context) ([]*gridv1.Event, error) {
					return gridStore.EventsBySourceAndStatus(ctx, "meshcore", gridv1.EventStatus_EXPIRED)
				}),
			Interval: gridPollInterval(appConfig, meshSources...),
		})
	}
	if err := actx.Err(); err != nil {
		return err
	}

	scheduler := ingest.NewScheduler(gridStore, ingest.SchedulerConfig{
		Pollers:         pollers,
		Tuning:          appConfig.Grid.Sources,
		Enhancer:        a.nwsEnhancer,
		EnhancerModel:   a.model,
		BudgetPerTick:   appConfig.Grid.Enhancement.BudgetPerTick,
		MeshMaintenance: meshMaintenanceConfig(appConfig),

		StatsRefreshFirstAfter: statsRefreshFirstAfter,
	})
	scheduler.Start(runCtx)
	a.mu.Lock()
	a.scheduler = scheduler
	a.mu.Unlock()

	// Upstream cache warming starts with production service, not in standby:
	// a standby must not double the Google Routes / OpenWeather spend.
	periodicRefresh := services.NewPeriodicRefreshService(a.roadsService, a.weatherService, appConfig)
	if err := periodicRefresh.StartPeriodicRefresh(runCtx); err != nil {
		logging.Errorw(ctx, "Failed to start periodic refresh", "error", err)
	}

	// Grid API service: backs the /api/v1 GridService RPCs and the hand-built
	// summary + .geojson gateway routes (the census geocoder powers
	// /api/v1/places:resolve?address=).
	gridapiService := gridapi.NewService(gridStore, a.weatherService, census.NewClient(), appConfig, a.hazards)

	// Caltrans CCTV camera directory (GET /api/v1/cameras). The lists are
	// near-static, so they refresh in the background and a request never waits
	// on Caltrans. Without districts the RPC answers UNAVAILABLE.
	if cams := appConfig.Roads.CaltransFeeds.CWWP2.Cameras; len(cams.Districts) > 0 {
		camClient := cwwp2.NewClient()
		if base := appConfig.Roads.CaltransFeeds.CWWP2.BaseURL; base != "" {
			camClient.BaseURL = base
		}
		cameraService := services.NewCameraService(camClient, cams.Districts, cams.RefreshInterval)
		go cameraService.Run(runCtx)
		gridapiService.Cameras = cameraService
		logging.Infow(ctx, "Camera directory enabled",
			"districts", cams.Districts, "nearMeters", cams.Near())
	}

	// MCP endpoint (docs/design/mcp-design.md): read-only tools for LLM agents over
	// Streamable HTTP. The tools call the /api/v1 surface in-process against the
	// gRPC-Gateway mux, which only exists after prefab.New wires the gateway — so
	// MCP holds a deferred handler we point at that mux below.
	gatewayMux := &deferredHandler{}
	mcpHandler := mcp.NewHandler(gatewayMux)

	// GridService: the proto-defined /api/v1 entity/query surface over
	// gRPC-Gateway (docs/design/grpc-gateway-migration-plan.md). Gateway annotations
	// mount under /api/, which Prefab already serves.
	gridServer := gridapi.NewGridServer(gridapiService)

	// Every route that can reach the store goes through the admission gate,
	// which is how Quiesce stops admitting (prefab's listener cannot be
	// stopped short of a signal). gRPC covers every gateway-routed RPC, since
	// the gateway reaches the service over a loopback gRPC connection; the
	// hand-mounted routes are wrapped one by one.
	gate := a.gate
	gated := gateRuntime(gate)

	// Prefab server. gRPC + gateway serve the /api/v1 GridService; MCP and the
	// static site are plain HTTP handlers. Its base context is the process
	// context, NOT runCtx: cancelling it closes the gateway's loopback gRPC
	// connection, and a released Grid then answers 499 (cancelled) where it
	// should answer the gate's 503. In-flight cancellation is the gate's job.
	server := prefab.New(
		prefab.WithContext(ctx),
		prefab.WithGRPCReflection(),
		prefab.WithGRPCInterceptor(gate.UnaryInterceptor()),
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
			if err := gridServer.RegisterGatewayRoutes(mux, gated); err != nil {
				return err
			}
			// The one WRITE route on /api/v1, mounted only when a reporter is
			// configured so the default build has no write surface at all. It is
			// hand-mounted rather than proto-defined because it takes a foreign
			// document verbatim (whatever shape a monitor already produces), and
			// deliberately bypasses the ETag/Cache-Control interceptors, which
			// exist for GETs.
			//
			// CORS keeps this browser-unreachable cross-origin: corsAllowMethods is
			// [GET], so the POST preflight is denied — the same property that
			// protects /mcp. Do not add POST to that list.
			if pushRegistry.Enabled() {
				if err := mux.HandlePath("POST", "/api/v1/ingest/{stream}",
					gated(func(w http.ResponseWriter, r *http.Request, pp map[string]string) {
						pushRegistry.ServeStream(w, r, pp["stream"])
					})); err != nil {
					return err
				}
			}
			// Point MCP at the fully-wired gateway so its tools query /api/v1
			// in-process (same mux prefab serves at /api/).
			gatewayMux.h = mux
			return nil
		}),
		prefab.WithHTTPHandlerFunc("/mcp", gate.HandlerFunc(mcpHandler.ServeHTTP)),
		// Publish the generated OpenAPI spec for /api/v1 (protoc-gen-openapiv2).
		// Exact path, so it wins over the gateway's /api/ subtree mount.
		prefab.WithHTTPHandlerFunc("/api/openapi.json", gate.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(gridv1.OpenAPISpec)
		})),
		prefab.WithHTTPHandlerFunc("/", gate.HandlerFunc(siteHandler)),
	)

	// Everything above runs before the listener opens, so during a deploy it
	// is time the load balancer has no healthy target. Keep it short, and keep
	// it measured.
	logging.Infow(ctx, "Server initialization complete, starting HTTP services",
		"activation", time.Since(activateStart), "sinceProcessStart", time.Since(processStart))

	// Our listener must be the one WaitListening finds: refuse to start on a
	// port something else already holds.
	port := prefab.ConfigInt("server.port")
	if err := handoff.PortFree(port); err != nil {
		return err
	}
	done := make(chan struct{})
	a.mu.Lock()
	a.server, a.serverDone = server, done
	a.mu.Unlock()
	// prefab's Start blocks until its own SIGTERM handler has shut it down,
	// which is also the only way to stop its listener.
	go func() {
		err := server.Start()
		a.mu.Lock()
		a.serverErr = err
		a.mu.Unlock()
		close(done)
	}()
	if err := handoff.WaitListening(actx, port, done, 15*time.Second); err != nil {
		a.mu.Lock()
		serr := a.serverErr
		a.mu.Unlock()
		if serr != nil {
			err = fmt.Errorf("%w: %w", err, serr)
		}
		return fmt.Errorf("production listener: %w", err)
	}
	// prefab's Start returns nil only after its own SIGTERM shutdown. An error
	// after this point means production stopped serving while we still hold
	// grid.db, which the pre-handoff main treated as fatal.
	go func() {
		<-done
		a.mu.Lock()
		serr := a.serverErr
		a.mu.Unlock()
		if serr != nil && a.onFatal != nil {
			a.onFatal(fmt.Errorf("production server stopped: %w", serr))
		}
	}()
	return nil
}

func (a *gridApp) hook(stage string) {
	if a.activateHook != nil {
		a.activateHook(stage)
	}
}

// Quiesce releases grid.db. In order: stop admitting and drain requests, stop
// the ingest scheduler, disconnect MQTT, cancel and wait for the remaining
// background work, close the store (which releases its lock). Each step runs
// once; a retry after a failure resumes at the step that failed.
func (a *gridApp) Quiesce(_ context.Context) error {
	ctx := a.ctx
	ctl := a.cfg.Grid.Control
	start := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.quiesced.drained && a.gate != nil {
		res := a.gate.Drain(ctl.Drain(), 2*time.Second)
		logging.Infow(ctx, "Quiesce: request drain complete",
			"inFlight", res.InFlight, "cancelled", res.Cancelled, "remaining", res.Remaining,
			"duration", res.Duration)
		if res.Remaining > 0 {
			// Not fatal: closing the store below is the barrier. A handler
			// that outlives it gets "database is closed", never a query.
			logging.Warnw(ctx, "Quiesce: requests ignored cancellation; the store close will cut them off",
				"remaining", res.Remaining)
		}
	}
	a.quiesced.drained = true

	// One budget for every background worker, so the worst case (drain +
	// this) stays inside ECS's default 30s stop timeout.
	workersDeadline := time.Now().Add(ctl.WorkerStop())

	if !a.quiesced.workers && a.scheduler != nil {
		t := time.Now()
		if err := a.scheduler.Stop(2*time.Second, max(time.Until(workersDeadline)-2*time.Second, 0)); err != nil {
			logging.Errorw(ctx, "Quiesce: ingest scheduler did not stop", "error", err, "duration", time.Since(t))
			return err
		}
		logging.Infow(ctx, "Quiesce: ingest scheduler stopped", "duration", time.Since(t))
	}
	a.quiesced.workers = true

	if !a.quiesced.mesh && a.meshReg != nil {
		t := time.Now()
		a.meshReg.Close()
		logging.Infow(ctx, "Quiesce: MeshCore subscriber closed", "duration", time.Since(t))
	}
	a.quiesced.mesh = true

	if !a.quiesced.bg {
		t := time.Now()
		if a.runCancel != nil {
			a.runCancel()
		}
		bgDone := make(chan struct{})
		go func() { a.bg.Wait(); close(bgDone) }()
		if !handoff.WaitClosed(bgDone, max(time.Until(workersDeadline), 100*time.Millisecond)) {
			err := fmt.Errorf("background store readers still running %s after cancellation", ctl.WorkerStop())
			logging.Errorw(ctx, "Quiesce: background work did not stop", "error", err)
			return err
		}
		logging.Infow(ctx, "Quiesce: background work stopped", "duration", time.Since(t))
	}
	a.quiesced.bg = true

	if !a.quiesced.store && a.store != nil {
		t := time.Now()
		if err := a.store.Close(); err != nil {
			logging.Errorw(ctx, "Quiesce: grid store close failed", "error", err, "duration", time.Since(t))
			return fmt.Errorf("close grid store: %w", err)
		}
		logging.Infow(ctx, "Quiesce: grid store closed and lock released", "duration", time.Since(t))
	}
	a.quiesced.store = true

	logging.Infow(ctx, "Quiesce: grid.db ownership released", "duration", time.Since(start))
	return nil
}

// Exit waits for prefab's own signal-driven shutdown (it received the same
// SIGTERM) so its final log lines land before the process ends.
func (a *gridApp) Exit(ctx context.Context) {
	a.mu.Lock()
	done := a.serverDone
	a.mu.Unlock()
	if done != nil {
		select {
		case <-done:
			a.mu.Lock()
			err := a.serverErr
			a.mu.Unlock()
			if err != nil {
				logging.Warnw(a.ctx, "HTTP server ended with an error", "error", err)
			}
		case <-ctx.Done():
			logging.Warnw(a.ctx, "HTTP server did not stop before exit")
		}
	}
	logging.Infow(a.ctx, "Shutdown complete", "sinceProcessStart", time.Since(processStart))
}

// gateRuntime adapts the admission gate to gateway-mounted routes.
func gateRuntime(g *handoff.Gate) func(runtime.HandlerFunc) runtime.HandlerFunc {
	return func(h runtime.HandlerFunc) runtime.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request, pp map[string]string) {
			g.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h(w, r, pp)
			})).ServeHTTP(w, r)
		}
	}
}
