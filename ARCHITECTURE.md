# ERSN API Server - Current Architecture Flow

> **Note (2026-07-05):** The diagram below describes the original `/api/v1`
> roads/weather/hazards request path. It is still accurate for those services,
> but two claims have since drifted — see the inline **[STALE]** markers — and a
> whole new subsystem (the **v2 grid event store**) now sits beside it. Read
> [v2: Grid event store](#v2-grid-event-store) for the current end-to-end flow.

## Information Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────────────┐
│                              CLIENT REQUESTS                                            │
└─┬─────────────────────────────────────────────────────────────────────────────────────┬─┘
  │                                                                                     │
  │ HTTP GET /api/v1/roads          HTTP GET /api/v1/weather         HTTP GET /         │
  | ▼                               ▼                                ▼                  │
┌─────────────────────────────────────────────────────────────────────────────────────────┐
│                            PREFAB SERVER FRAMEWORK                                      │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐                          │
│  │   gRPC Gateway  │  │   gRPC Gateway  │  │  Homepage       │                          │
│  │   (HTTP→gRPC)   │  │   (HTTP→gRPC)   │  │  Handler        │                          │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘                          │
│           │                       │                       │                             │
│           ▼                       ▼                       ▼                             │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐                          │
│  │  RoadsService   │  │ WeatherService  │  │  Static HTML    │                          │
│  │                 │  │                 │  │  Response       │                          │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘                          │
└─┬─────────────────┬───────────────────────────────────────────────────────────────────┬─┘
  │                 │                                                                   │
  ▼                 ▼                                                                   │
┌─────────────────┐ ┌─────────────────────────────────────────────────────────────┐     │
│                 │ │              PERIODIC REFRESH SERVICE                       │     │
│  CACHE CHECK    │ │  ┌─────────────┐     Every 5 minutes:                       │     │
│                 │ │  │   Timer     │────▶ RoadsService.ListRoads()              │     │
│  ┌─────────────┐│ │  │  Goroutine  │     (Simulated API Request)                │     │
│  │ Fresh Data? ││ │  └─────────────┘                                            │     │
│  │             ││ │                                                             │     │
│  │ YES → Return││ │                                                             │     │
│  │ NO  → Fetch ││ └─────────────────────────────────────────────────────────────┘     │
│  └─────────────┘│                                                                     │
└─────────────────┘                                                                     │
          │                                                                             │
          ▼                                                                             │
┌─────────────────────────────────────────────────────────────────────────────────────────┐
│                          DATA REFRESH PIPELINE                                          │
│                                                                                         │
│  ┌─────────────────┐                         ┌──────────────────┐                       │
│  │ refreshRoadData │─────────────────────────│refreshWeatherData│                       │
│  │                 │                         │                  │                       │
│  │ For each road:  │                         │ For each loc:    │                       │
│  │ ▼               │                         │ ▼                │                       │
│  │ ┌─────────────┐ │                         │ ┌─────────────┐  │                       │
│  │ │processMonito│ │                         │ │getWeatherFor│  │                       │
│  │ │redRoad()    │ │                         │ │Location()   │  │                       │
│  │ └─────────────┘ │                         │ └─────────────┘  │                       │
│  └─────────────────┘                         └──────────────────┘                       │
│           │                                                                             │
│           ▼                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────────────────┐ │
│  │                    EXTERNAL API CALLS                                           │ │
│  │                                                                                 │ │
│  │ ┌─────────────┐  ┌─────────────────┐  ┌─────────────────┐                     │ │
│  │ │   Google    │  │    Caltrans     │  │  OpenWeatherMap │                     │ │
│  │ │   Routes    │  │  KML Feeds      │  │      API        │                     │ │
│  │ │             │  │                 │  │                 │                     │ │
│  │ │ Traffic +   │  │ Lane Closures + │  │ Weather Data +  │                     │ │
│  │ │ Polylines   │  │ CHP Incidents   │  │ Alerts          │                     │ │
│  │ └─────────────┘  └─────────────────┘  └─────────────────┘                     │ │
│  │       │                   │                     │                             │ │
│  └───────┼───────────────────┼─────────────────────┼─────────────────────────────┘ │
│          │                   │                     │                               │
│          ▼                   ▼                     ▼                               │
│  ┌─────────────────────────────────────────────────────────────────────────────────┐ │
│  │                    DATA PROCESSING PIPELINE                                     │ │
│  │                                                                                 │ │
│  │ Google Response    │    Caltrans KML      │    Weather Response                 │ │
│  │       │            │          │           │           │                        │ │
│  │       ▼            │          ▼           │           ▼                        │ │
│  │ ┌─────────────┐    │ ┌─────────────────┐  │  ┌─────────────────┐              │ │
│  │ │ Extract     │    │ │ Parse KML +     │  │  │ Format Weather  │              │ │
│  │ │ Polyline    │    │ │ Extract Coords  │  │  │ + Alerts        │              │ │
│  │ │ + Traffic   │    │ │                 │  │  │                 │              │ │
│  │ └─────────────┘    │ └─────────────────┘  │  └─────────────────┘              │ │
│  │       │            │          │           │                                    │ │
│  │       └────────────┼──────────▼           │                                    │ │
│  │                    │ ┌─────────────────┐  │                                    │ │
│  │                    │ │ Route-Aware     │  │                                    │ │
│  │                    │ │ Classification: │  │                                    │ │
│  │                    │ │ • OnRoute       │  │                                    │ │
│  │                    │ │ • Nearby        │  │                                    │ │
│  │                    │ │ • Distant       │  │                                    │ │
│  │                    │ └─────────────────┘  │                                    │ │
│  │                    │          │           │                                    │ │
│  │                    │          ▼           │                                    │ │
│  │                    │ ┌─────────────────┐  │                                    │ │
│  │                    │ │ Alert           │  │                                    │ │
│  │                    │ │ Enhancement &   │  │                                    │ │
│  │                    │ │ Status Analysis │  │                                    │ │
│  │                    │ │ ContentHasher   │  │                                    │ │
│  │                    │ │       │         │  │                                    │ │
│  │                    │ │       ▼         │  │                                    │ │
│  │                    │ │ ┌─────────────┐ │  │                                    │ │
│  │                    │ │ │ Check Cache │ │  │                                    │ │
│  │                    │ │ │ 24h TTL     │ │  │                                    │ │
│  │                    │ │ └─────────────┘ │  │                                    │ │
│  │                    │ │       │         │  │                                    │ │
│  │                    │ │       ▼         │  │                                    │ │
│  │                    │ │ ┌─────────────┐ │  │                                    │ │
│  │                    │ │ │ OpenAI API  │ │  │                                    │ │
│  │                    │ │ │• Status Det │ │  │                                    │ │
│  │                    │ │ │• Enhancement│ │  │                                    │ │
│  │                    │ │ └─────────────┘ │  │                                    │ │
│  │                    │ └─────────────────┘  │                                    │ │
│  └─────────────────────────────────────────────────────────────────────────────────┘ │
│                                     │                                               │
│                                     ▼                                               │
│  ┌─────────────────────────────────────────────────────────────────────────────────┐ │
│  │                              CACHE STORAGE                                     │ │
│  │                                                                                 │ │
│  │  ┌─────────────────┐    ┌─────────────────────┐    ┌─────────────────┐        │ │
│  │  │  roads:all      │    │ enhanced_alert:     │    │ Weather data    │        │ │
│  │  │  (5m TTL)       │    │ {content_hash}      │    │ (5m TTL)        │        │ │
│  │  │                 │    │ (24h TTL)           │    │                 │        │ │
│  │  │ • Road status   │    │                     │    │ • Current cond  │        │ │
│  │  │ • Status explan │    │ • AI enhanced       │    │ • Alerts        │        │ │
│  │  │ • Traffic data  │    │ • Status analysis   │    │                 │        │ │
│  │  │ • Enhanced      │    │ • Structured desc   │    │                 │        │ │
│  │  │   alerts        │    │ • Impact/duration   │    │                 │        │ │
│  │  └─────────────────┘    └─────────────────────┘    └─────────────────┘        │ │
│  └─────────────────────────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────────────────────┘
```

## Key Components and Data Flows

### 1. Request Processing Flow
- **Entry**: HTTP requests → Prefab gRPC Gateway → gRPC Service methods
- **Cache Strategy**: Check cache first, refresh if stale, return data
- **Background Warmth**: Periodic refresh simulates requests to maintain cache

### 2. External API Integration ✅ **SIMPLIFIED CONFIG**
- **Google Routes**: Traffic conditions + polylines (API key via `PF__GOOGLE_ROUTES__API_KEY`)
- **Caltrans KML**: Real-time incidents parsed from XML feeds  
- **OpenWeather**: Current conditions and weather alerts (API key via `PF__OPENWEATHER__API_KEY`)
- **OpenAI**: AI enhancement for alerts (API key via `PF__OPENAI__API_KEY`)

### 3. Data Processing Layers
- **Route-Aware Classification**: Uses actual Google polylines to classify alerts as OnRoute/Nearby/Distant
- **AI Enhancement**: Content-based caching prevents duplicate OpenAI calls
- **Geographic Processing**: Polyline decoding and coordinate-based filtering

### 4. Caching Architecture
- **Single Cache Instance**: JSON-based with TTL support
- **Multi-Layer TTL**: 5m for the aggregate roads/weather response cache, 24h for
  enhanced alerts. **[STALE]** The Google Routes API calls themselves are gated by
  a separate **45m per-road** cache (see `roads.go` / `prefab.yaml`) to stay under
  the free SKU budget — the 5m figure is the response cache, not the upstream call
  gate.
- **Content-Based Deduplication**: Prevents redundant AI processing
- **[STALE]** The `/api/v1/hazards` **event layers** (wildfire, evacuation,
  weather_alert, earthquake, road_incident) are no longer assembled from live
  typed models per request — they are projected from the **v2 grid event store**
  (below), which is now their last-good cache. The three condition layers
  (road_segment, chain_control, fire_weather) remain live projections.

### 5. Configuration System ✅ **NEWLY SIMPLIFIED**
- **Unified Structure**: Single config.Config struct with top-level client configurations
- **Environment Mapping**: Prefab transforms `PF__SECTION__FIELD_NAME` → `section.fieldName`
- **Service Integration**: All services receive full config instead of sub-configs
- **Consistent Naming**: CamelCase throughout for predictable env var mapping

## Current Complexity Areas (Simplification Opportunities)

### 🔄 Route-Aware Processing Pipeline
**Current Flow:**
```
Raw KML → Parse → Extract Coords → Route Classification → AI Enhancement → Cache
```
**Complexity:** Multiple libraries (geo utils, route matcher, content hasher) handling overlapping concerns

### 🧠 AI Enhancement & Status Determination Chain
**Current Flow:**
```
Content Hash → Cache Check → OpenAI API → Status Analysis → Enhancement → Store Enhanced
```
**Features:**
- **Smart Status Determination**: AI analyzes incidents to determine road status (open/restricted/closed)
- **Status Explanations**: Clear explanations provided when roads are restricted or closed
- **Alert Enhancement**: Technical alerts converted to human-readable descriptions
- **Classification Logic**: Distinguishes mainline closures vs ramp closures for accurate status
**Complexity:** Separate caching logic from main cache, complex fallback handling

### 🌐 External API Client Patterns
**Similar Patterns:**
- Rate limiting and timeout handling
- JSON/XML parsing and error handling  
- Response caching and staleness detection

### 📊 Configuration Management ✅ **SIMPLIFIED**
**Unified Configuration Structure:**
- **Single Config Source**: Prefab YAML with environment variable mapping
- **Top-Level Client Configs**: GoogleRoutes, OpenAI, OpenWeather moved to root level  
- **CamelCase Consistency**: All fields use camelCase for Prefab env var transformation
- **Explicit Fields**: Replaced embedded RefreshConfig with explicit fields (koanf compatibility)
- **Environment Variables**: `PF__CLIENT__FIELD` → `client.field` mapping works correctly

**Configuration Reduction:**
- **Before**: 125 lines, nested structures, inconsistent naming
- **After**: ~98 lines, flat structure, consistent camelCase naming
- **Eliminated**: Obsolete StoreConfig, unused chain_controls section

## v2: Grid event store

Added 2026-07-05 (`docs/v2-implementation-plan.md`). A write path independent of
the request path above: a scheduler polls each upstream on its own cadence,
normalizes results into canonical `grid.v1.Event` protos, and persists them —
with full revision history — into SQLite. Both the new `/v1` API and the
re-backed `/api/v1` hazard event layers read from that store. The store is the
system of record; a restart rehydrates events and revisions with no re-fetch.

```
        WRITE PATH (ingest — one goroutine per poller, jittered, panic-recovered)
┌──────────────────────────────────────────────────────────────────────────────┐
│  internal/ingest.Scheduler                                                     │
│    per tick, per poller:                                                       │
│                                                                                │
│  ┌────────────┐   Poll(ctx, prior)   ┌──────────────────┐                      │
│  │ Normalizer │ ───────────────────▶ │ upstream clients │  usgs / calfire /    │
│  │ (1 scope)  │ ◀─── PollResult ──── │ (internal/clients│  wfigs / caloes /    │
│  └────────────┘   {Events, PerSource,│  + Roads/Weather │  nws / chp / caltrans│
│        │           SweepSuppress}    │  services)       │                      │
│        ▼                             └──────────────────┘                      │
│  ┌────────────────────────┐   NeedsUpdate? (content-hash gate)                 │
│  │ content-hash gate       │ ── unchanged ─▶ no revision; refresh place links  │
│  │ (store.ContentHash:     │                 + TouchSeen (liveness only)       │
│  │  zeroes revision,       │ ── changed ───▶ UpsertEvent: new revision,        │
│  │  ingested/observed_at,  │                 event_revisions snapshot,         │
│  │  fetched_at, enhancement│                 event_places recompute, R*Tree    │
│  │  , summary, place_ids)  │                                                   │
│  └────────────────────────┘                                                    │
│        │                                                                       │
│        ▼   disappearance sweep (ONLY for sources that fetched cleanly AND were │
│  ┌────────────────────────┐   not SweepSuppress'd — a failed/incomputable      │
│  │ lifecycle transitions   │   fetch NEVER resolves events; the all-clear is a │
│  │ resolve → RESOLVED      │   recorded revision, not a silent delete)         │
│  │ expire  → EXPIRED       │                                                   │
│  └────────────────────────┘                                                    │
│        │                                                                       │
│        ▼  RecordAttempt(source, err) → source health OK→STALE→UNAVAILABLE      │
└────────┼───────────────────────────────────────────────────────────────────────┘
         ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│  internal/store  (SQLite, WAL, single-writer)                                  │
│  events · event_revisions · places · event_places · event_geo(rtree) · sources│
│  proto blob is canonical; scalar columns are indexes only                      │
└────────┬─────────────────────────────────────────────────────────────┬─────────┘
         │  READ PATHS (concurrent under WAL)                           │
         ▼                                                              ▼
┌───────────────────────────────────┐        ┌──────────────────────────────────┐
│ internal/gridapi  →  /v1/*         │        │ internal/hazards  →  /api/v1/*    │
│  events, history, places, resolve, │        │  event layers projected from the │
│  summary (mode/domains/evac), map, │        │  store via gridapi.ProjectEvents │
│  sources — protojson (snake_case)  │        │  (byte-compatible envelope);     │
│  + store→GeoJSON ProjectEvents     │        │  condition layers stay live      │
└───────────────────────────────────┘        └──────────────────────────────────┘
         ▲                                                              ▲
         └──────────────── same store state, same fail-loud source_status ┘
```

**Key invariants** (details in `internal/store/CLAUDE.md`,
`internal/ingest/CLAUDE.md`):
- **Fail-loud lifecycle.** An error — or a clean fetch that can't prove a source's
  full current set — never resolves/expires events. Mechanisms: `PerSource`
  errors skip that source's sweep; `SweepSuppress` skips a source that fetched
  cleanly but couldn't compute disappearance; an empty configured scope is a hard
  Poll error, never a success-empty (which would fabricate an all-clear).
- **Content-hash gate.** Revisions are written only on real content change;
  `enhancement`/`summary` are excluded from the hash so AI output never churns
  history and is not regenerated per poll (the spec §6 fix).
- **Store as last-good cache.** A down source with stored active events serves
  `STALE` (the data), not `UNAVAILABLE`-empty; `UNAVAILABLE` is reserved for a
  failed source with nothing to serve — preserving the evacuation "an error never
  becomes a 0" invariant.

