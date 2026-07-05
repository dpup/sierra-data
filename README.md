# ERSN Info Server

A real-time API server providing road, weather, and hazard information for the Ebbett's Pass region, combining data from Google Routes API, Caltrans feeds, OpenWeatherMap, USGS, CAL FIRE, and Cal OES.

## Overview

The server dynamically builds routes between geographic points using the Google Routes API, retrieving real-time traffic data and estimated travel times. Polyline geometry from Google is used to cross-reference Caltrans feeds, with alerts filtered and classified as on-route or nearby based on spatial relevance. To improve usability, OpenAI is integrated to automatically convert technical Caltrans alerts into clear, human-readable summaries.

Weather data combines OpenWeatherMap current conditions (fetched per configured location) with authoritative National Weather Service zone alerts — each location is mapped to its NWS forecast zone, so alerts come from NWS rather than OpenWeather's rate-capped One Call API.

The architecture is modular and location-agnostic, allowing easy adaptation to other regions or road networks by updating configuration.

**Live API available at: https://info.ersn.net**

## Features

- **Real-time Road Conditions**: Live traffic data, travel times, and congestion levels from Google Routes API
- **Intelligent Road Alerts**: AI-enhanced alerts from Caltrans feeds including lane closures, CHP incidents, and construction
- **Smart Alert Classification**: Route-aware filtering (ON_ROUTE/NEARBY/DISTANT) based on spatial analysis
- **AI-Enhanced Descriptions**: Automatic OpenAI conversion of technical alerts into clear, human-readable summaries
- **Weather Information**: Current conditions from OpenWeatherMap plus authoritative NWS zone alerts for multiple locations
- **Unified Hazard Layer**: Map-ready GeoJSON aggregating wildfires, earthquakes, evacuations, weather alerts, and road incidents into one standardized interface, with a fail-loud `source_status` and a life-safety evacuation contract
- **Grid Event Store (v2)**: Every hazard source normalized into a canonical event model, persisted in SQLite with full revision history, and served through a new `/v1` API plus an embedded data site — see [Grid Info Service (v2)](#grid-info-service-v2)
- **Stale-while-revalidate Caching**: Sub-100ms responses by serving cached data while refreshing in background
- **REST API**: Clean HTTP endpoints with comprehensive JSON responses
- **gRPC Support**: Native gRPC services with automatic HTTP gateway
- **Configurable & Scalable**: Location-agnostic architecture with simple YAML configuration

## API Endpoints

### Roads API

#### List All Roads
```http
GET /api/v1/roads
```

**Response Example:**
```json
{
  "roads": [
    {
      "id": "hwy4-angels-murphys",
      "name": "Hwy 4",
      "section": "Angels Camp to Murphys",
      "status": "OPEN",
      "statusExplanation": "",
      "durationMinutes": 11,
      "distanceKm": 13,
      "congestionLevel": "CLEAR",
      "delayMinutes": 0,
      "alerts": [
        {
          "type": "CONSTRUCTION",
          "severity": "INFO",
          "classification": "NEARBY",
          "title": "Lane Closure Advisory",
          "description": "Routine maintenance work on shoulder, no traffic impact expected.",
          "condensedSummary": "Shoulder work, no delays",
          "impact": "IMPACT_NONE",
        }
      ]
    }
  ],
  "lastUpdated": "2025-09-11T01:52:05.646618Z"
}
```

#### Get Specific Road
```http
GET /api/v1/roads/{road_id}
```

**Response Example:**
```json
{
  "road": {
    "id": "hwy4-murphys-arnold",
    "name": "Hwy 4",
    "section": "Murphys to Arnold",
    "status": "RESTRICTED",
    "statusExplanation": "Right lane blocked due to multi-vehicle accident near mile marker 45",
    "durationMinutes": 15,
    "distanceKm": 20,
    "congestionLevel": "CLEAR",
    "delayMinutes": 0,
    "alerts": [
      {
        "id": "250911GG0206",
        "type": "INCIDENT",
        "severity": "WARNING",
        "classification": "ON_ROUTE",
        "title": "CHP Incident 250911GG0206",
        "description": "Multi-vehicle accident blocking right lane near mile marker 45. Expect 15-20 minute delays while emergency crews clear the scene.",
        "condensedSummary": "Accident blocking right lane, 15-20 min delays",
        "startTime": "2025-09-11T01:30:00Z",
        "location": {
          "latitude": 38.2345,
          "longitude": -120.1234
        },
        "locationDescription": "Highway 4 near mile marker 45",
        "impact": "IMPACT_MODERATE",
        "timeReported": "2025-09-11T01:30:00Z",
        "lastUpdated": "2025-09-11T01:45:00Z",
        "distanceToRouteMeters": 3.5,
        "metadata": {
          "lanes_affected": "1 of 2",
          "emergency_services": "CHP on scene"
        }
      }
    ]
  },
  "lastUpdated": "2025-09-11T01:52:05.646618Z"
}
```

**Road Status Values:**
- `OPEN` - Road is open to traffic
- `CLOSED` - Road is closed
- `RESTRICTED` - Limited access or restrictions
- `MAINTENANCE` - Under maintenance

**Status Explanation:**
When a road's status is `RESTRICTED` or `CLOSED`, the `statusExplanation` field provides a clear, human-readable explanation of the reason. The AI intelligently distinguishes between mainline road impacts vs ramp/exit impacts:

**Examples of RESTRICTED status:**
- "Right lane blocked due to multi-vehicle accident near mile marker 45" (mainline lane closure)
- "Off-ramp to Treasure Island is closed" (ramp closure, mainline open)
- "On-ramp from Main Street is blocked due to construction" (ramp closure)

**Examples of CLOSED status:**
- "Full mainline closure due to emergency road repairs"
- "Complete highway shutdown between mile markers 15-20"
- "All lanes blocked by major incident"

**Congestion Levels:**
- `CLEAR` - Free flowing traffic
- `LIGHT` - Light traffic
- `MODERATE` - Moderate traffic
- `HEAVY` - Heavy traffic
- `SEVERE` - Severe congestion

### Road Alerts

The Roads API provides intelligent alerts that combine data from multiple sources and uses AI enhancement for better readability.

**Alert Types:**
- `CLOSURE` - Road closures and lane restrictions
- `CONSTRUCTION` - Planned construction activities
- `INCIDENT` - Traffic accidents and emergency situations
- `WEATHER` - Weather-related road conditions

**Alert Severity:**
- `INFO` - Informational notices
- `WARNING` - Moderate impact on travel
- `CRITICAL` - Severe impact or safety concerns

**Alert Classification:**
- `ON_ROUTE` - Directly affects route path (< 100m from route)
- `NEARBY` - In surrounding area but not blocking route
- `DISTANT` - Too far from route to be relevant

**Distance Information:**
- `distanceToRouteMeters` - Distance from alert location to route in meters
- Provided for all alert classifications to enable client-side proximity rendering
- ON_ROUTE alerts typically show distances < 100m
- NEARBY alerts show distances from 100m to several kilometers
- Useful for client applications to display "2.1 km from route" type information

**AI Enhancement Features:**
- **Smart Road Status Determination**: AI intelligently analyzes incident titles and descriptions to determine accurate road status (open/restricted/closed) with detailed explanations
- **Mainline vs Ramp Intelligence**: AI distinguishes between:
  - **Mainline impacts**: Lane closures, accidents, or construction on the main highway (status: RESTRICTED/CLOSED)
  - **Ramp/Exit impacts**: On-ramp, off-ramp, or exit closures that don't affect main traffic flow (status: RESTRICTED with specific explanation)
- **Smart Descriptions**: Technical Caltrans alerts automatically converted to human-readable summaries using OpenAI GPT-4
- **Chain Control Detection**: AI identifies R1/R2 chain requirements from incident text and weather conditions
- **Impact Assessment**: AI evaluates impact levels (`AlertImpact`): `IMPACT_NONE`, `IMPACT_LIGHT`, `IMPACT_MODERATE`, `IMPACT_SEVERE`
- **Duration Estimates**: AI provides duration estimates (`AlertDuration`): `DURATION_UNKNOWN`, `DURATION_UNDER_ONE_HOUR`, `DURATION_SEVERAL_HOURS`, `DURATION_ONGOING`
- **Content-Based Caching**: 24-hour cache prevents duplicate AI processing of identical incident content
- **Condensed Summaries**: Short format optimized for mobile displays
- **Structured Metadata**: Additional contextual information like lanes affected, emergency services on scene

**Data Sources:**
- **Caltrans KML Feeds**: Lane closures, CHP incidents, and chain control status
- **Google Routes API**: Real-time traffic conditions and route geometry for spatial matching
- **OpenAI Enhancement**: Automatic conversion of technical alerts into clear, actionable information

**Chain Control:** Roads include a `chainControlInfo` object (level R1/R2/R3,
location, direction, effective time) sourced from the Caltrans chain-control KML
feed. Outside winter the feed is typically empty and roads report no chain
requirements.

### Incidents API

Region-wide CHP/Caltrans dispatch incidents, surfaced independently of the
monitored roads (issue #7). Unlike road `alerts[]`, this is a flat list scoped by
a configured geographic area rather than per-route. Every incident is
AI-enhanced with a readable `description` plus `condensedSummary`, `impact`,
and `metadata` (cached 24h by content, capped per refresh), and `severity` is
derived from the model's impact assessment. An incident may briefly appear with
structural fields and keyword-heuristic severity until its enhancement lands on
a following refresh.

```http
GET /api/v1/incidents/{area}
```

The `area` is a path param (an area id like `mother-lode`); an unknown area
returns 404. Areas are defined in `prefab.yaml` under `roads.incidentAreas`.

**Response Example** (`GET /api/v1/incidents/mother-lode`):
```json
{
  "incidents": [
    {
      "id": "260625SA0982",
      "type": "INCIDENT",
      "severity": "WARNING",
      "location": { "latitude": 38.07, "longitude": -120.54 },
      "locationDescription": "Sr49 / Monitor Rd",
      "description": "Traffic Hazard",
      "status": "ACTIVE",
      "logNumber": "260625SA0982",
      "started": "2026-06-25T17:46:00-07:00",
      "lastUpdated": "2026-06-25T18:34:00-07:00",
      "area": "mother-lode"
    }
  ],
  "lastUpdated": "2026-06-26T01:33:38Z",
  "area": "mother-lode"
}
```

Areas are bounding boxes defined in `prefab.yaml` under `roads.incidentAreas`.
The `mother-lode` area covers the Gold Country foothills while excluding the
Central Valley floor.

### Weather API

#### List All Weather Locations
```http
GET /api/v1/weather
```

**Response Example:**
```json
{
  "weatherData": [
    {
      "locationId": "murphys",
      "locationName": "Murphys, CA",
      "weatherMain": "Clear",
      "weatherDescription": "clear sky",
      "weatherIcon": "01d",
      "temperatureCelsius": 22,
      "feelsLikeCelsius": 21,
      "humidityPercent": 45,
      "windSpeedKmh": 8,
      "windDirectionDegrees": 230,
      "visibilityKm": 16,
      "alerts": []
    }
  ],
  "lastUpdated": "2025-09-11T01:52:05.646618Z"
}
```

#### Get Weather Alerts
```http
GET /api/v1/weather/alerts
GET /api/v1/weather/alerts?zones=CAZ064,CAZ065   # filter to NWS zones
```

Returns authoritative **NWS** zone alerts (`source: "NWS"`), carrying `severity`
and `zones`. The optional `?zones=` filter narrows the alerts to the given
forecast zones (issue #4). OpenWeatherMap-sourced alerts were removed 2026-07-04
(see CHANGELOG) — for US locations they were relabeled NWS data fetched through
a rate-capped endpoint.

**Response Example:**
```json
{
  "alerts": [
    {
      "id": "urn:oid:2.49.0.1.840.0.abc",
      "source": "NWS",
      "event": "Red Flag Warning",
      "severity": "CRITICAL",
      "zones": ["CAZ064", "CAZ065"],
      "senderName": "NWS Sacramento CA",
      "headline": "Red Flag Warning in effect until 8 PM PDT",
      "summary": "Gusty winds and low humidity.",
      "startTime": "2026-06-25T19:33:00-07:00",
      "endTime": "2026-06-28T17:00:00-07:00"
    }
  ],
  "lastUpdated": "2026-06-26T01:52:05.646618Z"
}
```

#### Fire-Weather Classification (issue #5)

`GET /api/v1/weather` (and `/weather/{id}`) includes a single region-wide
`fireWeather` object at the response top level, derived from authoritative NWS
fire-weather products (these products are issued per forecast zone, not per
point, so the classification is regional):

```json
"fireWeather": {
  "state": "RED_FLAG",           // NORMAL | ELEVATED | RED_FLAG
  "sourceEvent": "Red Flag Warning",
  "headline": "Red Flag Warning in effect until 8 PM PDT",
  "senderName": "NWS Sacramento CA",
  "effective": "2026-06-26T10:00:00-07:00",
  "expires": "2026-06-26T20:00:00-07:00",
  "zones": ["CAZ064", "CAZ065"]
}
```

`state` escalates `NORMAL` → `ELEVATED` (Fire Weather Watch) → `RED_FLAG` (Red
Flag Warning). It is only ever `RED_FLAG` when NWS has an active Red Flag Warning
for the relevant zone — never a value the feed can't confirm.

### Hazards API

A unified, **map-ready** aggregation layer that re-projects every hazard source
into one standardized interface a maps client (MapLibre GL, Leaflet, OpenLayers)
can layer directly. Unlike the rest of the API, these endpoints are hand-built
GeoJSON/JSON, so field names are `snake_case`. Full design:
[`docs/hazard-aggregation-design.md`](docs/hazard-aggregation-design.md).

#### Per-Layer GeoJSON

```
GET /api/v1/hazards/{area}/{layer}.geojson
```

Returns one [RFC 7946](https://datatracker.ietf.org/doc/html/rfc7946)
`FeatureCollection` (`Content-Type: application/geo+json`) plus a `metadata`
member. Layers: `road_incident`, `chain_control`, `road_segment`,
`weather_alert`, `fire_weather`, `earthquake`, `wildfire`, `evacuation`.
Coordinates are `[longitude, latitude]`.

Every feature shares a common `properties` envelope plus a namespaced per-kind
block, on a single severity scale `INFO | MINOR | MODERATE | SEVERE | EXTREME`
(with an integer `severity_rank` 0–4 for sort/color):

```json
{
  "type": "FeatureCollection",
  "metadata": {
    "layer": "wildfire", "area": "calaveras",
    "source_status": "OK",                 // OK | STALE | UNAVAILABLE
    "last_source_update": "",              // RFC3339 time of last good fetch (STALE only)
    "attribution": "CAL FIRE / WFIGS", "schema_version": 1
  },
  "features": [{
    "type": "Feature",
    "geometry": { "type": "Polygon", "coordinates": [ /* [lng,lat] rings */ ] },
    "properties": {
      "id": "calfire:...", "layer": "wildfire", "kind": "Wildfire",
      "severity": "SEVERE", "severity_rank": 3,
      "headline": "Salt Springs Fire — 1,200 ac, 20% contained",
      "wildfire": { "acres": 1200, "containment": 20, "county": "Calaveras", "has_perimeter": true },
      "source": { "id": "calfire", "name": "CAL FIRE", "url": "..." }
    }
  }]
}
```

`metadata.source_status` is the honesty mechanism. Each layer is **fail-loud**:
on a source error it returns `UNAVAILABLE` with empty features (or `STALE` with a
`last_source_update` when serving the last good cached fetch) — never a fabricated
clear state.

#### Situation Rollup (one fetch for a dashboard)

```
GET /api/v1/situation/{area}
```

A lightweight `application/json` rollup (not a bundle of FeatureCollections — the
map still fetches the per-layer `.geojson` above): per-layer `source_status` +
`feature_count`, and a cross-layer `summary` (`highest_severity`,
`severity_counts`, `top_headlines`) plus a `scanners` sidecar.

**Life-safety contract:** `summary.active_evacuations` separates *feed health*
from *content*, with `summary.evacuation_status` (`OK` | `STALE` | `UNAVAILABLE`)
saying which:

- `UNAVAILABLE` → `active_evacuations: null` — Cal OES errored. Render "unknown —
  check the official source", **never** as zero.
- `OK` → `active_evacuations: 0` — Cal OES is healthy and reports no active zones.
  Render "no active evacuations reported" (caveated, never a guaranteed all-clear).
- `OK`/`STALE` → `active_evacuations: N>0` — active zones.

The invariant is **an error never becomes a `0`**, so a quiet day (`0`) is
distinguishable from an outage (`null`). The evacuation layer always carries the
authoritative [Genasys](https://protect.genasys.com/) viewer link in its
`source_url`, in every state.

#### Scanner Feeds

```
GET /api/v1/scanners/{area}
```

Operator-configured Broadcastify public-safety scanner feeds for the area
(`feed_id`, `channel_label`, `agency`, `broadcastify_url`) — link-out only.

Areas (bounds, scanner feeds, incident region) are configured under
`hazards.areas` in `prefab.yaml`.

## Grid Info Service (v2)

The v2 layer normalizes every hazard source into a canonical **event** model and
persists it in SQLite (`grid.dbPath`) with full revision history — every state
change, including the all-clear when an event leaves its feed, is a recorded
revision, and a restart rehydrates from the store rather than re-fetching.
Event-shaped sources (wildfire, evacuation, weather alert, earthquake, road
incident) flow through an ingest scheduler into the store; the existing
`/api/v1/hazards` event layers are re-backed by that store (same GeoJSON
envelope). A new first-principles API is served at `/v1` (JSON is snake_case,
timestamps RFC 3339, ETags everywhere), and an embedded data site at `/` provides
a source-health board, event explorer, place directory + zone resolver, map
previewer, and the live API reference. Full docs: the site's `/docs.html` (when
deployed) and [`docs/v2-api-spec.md`](docs/v2-api-spec.md); implementation notes
in [`docs/v2-implementation-plan.md`](docs/v2-implementation-plan.md).

| Endpoint | Returns |
|---|---|
| `GET /v1/places/{place}/summary` | Place rollup: `mode`, per-domain status, top events, evac fail-loud invariant, source health |
| `GET /v1/places/{place}/map/{layer}.geojson` | RFC 7946 FeatureCollection (envelope identical to `/api/v1/hazards`) |
| `GET /v1/events?place=&layer=&status=&severity_min=&since=&page_token=` | Cross-layer event query (subsumes incidents + weather alerts) |
| `GET /v1/events/{id}` / `GET /v1/events/{id}/history` | Current revision / revision timeline |
| `GET /v1/history?place=&from=&to=&layer=` | Cross-event revision archive |
| `GET /v1/places?kind=&q=` / `GET /v1/places/{place}` | Place directory |
| `GET /v1/places/resolve?lat=&lng=` \| `?address=` | Point/address → containing places |
| `GET /v1/roads?place=` / `GET /v1/weather?place=` | Conditions passthrough (weather minus alerts) |
| `GET /v1/scanners?place=` | Broadcastify feed config |
| `GET /v1/sources` | Source registry + per-source health |

Weather alerts have moved off `/v1/weather` — they are events
(`/v1/events?layer=weather_alert`). `/api/v1` is unchanged in shape and runs
beside `/v1` on the same store; it will be retired after consumers cut over (see
`docs/v2-api-spec.md` §6).

## Quick Start

### Prerequisites

- Go 1.25+
- Google Routes API key
- OpenWeatherMap API key

### Installation

1. Clone the repository:
   ```bash
   git clone <repository-url>
   cd info.ersn.net
   ```

2. Set up environment variables:
   ```bash
   export PF__GOOGLE_ROUTES__API_KEY="your-google-api-key"
   export PF__OPENWEATHER__API_KEY="your-openweather-api-key"
   export PF__OPENAI__API_KEY="your-openai-api-key"  # Optional, for AI-enhanced alerts
   ```

3. Build and run:
   ```bash
   make build
   make run
   ```

4. Test the API:
   ```bash
   # Test locally
   curl http://localhost:8181/api/v1/roads
   curl http://localhost:8181/api/v1/weather
   
   # Or test the live API
   curl https://info.ersn.net/api/v1/roads
   curl https://info.ersn.net/api/v1/weather
   ```

### Configuration

The server uses `prefab.yaml` for configuration with a **simplified structure** and supports environment variable overrides using the `PF__` prefix. Configuration has been streamlined with top-level client configs and consistent camelCase naming:

```yaml
# Client Configurations - Top Level
googleRoutes:
  # apiKey set via PF__GOOGLE_ROUTES__API_KEY

openai:
  # apiKey set via PF__OPENAI__API_KEY  
  model: "gpt-5-mini"
  timeout: "30s"
  maxRetries: 3

openweather:
  # apiKey set via PF__OPENWEATHER__API_KEY

# Service Configurations
roads:
  refreshInterval: "5m"
  staleThreshold: "10m"
  monitoredRoads:
    - name: "Hwy 4"
      section: "Angels Camp to Murphys"
      id: "hwy4-angels-murphys"
      origin:
        latitude: 38.067400
        longitude: -120.540200
      destination:
        latitude: 38.139117
        longitude: -120.456111

weather:
  refreshInterval: "5m"
  staleThreshold: "10m"
  locations:
    - id: "murphys"
      name: "Murphys, CA"
      coordinates:
        latitude: 38.139117
        longitude: -120.456111
```

## Deployment

The project includes built-in support for AWS ECR and ECS deployment:

### AWS ECR/ECS Deployment

Set the required environment variables:
```bash
export DOCKER_REGISTRY=your-account-id.dkr.ecr.region.amazonaws.com
export ECS_CLUSTER=your-cluster-name
export ECS_SERVICE=your-service-name
export ECS_TASK_DEFINITION=your-task-definition-name
```

Deploy to ECS:
```bash
# Build, push to ECR, and deploy to ECS in one command
make ecs-deploy

# Or run individual steps:
make ecr-push    # Build and push Docker image to ECR
make ecs-deploy  # Update ECS service with latest image
```

**What happens during deployment:**
1. Builds Docker image for linux/amd64 platform
2. Authenticates and pushes to ECR
3. Updates ECS task definition with new image
4. Triggers ECS service deployment
5. Waits for deployment to complete

**Prerequisites:**
- AWS CLI configured with appropriate permissions
- Docker installed locally
- `jq` installed for JSON processing (`brew install jq`)

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
# Run all tests
make test

# Run specific test suites
make test-contract      # gRPC contract tests
make test-integration   # External API integration tests
make test-unit         # Unit tests

# Test individual API clients
make test-google       # Test Google Routes API
make test-caltrans     # Test Caltrans KML parsing
make test-weather      # Test OpenWeatherMap API
```

### Code Quality

```bash
# Run linting
make lint

# Format code
make fmt

# Run both linting and tests
make lint && make test
```

### Project Structure

```
/
├── api/v1/                     # Protocol Buffer definitions
│   ├── roads.proto            # gRPC service for road conditions
│   ├── weather.proto          # gRPC service for weather data
│   └── common.proto           # Shared proto definitions
├── cmd/                       # CLI applications
│   ├── server/                # Main API server
│   ├── test-google/           # Google Routes API testing tool
│   ├── test-caltrans/         # Caltrans data testing tool
│   └── test-weather/          # Weather API testing tool
├── internal/                  # Private application code
│   ├── services/              # gRPC service implementations
│   ├── clients/               # External API clients
│   ├── cache/                 # In-memory caching with TTL
│   └── config/                # Configuration management
├── tests/                     # Test support
│   └── testdata/              # Static fixture data
├── prefab.yaml                # Server configuration
├── Dockerfile                 # Sample docker file
└── Makefile                   # Build automation
```

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

3. Restart the server to pick up configuration changes:
   ```bash
   make stop && make run-bg
   ```

### API Rate Limits

- **Google Routes API**: 3,000 queries per minute
- **OpenWeatherMap API**: 60 calls per minute (free tier). Only `/data/2.5/weather` is used — the One Call 3.0 endpoint has a separate 1,000 calls/day cap and is deliberately avoided (alerts come from NWS instead)
- **Caltrans KML Feeds**: No official limits, but feeds are refreshed every 5-30 minutes

### Architecture

The server uses a hybrid architecture:

- **gRPC Services**: Core business logic implemented as gRPC services
- **HTTP Gateway**: Automatic REST API generation from gRPC definitions
- **External Clients**: Dedicated clients for each external API
- **Caching Layer**: In-memory cache with TTL for performance
- **Configuration**: Prefab framework for flexible configuration management

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