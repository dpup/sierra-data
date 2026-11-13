# The Grid

The data service behind **S.I.E.R.R.A**: a public, read-only, keyless API of
hazard, road, weather, power and mesh-network data for the central Sierra
(Calaveras, Tuolumne and neighbouring counties, along the Hwy 4 / Hwy 49
corridor).

Live at **https://data.sierragridteam.org** (its former name, `info.ersn.net`,
no longer resolves); ersn.net is a consuming site.

## What it does

Pollers read each upstream source (CAL FIRE and FIRIS, Cal OES evacuation zones,
the National Weather Service, USGS, CHP, Caltrans, PG&E, county burn lines, and
MeshCore community MQTT bridges), normalise every occurrence into one `Event`
model, and store it in PostgreSQL with its full revision history. Road travel times
come from the Google Routes API and current weather from OpenWeatherMap. CHP
incidents are rewritten into plain language by OpenAI, with the original text
kept verbatim.

The store is served as one API, `/api/v1`: gRPC with a gRPC-Gateway JSON
mapping (camelCase fields, RFC 3339 timestamps), plus RFC 7946 GeoJSON map
layers. The same binary serves a one-page overview for API consumers at `/`
(with `/llms.txt` and `/changelog`) and an MCP endpoint at `/mcp` for LLM
agents. The data explorer is a separate project,
[sierra-grid](https://github.com/sierragridteam/sierra-grid), at
grid.sierragridteam.org.

Every source reports its health (`OK | STALE | UNAVAILABLE`). A failed source
is reported as unknown, never as an all-clear: in particular, an evacuation
count of `null` means "unknown" and is distinct from `0`.

## Using the API

No key, CORS open to any origin. A place dashboard takes four calls:

```bash
# 1. Which places contain a point (most specific first)? Or list them: /api/v1/places
curl 'https://data.sierragridteam.org/api/v1/places:resolve?lat=38.2650&lng=-120.3337'

# 2. One-fetch rollup for a place: mode, per-domain status, evacuation count, source health
curl https://data.sierragridteam.org/api/v1/places/calaveras-county/summary

# 3. What is in effect there now
curl 'https://data.sierragridteam.org/api/v1/events?place=calaveras-county&status=ACTIVE'

# 4. A map layer as a GeoJSON FeatureCollection
curl https://data.sierragridteam.org/api/v1/places/calaveras-county/map/wildfire.geojson
```

- **Overview, conventions and a prompt for agents:** https://data.sierragridteam.org/ (and `/llms.txt`)
- **OpenAPI spec** (the reference; for code generation and agents): https://data.sierragridteam.org/api/openapi.json
- **Changelog** of API changes, with migration notes: https://data.sierragridteam.org/changelog ([`CHANGELOG.md`](CHANGELOG.md))
- **MCP** for LLM agents: `https://data.sierragridteam.org/mcp` (Streamable HTTP, keyless)

## Quick Start

### Prerequisites

- Go 1.25+
- Docker, for the local PostgreSQL that `make run` and `make test` use
- API keys for Google Routes, OpenWeatherMap and OpenAI. The server refuses to
  start without an OpenAI key (it enhances CHP road incidents); for a local run
  without enhancement, any non-empty placeholder lets it start.

### Installation

1. Clone the repository:
   ```bash
   git clone git@github.com:sierragridteam/sierra-data.git
   cd sierra-data
   ```

2. Set up environment variables:
   ```bash
   export PF__GOOGLE_ROUTES__API_KEY="your-google-api-key"
   export PF__OPENWEATHER__API_KEY="your-openweather-api-key"
   export PF__OPENAI__API_KEY="your-openai-api-key"  # Required to start; used for AI-enhanced incidents
   ```

3. Build and run (the server listens on port 8181):
   ```bash
   make build
   make run
   ```

4. Query it:
   ```bash
   curl http://localhost:8181/api/v1/sources
   curl 'http://localhost:8181/api/v1/events?place=ebbetts-pass&status=ACTIVE'
   ```

### Configuration

Configuration lives in `prefab.yaml`: the coverage areas, monitored roads,
weather locations, and per-source poll intervals under `grid`. Any key can be
overridden by an environment variable with the `PF__` prefix, where `__`
separates levels and `_` marks a camelCase boundary: `grid.dbPath` is
`PF__GRID__DB_PATH`. The server refuses to start on a `PF__` variable that
matches no configuration key, and names the key it expected.

Secrets (API keys, the MeshCore broker credentials) are set only through
environment variables, never in `prefab.yaml`.

## Deployment

`main` deploys to the Grid account's ECS services through GitHub Actions:
CI passing on a push to `main` starts the Deploy workflow
(`.github/workflows/deploy.yml`), which waits for a reviewer in the
`production` environment, then builds and pushes the `grid` and `relay`
images (tagged with the commit and the git tag), registers task-definition
revisions with only the image changed, and follows each ECS blue/green
deployment to its end. A service whose image did not change is not
redeployed.

```bash
make deploy               # HEAD: on main, pushed, clean, CI green; dispatches and watches the run
make deploy REF=v1.4.0    # redeploy an earlier tag or commit on main
make deploy-status        # recent Deploy runs
```

`make deploy` needs `gh`, not AWS credentials. Repository variables,
rollback and what the Grid does during a deploy:
[docs/deploy-handoff.md](docs/deploy-handoff.md).

`legacy/efs-sqlite` is the archived SQLite-on-EFS stack that ran in the old
AWS account until 2026-11-13; nothing deploys from it, and its
`make ecs-deploy` refuses on `main`.

## Development

### Build Commands

```bash
# Build all binaries
make build

# Build specific components
make server    # Main API server
make tools     # CLI testing tools

# Generate protobuf code
make proto

# Clean build artifacts
make clean
```

### Running the Server

```bash
# Run in foreground
make run

# Run in background for testing
make run-bg

# Stop background server
make stop
```

### Testing

```bash
# Run all tests against the Docker PostgreSQL
make test

# Go tests only, without a database (store-backed tests skip)
go test ./...

# Probe individual upstream sources
make test-google       # Google Routes API
make test-caltrans     # Caltrans feeds (FEED=cwwp2 for the CWWP2 portal)
make test-weather      # OpenWeatherMap
make test-pge          # PG&E outages / PSPS
make test-meshcore     # MeshCore MQTT broker
```

### Code Quality

```bash
make lint   # golangci-lint
make fmt    # go fmt
```

### Project Structure

```
/
├── api/grid/v1/          # grid.v1 protos: Event, Place, … and the GridService (/api/v1)
├── api/v1/               # older road/weather protos used internally
├── cmd/
│   ├── server/           # the Grid server (API, MCP, landing page in server/landing)
│   ├── mesh-relay/       # SIERRA Mesh Relay, a separate service
│   ├── burn-line/        # county burn-line reader (runs from CI)
│   └── test-*/           # per-source diagnostic CLIs
├── internal/
│   ├── store/            # PostgreSQL event store (events, revisions, places, sources)
│   ├── ingest/           # poller scheduler and per-source normalizers
│   ├── gridapi/          # GridService implementation and GeoJSON layers
│   ├── hazards/          # GeoJSON layer projection
│   ├── mcp/              # MCP endpoint
│   ├── pushingest/       # authenticated push ingest
│   ├── clients/          # upstream API clients
│   └── services/         # road and weather services
├── docs/                 # design records and operator guides
├── prefab.yaml           # configuration
├── CHANGELOG.md          # API changes, published at /changelog
└── Makefile
```

`AGENTS.md` and the `AGENTS.md` files in subdirectories hold the detailed
development notes.

### Adding New Roads

1. Update `prefab.yaml` with new road coordinates (note the simplified structure):
   ```yaml
   roads:
     monitoredRoads:  # Note: camelCase naming
       - name: "Highway Name"
         section: "Start to End"
         id: "unique-road-id"
         origin:
           latitude: 0.0
           longitude: 0.0
         destination:
           latitude: 0.0
           longitude: 0.0
   ```

2. Test with the Google Routes API tool:
   ```bash
   make test-google
   ```

3. Restart the server to pick up configuration changes, then check the road's
   incidents and segments:
   ```bash
   make run-bg
   curl 'http://localhost:8181/api/v1/events?layer=road_incident&status=ACTIVE'
   curl http://localhost:8181/api/v1/places/ebbetts-pass/map/road_segment.geojson
   ```

### Upstream API Limits

- **Google Routes API**: 3,000 queries per minute
- **OpenWeatherMap API**: 60 calls per minute (free tier). Only `/data/2.5/weather` is used — the One Call 3.0 endpoint has a separate 1,000 calls/day cap and is deliberately avoided (alerts come from NWS instead)
- **Caltrans KML feeds and CWWP2 portal**: no published limits; polled every 5–15 minutes

### Architecture

- **Ingest**: one scheduler polls every source and is the only writer of
  events to the SQLite store; every content change or lifecycle transition is a
  stored revision.
- **API**: the `GridService` proto served over gRPC and gRPC-Gateway at
  `/api/v1`, reading from the store. The GeoJSON layers are hand-built handlers
  on the same mux.
- **Caches**: in-memory TTL caches front the road and weather services.
- **Framework**: [Prefab](https://github.com/dpup/prefab) provides the server,
  configuration and logging.

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Make your changes following the existing code style
4. Run tests and linting (`make lint && make test`)
5. Commit your changes (`git commit -m 'Add amazing feature'`)
6. Push to the branch (`git push origin feature/amazing-feature`)
7. Open a Pull Request

## Support

For questions, issues, or feature requests, please open an issue on the GitHub repository.

## License

MIT License

Copyright (c) 2025 Daniel Pupius

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.