# Hazard Aggregation & Unified Geo Feed — Technical Design

Status: **Implemented (M0–M5)** · Owner: info.ersn.net · Last updated: 2026-06-26

## 1. Summary

The S.I.E.R.R.A "county situation" page needs live wildfire, evacuation, weather,
seismic, road-incident, and scanner data for the Calaveras / Hwy 4 & 49 region.
Today those live in 5–6 separate upstream systems with different schemas,
geometries, auth, CORS, and reliability.

The value info.ersn.net provides is **aggregation into one unified, standardized,
map-ready interface** — not 1:1 proxying. Concretely:

1. Every hazard, from every source, is normalized into **one GeoJSON Feature
   model** with a common `properties` envelope (id, kind, severity, provenance,
   timing) plus typed per-kind fields.
2. Severity from every source is mapped onto **one comparable scale**, so a
   client can sort "most urgent first" and color a map without source-specific
   logic.
3. Geometry is **RFC 7946 GeoJSON** (points, polygons, lines, WGS84) so any open
   maps product (MapLibre GL JS, Leaflet, OpenLayers, deck.gl) can drop a layer
   on a map with zero transformation.

## 2. Goals / Non-goals

**Goals**
- One normalized, documented schema across all hazard sources.
- Map-native output: a client can `addSource({type:'geojson', data: <url>})` per
  layer and style by `properties.severity` / `properties.kind`.
- Comparable, standardized severity for prioritization and color.
- Honest provenance & freshness per layer; fail-loud for life-safety data.
- Reuse existing clients/patterns (NWS, incidents, TTL cache, CORS, `{area}`).
- Backward compatible — existing typed APIs (`/roads`, `/weather`, `/incidents`)
  are untouched; this is additive.

**Non-goals**
- Vector tiles / tile server. County scale = a few dozen features; raw GeoJSON
  over HTTP is sufficient. Revisit only if payloads grow.
- Authoritative evacuation status. We surface *presence* of active orders and
  link out; we never assert "you are safe."
- Hosting/rebroadcasting scanner audio (licensing) — we export feed config only.
- Real-time push (websockets). Polling + `Cache-Control` is enough at this scale.

## 3. Core decision: GeoJSON is the canonical interface

The existing APIs are proto + grpc-gateway (typed camelCase JSON). GeoJSON's
`geometry` is a polymorphic union (`coordinates` is `[lng,lat]` for a Point but
nested arrays for a Polygon) that proto3 models awkwardly, and grpc-gateway's
marshaling fights RFC 7946's exact shape.

**Decision:** serve the hazard/geo endpoints as **hand-built RFC 7946 GeoJSON**
from a dedicated Go package, *not* through the proto services. These are public,
read-only, browser-facing endpoints with no internal gRPC consumers, so we lose
nothing by stepping outside the gateway, and we gain a clean, standards-exact
contract.

**Alternatives considered:** keeping these in proto/grpc-gateway via a
`google.protobuf.Struct` geometry field or a custom JSONPb marshaler. Both can
emit GeoJSON-ish JSON, but `Struct` is clumsy to build and a custom marshaler
re-implements RFC 7946 shaping inside the gateway for no gain over a plain Go
handler — so we serve GeoJSON directly.

**CORS/security:** handlers registered via Prefab `WithHTTPHandler` are wrapped
with the **same `securityMiddleware`** (CORS allowlist + OPTIONS handling) as the
gateway — verified in Prefab `builder.go`: the handler-registration loop applies
`securityMiddleware(handler, b.securityHeaders)` to every registered handler, not
just the gateway. So the hazard endpoints inherit the existing
`*.ersn.net` / sierragridteam.org allowlist automatically; **no manual
`SecurityHeaders.Apply` is needed.**

## 4. Data model

### 4.1 The unified Feature envelope

Every hazard is a GeoJSON `Feature`. `geometry` is standard GeoJSON (or `null`
for non-located items like a county-wide advisory). `properties` carries a common
envelope shared by **all** layers, plus a namespaced typed block per kind:

```jsonc
{
  "type": "Feature",
  "geometry": { "type": "Point", "coordinates": [-120.5402, 38.0674] },
  "properties": {
    // ---- common envelope (present on every feature) ----
    "id": "calfire:2026-salt-springs",      // globally unique, source-namespaced
    "layer": "wildfire",                     // see taxonomy §4.4
    "kind": "Wildfire",                      // human label
    "category": "active",                    // source sub-type / status slug
    "severity": "SEVERE",                    // unified scale §4.2
    "severity_rank": 3,                      // 0..4, for sort/color
    "headline": "Salt Springs Fire — 1,377 ac, 20% contained",
    "description": "Uphill runs on the north flank...",   // optional, long
    "status": "active",
    "effective": "2026-06-26T14:02:00Z",     // RFC3339, nullable
    "expires": null,
    "updated_at": "2026-06-26T15:40:00Z",
    "area_label": "Hathaway Pines & Avery",  // optional human area
    "source": {
      "id": "calfire",
      "name": "CAL FIRE",
      "url": "https://www.fire.ca.gov/incidents/...",
      "attribution": "CAL FIRE / WFIGS",
      "fetched_at": "2026-06-26T15:42:10Z"
    },
    // ---- per-kind typed block (only the relevant one) ----
    "wildfire": {
      "acres": 1377, "containment": 20, "behavior": "uphill runs",
      "personnel": "220 + 1 air tanker", "cause": "Under investigation",
      "evac_map_url": "https://protect.genasys.com/..."
    }
  }
}
```

Rules:
- The **common envelope is identical across layers** — that's the unification. A
  client renders any feature's card from `headline/severity/source/updated_at`
  without knowing its kind.
- Per-kind data lives under a single namespaced key (`wildfire`, `earthquake`,
  `evacuation`, `weather`, `incident`) so consumers can ignore kinds they don't
  handle and there are no field collisions.
- Unit-bearing fields name their unit (`wind_gust_mph`, `depth_km`,
  `distance_km`) — consistent with the existing API convention.
- **URL fields are scheme-validated.** Any URL sourced from an upstream feed
  (`source.url`, `evac_map_url`, `broadcastify_url`) must be `https://`/`http://`
  or it is dropped — upstream data is untrusted, and a `javascript:`/`data:` URL
  rendered as a link in a map popup is an XSS / open-redirect vector. Adapters
  validate before the field reaches the response.

### 4.2 Standardized severity (the core normalization)

One scale, `INFO < MINOR < MODERATE < SEVERE < EXTREME` with explicit
`severity_rank` **INFO=0, MINOR=1, MODERATE=2, SEVERE=3, EXTREME=4**. Every source
maps onto it; this drives prioritized ordering and the map color ramp.

This scale expresses **response urgency to the public**, not physical magnitude —
it is an editorial prioritization, so an Evacuation Order (EXTREME) intentionally
outranks an M5 earthquake (SEVERE). Within a rank, ties are ordered by
`updated_at` descending; consumers should treat the rank as "how loudly to alert,"
not as a comparable hazard measure. Canonical client sort: `severity_rank` desc,
then `updated_at` desc.

| Source | Source value → unified |
|---|---|
| NWS alert | Extreme→EXTREME, Severe→SEVERE, Moderate→MODERATE, Minor→MINOR, Unknown→INFO |
| Fire weather | Red Flag→SEVERE, Fire Weather Watch→MODERATE, normal→INFO |
| Evacuation | Order→EXTREME, Warning→SEVERE, Shelter-in-place→SEVERE, Advisory→MODERATE |
| Wildfire | heuristic: growing & <50% contained→SEVERE; active→MODERATE; contained→MINOR; out→INFO (thresholds configurable) |
| Earthquake | M≥5→SEVERE, 4–5→MODERATE, 2.5–4→MINOR, <2.5→INFO |
| Road incident | reuse existing AlertSeverity: CRITICAL→SEVERE, WARNING→MODERATE, INFO→MINOR, UNSPECIFIED→INFO |
| Chain control | R3→SEVERE, R2→MODERATE, R1→MINOR, NONE→INFO |

Every value of a reused enum must map (incl. `ALERT_SEVERITY_UNSPECIFIED`) so the
normalizer never produces an undefined rank.

Severity→color palette (the canonical, source-agnostic ramp; orange-escalation
lives here):

| Severity | Color | Note |
|---|---|---|
| EXTREME | `#7f1d1d` | dark red |
| SEVERE | `#c2410c` | orange-red (the "orange escalation") |
| MODERATE | `#b45309` | amber |
| MINOR | `#a16207` | muted amber |
| INFO | `#6b7280` | gray |

(Hues cluster in red-amber; pair color with the severity label/icon for
colorblind users — color alone is not the only signal.)

### 4.3 Geometry conventions

- **CRS:** WGS84, GeoJSON axis order **`[longitude, latitude]`**. (Internal
  `api.Coordinates{latitude,longitude}` must be swapped on the way out — a
  classic bug source; centralize in one helper.)
- **Types:** `Point` (incidents, quake epicenters, fire origin, town markers),
  `Polygon`/`MultiPolygon` (fire perimeters, evac zones, NWS warning zones, area
  bounds), `LineString` (monitored road segments — we already have Google
  polylines to decode; road closures).
- **Precision:** trim coordinates to 5 decimals (~1.1 m) to cut payload.
- **Simplification:** simplify polygons (Douglas–Peucker, or `maxAllowableOffset`
  on ArcGIS queries) to a target budget (e.g. ≤ ~15 KB/feature). A single raw
  fire perimeter is ~50 KB otherwise.
- **Null geometry is valid** (e.g. a county-wide advisory). Such features are
  excluded from map layers and rendered as a full-width banner/list card, sorted
  by `severity_rank` alongside located features, showing `headline`,
  `source.name`, `updated_at`, and `source.url`. (§4.1's example includes a
  located feature; an adapter emitting a null-geometry feature sets
  `"geometry": null` with the same `properties` envelope.)
- Aim for RFC 7946 right-hand-rule winding (most clients tolerate either).

### 4.4 Layer taxonomy

`layer` ∈ `wildfire | evacuation | weather_alert | fire_weather | road_incident |
road_segment | chain_control | earthquake`. (`scanner` is non-geo config, §5.4.)

### 4.5 Provenance & freshness

RFC 7946 allows foreign top-level members, so each FeatureCollection carries a
`metadata` member (ignored by map libs, read by our clients):

```jsonc
{
  "type": "FeatureCollection",
  "features": [ ... ],
  "metadata": {
    "layer": "evacuation",
    "area": "ebbetts-pass",
    "generated_at": "2026-06-26T15:42:11Z",
    "source_status": "OK",        // OK | STALE | UNAVAILABLE
    "last_source_update": "2026-06-26T15:38:00Z",
    "attribution": "Cal OES / California County Governments — reference only",
    "source_url": "https://protect.genasys.com/...",
    "schema_version": 1
  }
}
```

`source_status` is the honesty mechanism and powers the page's "how this feed
works" strip and the fail-loud evac rule (§6.4). The three states render
differently and must not be collapsed:

| `source_status` | Features | Client render |
|---|---|---|
| `OK` | current | normal |
| `STALE` | last-good served | render with a "data ~N min old" indicator (N from `generated_at − last_source_update`) |
| `UNAVAILABLE` | none | suppress the map layer; show an empty-state banner linking `source_url` |

For the evac layer (§6.4): `UNAVAILABLE`/`null` means the feed errored — render
"unknown, check official source," never imply all-clear. `OK` with **zero**
features is distinct: Cal OES is healthy and reports no active zones — render "no
active evacuations reported," still linking `source_url` and never as a guarantee.
The safety property is that an error never collapses into a `0`.

## 5. API surface

### 5.1 Per-layer FeatureCollections (map-native)

```
GET /api/v1/hazards/{area}/{layer}.geojson
    e.g. /api/v1/hazards/ebbetts-pass/wildfire.geojson
```
Returns one RFC 7946 FeatureCollection (+ `metadata`). This is what a map source
points at. Each layer is independently cached and independently statused.

### 5.2 Aggregated situation document (one fetch for the page)

```
GET /api/v1/situation/{area}
```
As implemented, `/situation` is a lightweight rollup (status + summary), **not**
a bundle of FeatureCollections — the map still fetches the per-layer `.geojson`
endpoints in §5.1. This keeps the situation payload small and lets a dashboard
render its banner from one fetch.

```jsonc
{
  "area": "ebbetts-pass",
  "area_name": "Calaveras County",
  "generated_at": "2026-06-26T15:42:11Z",
  "summary": {
    "highest_severity": "SEVERE",
    "highest_severity_rank": 3,
    "severity_counts": { "INFO": 5, "MODERATE": 13 },
    "total_features": 19,
    "active_evacuations": 2,                 // integer count, OR null when unknown
    "evacuation_status": "OK",               // OK | UNAVAILABLE — disambiguates null
    "top_headlines": [ { "layer": "wildfire", "severity": "SEVERE",
                         "severity_rank": 3, "headline": "...", "source": "CAL FIRE" } ]
  },
  "layers": [                                 // array, one entry per §4.4 layer
    { "layer": "wildfire", "source_status": "OK", "feature_count": 1,
      "highest_severity": "SEVERE", "attribution": "...", "source_url": "..." },
    { "layer": "evacuation", "source_status": "UNAVAILABLE", "feature_count": 0,
      "attribution": "Cal OES ... reference only", "source_url": "https://protect.genasys.com/" }
    /* ... road_incident, chain_control, weather_alert, fire_weather, earthquake, road_segment */
  ],
  "scanners": [ /* §5.4 */ ]
}
```
**Life-safety contract:** `summary.active_evacuations` separates *feed health*
from *content*, and `summary.evacuation_status` (`OK` | `STALE` | `UNAVAILABLE`)
disambiguates:

- `UNAVAILABLE` → `active_evacuations: null`. Cal OES errored/unreachable. A
  client MUST render `null` as an explicit warn state ("evacuation status
  unavailable — check [Genasys]"), never as "no active evacuations."
- `OK` → `active_evacuations: 0`. Cal OES is healthy and reports no active zones.
  Render "no active evacuations reported by Cal OES" — still linking Genasys and
  framed as reference-only, **never** a guaranteed all-clear.
- `OK`/`STALE` → `active_evacuations: N>0`. Active zones (STALE = served from the
  last good fetch; see `last_source_update`).

The invariant is **an error never collapses into a `0`** — a confirmed-empty
(`OK`/`0`) and a failure (`UNAVAILABLE`/`null`) are deliberately distinguishable,
because a perpetual "unavailable" on quiet days is itself misleading. (Do not
treat `active_evacuations` as a string — an earlier draft showed
`"yes|no|unknown"`; the shipped field is `int|null`.)

Each `layers[]` entry is a status summary (`source_status`, `feature_count`,
`highest_severity`), not a FeatureCollection — pull the geometry from the
matching `/hazards/{area}/{layer}.geojson` endpoint. One failing source degrades
its own entry (`source_status: UNAVAILABLE`), never the rollup.

**Why aggregate server-side** (vs. a client fetching the per-layer feeds in §5.1
and merging itself): `summary` requires cross-layer logic — `highest_severity`,
the `active_evacuations` rollup with its `unknown` fail-loud state, and a
combined headline — that every client would otherwise re-implement (and the evac
fail-loud rule must not be left to each consumer to get right). The aggregator is
a thin compose over the same independently-cached per-layer collections. The
`all.geojson` variant (§10) remains deferred until a consumer needs it.

### 5.3 Versioning

`/api/v1` for the route; `metadata.schema_version` for the GeoJSON properties
contract. Evolution is additive (new properties, new layers); breaking changes
bump `schema_version` and are noted in `CHANGELOG.md`.

### 5.4 Non-geo sidecar: scanner config

```
GET /api/v1/scanners/{area}   →  [ { feed_id, channel_label, agency,
                                     broadcastify_url } ]
```
Static, operator-authored config (no upstream fetch). The response carries only
the `feed_id` and a `broadcastify_url` — **never a raw HTML `embed` snippet** (a
server-emitted HTML fragment rendered via `innerHTML` would be a stored-XSS path).
The client constructs the official Broadcastify embed iframe from `feed_id`
itself; listener counts come from that widget, not from us (the Catalog API is
$2,500/mo). Calaveras feed IDs are known (13524 Sheriff/CAL FIRE, 28469 Fire/USFS,
41042 CAL FIRE TCU, 45443 CHP Stockton).

## 6. Architecture

### 6.1 Source adapters → normalizer → aggregator

```
internal/hazards/
  geojson/        RFC7946 Feature/FeatureCollection/Geometry + helpers
                  (point, polygon, simplify, trimPrecision, latLngToLonLat)
  model.go        unified properties envelope + severity scale + mapping
  adapters/
    calfire/      CAL FIRE incidents  (List API; no CORS → server-only)
    wfigs/        NIFC perimeters     (ArcGIS, f=geojson, bbox)
    usgs/         earthquakes         (FDSN query, geojson, bbox)
    caloes/       evacuation zones    (ArcGIS aggregation layer; active-only)
    nws/          weather + fire wx   (reuse existing internal/clients/nws)
    incidents/    road incidents      (reuse existing RoadsService.ListIncidents)
    roads/        road segments       (reuse monitored-road polylines)
    chaincontrol/ chain control       (reuse caltrans ParseChainControlsDetailed)
  aggregator.go   fan-out adapters concurrently; per-layer cache + status;
                  assemble FeatureCollections and the situation document
  service.go      HTTP handlers for the endpoints in §5
```

Each adapter implements:
```go
type SourceStatus int // OK | STALE | UNAVAILABLE  (maps to metadata.source_status)

type LayerStatus struct {
    Status           SourceStatus
    LastSourceUpdate time.Time   // upstream's own freshness stamp, when available
}

type Adapter interface {
    Layer() string
    Fetch(ctx context.Context, area config.HazardArea) ([]geojson.Feature, LayerStatus, error)
}
```
Adapters own their upstream quirks (auth, bbox query, GeoJSON vs JSON parse) and
return *already-normalized* Features + a `LayerStatus`. The aggregator copies
`LayerStatus.Status` into each collection's `metadata.source_status` (§4.5).

Each adapter MUST bound the upstream response with
`io.LimitReader(resp.Body, maxBytes)` before decoding (suggested ~5 MB for ArcGIS
polygon GeoJSON, ~1 MB for incident/quake JSON) — the per-feature size budget in
§4.3 is applied *after* parse, so an unbounded body is an OOM vector regardless.

### 6.2 Relationship to existing APIs

`/roads`, `/weather`, `/incidents` stay exactly as they are (ersn.net depends on
them). The hazard layer **re-projects** the existing feeds (incidents → Points,
weather alerts → zone Polygons, monitored roads → LineStrings, fire weather →
area Polygon) into the unified model by calling the existing services in-process.
So the map gets one consistent source, and a given incident may appear both in
`/incidents` (typed) and `/hazards/.../road_incident.geojson` (geo) — intentional;
different consumers.

### 6.3 Serving GeoJSON + CORS

Mount via Prefab `WithHTTPHandler("/api/v1/hazards/", h)` and
`WithHTTPHandler("/api/v1/situation/", h)`. Because these sit *under* `/api/` but
bypass the gateway's security wrapper, apply CORS explicitly: construct a
`prefab.SecurityHeaders` from the same `server.security` config and call
`.Apply(w, r)` at the top of each handler (or a small shared middleware). This
reuses the exact origin allowlist (`*.ersn.net`, sierragridteam.org) and OPTIONS
handling we already configured — no second CORS policy. Set the same
`Cache-Control`/`Last-Modified` we added for the typed endpoints.

### 6.4 Caching, refresh, error isolation, fail-loud

- Per-layer cache keys `hazards:{area}:{layer}`, independent TTLs (wildfire 5m,
  evac 2–3m, quake 5m, weather reuse existing, scanners ∞/config). Reuse the
  existing TTL cache + periodic refresh.
- Adapters run **concurrently**, each under an aggregator-imposed deadline
  (`context.WithTimeout(ctx, ~15s)`, inside the existing 30s transport ceiling),
  so a hung upstream fails fast instead of leaking a goroutine/FD per refresh and
  can't block the others.
- On adapter error or stale upstream: that layer's `metadata.source_status` =
  `UNAVAILABLE`/`STALE`, serve last-good features if any, and **never fabricate**
  a clear state.
- **Evac fail-loud (hard rule):** the load-bearing invariant is **an error never
  becomes a `0`**. A *failed* fetch (transport error, non-2xx, or an ArcGIS
  HTTP-200 error-envelope) is `UNAVAILABLE` (→ `active_evacuations: null`,
  "unknown"); a *clean* fetch that returns no active zones is `OK`/`0` ("no active
  evacuations reported"). Both are caveated and always carry the Genasys
  `source_url` — a confirmed-empty is "no active zones per Cal OES," never a
  guaranteed all-clear. We do NOT collapse confirmed-empty into `UNAVAILABLE`: a
  perpetual "unavailable" on quiet days is itself misleading and trains users to
  ignore the signal. To preserve the invariant, empty results are never cached
  (so a later fetch error falls through to `UNAVAILABLE`, not a replayed stale
  `0`), and the adapter only ever emits *active* Order/Warning/Advisory features.

### 6.5 Config / area model

Extend the area pattern we already use (`roads.incidentAreas` bbox,
`weather.nws.zones`) with a hazards area:
```yaml
hazards:
  areas:
    - id: ebbetts-pass
      name: "Calaveras County"
      bounds: { minLatitude: 37.8, maxLatitude: 38.55, minLongitude: -120.9, maxLongitude: -120.0 }
      center: { latitude: 38.20, longitude: -120.55 }   # map default view
      defaultZoom: 9
      nwsZones: [CAZ064, CAZ065]
      evacCounties: ["Calaveras", "Tuolumne", "Amador"]  # foothill neighbours
      scannerFeeds:
        - { feed_id: "13524", channel_label: "Sheriff / CAL FIRE Dispatch", agency: "Calaveras SO / CAL FIRE" }
        - { feed_id: "28469", channel_label: "Fire / USFS", agency: "CAL FIRE / USFS" }
```

## 7. Source mapping (quick reference)

| Layer | Upstream | Geometry | Auth/CORS | Cadence | Key caveat |
|---|---|---|---|---|---|
| wildfire (incidents) | CAL FIRE `IncidentApi/List` | Point | none / **no CORS** | ~5 min | server proxy required; no cause/personnel |
| wildfire (perimeter) | NIFC WFIGS ArcGIS | Polygon | none / CORS ok | ~5 min | simplify; empty is normal |
| evacuation | Cal OES aggregation FeatureServer | Polygon | none / CORS ok | 5–10 min | **active-only → fail-loud**; reference-only |
| weather_alert | NWS (built) | zone Polygon | none / CORS ok | event | flood events already flow through `GetActiveZoneAlerts`; **new work** = extract gust/min-RH from alert `parameters` into typed props (assign M3) |
| fire_weather | NWS (built) | area Polygon | none | event | already shipped |
| road_incident | our `/incidents` | Point | internal | 5 min | already shipped, re-projected |
| road_segment | our monitored roads | LineString | internal | 15 min | decode Google polyline |
| chain_control | Caltrans cc.kml (existing caltrans client) | Point | internal | 10 min | reuse `ParseChainControlsDetailed`; R1/R2/R3 severity |
| earthquake | USGS FDSN query | Point | none / CORS ok | ~5 min | filter to bbox; lowest priority |
| scanner | operator config | — | — | static | client embed; no audio export |

## 8. Map-client integration (open maps)

Target **MapLibre GL JS** (and Leaflet) — both consume GeoJSON natively.

```js
map.addSource('wildfire', { type: 'geojson',
  data: 'https://info.ersn.net/api/v1/hazards/ebbetts-pass/wildfire.geojson' });

// polygons (perimeters / zones) colored by unified severity
map.addLayer({ id: 'wildfire-fill', type: 'fill', source: 'wildfire',
  filter: ['==', ['geometry-type'], 'Polygon'],
  paint: { 'fill-color': ['match', ['get','severity'],
            'EXTREME','#7f1d1d','SEVERE','#c2410c','MODERATE','#b45309',
            'MINOR','#a16207','INFO','#6b7280','#6b7280'],
           'fill-opacity': 0.35 }});

// points (origins / incidents / quakes) — same severity->color match as the fill
map.addLayer({ id: 'wildfire-pt', type: 'circle', source: 'wildfire',
  filter: ['==', ['geometry-type'], 'Point'],
  paint: { 'circle-radius': 6,
    'circle-color': ['match', ['get','severity'],
      'EXTREME','#7f1d1d','SEVERE','#c2410c','MODERATE','#b45309',
      'MINOR','#a16207','INFO','#6b7280','#6b7280'] }});
```

Clients read provenance/freshness from `metadata` (foreign member), sort cards by
`severity_rank` desc then `updated_at`, and honor `source_status` to show staleness.
Standard GeoJSON means Leaflet (`L.geoJSON`), OpenLayers, and deck.gl work the
same way with no server changes.

## 9. Build plan

| Milestone | Scope | Proves | Status |
|---|---|---|---|
| **M0** | `geojson` package, unified model, severity mapping, this doc | the contract | ✅ shipped |
| **M1** | Re-project **existing** feeds (incidents, weather alert, fire wx, road segments, chain control — all reuse current clients) into `/hazards/{area}/{layer}.geojson` | the model with zero new upstreams → a real map layer immediately | ✅ shipped |
| **M2** | `usgs` earthquakes + `scanners` config endpoint (link-out only — `broadcastify_url`, no non-owner embed) | cheap breadth | ✅ shipped |
| **M3** | `calfire` + `wfigs` wildfire (incidents + simplified perimeters, joined by name) | the marquee layer | ✅ shipped |
| **M4** | `caloes` evacuations with fail-loud + link-out | the high-liability layer, done carefully | ✅ shipped |
| **M5** | `/situation/{area}` aggregator, per-layer status, docs + CHANGELOG | the unified page fetch | ✅ shipped |

M1 is deliberately first: it ships a working, map-ready unified feed using only
data we already have, validating the schema before we take on new upstreams.

## 10. Risks & open questions

- **Evac false-negative** — mitigated by fail-loud + link-out; never assert clear.
- **Perimeter payload size** — simplify + precision-trim; budget per feature.
- **Coordinate-order bug** ([lon,lat] vs our [lat,lng]) — single conversion helper.
- **Wildfire severity heuristic** is subjective — make thresholds config, document.
- **Broadcastify embed key** — confirm a non-owner site may embed; else link out.
- **CAL FIRE API is undocumented/unsupported** — parse defensively, cache, degrade.
- **Open:** do we also emit a combined `all.geojson` (every layer, one collection,
  discriminated by `properties.layer`) for the simplest possible client? Cheap to
  add once the per-layer collections exist.
