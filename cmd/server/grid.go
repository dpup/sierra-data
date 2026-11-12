package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"
	"github.com/dpup/prefab/plugins/etag"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/cache"
	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/clients/census"
	"github.com/sierragridteam/sierra-data/internal/clients/cwwp2"
	"github.com/sierragridteam/sierra-data/internal/clients/google"
	"github.com/sierragridteam/sierra-data/internal/clients/nws"
	"github.com/sierragridteam/sierra-data/internal/clients/weather"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/gridapi"
	"github.com/sierragridteam/sierra-data/internal/handoff"
	"github.com/sierragridteam/sierra-data/internal/hazards"
	"github.com/sierragridteam/sierra-data/internal/ingest"
	"github.com/sierragridteam/sierra-data/internal/lib/alerts"
	"github.com/sierragridteam/sierra-data/internal/mcp"
	"github.com/sierragridteam/sierra-data/internal/pushingest"
	"github.com/sierragridteam/sierra-data/internal/services"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// gridApp is the Grid as internal/handoff drives it, in two roles.
//
// Reader (every process, from boot to exit): prepareGrid builds everything
// that does not touch the database; Serve opens the store as a reader and
// starts the production listener, which serves every GET from then on;
// Exit closes the admission gate, drains and closes the store.
//
// Writer (at most one process per database, writer.go): Activate starts the
// campaign for the writer lease; the holder seeds and runs the
// ingest scheduler, the MeshCore subscriber and push ingest; losing the
// lease stops all of that and the process campaigns again. Quiesce stops
// campaigning and releases the lease, and the process keeps serving reads.
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

	// onFatal is the controller's Fail: the production server stopping is
	// fatal (main sets it).
	onFatal func(error)
	// activateHook, when set, runs at named points of a writer term's start
	// (tests inject failures and panics there).
	activateHook func(stage string)
	// instanceID names this process in writer_lease.holder (the handoff
	// controller's id; main sets it).
	instanceID string
	// strictActivate is the coordinator mode (--standby): Activate waits for
	// the lease up to grid.database.lockTimeout and fails back to STANDBY
	// without it, as a coordinator expects. Otherwise Activate starts a
	// background campaign and returns at once.
	strictActivate bool

	// Reader: set by Serve, torn down by Exit.
	mu         sync.Mutex
	store      *store.Store
	gate       *handoff.Gate
	readCancel context.CancelFunc // ends the camera directory refresh
	server     *prefab.Server
	serverDone chan struct{} // closed when prefab's Start returns
	serverErr  error         // its result, valid once serverDone is closed
	ingest     *ingestRouter // nil when push ingest is not configured
	advertise  string        // host:port recorded in writer_lease.address
	exiting    bool
	// serverStopWait, when positive, caps how long Exit waits for prefab's
	// signal-driven shutdown (tests stop the controller without a signal, so
	// prefab never stops on its own there).
	serverStopWait time.Duration

	// Writer role (writer.go).
	w writerRole
}

// prepareGrid builds the database-free half of the Grid: config-derived
// clients and the read-path services. Nothing here opens the database, and nothing
// here starts upstream polling (that waits for activation, so a standby never
// spends API budget). Invalid configuration fails here, before any handoff.
func prepareGrid(ctx context.Context, appConfig *config.Config) (*gridApp, error) {
	start := time.Now()
	if appConfig.Grid.Database.URL == "" {
		return nil, errors.New("grid.database.url is required (PF__GRID__DATABASE__URL; locally `make db-url`)")
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

// Serve opens the grid store as a reader and starts the production
// listener: from here every GET route is served, whatever the writer role
// does. It does not take the lease, write or seed. It does migrate: every
// process applies pending migrations at boot under the migration advisory
// lock (store.OpenReader; the policy is in internal/store/migrations), so
// readiness never waits for a writer. Idempotent.
func (a *gridApp) Serve(ctx context.Context) error {
	a.mu.Lock()
	if a.store != nil {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	start := time.Now()
	appConfig := a.cfg
	dbCfg := appConfig.Grid.Database
	gridStore, err := store.OpenReader(dbCfg.URL,
		store.WithMaxConns(dbCfg.MaxConns),
		store.WithMinConns(dbCfg.MinConns),
		store.WithLockContext(ctx),
		store.WithWildfireProximity(appConfig.Grid.Wildfire.PlaceBuffer()),
		store.WithTownRadius(appConfig.Grid.Places.TownRadiusMeters))
	if err != nil {
		return fmt.Errorf("open grid store: %w", err)
	}
	if st, err := gridStore.Settings(ctx); err == nil {
		logging.Infow(a.ctx, "Grid store opened (reader)",
			"database", st.Database, "serverVersion", st.ServerVersion,
			"schemaVersion", st.SchemaVersion,
			"maxConns", st.MaxConns, "minConns", st.MinConns, "duration", time.Since(start))
	}
	readCtx, readCancel := context.WithCancel(a.ctx)
	gate := handoff.NewGate()
	a.mu.Lock()
	a.store, a.gate, a.readCancel = gridStore, gate, readCancel
	a.mu.Unlock()
	fail := func(err error) error {
		readCancel()
		a.mu.Lock()
		a.store, a.gate = nil, nil
		a.mu.Unlock()
		gridStore.Close()
		return err
	}
	a.hazards.UseRoadHistory(gridStore)

	// The push-ingest route exists on every process; the writer serves it,
	// any other process forwards it there (ingestroute.go). Validated in
	// prepareGrid, so an error here is unexpected.
	pushCfg, err := pushingest.NewRegistry(appConfig.Grid.Ingest)
	if err != nil {
		return fail(fmt.Errorf("invalid grid.ingest configuration: %w", err))
	}
	if pushCfg.Enabled() {
		a.ingest = newIngestRouter(gridStore, a.instanceID, a.holder(), appConfig)
	}

	// Grid API service: backs the /api/v1 GridService RPCs and the hand-built
	// summary + .geojson gateway routes (the census geocoder powers
	// /api/v1/places:resolve?address=).
	gridapiService := gridapi.NewService(gridStore, a.weatherService, census.NewClient(), appConfig, a.hazards)

	// Caltrans CCTV camera directory (GET /api/v1/cameras). The lists are
	// near-static, so they refresh in the background and a request never waits
	// on Caltrans. Without districts the RPC answers UNAVAILABLE. A read-path
	// cache: every process keeps its own.
	if cams := appConfig.Roads.CaltransFeeds.CWWP2.Cameras; len(cams.Districts) > 0 {
		camClient := cwwp2.NewClient()
		if base := appConfig.Roads.CaltransFeeds.CWWP2.BaseURL; base != "" {
			camClient.BaseURL = base
		}
		cameraService := services.NewCameraService(camClient, cams.Districts, cams.RefreshInterval)
		go cameraService.Run(readCtx)
		gridapiService.Cameras = cameraService
		logging.Infow(a.ctx, "Camera directory enabled",
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
	// which closes only at exit (prefab's listener cannot be stopped short of
	// a signal). gRPC covers every gateway-routed RPC, since the gateway
	// reaches the service over a loopback gRPC connection; the hand-mounted
	// routes are wrapped one by one.
	gated := gateRuntime(gate)
	ingest := a.ingest

	// Prefab server. gRPC + gateway serve the /api/v1 GridService; MCP and the
	// static site are plain HTTP handlers. Its base context is the process
	// context: cancelling it closes the gateway's loopback gRPC connection,
	// and a closing Grid then answers 499 (cancelled) where it should answer
	// the gate's 503. In-flight cancellation is the gate's job.
	server := prefab.New(
		prefab.WithContext(a.ctx),
		prefab.WithGRPCReflection(),
		prefab.WithGRPCInterceptor(gate.UnaryInterceptor()),
		// Cache-Control: public, max-age=30 on every (read-only) GridService
		// response — the freshness lifetime that complements the ETag revalidation
		// below. Restores what the hand-built /v1 handlers set before the migration.
		prefab.WithGRPCInterceptor(gridapi.CacheControlInterceptor(30)),
		// Conditional GET (ETag/If-None-Match -> 304) for RPCs that call
		// etag.Guard — event detail (revision + in-place refreshes), the
		// event/history lists (DataVersion + per-layer RefreshVersion +
		// filters), and places — so a match skips the expensive load. Every
		// validator starts with the build and the database's places version,
		// so every process serving one database agrees. The hand-built
		// .geojson route bypasses the gRPC interceptor and keeps its own
		// data-hash ETag. (GetPlaceSummary is not yet guarded.)
		prefab.WithPlugin(etag.Plugin()),
		prefab.WithGRPCService(&gridv1.GridService_ServiceDesc, gridServer),
		prefab.WithGRPCGateway(func(ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error {
			// Raise the loopback client's 4 MiB receive limit, so an event
			// bigger than the list page budget is still served (alone on its
			// page, or from GetEvent). See gridapi.PageByteBudget.
			opts = append(append([]grpc.DialOption{}, opts...), gridapi.GatewayDialOptions()...)
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
			// exist for GETs. The writer serves it; any other process forwards.
			//
			// CORS keeps this browser-unreachable cross-origin: corsAllowMethods is
			// [GET], so the POST preflight is denied — the same property that
			// protects /mcp. Do not add POST to that list.
			if ingest != nil {
				if err := ingest.Mount(mux, gated); err != nil {
					return err
				}
			}
			// Point MCP at the fully-wired gateway so its tools query /api/v1
			// in-process (same mux prefab serves at /api/).
			gatewayMux.h = mux
			return nil
		}),
		prefab.WithHTTPHandlerFunc("/mcp", gate.HandlerFunc(mcpHandler.ServeHTTP)),
		// Publish the OpenAPI spec (generated from grid.proto plus the hand-written
		// overlay; see api/grid/v1/openapi.go). A route mounted by hand here or in
		// gridapi needs a path in openapi.overlay.yaml, or
		// TestHandBuiltRoutesAreDocumented fails.
		// Exact path, so it wins over the gateway's /api/ subtree mount.
		prefab.WithHTTPHandlerFunc("/api/openapi.json", gate.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(gridv1.OpenAPISpec)
		})),
		// Health on the production port, for the container and load-balancer
		// checks. Not gated: they answer from the process state itself.
		prefab.WithHTTPHandlerFunc("/livez", a.serveLivez),
		prefab.WithHTTPHandlerFunc("/readyz", a.serveReadyz),
		prefab.WithHTTPHandlerFunc("/", gate.HandlerFunc(siteHandler)),
	)

	// Our listener must be the one WaitListening finds: refuse to start on a
	// port something else already holds.
	port := prefab.ConfigInt("server.port")
	if err := handoff.PortFree(port); err != nil {
		return fail(err)
	}
	done := make(chan struct{})
	a.mu.Lock()
	a.server, a.serverDone = server, done
	a.advertise = advertiseAddrFor(appConfig, port)
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
	if err := handoff.WaitListening(ctx, port, done, 15*time.Second); err != nil {
		a.mu.Lock()
		serr := a.serverErr
		a.mu.Unlock()
		if serr != nil {
			err = fmt.Errorf("%w: %w", err, serr)
		}
		return fmt.Errorf("production listener: %w", err)
	}
	// prefab's Start returns nil only after its own SIGTERM shutdown. An error
	// means production stopped serving, which is fatal.
	go func() {
		<-done
		a.mu.Lock()
		serr := a.serverErr
		a.mu.Unlock()
		if serr != nil && a.onFatal != nil {
			a.onFatal(fmt.Errorf("production server stopped: %w", serr))
		}
	}()
	logging.Infow(a.ctx, "Serving reads", "port", port, "duration", time.Since(start),
		"sinceProcessStart", time.Since(processStart))
	return nil
}

// Ready is the read path's readiness (/readyz, handoff /status.ready): the
// store is open (which means migrated to at least this build's schema), and
// the process is not exiting.
// The writer role does not enter into it: a reader serves.
func (a *gridApp) Ready() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.store != nil && !a.exiting
}

func (a *gridApp) serveLivez(w http.ResponseWriter, _ *http.Request) {
	writeHealth(w, http.StatusOK, map[string]any{"live": true})
}

func (a *gridApp) serveReadyz(w http.ResponseWriter, _ *http.Request) {
	ready := a.Ready()
	code := http.StatusServiceUnavailable
	if ready {
		code = http.StatusOK
	}
	writeHealth(w, code, map[string]any{"ready": ready, "writer": a.DatabaseHeld()})
}

func writeHealth(w http.ResponseWriter, code int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// Exit is the reader's end, after the controller has released the writer
// role (SIGTERM quiesces first): /readyz turns 503, the admission gate closes
// and drains in-flight requests, prefab's own signal-driven shutdown is
// awaited, and the store is closed (releasing the lease too, if a failed
// quiescence left it held).
func (a *gridApp) Exit(ctx context.Context) {
	a.mu.Lock()
	a.exiting = true
	gate, done, st, readCancel := a.gate, a.serverDone, a.store, a.readCancel
	a.mu.Unlock()
	if gate != nil {
		res := gate.Drain(a.cfg.Grid.Control.Drain(), 2*time.Second)
		logging.Infow(a.ctx, "Exit: request drain complete",
			"inFlight", res.InFlight, "cancelled", res.Cancelled, "remaining", res.Remaining,
			"duration", res.Duration)
	}
	if readCancel != nil {
		readCancel()
	}
	if a.serverStopWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.serverStopWait)
		defer cancel()
	}
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
	if st != nil {
		a.hazards.UseRoadHistory(nil)
		if err := st.Close(); err != nil {
			logging.Warnw(a.ctx, "Exit: closing the grid store", "error", err)
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
