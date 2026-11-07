# The Grid

The data service behind **S.I.E.R.R.A**: a public, read-only, keyless API of
hazard, road, weather, power and mesh-network data for the central Sierra
(Calaveras, Tuolumne and neighbouring counties, along the Hwy 4 / Hwy 49
corridor).

Live at **https://data.sierragridteam.org**. `info.ersn.net` remains a CNAME
alias, and ersn.net is a consuming site.

## What it does

Pollers read each upstream source (CAL FIRE and FIRIS, Cal OES evacuation zones,
the National Weather Service, USGS, CHP, Caltrans, PG&E, county burn lines, and
MeshCore community MQTT bridges), normalise every occurrence into one `Event`
model, and store it in SQLite with its full revision history. Road travel times
come from the Google Routes API and current weather from OpenWeatherMap. CHP
incidents are rewritten into plain language by OpenAI, with the original text
kept verbatim.

The store is served as one API, `/api/v1`: gRPC with a gRPC-Gateway JSON
mapping (camelCase fields, RFC 3339 timestamps), plus RFC 7946 GeoJSON map
layers. The same binary serves an embedded site at `/` that is itself a client
of that API, and an MCP endpoint at `/mcp` for LLM agents.

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

- **Reference and rendering rules:** https://data.sierragridteam.org/docs
- **OpenAPI spec** (for code generation and agents): https://data.sierragridteam.org/api/openapi.json
- **Changelog** of API changes, with migration notes: https://data.sierragridteam.org/changelog ([`CHANGELOG.md`](CHANGELOG.md))
- **MCP** for LLM agents: https://data.sierragridteam.org/mcp-guide

## Quick Start

### Prerequisites

- Go 1.25+
- Node 22+ (`make build`, `make run` and `make test` build the embedded site)
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

The repository includes a self-contained client for the Sierra deployment
coordinator in AWS. Deployers need this checkout and AWS credentials; no
infrastructure-repository checkout, deployment token or bastion connection is
required. The AWS endpoint manages task definitions, standby slots, database
ownership, load-balancer registration and cleanup.

### Deploy

With the existing build toolchain installed and AWS credentials configured:

```bash
# Uses the normal AWS credential chain: environment credentials, SSO/profile, etc.
make ecs-deploy

# Or choose a named profile and an explicit image tag.
AWS_PROFILE=my-profile make ecs-deploy DOCKER_TAG=release-abc123
```

This checks the deployment client, discovers both ECR repositories from
`sierra-data-coordinator`, builds and pushes Grid and relay with the same tag,
then waits for the coordinated handoff. The default deployment tag is the
current commit's 12-character SHA. Both images are built even when only one
binary changed. Tags are resolved to immutable digests after the pushes.

It refuses to build from a working tree with uncommitted changes to tracked
files, which would ship under the commit's tag and come back on a rollback to
it. Commit or discard them, or override with `make ecs-deploy ALLOW_DIRTY=1`.

For images already pushed under the same tag in both repositories:

```bash
python3 scripts/deploy.py deploy --tag release-abc123 --wait
```

The region defaults to `AWS_REGION`, then `AWS_DEFAULT_REGION`, then
`us-east-1`. Set `SIERRA_DEPLOY_FUNCTION` (or `--function` on the script) to use
another configured coordinator. `--profile` overrides the normal AWS profile.
No `ECS_CLUSTER`, `ECS_SERVICE`, `ECS_TASK_DEFINITION` or `DOCKER_REGISTRY`
settings are needed by `make ecs-deploy`.

### Status and rollback

```bash
make ecs-status          # owner, any operation in progress, the last operation's phase timeline
make ecs-status JSON=1   # the coordinator's raw status document
make ecs-rollback
```

Rollback starts a fresh standby on the previous image pair and uses the same
ownership handoff. Successful deployment exits zero. A failed deployment that
was automatically rolled back exits nonzero; inspect status before retrying.
A CLI timeout leaves the server-side workflow running. `scripts/deploy.py
resume --wait` resumes a terminated workflow after the underlying issue is
resolved. Reuse `--operation-id` only when retrying the same start request.

The coordinator owns service counts, task-definition registration and target
membership. Application deployments go through this client; runtime settings
such as environment variables, resource limits and volumes are managed by the
infrastructure configuration.

### Prerequisites and permissions

- Python 3.9+ and AWS CLI v2 for the deployment client; it has no pip dependencies.
- Make, Docker and the repository's Go/site build toolchain for `--build`.
- Credentials with ECR push access to the configured repositories,
  `ecr:DescribeImages`, and `lambda:GetFunctionConfiguration` /
  `lambda:InvokeFunction` on the coordinator. The client needs no direct ECS
  mutation or Secrets Manager access.

Client tests run with `make test-deploy-client` and use mocked AWS/build calls.
They do not build images or deploy anything.

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
# Run all tests (builds the embedded site first; needs Node 22+)
make test

# Go tests only, without Node (site tests skip when the site is not built)
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
│   ├── server/           # the Grid server (API, MCP, embedded site)
│   ├── mesh-relay/       # SIERRA Mesh Relay, a separate service
│   ├── burn-line/        # county burn-line reader (runs from CI)
│   └── test-*/           # per-source diagnostic CLIs
├── internal/
│   ├── store/            # SQLite event store (events, revisions, places, sources)
│   ├── ingest/           # poller scheduler and per-source normalizers
│   ├── gridapi/          # GridService implementation and GeoJSON layers
│   ├── hazards/          # GeoJSON layer projection
│   ├── mcp/              # MCP endpoint
│   ├── pushingest/       # authenticated push ingest
│   ├── clients/          # upstream API clients
│   └── services/         # road and weather services
├── web/                  # Astro source for the embedded site (built into site/dist)
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