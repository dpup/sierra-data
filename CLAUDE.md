# ERSN Info Server Development Guidelines

Last updated: 2025-09-13

## Active Technologies

**Language/Version**: Go 1.21+  
**Primary Dependencies**: gRPC, gRPC Gateway, Prefab framework (github.com/dpup/prefab), Protocol Buffers  
**Storage**: In-memory caching (no persistent storage required)  
**Testing**: Go testing framework with testify, contract tests for gRPC services  
**Target Platform**: Linux/macOS server, containerizable

## Project Structure
```
/
├── api/v1/                     # Protocol Buffer definitions
│   ├── roads.proto            # gRPC service for road conditions
│   ├── weather.proto          # gRPC service for weather data
│   └── common.proto           # Shared proto definitions
├── bin/                        # Compiled binaries
├── cmd/                       # CLI applications
│   ├── server/                # Main API server
│   ├── test-google/           # Google Routes API testing tool
│   ├── test-caltrans/         # Caltrans data testing tool
│   └── test-weather/          # Weather API testing tool
├── internal/                  # Private application code
│   ├── services/              # gRPC service implementations
│   ├── clients/               # External API clients
│   ├── cache/                 # In-memory caching with TTL
│   ├── config/                # Configuration management
│   └── lib/                   # Shared libraries
├── tests/                     # Test files and test data
└── Makefile                   # Build automation
```

## Commands

Whenever possible you MUST use a command provided by the makefile. If you need additional functionality
discuss with the operator improvements to the makefile commands.

**Toolchain note**: The sandbox does not ship Go or protoc preinstalled. To build
or run tests you need Go 1.24+ on `PATH`. `make proto` additionally requires
`protoc` plus the plugins `protoc-gen-go`, `protoc-gen-go-grpc`,
`protoc-gen-grpc-gateway`, and `protoc-gen-openapiv2` (install the plugins with
`go install`). Proto generation is deterministic — regenerating unchanged protos
produces no diff.

### Build & Development
```bash
# Generate protobuf code
make proto

# Build all binaries
make build

# Build specific components
make server
make tools

# Run server in foreground
make run

# Run server in background for testing
make run-bg

# Stop background server
make stop

# Clean build artifacts
make clean
```

**IMPORTANT**: Always use `make run-bg` to start the server in background, not manual `./bin/server &` commands. The Makefile handles proper process management.

### Testing
```bash
# Run all tests
make test

# Run specific test suites
make test-contract
make test-integration
make test-unit

# Test external API clients
./bin/test-google
./bin/test-caltrans  
./bin/test-weather
```

### API Testing
```bash
# Test live endpoints (local dev defaults to port 8181)
curl http://localhost:8181/api/v1/roads
curl http://localhost:8181/api/v1/weather

# Format JSON responses
curl -s http://localhost:8181/api/v1/roads | jq .
```

## Code Style

**Go Conventions**:
- Follow standard Go formatting: `go fmt`, `go vet`
- Use structured logging via Prefab framework
- gRPC-first design with Protocol Buffers
- Environment variables for sensitive configuration
- Graceful error handling with proper context

**API Design**:
- REST endpoints via gRPC Gateway
- CORS enabled for static website integration
- Consistent JSON response format
- No authentication required
- Cache-friendly with appropriate TTLs

## Development Workflow

For new features, follow this structured approach:

1. **Plan**: Understand requirements and design approach
2. **Implement**: Write tests first, then implementation
3. **Test**: Validate with unit tests and integration tests
4. **Document**: Update relevant documentation

**Development Principles**:
- **Test-Driven Development**: Write failing tests before implementation
- **Library-First**: Build standalone, testable libraries
- **CLI Testing Tools**: Each external API gets a dedicated test tool
- **Integration Focus**: Validate external API contracts

## Environment Setup

**Required Environment Variables**:
```bash
# API Keys (required for production)
export PF__GOOGLE_ROUTES__API_KEY="your-google-routes-api-key"
export PF__OPENWEATHER__API_KEY="your-openweather-api-key"
export PF__OPENAI__API_KEY="your-openai-api-key"  # For AI-enhanced alerts

# Optional Configuration (local dev defaults to 8181 via prefab.yaml)
export PORT=8181
```

**Configuration Files**:
- `prefab.yaml` - Application configuration (API refresh intervals, route definitions)
- Environment variables override config file values for secrets
- Use `.envrc` for local development (already in .gitignore)

## External API Integration

**Google Routes API**:
- Rate limit: 3,000 QPM (queries per minute)
- Requires field mask for optimal performance
- Coordinate-based POST requests to `/directions/v2:computeRoutes`
- **Billing/SKU**: the request uses `routingPreference: TRAFFIC_AWARE_OPTIMAL`
  (Compute Routes **Pro** SKU, 5,000 free/month). Do NOT add
  `extraComputations: TRAFFIC_ON_POLYLINE` or request
  `routes.travelAdvisory.speedReadingIntervals` — those bump it to the
  **Enterprise** SKU (only 1,000 free/month) and that per-segment speed data is
  not exposed by the API. A 45m per-road cache keeps total calls under 5k/month.

**OpenWeatherMap API**:
- Rate limit: 60 calls/minute (free tier)
- Current weather: `/data/2.5/weather` — the ONLY endpoint the server uses.
  One call per location per `weather.refreshInterval` (15m), request-driven:
  7 locations ≈ 672 calls/day worst case.
- **Do NOT use `/data/3.0/onecall`** (One Call 3.0): it has a separate 1,000
  calls/day free cap that per-location alert fetching blew through (2026-07).
  For US locations its alerts are relabeled NWS data — alerts come from NWS
  directly instead (each `weather.locations` entry carries a `zone`). The
  client method survives only for the `test-weather` diagnostic CLI.

**Caltrans KML Feeds**:
- Chain control status, lane closures, CHP incidents
- XML parsing with geographic filtering
- Refresh intervals: 5-15 minutes based on data type
- NOTE: As of 2026 these feeds use a new `iw-*` HTML layout with blank `<name>`
  elements and Pacific-time stamps. See `internal/clients/CLAUDE.md` before
  touching KML parsing.

**National Weather Service** (`api.weather.gov`):
- Authoritative zone alerts (watches/warnings) and fire-weather products
- No API key; requires a descriptive `User-Agent` (`weather.nws.userAgent`)
- Zones for the service area: CAZ064/065 (Calaveras), CAZ258/259 (Tuolumne)
- Powers `/weather/alerts` zone alerts and the `fire_weather` classification

**OpenAI API** (Optional):
- **AI-Enhanced Road Status Determination**: Intelligently analyzes traffic incidents to determine accurate road status (open/restricted/closed)
- **Status Explanations**: Provides clear explanations when roads are restricted or closed (populates `status_explanation` field)
- **Smart Classification**: Distinguishes between mainline road closures vs ramp/exit closures for accurate status determination
- **Alert Enhancement**: Processes raw Caltrans data into user-friendly alert descriptions
- **Structured Outputs**: Uses OpenAI structured outputs for consistent response format
- **Content-Based Caching**: 24-hour cache prevents duplicate AI calls for identical content

## API Endpoints

**When you change the API surface** (add/rename/retype a JSON field, change a
status code or URL, add an endpoint), record it in `CHANGELOG.md` as a new dated
section at the top (no formal releases — we deploy from `main`, so entries are
timestamped). That's how consuming sites (ersn.net, sierragridteam.org) learn
what to update. Flag anything that changes an existing response shape as a
breaking change with a migration note.

**Roads Service** (`/api/v1/roads`):
- `GET /api/v1/roads` - List all configured roads with current conditions
- `GET /api/v1/roads/{road_id}` - Get specific road details
- `GET /api/v1/metrics` - Alert processing metrics (currently returns 501 Unimplemented; not yet wired to real counters)
- `GET /api/v1/incidents/{area}` - Region-wide CHP/Caltrans incident feed for an area, e.g. `/api/v1/incidents/mother-lode` (flat, not route-scoped; areas configured under `roads.incidentAreas` in `prefab.yaml`). All incidents are AI-enhanced (`description`, `condensed_summary`, `impact`, `metadata`), with `severity` driven by the model's impact assessment; keyword-heuristic severity is only the pre-enhancement placeholder.
- Returns: Road status, status explanations, traffic conditions, chain controls, AI-enhanced alerts

**Key API Response Fields**:
- `status`: Current road status (OPEN/RESTRICTED/CLOSED/MAINTENANCE)
- `status_explanation`: AI-generated explanation when status is RESTRICTED or CLOSED
- `alerts[].description`: AI-enhanced human-readable alert descriptions
- `alerts[].condensed_summary`: Mobile-optimized short summaries
- `alerts[].impact`: AI-assessed impact levels (none/light/moderate/severe)
- `alerts[].metadata`: Structured additional information from AI analysis

**Weather Service** (`/api/v1/weather`):
- `GET /api/v1/weather` - Current weather for all configured locations (each includes a `fire_weather` classification)
- `GET /api/v1/weather/{location_id}` - Get specific location weather
- `GET /api/v1/weather/alerts` - Active weather alerts (authoritative NWS zone alerts, `source: NWS`; OpenWeatherMap alerts removed 2026-07-04)
- `GET /api/v1/weather/alerts?zones=CAZ064,CAZ065` - Filter to alerts in specific forecast zones
- Returns: Temperature, conditions, visibility, wind, alerts, fire-weather state
- Per-location `alerts` are the NWS alerts for the location's configured `zone` (prefab.yaml)

**Fire-weather** (`weather_data[].fire_weather`): `state` escalates `normal` →
`elevated` (Fire Weather Watch) → `red-flag` (Red Flag Warning), derived only
from authoritative NWS products — never a Red Flag NWS hasn't issued.

**Hazards Service** (`/api/v1/hazards`, `/api/v1/situation`, `/api/v1/scanners`):
a unified, map-ready aggregation layer. See `docs/hazard-aggregation-design.md`
and `internal/hazards/CLAUDE.md` for the full model; these endpoints are
hand-built GeoJSON/JSON (not grpc-gateway), so field names are `snake_case`.
- `GET /api/v1/hazards/{area}/{layer}.geojson` - one RFC 7946 `FeatureCollection`
  per layer for a maps client (MapLibre/Leaflet). Layers: `road_incident`,
  `chain_control`, `road_segment`, `weather_alert`, `fire_weather`, `earthquake`,
  `wildfire`, `evacuation`. Every feature shares a `properties` envelope
  (`id, layer, kind, severity, severity_rank, headline, source, …`) on the unified
  severity scale `INFO..EXTREME` (rank 0–4). Coordinates are `[lng, lat]`.
- `GET /api/v1/situation/{area}` - one-call rollup: per-layer status +
  cross-layer `summary` (`highest_severity`, `severity_counts`, `top_headlines`,
  `active_evacuations`) + a `scanners` sidecar.
- `GET /api/v1/scanners/{area}` - Broadcastify scanner feeds (link-out only).
- **`metadata.source_status`** is `OK | STALE | UNAVAILABLE` — the honesty
  mechanism. A layer is fail-loud: on source error it returns `UNAVAILABLE` with
  empty features (or `STALE` + `last_source_update` when serving a cached last-good
  fetch), never a fabricated clear state.
- **Evacuation is life-safety / fail-loud**: the invariant is *an error never
  becomes a `0`*. A Cal OES failure is `UNAVAILABLE` → `situation.active_evacuations:
  null` (render "unknown — check Genasys"); a clean fetch with no active zones is
  `OK` → `active_evacuations: 0` (render "no active evacuations reported", a
  caveated confirmed-empty, not a guarantee). `metadata.source_url` always links
  the authoritative Genasys viewer in every state. Areas are configured under
  `hazards.areas` in `prefab.yaml`.

## Performance & Monitoring

**Response Time Targets**:
- Weather API: < 1 second
- Roads API: < 2 seconds  
- Cache refresh: 15-minute intervals for weather (API-budget driven), 5–15 min for road feeds
- Stale data: cache serves stale up to 2× the refresh interval on upstream failure

**Logging**:
- Structured JSON logs via Prefab framework
- Request/response logging with sensitive data masking
- External API call tracking with rate limit monitoring

## Development Tips

**Testing External APIs**:
- Use CLI tools (`test-google`, `test-caltrans`, `test-weather`) for debugging
- Check API key restrictions in Google Cloud Console (no HTTP referrer blocks)
- Monitor rate limits and implement proper backoff strategies

**Debugging Common Issues**:
- **Google Routes API 403**: Check API key referrer restrictions
- **Server won't start**: Verify environment variables are set
- **Slow responses**: Check external API timeouts and cache hit rates
- **Stale data**: Verify background refresh goroutines are running

**Adding New Roads**:
1. Update `prefab.yaml` with new road coordinates
2. Test with `./bin/test-google` using new coordinates
3. Restart server to pick up configuration changes
4. Verify new road appears in `/api/v1/roads` response

**Adding New Weather Locations**:
1. Update `prefab.yaml` weather locations section, including the `zone` field
   (the NWS forecast zone containing the location — must also be listed in
   `weather.nws.zones`, or the location gets no alerts)
2. Test with `./bin/test-weather` using new coordinates
3. Restart server and verify in `/api/v1/weather` response
4. Note each location adds one `/data/2.5/weather` call per refresh interval

## AI Enhancement System

**Road Status Determination**:
- AI analyzes Caltrans incident data to determine accurate road status
- Distinguishes between mainline closures (status: CLOSED) vs ramp closures (status: RESTRICTED)
- Provides clear explanations in `status_explanation` field when roads are not fully open
- Examples: "Right lane blocked due to accident" vs "Off-ramp closure to Treasure Island"

**Alert Processing Pipeline**:
1. **Content Hashing**: Generate hash of raw alert content for caching
2. **Cache Check**: Check 24-hour cache to avoid duplicate OpenAI calls
3. **AI Analysis**: If cache miss, send to OpenAI for enhancement and status determination
4. **Response Processing**: Parse structured OpenAI response into API-ready format
5. **Cache Storage**: Store enhanced result with 24-hour TTL

**AI Enhancement Features**:
- **Human-Readable Descriptions**: Converts technical Caltrans language to clear, actionable information
- **Impact Assessment**: Categorizes impact as none/light/moderate/severe
- **Duration Estimates**: Provides duration context (unknown/< 1 hour/several hours/ongoing)
- **Condensed Summaries**: Creates mobile-friendly short descriptions
- **Structured Metadata**: Extracts additional context (lanes affected, emergency services, etc.)

**Development Best Practices**:
- Monitor OpenAI API usage and costs through logging
- Test AI enhancements with `./bin/test-caltrans` tool
- Verify status determination logic with different incident types
- Check cache hit rates to ensure efficient AI usage
- Validate structured output parsing for robustness

**Security Guidelines**:
- API keys are stored in `.envrc` (git-ignored)
- Never commit real API keys to the repository
- Use placeholder examples in documentation and configs
- Rotate API keys if they're accidentally exposed

