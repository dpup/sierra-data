# gRPC Services

These implement the proto services in `api/v1`. They orchestrate clients,
caching, route classification, and AI enhancement.

| File              | Responsibility |
|-------------------|----------------|
| `roads.go`        | `RoadsService`: per-road traffic, alerts, status, chain control. |
| `incidents.go`    | `RoadsService.ListIncidents`: region-wide CHP/Caltrans incident feed. |
| `weather.go`      | `WeatherService`: current conditions + combined alerts list. |
| `weather_nws.go`  | NWS zone alerts + fire-weather classification for `WeatherService`. |
| `periodic_refresh.go` | Background goroutine that warms the roads cache. |

## Caching model (read this before adding an endpoint)

Every read endpoint follows the same shape:

1. `Get(key, &dst)` — serve **fresh** data only (`Get` returns found=false for
   stale entries).
2. On miss/stale, refresh from upstream, then `Set(key, data, ttl, kind)`.
3. On refresh failure, fall back to stale cache via `GetWithMetadata(key, &dst)`
   — the accessor that returns stale entries — gated by `!IsVeryStale(key)`
   (2× the refresh interval). **Don't use `Get` in a stale-fallback branch; it
   can never return stale data** (this exact bug made the weather fallbacks
   dead code until 2026-07).

Staleness tiers: fresh (< TTL) → servable-stale (< 2×TTL) → very stale
(evicted by the hourly cleanup goroutine started in `cmd/server/main.go`).

The cache is in-memory JSON (TTL-based), so any value must be JSON-serializable
(this is why `nws.Alert` uses exported fields). TTLs: API data ~5–15m,
AI-enhanced alerts 24h (keyed by content hash to dedupe OpenAI calls).

Roads are kept warm by `periodic_refresh.go`; weather/incidents refresh lazily on
request. Google Routes has a separate 20-minute cache (`google_routes_<id>`) to
stay within the monthly API budget — adding monitored roads increases that load.

## Adding a new endpoint

1. Add the RPC + messages to the relevant `.proto`, then `make proto`
   (see root CLAUDE.md for the toolchain — Go/protoc are not pre-installed).
2. Implement the method on the existing service struct (the gateway wiring in
   `cmd/server/main.go` is already registered per-service, so new RPCs on an
   existing service need no extra registration).
3. Request fields map automatically: fields named in the path template are path
   params (`/incidents/{area}` → `ListIncidentsRequest.area`), the rest become
   query params (`?zones=` → repeated `zones`). Convention: path params identify
   a resource (road/location/area id); query params filter a collection.
4. Add focused unit tests next to the file (construct inputs directly; don't hit
   the network).

## Region-wide incidents (`incidents.go`)

Surfaces the same Caltrans/CHP data as road alerts, but as a flat list scoped by
a configured bounding box (`roads.incidentAreas`) instead of per-route. Parsing
of log number / type / location / time is done structurally from the KML
description. See `internal/clients/CLAUDE.md` for the 2026 feed-format caveat.

**Every incident is AI-enhanced** (`enhanceIncident`), via the same
content-hash 24h cache as road alerts (`enhanceRawAlert` in `roads.go` —
shared, so an incident that is also a road alert costs one OpenAI call).
`severity` is driven by the model's impact assessment (`severityFromImpact`,
mirroring the roads mapping); the keyword heuristic (`incidentSeverity`) is
only the placeholder until enhancement lands. Cache-miss calls are capped at
`maxIncidentEnhancementsPerRefresh` per refresh to bound latency/cost;
deferred incidents pick up enhancement on a later refresh. Enhancement is
strictly additive: failures keep the structural fields and heuristic severity.
The model is `openai.model` in prefab.yaml (gpt-5-mini); the enhancer handles
gpt-5-family param differences (no temperature, `max_completion_tokens`,
`reasoning_effort=low`) — see `internal/lib/alerts/enhancer.go`.

Each incident normalizes to the same primitives the other APIs use (shared
`AlertType`/`AlertSeverity` enums, `Coordinates`, `google.protobuf.Timestamp`,
`location_description`). `normalizeIncidents` then keeps the list clean:

- **Drops geometry-only placemarks.** The lane-closure feed emits a separate
  LineString "path" placemark per closure with no description — skipped by the
  empty-description check.
- **Dedupes by `id`.** Closures are repeated across directions in `lcs2way`;
  only the first is kept.

CHP incidents carry a `started` time; lane closures are scheduled operations with
no dispatch time, so their `started` is null (expected, not a bug).

## Weather alerts & fire weather

`ListWeatherAlerts` returns authoritative **NWS** zone alerts only (source
`NWS`). `?zones=CAZ064,...` filters to alerts in those zones. Per-location
`weather_data[].alerts` are the NWS alerts for that location's configured
`zone` (see `nwsAlertsForZone` in `weather_nws.go`). OpenWeatherMap One Call
alerts were removed 2026-07-04: for US locations they duplicate NWS, and the
One Call 3.0 endpoint's 1,000 calls/day cap was being exceeded — don't
reintroduce per-location One Call fetches. OpenWeather now serves current
conditions only (`/data/2.5/weather`, one call per location per refresh).

`fire_weather` is **region-wide** (NWS fire-weather products are issued by zone,
not point), so it lives on the response (`ListWeatherResponse` /
`GetLocationWeatherResponse`) computed once from the configured `weather.nws.zones`
— not duplicated on every location.
