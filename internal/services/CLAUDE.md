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
| `cameras.go`      | `CameraService`: the Caltrans CCTV directory behind `GET /api/v1/cameras`. Own background refresh, not the TTL cache — see below. |

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

Roads AND current weather conditions are kept warm by `periodic_refresh.go`;
incidents still refresh lazily on request.

`weather:all` was the last major cache refreshing lazily, and its TTL equals the
refresher's interval — so every time it expired, the next USER REQUEST paid the
full 7-location OpenWeather fan-out plus the NWS fire-weather computation. That
is a multi-second cliff landing on whoever asked first, on a 15-minute cadence.
`refreshCacheData` now calls `ListWeather` (the public method, so cache key and
TTL cannot drift; a fresh hit is a no-op). It makes the documented worst case the
actual case: 7 locations x 96 ticks/day = 672 calls/day, every day. Adding
`weather.locations` raises that linearly.

A failure in one upstream must not abandon the rest of the tick — roads and
weather are independent, so `refreshCacheData` logs and continues rather than
returning early.

Google Routes has a separate 20-minute cache (`google_routes_<id>`) to stay
within the monthly API budget — adding monitored roads increases that load.

**The camera directory is the one exception to the model above.**
`CameraService` refreshes every district in the background (`Run`, 6h; 5 min
after a failure) and keeps each district's last good list for as long as a
failure lasts, served as `STALE` with its fetch time. It never evicts at 2×TTL.
The camera list is near-static (edited weeks to months apart), so last week's
list is still right, and the images it points at are live regardless. Evicting
it would turn a portal blip into "no cameras". Out-of-service cameras and rows
with no position or https image are dropped there; the geography filter lives in
`internal/gridapi/cameras.go`, which has the place directory.

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
a configured bounding box (`roads.incidentAreas`) instead of per-route. Its only
caller is the grid's road-incident poller. Parsing of log number / type /
location / time is done structurally from the KML description. See
`internal/clients/CLAUDE.md` for the 2026 feed-format caveat.

**With CWWP2 lane closures configured (`laneClosureDistricts`), `refreshIncidents`
does not read `lcs2way.kml` at all**: the grid's `LaneClosureNormalizer` owns
closures, and reading the KML here would only spend enhancement budget on
incidents nobody stores. CHP is then the only feed, so a CHP failure fails the
refresh, and `IncidentFeedHealth`'s `laneErr` stays nil.

## Per-road status closures (`roads_lane_closures.go`)

Per-road status (`refreshRoadData`, behind the `road_segment` layer) takes its
lane closures from `segmentLaneClosures`. With `laneClosureDistricts` empty that
is `lcs2way.kml`, fetch errors swallowed, as before. With them set it is the
CWWP2 windows that are **set up right now** (`PhaseAt == PhaseActive`) — the
same rows the `road_incident` layer shows as ACTIVE, so a segment's status and
the closure events agree. SCHEDULED windows never count; `Unrecognized` rows are
logged and skipped. `caltrans.IncidentFromCWWP2LaneClosure` reshapes each window
into the `CaltransIncident` lcs2way.kml produced, so route matching, the
Closure-ID alert id and the AI `road_status` call are unchanged. Its text is
built only from fields fixed for the window's life: it is the enhancement cache
key, so a time-varying field would cost an OpenAI call per closure per refresh.

**Fail-loud, unlike the KML path.** Any district failing fails the set (a missing
district reads as "no closures", so OPEN, across its footprint). The last good
ACTIVE set (`cwwp2_lane_closures:active`) is served while servable-stale
(< 2x `roads.refreshInterval`); past that `refreshRoadData` errors, and the
periodic refresher keeps the last good `roads:all` instead of publishing a guessed
OPEN. Budget: one fetch per district per roads refresh (15m), the cadence
lcs2way.kml had; it uses the shared `cwwp2.Client` from `cmd/server`
(`UseCWWP2LaneClosures`), the one the lane-closure poller also uses.

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
- **Dedupes by `id`.** A closure is published twice in `lcs2way` — once per
  endpoint — so the pair collapses to one incident. The survivor is chosen
  deterministically (southernmost/westernmost, `southWestOf`), NOT "first
  wins": the endpoints are ~2.5 km apart and the stored location is part of the
  grid event's content hash, so feed-order-dependent selection made geometry
  flip-flop and mint a revision every time Caltrans reordered its rows.

**The id is `incidentID`, and getting it wrong is expensive.** `Closure ID` is a
route-level PROJECT id — on a live feed 593 rows carried only 271 of them, and
`C99CB` alone covered 16 unrelated Route 99 ramps. Since the id is also the dedup
key above, 15 of those 16 were dropped every poll and the survivor varied with
feed order, so a single grid event's history walked 30 km. The key is
`(Closure ID, Log Number, location text)`; the text is the discriminator that
separates closures sharing the first two, and the endpoint pair shares all three
by design. CHP is unaffected — its log number is genuinely unique. Ids are
namespaced per feed (`chp:` / `caltrans:`) so they agree with
`provenance.sourceId`.

**Enhancement is GROUNDED — the model may not invent geography.** `RawAlert`
carries a `PlaceNames` list (`nearbyPlaceNames`, `nearby_places.go`), built from
config alone — no store, no network, because this service runs upstream of the
grid store and the place directory is unreachable here. It offers configured
towns, monitored corridors and the settlements along them **within 15 km**,
closest first, with coordinate-known places ranked above corridor keywords
(a keyword has no coordinates of its own). The system prompt then forbids naming
any locality outside that list.

**The distance cap is the load-bearing part, and an empty list is a finding.**
Without a cap, grounding just trades a wrong place for a less-wrong one: a
collision at Sonora Pass was labelled "(near Merced)" — 134 km away, from a CHP
dispatch-centre token — and the nearest configured town is still 52.8 km off.
An empty list is serialized explicitly as `[]` (never omitted) and means "we
looked and nothing is near", at which point the model must name nothing and the
feed's own route-and-cross-street text survives.

`PlaceNames` is deliberately NOT in the content hash — it is a deterministic
function of coordinates already inside the hashed `location`, so including it
would dump the cache on any config edit. The PROMPT version is in the hash, so
a prompt fix invalidates cached enhancements instead of serving pre-fix text for
the 24h TTL.

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
