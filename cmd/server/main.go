package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"
	_ "time/tzdata" // Embed the IANA tz database so America/Los_Angeles resolves in minimal containers

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"

	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/cache"
	"github.com/dpup/info.ersn.net/server/internal/clients/caltrans"
	"github.com/dpup/info.ersn.net/server/internal/clients/google"
	"github.com/dpup/info.ersn.net/server/internal/clients/nws"
	"github.com/dpup/info.ersn.net/server/internal/clients/weather"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
	"github.com/dpup/info.ersn.net/server/internal/lib/alerts"
	"github.com/dpup/info.ersn.net/server/internal/services"
)

func main() {
	// Initialize structured logging
	logger := logging.NewProdLogger()
	ctx := logging.With(context.Background(), logger)

	logging.Info(ctx, "Starting ERSN Info Server")

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

	// Unified hazard/situation GeoJSON feed (re-projects the feeds above).
	hazardsService := hazards.NewService(appConfig, roadsService, weatherService, caltransClient, cacheInstance)

	logging.Infow(ctx, "Live Data API Server starting",
		"roads_monitored", len(appConfig.Roads.MonitoredRoads),
		"weather_locations", len(appConfig.Weather.Locations))

	// Start periodic refresh to maintain cache warmth (replaces complex cache warmer)
	periodicRefresh := services.NewPeriodicRefreshService(roadsService, appConfig)
	if err := periodicRefresh.StartPeriodicRefresh(ctx); err != nil {
		logging.Errorw(ctx, "Failed to start periodic refresh", "error", err)
	}

	// Create Prefab server with GRPC reflection enabled
	// Server configuration (port, etc.) will be loaded from prefab.yaml/env vars
	server := prefab.New(
		prefab.WithContext(ctx),
		prefab.WithGRPCReflection(),
		prefab.WithGRPCInterceptor(cacheHeadersInterceptor),
		prefab.WithHTTPHandler(hazards.HandlerPrefix, hazardsService),
		prefab.WithHTTPHandlerFunc(hazards.ScannersPrefix, hazardsService.ServeScanners),
		prefab.WithHTTPHandlerFunc(hazards.SituationPrefix, hazardsService.ServeSituation),
		prefab.WithHTTPHandlerFunc("/", homepageHandler),
		prefab.WithHTTPHandlerFunc("/api/docs/roads.swagger.json", openAPIHandler("api/v1/roads.swagger.json")),
		prefab.WithHTTPHandlerFunc("/api/docs/weather.swagger.json", openAPIHandler("api/v1/weather.swagger.json")),
		prefab.WithHTTPHandlerFunc("/api/docs/common.swagger.json", openAPIHandler("api/v1/common.swagger.json")),
	)

	// Register gRPC services using Prefab's service registrar
	api.RegisterRoadsServiceServer(server.ServiceRegistrar(), roadsService)
	api.RegisterWeatherServiceServer(server.ServiceRegistrar(), weatherService)

	// Register gateway handlers using Prefab's gateway args
	if err := api.RegisterRoadsServiceHandlerFromEndpoint(server.GatewayArgs()); err != nil {
		logging.Errorw(ctx, "Failed to register Roads service gateway", "error", err)
		log.Fatalf("Failed to register Roads service gateway: %v", err)
	}

	if err := api.RegisterWeatherServiceHandlerFromEndpoint(server.GatewayArgs()); err != nil {
		logging.Errorw(ctx, "Failed to register Weather service gateway", "error", err)
		log.Fatalf("Failed to register Weather service gateway: %v", err)
	}

	logging.Info(ctx, "Server initialization complete, starting HTTP and gRPC services")

	// Start the server (blocks until shutdown)
	if err := server.Start(); err != nil {
		logging.Errorw(ctx, "Server failed", "error", err)
		log.Fatalf("Server failed: %v", err)
	}
}

// homepageHandler serves a simple HTML homepage at the server root
func homepageHandler(w http.ResponseWriter, r *http.Request) {
	// Only handle the root path
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	html := `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>info.ersn.net</title>
    <style>
        body { 
            font-family: 'Courier New', Consolas, monospace; 
            background: #000; 
            color: #0f0; 
            padding: 20px; 
            line-height: 1.4; 
        }
        a { color: #0ff; text-decoration: none; }
        a:hover { text-decoration: underline; }
        pre { margin: 0; }
        .header { color: #ff0; }
        .section { margin: 20px 0; }
    </style>
</head>
<body>
<pre>
<span class="header"> ___ ___  ___ _  _ 
| __| _ \/ __| \| |
| _||   /\__ \ .' |
|___|_|_\|___/_|\_|</span>

<span class="header">info.ersn.net</span>

Real-time API server providing road, weather, and hazard information
for the Ebbett's Pass / Highway 4 corridor.

<span class="header">Repository:</span>
<a href="https://github.com/dpup/info.ersn.net">https://github.com/dpup/info.ersn.net</a>

<span class="header">Website:</span>
<a href="https://ersn.net">https://ersn.net</a>

<span class="header">API Endpoints:</span>

  Roads API:
    <a href="/api/v1/roads">GET /api/v1/roads</a>               - List all monitored roads
    <a href="/api/v1/roads/hwy4-angels-murphys">GET /api/v1/roads/{road_id}</a>     - Get specific road details
    <a href="/api/v1/incidents/mother-lode">GET /api/v1/incidents/{area}</a>    - Region-wide CHP/Caltrans incidents

  Weather API:
    <a href="/api/v1/weather">GET /api/v1/weather</a>             - Current weather + fire-weather state
    <a href="/api/v1/weather/alerts">GET /api/v1/weather/alerts</a>      - Active NWS zone alerts
    <a href="/api/v1/weather/alerts?zones=CAZ064,CAZ065,CAZ258,CAZ259">GET /api/v1/weather/alerts?zones=...</a> - Filter to NWS forecast zones

  Hazards API (unified GeoJSON for map clients):
    <a href="/api/v1/hazards/calaveras/road_incident.geojson">GET /api/v1/hazards/{area}/{layer}.geojson</a> - road_incident, chain_control, road_segment, weather_alert, fire_weather, earthquake, wildfire, evacuation
    <a href="/api/v1/situation/calaveras">GET /api/v1/situation/{area}</a>     - One-call rollup: per-layer status + severity summary (evac unknown-aware)
    <a href="/api/v1/scanners/calaveras">GET /api/v1/scanners/{area}</a>      - Broadcastify scanner feeds for the area

<span class="header">API Documentation:</span>
  <a href="/api/docs/roads.swagger.json">Roads API OpenAPI Spec</a>            - Machine-readable API docs (Roads)
  <a href="/api/docs/weather.swagger.json">Weather API OpenAPI Spec</a>          - Machine-readable API docs (Weather)
  <a href="/api/docs/common.swagger.json">Common Types OpenAPI Spec</a>         - Shared message definitions

<span class="header">Data Sources:</span>
  • Google Routes API               - Traffic conditions and travel times
  • Caltrans KML Feeds              - Lane closures, CHP incidents, chain control
  • OpenWeatherMap API              - Current weather conditions
  • National Weather Service        - Zone alerts and fire-weather products
  • USGS (FDSN)                     - Earthquakes
  • CAL FIRE + NIFC WFIGS           - Active wildfires and perimeters
  • Cal OES (Genasys)               - Evacuation zones (reference only)
  • Broadcastify                    - Public-safety scanner feeds
  • OpenAI                          - AI enhancement of road alerts

<span class="header">Example Usage:</span>
  curl <a href="/api/v1/roads">https://info.ersn.net/api/v1/roads</a>
  curl <a href="/api/v1/weather">https://info.ersn.net/api/v1/weather</a>
  curl <a href="/api/v1/situation/calaveras">https://info.ersn.net/api/v1/situation/calaveras</a>
  curl <a href="/api/v1/hazards/calaveras/wildfire.geojson">https://info.ersn.net/api/v1/hazards/calaveras/wildfire.geojson</a>
  curl <a href="/api/v1/weather/alerts?zones=CAZ064,CAZ065,CAZ258,CAZ259">https://info.ersn.net/api/v1/weather/alerts?zones=CAZ064,CAZ065</a>
</pre>
</body>
</html>`

	if _, err := fmt.Fprint(w, html); err != nil {
		slog.Error("Failed to write homepage HTML", "error", err)
	}
}

// openAPIHandler serves OpenAPI specification files with proper headers
func openAPIHandler(filename string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		http.ServeFile(w, r, filename)
	}
}
