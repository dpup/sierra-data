# The Grid — Development Guidelines

The Grid is the S.I.E.R.R.A data service (primary domain `data.sierragridteam.org`;
`info.ersn.net` is a legacy CNAME alias, ersn.net a consuming site). The Go module
path and GitHub repo are `github.com/dpup/sierra-data` (renamed 2026-07-06 from
`github.com/dpup/info.ersn.net/server`).

Last updated: 2026-07-06

## Active Technologies

**Language/Version**: Go 1.25+ (`go.mod` declares 1.25.0 — a transitive dep, `golang.org/x/sys`, requires it; the Dockerfile builds on `golang:1.25-alpine`)  
**Primary Dependencies**: gRPC, gRPC Gateway, Prefab framework (github.com/dpup/prefab), Protocol Buffers  
**Storage**: SQLite (pure-Go `modernc.org/sqlite`, WAL) is the grid event store's system of record — events, revision history, the place directory, and source health, persisted at `grid.dbPath` (`PF__GRID__DBPATH`). In-memory TTL caches remain on the read path for the roads/weather/hazards services.  
**Testing**: Go testing framework with testify, contract tests for gRPC services  
**Target Platform**: Linux/macOS server, containerizable

## Project Structure
```
/
├── api/v1/                     # Protocol Buffer definitions (grpc-gateway services)
│   ├── roads.proto            # gRPC service for road conditions
│   ├── weather.proto          # gRPC service for weather data
│   └── common.proto           # Shared proto definitions
├── api/grid/v1/                # grid.v1 messages (Event/Place/...) + the GridService /api/v1 surface
├── bin/                        # Compiled binaries
├── cmd/                       # CLI applications
│   ├── server/                # Main API server (main.go, site.go, gridadapter.go)
│   ├── test-google/           # Google Routes API testing tool
│   ├── test-caltrans/         # Caltrans data testing tool
│   └── test-weather/          # Weather API testing tool
├── internal/                  # Private application code
│   ├── services/              # gRPC service implementations
│   ├── clients/               # External API clients
│   ├── cache/                 # In-memory caching with TTL
│   ├── config/                # Configuration management
│   ├── hazards/               # /api/v1 unified GeoJSON hazard layers
│   ├── store/                 # SQLite grid event store (events, revisions, places, sources)
│   ├── ingest/                # Poller scheduler + per-source normalizers → the store
│   ├── gridapi/               # /api/v1 GridService impl (gRPC) + hand-built GeoJSON
│   ├── places/                # Grid place directory seeder (areas/counties/towns/corridors)
│   └── lib/                   # Shared libraries (incl. lib/geojson: geometry + PIP)
├── data/places/               # Checked-in Census county polygons (counties.geojson)
├── site/                      # Embedded data.sierragridteam.org static site (served at /)
├── tests/                     # Test files and test data
└── Makefile                   # Build automation
```

The runtime SQLite database lives under `data/` (`data/grid.db`), which is
git-ignored; only `data/places/` is checked in.

## Commands

Whenever possible you MUST use a command provided by the makefile. If you need additional functionality
discuss with the operator improvements to the makefile commands.

**Toolchain note**: The sandbox does not ship Go or protoc preinstalled. To build
or run tests you need Go 1.25+ on `PATH`. `make proto` additionally requires
`protoc` plus the plugins `protoc-gen-go`, `protoc-gen-go-grpc`,
`protoc-gen-grpc-gateway`, and `protoc-gen-openapiv2` — installed at pinned
versions by `make proto-tools` (a prerequisite of `make proto`; versions are
declared as vars in the `Makefile`, aligned to `go.mod`). Proto generation is
deterministic — regenerating unchanged protos produces no diff. `make proto`
generates both `api/v1` (the older grpc-gateway services) and `api/grid/v1`, which
now carries the `grid.v1` messages **and** the `GridService` (gateway + openapi) —
the proto-defined `/api/v1` data surface. Generated `*.pb.go` (incl.
`*_grpc.pb.go`, `*.pb.gw.go`, `*.swagger.json`) are committed. The grid store uses the pure-Go SQLite driver
`modernc.org/sqlite`, so the `CGO_ENABLED=0` cross-compile in the Dockerfile
still works — do not introduce a cgo SQLite driver.

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

**Server management inside the container**: the Moat sandbox has **no `pkill`
and no working `ps`** for finding these processes, so `make stop` (and `make
run-bg`, which stops any existing server first) is the only reliable lifecycle
path — prefer it over ad-hoc kills. When you must kill a server by hand:

- Find PIDs by scanning `/proc` for the command line, not `ps`/`pgrep`:
  ```bash
  for d in /proc/[0-9]*; do tr '\0' ' ' < "$d/cmdline" 2>/dev/null \
    | grep -q './bin/server' && echo "${d#/proc/}"; done
  ```
  then `kill -9 <pid>` (a bash builtin — available even though `pkill` isn't).
- Do **not** rely on `pkill -f`: it is absent, and even where present it matches
  argv, so an env-var prefix like `PF__SERVER__PORT=8188 ./bin/server` is **not**
  matchable that way (the process argv is just `./bin/server`). Killing by a
  port set via env therefore fails silently and leaks servers that keep the port
  bound — the next launch then hits "address already in use" and you unknowingly
  test the stale binary.
- Port `8181` may be served by an instance **outside** the sandbox (forwarded
  in) that this container cannot see or kill. To verify a local build, run it on
  a different port with a throwaway DB (`PF__SERVER__PORT=<port>
  PF__GRID__DBPATH=<scratch>/verify.db ./bin/server`) rather than fighting 8181,
  and confirm you bound it by checking the "Listening for traffic" log line.

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
curl http://localhost:8181/api/v1/events
curl http://localhost:8181/api/v1/conditions

# Format JSON responses
curl -s http://localhost:8181/api/v1/events?layer=road_incident | jq .
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

# Grid event store database path (default ./data/grid.db via prefab.yaml).
# Production points this at the EBS mount; the Dockerfile sets it and declares
# a /data volume so events/revisions/source-health survive container replacement.
export PF__GRID__DBPATH=/data/grid.db
```

**Configuration Files**:
- `prefab.yaml` - Application configuration (API refresh intervals, route
  definitions, and the `grid` section: `dbPath`, per-source poll intervals +
  disappearance policy, NWS-alert enhancement budget)
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
- Zones for the service area (NWS Sacramento/STO, elevation-banded, cover both
  Calaveras & Tuolumne): CAZ137 (1000–3000 ft), CAZ138 (3000–5000 ft), CAZ139
  (above 5000 ft). Always verify a zone with `api.weather.gov/points/{lat},{lng}`
  — do NOT guess codes (the old CAZ064/065/258/259 were wrong; CAZ065 is a SoCal
  zone that leaked out-of-area alerts).
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

**One surface: `/api/v1`, proto-defined gRPC + gRPC-Gateway** (migrated 2026-07-09;
see `docs/grpc-gateway-migration-plan.md`, `docs/v2-api-spec.md`). The `GridService`
proto (`api/grid/v1/grid.proto`) is served over the gateway that Prefab mounts at
`/api/`; the impl is `internal/gridapi` (`GridServer` wrapping `Service`), reading
everything from the grid event store. Field names are **camelCase** (protojson
`UseProtoNames:false`), timestamps RFC 3339, errors gRPC-standard
`{code, codeName, message, details}` with the mapped HTTP status. gRPC reflection
is on. Conditional-GET/ETag is not yet wired (deferred behind a future prefab
`WithETag`). The prior hand-built REST surfaces (the old `/api/v1/roads|weather|
hazards|situation|incidents` and the snake_case `/v1`) have all been **removed** —
they fold into the endpoints below.

The whole surface is **camelCase**. **One endpoint stays hand-built** (mounted on
the gateway mux via `mux.HandlePath`, `gridapi.RegisterGatewayRoutes`): the
`.geojson` map layers (RFC 7946 geometry, which proto3 models poorly) — its
`properties`/`metadata` are camelCase too (json struct tags in `internal/hazards`).
The place `summary` is now the `GetPlaceSummary` proto RPC; its
`activeEvacuations` is a `google.protobuf.Int32Value` so it still serializes as an
explicit JSON `null` (null=UNAVAILABLE vs 0=confirmed-empty vs N) under the
gateway's `EmitUnpopulated` marshaler.

**Grid Info Service** (`/api/v1/...`), the `GridService` RPCs:
- `GET /api/v1/events` - cross-layer event query
  (`place,layer,status,severity_min,since,page_token,page_size`; default status
  `ACTIVE,SCHEDULED`; keyset pagination → `{events, nextPageToken}`). **`place`
  accepts the slug (`hwy4-murphys-arnold`) or the namespaced id
  (`corridor:hwy4-murphys-arnold`)** — both resolve (`Store.GetPlace` keys on `id`
  when the value contains `:`, else `slug`); the bare slug is the intended form.
  Events attach to a place geometrically: point events fall inside a polygon place
  (county/area) or within `corridorBufferMeters` (~1.5 km) of a corridor
  LineString. This is the road-incident feed (`layer=road_incident`, scope by
  corridor `place`) and the
  weather-alert listing (`layer=weather_alert`) — there is no separate roads or
  incidents endpoint. All road incidents are AI-enhanced (`enhancement`:
  description/summary/impact/metadata), with `severity` driven by the model's
  impact assessment.
- `GET /api/v1/events/{id}` / `GET /api/v1/events/{id}/history` - current revision /
  revision timeline.
- `GET /api/v1/history` - cross-event revision archive (`place,from,to,layer`).
- `GET /api/v1/places` / `GET /api/v1/places/{place}` - directory (`kind`,`q`);
  places addressable by slug (`ebbetts-pass`) or id (`county:calaveras-county`),
  slugs globally unique.
- `GET /api/v1/places:resolve?lat=&lng=` or `?address=` - point/address →
  containing places, most-specific first (AIP colon custom-verb, so it doesn't
  collide with `/places/{place}`; address path geocodes via the keyless Census
  geocoder, `internal/clients/census`). Response `query.matchedAddress`.
- `GET /api/v1/conditions` - `GetConditions`: current weather + the region's
  `fireWeather` classification, optional `?place=` bbox filter. **Drops
  per-location alerts** — alerts are events (`/api/v1/events?layer=weather_alert`).
  There is no roads-conditions passthrough (road conditions are the `road_segment`
  / `chain_control` geojson layers).
- `GET /api/v1/scanners?place=` - Broadcastify feed config.
- `GET /api/v1/sources` - the source registry + per-source health (a source's own
  health is `status`: `OK|STALE|UNAVAILABLE`, last success/attempt, poll interval,
  last error).

**Summary + map:**
- `GET /api/v1/places/{place}/summary` - `GetPlaceSummary` RPC (camelCase): a
  one-fetch place rollup — `mode` (QUIET/WATCH/ACTIVE), a cross-layer `summary`,
  per-`domains[]` status (`fire`/`evacuation`/`weather`/`roads`/`seismic`),
  `topEvents`, and a `sources[]` health sidecar.
- `GET /api/v1/places/{place}/map/{layer}.geojson` - hand-built, one RFC 7946
  `FeatureCollection` per layer for a maps client (MapLibre/Leaflet). Layers:
  `road_incident`, `chain_control`, `road_segment`, `weather_alert`,
  `fire_weather`, `earthquake`, `wildfire`, `evacuation` (these are layer *values*,
  still snake_case). Every feature shares a camelCase `properties` envelope
  (`id, layer, kind, severity, severityRank, headline, source, …`) on the unified
  severity scale `INFO..EXTREME` (rank 0–4). Coordinates
  are `[lng, lat]`. Event layers project from the store
  (`internal/gridapi.ProjectEvents`); the three condition layers (`road_segment`,
  `chain_control`, `fire_weather`) are live projections of the roads/weather
  services. See `docs/hazard-aggregation-design.md` and `internal/hazards/CLAUDE.md`.

**Fire-weather** (`conditions.fireWeather`, and the `fire_weather` geojson layer):
`state` escalates `normal` → `elevated` (Fire Weather Watch) → `red-flag` (Red Flag
Warning), derived only from authoritative NWS products — never a Red Flag NWS
hasn't issued.

**`sourceStatus` honesty (geojson `metadata`, summary `domains[]`)** is
`OK | STALE | UNAVAILABLE` — a layer is fail-loud: on source error it returns
`UNAVAILABLE` with empty features (or `STALE` + `lastSourceUpdate` when serving a
cached last-good fetch), never a fabricated clear state.

- **Evacuation is life-safety / fail-loud** on `summary`: the invariant is *an error
  never becomes a `0`*. A Cal OES failure is `UNAVAILABLE` →
  `summary.activeEvacuations: null` (with `evacuationStatus: UNAVAILABLE`; render
  "unknown — check Genasys"); a clean fetch with no active zones is `OK` →
  `activeEvacuations: 0` (render "no active evacuations reported", a caveated
  confirmed-empty, not a guarantee); `N>0` for active zones. `metadata.source_url`
  always links the authoritative Genasys viewer. Areas configured under
  `hazards.areas` in `prefab.yaml`.

**Persistence** (`internal/store`, SQLite at `grid.dbPath`, WAL mode): **the
store is the system of record** for grid events. The canonical value is the proto
blob (`grid.v1.Event`) — scalar columns exist only as query indexes and every
read rehydrates from the blob. Writes are single-writer (the ingest scheduler,
serialized through a mutex); reads run concurrently under WAL. Every content
change or lifecycle transition is a revision snapshot in `event_revisions`, so a
restart rehydrates events + history with no re-fetch. The in-memory TTL caches
(`internal/cache`) remain on the read path for the roads/weather/hazards services
— they are not the source of truth. See `internal/store/CLAUDE.md` and
`internal/ingest/CLAUDE.md` before touching the store or a poller.

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
4. Restart server; verify the road's incidents/segments appear via
   `/api/v1/events?layer=road_incident` and the `road_segment` map layer

**Adding New Weather Locations**:
1. Update `prefab.yaml` weather locations section, including the `zone` field
   (the NWS forecast zone containing the location — must also be listed in
   `weather.nws.zones`, or the location gets no alerts)
2. Test with `./bin/test-weather` using new coordinates
3. Restart server and verify in the `/api/v1/conditions` response
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

