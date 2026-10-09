# The Grid — Development Guidelines

The Grid is the S.I.E.R.R.A data service (primary domain `data.sierragridteam.org`;
`info.ersn.net` is a legacy CNAME alias, ersn.net a consuming site). The Go module
path and GitHub repo are `github.com/dpup/sierra-data` (renamed 2026-07-06 from
`github.com/dpup/info.ersn.net/server`).

Last updated: 2026-07-06

## Active Technologies

**Language/Version**: Go 1.25+ (`go.mod` declares 1.25.0 — a transitive dep, `golang.org/x/sys`, requires it; the Dockerfile builds on `golang:1.25-alpine`)  
**Primary Dependencies**: gRPC, gRPC Gateway, Prefab framework (github.com/dpup/prefab), Protocol Buffers  
**Storage**: SQLite (pure-Go `modernc.org/sqlite`, WAL) is the grid event store's system of record — events, revision history, the place directory, and source health, persisted at `grid.dbPath` (`PF__GRID__DB_PATH`). In-memory TTL caches remain on the read path for the roads/weather/hazards services.  
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
│   ├── test-weather/          # Weather API testing tool
│   └── test-pge/              # PG&E outage/PSPS testing tool (+ freshness probe)
├── internal/                  # Private application code
│   ├── services/              # gRPC service implementations
│   ├── clients/               # External API clients
│   ├── cache/                 # In-memory caching with TTL
│   ├── config/                # Configuration management
│   ├── hazards/               # /api/v1 unified GeoJSON hazard layers
│   ├── store/                 # SQLite grid event store (events, revisions, places, sources)
│   ├── ingest/                # Poller scheduler + per-source normalizers → the store
│   ├── pushingest/            # Authenticated push ingest (POST /api/v1/ingest/{stream})
│   ├── gridapi/               # /api/v1 GridService impl (gRPC) + hand-built GeoJSON
│   ├── places/                # Grid place directory seeder (areas/counties/towns/corridors)
│   └── lib/                   # Shared libraries (incl. lib/geojson: geometry + PIP)
├── data/places/               # Checked-in Census county polygons (counties.geojson)
├── site/                      # Embedded data.sierragridteam.org static site (served at /)
├── tests/                     # Test files and test data
├── docs/design/               # design docs & specs for work that has shipped (historical record)
├── docs/solutions/            # documented solutions to past problems (bugs, best practices, patterns), by category with YAML frontmatter (module, tags, problem_type)
├── CONCEPTS.md                # shared domain vocabulary (entities, named processes, status concepts)
└── Makefile                   # Build automation
```

The runtime SQLite database lives under `data/` (`data/grid.db`), which is
git-ignored; only `data/places/` is checked in.

The embedded site's build output (`site/dist`) is git-ignored too — unlike the
generated `*.pb.go`, it is **not** committed. `make server`/`run`/`test` rebuild
it from `web/` when it is stale (via `site-ensure`, ~1.5s), and the Docker image
builds it in its own `site-builder` stage, so a stale site cannot ship. Only
`site/dist/.gitkeep` is committed — `//go:embed all:dist` needs the directory to
exist — and `web/public/.gitkeep` restores it after `astro build` empties the
output directory. See `web/CLAUDE.md`.

## Commands

Whenever possible you MUST use a command provided by the makefile. If you need additional functionality
discuss with the operator improvements to the makefile commands.

**Toolchain note**: The sandbox does not ship Go or protoc preinstalled. To build
or run tests you need Go 1.25+ on `PATH`. `make build`/`server`/`run`/`test` also
need **Node 22+**, because they build the embedded site; plain `go build ./...`
and `go test ./...` stay Node-free (the site tests skip when `site/dist` holds
only its placeholder). `make proto` additionally requires
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
  PF__GRID__DB_PATH=<scratch>/verify.db ./bin/server`) rather than fighting 8181,
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
./bin/test-pge
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
- **One write endpoint, and only one**: `POST /api/v1/ingest/{stream}`
  (`internal/pushingest`) — the sole authenticated route (bearer token per
  reporter, hash-only in config). Streams today: `mesh.repeater` (operator
  repeater telemetry) and `burn.line` (county burn-day readings). Neither writes
  events: mesh buffers in memory, burn stages a store row, and the ingest
  scheduler remains the only writer of events. It stays browser-unreachable
  cross-origin because `corsAllowMethods: [GET]` denies the POST preflight.
  Do not add POST to that list.
- **CORS is open**: `corsOrigins: ["*"]` in `prefab.yaml` emits a literal
  `Access-Control-Allow-Origin: *` for every origin (prefab >= v0.6.1's wildcard
  sentinel). Safe here because the API is public, read-only, and keyless — a
  cross-origin page gets only what `curl` already gets. Two invariants keep it
  safe and MUST hold: `corsAllowMethods: [GET]` (this, not the origin list, is
  what keeps the `/mcp` POST surface browser-unreachable cross-origin — POST
  preflights are denied) and `corsAllowCredentials: false` (required with `*`;
  prefab refuses to start on `*` + credentials). Never reflect an origin *with*
  credentials — that's the CORS hole this deliberately avoids.
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
# Production points this at the EFS mount; the Dockerfile sets it and declares
# a /data volume so events/revisions/source-health survive container replacement.
# EFS, not EBS — this file said EBS until 2026-08-14 while the Dockerfile and
# internal/store/CLAUDE.md said EFS, and the difference decides whether WAL is
# safe (it is NOT on EFS: the -shm is memory-mapped). Keep grid.journalMode on
# TRUNCATE. It also means every random row read is a network round trip, which
# is why the store's index statistics matter so much — see store.Analyze.
export PF__GRID__DB_PATH=/data/grid.db

# Push-ingest credentials are NOT env vars: a reporter's token hash lives in
# prefab.yaml (grid.ingest.reporters[].tokenSha256) and the token itself only on
# the operator's machine. Mint with `make ingest-token`.
```

**Env-var naming — a camelCase config key needs an underscore.** prefab maps
`PF__A__B_C` → `a.bC`, so `grid.dbPath` is **`PF__GRID__DB_PATH`**;
`PF__GRID__DBPATH` maps to `grid.dbpath` and matches nothing. This is **enforced,
not just documented**: `config.ValidateEnvOverrides` (called from `LoadConfig`)
reflects over `Config` and refuses to start on a `PF__` override in our
namespaces that resolves to no real key, naming the variable you meant. You don't
need to remember the rule or register new keys — just read the error.

**Push-ingest credentials (a public repo holds only hashes)**: a reporter
authenticates with a bearer token whose **SHA-256 hash alone** lives in
`prefab.yaml` (`grid.ingest.reporters[].tokenSha256`). A hash of a 256-bit random
token cannot be replayed or reversed, so committing it is safe and adding a
reporter stays an ordinary PR — while the token itself exists only on the
operator's machine. `make ingest-token REPORTER=<id> NAME="..."` mints one and
writes the config entry (re-run with an existing id to ROTATE that reporter's
token in place, keeping its other settings); bare `make ingest-token` just prints
a pair. Never put the token in config, env, or a commit; a bad/typo'd hash is
FATAL at startup by design (a skipped reporter would silently never
authenticate).

**Configuration Files**:
- `prefab.yaml` - Application configuration (API refresh intervals, route
  definitions, and the `grid` section: `dbPath`, per-source poll intervals +
  disappearance policy, NWS-alert enhancement budget, and `grid.wildfire` —
  the fire layer's own, deliberately **wider** geography: `marginDegrees`
  grows the `hazards.areas` union for fire ingest, `placeBufferMeters` lets an
  approaching fire attach to an area/town it has not reached yet. Fire is the
  only layer with its own geography; see `internal/ingest/CLAUDE.md`.) Also
  `grid.power.outageStaleAfter` — the PG&E freeze detector, not a fetch timeout;
  see the PG&E notes below. And `grid.burn` — the tracked `counties`, the
  recorded `lines` (id, phone, the counties each speaks for, and whether we dial
  it) plus `burnDayStaleAfter`, the equivalent freeze detector for a pushed
  burn-line reading.
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

**Caltrans CWWP2 portal** (`cwwp2.dot.ca.gov`, JSON per district):
- Chain-control levels (merged with `cc.kml`) and **the `road_incident` layer's
  lane closures** (`roads.caltransFeeds.cwwp2.laneClosureDistricts: [3, 10]`,
  replacing `lcs2way.kml` there). One event per work window, SCHEDULED until the
  crew radios it set up; deterministic text and severity, no AI. Undocumented
  (the docs page answers 403), hand-templated JSON, no gzip.
- See the CWWP2 section of `internal/clients/CLAUDE.md` and "Lane closures" in
  `internal/ingest/CLAUDE.md`. Probe live with `./bin/test-caltrans -feed=cwwp2`.

**National Weather Service** (`api.weather.gov`):
- Authoritative zone alerts (watches/warnings) and fire-weather products
- No API key; requires a descriptive `User-Agent` (`weather.nws.userAgent`)
- Zones for the service area (NWS Sacramento/STO, elevation-banded, cover both
  Calaveras & Tuolumne): CAZ137 (1000–3000 ft), CAZ138 (3000–5000 ft), CAZ139
  (above 5000 ft). Always verify a zone with `api.weather.gov/points/{lat},{lng}`
  — do NOT guess codes (the old CAZ064/065/258/259 were wrong; CAZ065 is a SoCal
  zone that leaked out-of-area alerts).
- Powers `/weather/alerts` zone alerts and the `fire_weather` classification

**MeshCore MQTT bridges** (mesh-node presence, `MESH` layer — renamed from
`NETWORK` 2026-07-25; the enum number 13 is unchanged and `?layer=network`
survives as a legacy alias):
- MeshCore has **no native MQTT and no official broker/topic spec** — we
  subscribe to community bridges (`grid.meshcore.brokers`, several for
  resilience). **Live** (`grid.meshcore.enabled: true`) against
  `wss://mqtt.gomesh.dev:443/mqtt`; the subscriber credential is injected as
  `PF__GRID__MESHCORE__USERNAME`/`PASSWORD`, never committed.
- The map-ecosystem bridges publish a JSON envelope per packet to
  `meshcore/{IATA}/{PUBLIC_KEY}/packets` with `packet_type`, `SNR`, `RSSI`,
  `path`, and a hex `raw` payload. We ingest **only ADVERT packets
  (`packet_type` 4)** — unencrypted, carrying pubkey/role/location/name — and
  decode `raw` in `internal/clients/meshcore` (std-lib crypto; no heavy dep).
- **`mqtt.bayme.sh` is Bay Area Mesh's Meshtastic broker — a different protocol,
  not usable here.** MeshCore brokers: `mqtt.gomesh.dev` (ours; operator
  "gomesh.dev" — it was mislabeled "LetsMesh" until 2026-10-09),
  `mqtt.meshmapper.net`, LetsMesh US/EU, `mqttmc01.bostonme.sh` (all WSS+TLS on
  :443).
- **Auth: subscribing is operator-gated, not self-serve.** These brokers split
  auth (michaelhart/meshcore-mqtt-broker model): *publishing* is self-sovereign
  (username `v1_{PUBKEY}` + a self-signed Ed25519 JWT password, no allowlist),
  but *subscribing* — what we do — needs a **separate operator-issued
  `username:password`** account (`SUBSCRIBER_N=user:pass:role`). So a device key
  alone can't read the feed; request a subscriber credential (meshmapper: a
  Region Onboarding form → creds via Discord). Our client already supports this —
  the operator drops the issued user/pass into `grid.meshcore.brokers[]`; the
  device-key JWT path is not needed for a read-only subscriber. Before trusting a
  bridge, confirm the `raw` framing against a live capture, then flip
  `requireValidSignature` on. For local testing, self-host a broker + a publishing
  observer (Cisien/meshcoretomqtt) rather than waiting on access.
- Architecture: a long-lived subscriber (`meshcore.Registry`) buffers node state;
  `ingest.NetworkNormalizer` serves a snapshot on each scheduler tick (a push
  source wrapped as a poller). Lifecycle is `disappearance: expire` (no goodbye
  packet). **A configured broker that is not delivering fails the `meshcore`
  source** (`PerSource`: no sweep, STALE/UNAVAILABLE on `/sources`), even while
  a monitor is live, and with nothing else live `Poll` hard-errors. "Delivering"
  = a session that is really open (paho `IsConnectionOpen`, NOT `IsConnected`,
  which is true while reconnecting) and carried a message within
  `grid.meshcore.silenceAfter` (15m). That gap hid a six-day outage in
  2026-10; see "A deaf broker" in `internal/ingest/CLAUDE.md`. The MQTT client
  id gets a random per-process suffix so two of our processes never share a
  broker session.
- **Cross-check the feed with CoreScope** (`corescope.stonekitty.net`, open
  source: Kpa-clawbot/CoreScope): a keyless analyzer subscribed to the same
  gomesh broker. `/api/nodes/{pubkey}` lists a node's recent adverts with every
  receiving observer and path; `/api/observers`, `/api/mqtt/status`. If it sees
  adverts our store does not, the problem is ours. Each mesh event's
  `canonicalUrl` is the node's CoreScope page (`grid.meshcore.nodeUrl`).

**PG&E ArcGIS** (`ags.pge.esriemcs.com`, the `POWER` layer):
- Keyless and public, but **entirely undocumented** — no contract, no version,
  no published terms, no robots.txt. Same risk posture as the Caltrans KML
  feeds. Two sources, one poller: `pge` (outages) and `psps` (Public Safety
  Power Shutoffs), which are separate services that fail independently.
- **These endpoints fail by FREEZING, not by erroring** — the ETL stalls while
  the layers keep answering 200 with the last set. PG&E's own `lastupdate_time`
  stamp is the only way to see it, and `grid.power.outageStaleAfter` (1h) turns
  a stalled stamp into a source failure. Do NOT apply that gate to `psps`: its
  stamp legitimately idles for weeks between shutoff events.
- **Never consume `43/psps_staging`** — it holds PG&E's TEST events
  (`PSPS_05312024_SKN9_TEST52`, future-dated windows). It is only useful for
  reading the PSPS schema when no real event is running.
- Severity is customer-count driven and the **statewide median outage affects
  ONE customer**, so most rows are `INFO` by design; consumers filter with
  `severity_min`. We ingest every row regardless — dropping small ones would
  make the `resolve` sweep fabricate restorations.
- ETOR / de-energization end are **estimates PG&E routinely overruns** and are
  deliberately never mapped onto `Event.expires`.
- Diagnose with `./bin/test-pge` (`make test-pge`); see
  `internal/clients/CLAUDE.md` for field-type traps and query hygiene.

**CAL FIRE burn permits** (`burnpermit.fire.ca.gov`, the `calfire-burn` source):
- Per-county suspension of residential burning on State Responsibility Area land.
- **No API — an HTML scrape behind Akamai bot management.** The host 403s any
  client that does not present as a browser navigation, and a descriptive bot
  User-Agent is refused with otherwise identical headers; the required
  `Sec-Fetch-*` + Chrome UA header set is load-bearing. Its own `robots.txt` has
  ZERO Disallow rules, so the declared crawl policy permits what the edge blocks.
  Polled twice a day. Expect breakage; it is deliberately the SECONDARY source.
- An empty table is an ERROR, never "no county is suspended". Effective times are
  Pacific with no zone marker. See `internal/clients/CLAUDE.md`.

**County burn line** (a recorded phone line, the `burnline` source):
- The daily permissive-burn-day call. **The authority is a phone number, not an
  API** (Calaveras: 209-754-6600). `cmd/burn-line` places a recorded call via
  Twilio, transcribes it with Whisper, and extracts the status with a
  structured-output chat call.
- **It runs from CI, not the server** (`.github/workflows/burn-line.yml`, daily
  at 14:00 UTC) and PUSHES the readings to `POST /api/v1/ingest/burn.line` in ONE
  batched report (the endpoint rate-limits per reporter, so a push per line would
  429 everything after the first). A phone call costs
  money and must happen once a day; a scheduled workflow has those semantics, a
  restarting server does not.
- The value can therefore be wrong in ways an API cannot (a `confidence` and the
  cleaned transcript ride along), and a stopped pipeline is INVISIBLE except by
  age — `grid.burn.burnDayStaleAfter` (36h, one missed run plus slack) turns an
  old reading into a source failure and blanks the facet to UNKNOWN.
- **A LINE is the config unit, not a county** (`grid.burn.lines`): a county can
  have several relevant recordings, and one air-district line routinely covers
  several counties (list them all and it is dialed ONCE). A county's lines merge
  taking the MOST RESTRICTIVE answer; each line's own answer stays visible in
  `burnStatus.burnLines[]`.
- Calaveras is dialed today. Tuolumne's number (209-533-5598) is published on its
  event but not dialed (`enabled: false`), so it carries the CAL FIRE facet with
  `burnDay` UNKNOWN. **`prefab.yaml` is the single source of truth** — the
  workflow enumerates no numbers, it just runs the tool, so adding or enabling a
  line is one edit in one file.

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
see `docs/design/grpc-gateway-migration-plan.md`, `docs/design/v2-api-spec.md`). The `GridService`
proto (`api/grid/v1/grid.proto`) is served over the gateway that Prefab mounts at
`/api/`; the impl is `internal/gridapi` (`GridServer` wrapping `Service`), reading
everything from the grid event store. Field names are **camelCase** (protojson
`UseProtoNames:false`), timestamps RFC 3339, errors gRPC-standard
`{code, codeName, message, details}` with the mapped HTTP status. gRPC reflection
is on. Conditional GET (ETag/If-None-Match -> 304) is wired via prefab's `etag`
plugin on most read RPCs — event detail, the event/history lists, places, and
cameras (`GetPlaceSummary`/`GetConditions`/`ListSources`/`ResolvePlace`/
`ListScanners` are not yet guarded); the `.geojson` keeps its own body-hash
ETag. The prior
hand-built REST surfaces (the old `/api/v1/roads|weather|
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
  incidents endpoint. CHP road incidents are AI-enhanced (`enhancement`:
  description/summary/impact/metadata), with `severity` driven by the model's
  impact assessment. Caltrans lane closures (CWWP2) are not: their text and
  severity are composed from Caltrans's fields, and a planned window not yet set
  up is `SCHEDULED` (excluded from the place summary's rollups).
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
- `GET /api/v1/cameras?place=` - `ListCameras`: Caltrans CCTV cameras (CWWP2
  `cctv`, District 10) within `roads.caltransFeeds.cwwp2.cameras.nearMeters`
  (25 km) of a coverage area; `?place` keeps those within the same radius of
  the place, nearest first (`distanceMeters`). Snapshot `imageUrl` + HLS
  `streamUrl` are **links to Caltrans, never proxied**. Out-of-service cameras
  are dropped. Not events. The list is near-static, refreshed in the
  background every 6h (`services.CameraService`); a failed refresh serves
  the last good list as `sourceStatus: STALE`.
- `GET /api/v1/sources` - the source registry + per-source health (a source's own
  health is `status`: `OK|STALE|UNAVAILABLE`, last success/attempt, poll interval,
  last error). Includes one **health-only** row per configured push reporter.
- `POST /api/v1/ingest/{stream}` - **the one WRITE endpoint**, and the only one
  requiring a credential (`Authorization: Bearer`). Operator-run monitors push
  data no upstream feed publishes. Two streams: `mesh.repeater` carries MeshCore
  repeater admin telemetry + explicit reachability; `mesh.packet` forwards the
  raw advert frames a companion radio heard (the SIERRA backbone reaches the
  community MQTT brokers only every few days; a companion in Arnold hears it
  daily), handed to the MeshCore registry through the same decode and
  signature check as an MQTT reception. Not mounted unless
  `grid.ingest.reporters` is non-empty. It does NOT write the store — it buffers
  (or, for packets, updates the in-memory registry),
  and the mesh poller merges on its next tick, so single-writer discipline holds.
  `corsAllowMethods: [GET]` is what keeps it browser-unreachable cross-origin;
  never add POST there. See `internal/pushingest` and `internal/ingest/CLAUDE.md`;
  **`docs/mesh-reporter-guide.md` is the shareable setup guide** to hand an
  operator who is wiring up a monitor (payload, headers, rules, error handling).

**Summary + map:**
- `GET /api/v1/places/{place}/summary` - `GetPlaceSummary` RPC (camelCase): a
  one-fetch place rollup — `mode` (QUIET/WATCH/ACTIVE), a cross-layer `summary`,
  per-`domains[]` status (`fire`/`evacuation`/`weather`/`roads`/`seismic`/`power`,
  plus `comms` when the MeshCore source is enabled), `topEvents`, and a `sources[]`
  health sidecar. Mesh-node presence (`MESH`) is ambient `INFO` state: it is
  excluded from `totalActive`/`severityCounts`/`topEvents`/`mode` (like baseline
  conditions) and appears only in the `comms` domain.
- `GET /api/v1/places/{place}/map/{layer}.geojson` - hand-built, one RFC 7946
  `FeatureCollection` per layer for a maps client (MapLibre/Leaflet). Layers:
  `road_incident`, `chain_control`, `road_segment`, `message_sign`,
  `weather_alert`, `fire_weather`, `earthquake`, `wildfire`, `evacuation`,
  `power`, `mesh_node`, `mesh_link`, `camera` (these are layer *values*, still snake_case; `power` matches its enum name so
  `properties.layer` and `Event.layer` read identically). Every feature shares a camelCase `properties` envelope
  (`id, layer, kind, severity, severityRank, headline, source, …`) on the unified
  severity scale `INFO..EXTREME` (rank 0–4). Coordinates
  are `[lng, lat]`. Event layers project from the store
  (`internal/gridapi.ProjectEvents`); the four condition layers (`road_segment`,
  `chain_control`, `fire_weather`, `message_sign`) are live projections of the
  roads/weather services and the Caltrans CWWP2 portal. `message_sign` (what
  Caltrans's roadside message signs are showing) is always `INFO` and `/summary`
  never reads it: the text is context, mostly safety-campaign boilerplate.
  See `docs/design/hazard-aggregation-design.md` and `internal/hazards/CLAUDE.md`.
  Two more layers are served by `gridapi` itself: `mesh_link` (relay topology,
  `internal/gridapi/mesh.go`) and `camera` (Caltrans traffic cameras,
  `serveCameraLayer` in `internal/gridapi/cameras.go`). `camera` is the place-scoped
  `ListCameras` as GeoJSON — same cameras, distances and ids, every feature
  `INFO`, `sourceStatus` from the camera directory — and stays out of the
  place summary: cameras are reference views, not hazards.

**Burn status** (`layer=burn_status`, no geojson layer): per-county residential
burning status from TWO independent authorities that must BOTH permit a burn —
the county air district's permissive-burn-day call (daily) and CAL FIRE's
seasonal suspension on SRA land (twice a year). Carried as separate facets on one
ambient, permanently-ACTIVE event per county, so the **revision history is the
product** ("when did it change"). Excluded from the summary hazard rollup like
mesh presence; surfaces in its own `burn` domain. `permission` is deliberately
pessimistic: PROHIBITED is conclusive from either facet alone, ALLOWED needs both
known and permissive, everything else UNKNOWN. See `internal/ingest/CLAUDE.md`.

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
  confirmed-empty, not a guarantee); `N>0` for active zones. `metadata.sourceUrl`
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

