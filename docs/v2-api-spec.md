# Grid Info Service — API & Schema Spec (Draft v0.2)

Rework of info.ersn.net: add persistence, history, and fine-grained places underneath
the existing API. Wire format: protobuf (canonical model) with GeoJSON retained as a
projection. Persistence: SQLite.

**v0.1 → v0.2:** v0.1 was written blind to the current surface. Having read the live
OpenAPI specs and `docs/hazard-aggregation-design.md` (implemented M0–M5): the unified
envelope, 5-level severity with per-source mappings, provenance with fail-loud
semantics, and the `/situation` rollup **already exist**. This revision reframes the
work as what's actually missing — persistence, event lifecycle, places, cross-layer
query, digests — and keeps the shipped contracts intact. The strangler mostly inverts:
external contracts survive, internals change.

---

## 1. What exists / what's missing

**Exists and survives:**
- Typed proto/grpc-gateway APIs: `RoadsService` (roads, incidents-by-area, metrics),
  `WeatherService` (conditions, alerts, fire-weather state).
- Hand-built GeoJSON hazard layers: `/api/v1/hazards/{area}/{layer}.geojson` with the
  unified `properties` envelope (source-namespaced id, layer, severity + rank,
  headline, provenance, timing, namespaced per-kind block).
- `/api/v1/situation/{area}`: cross-layer rollup with the evac fail-loud invariant
  (error never collapses into 0).
- Unified severity `INFO < MINOR < MODERATE < SEVERE < EXTREME` with per-source
  mapping table and color ramp.
- `source_status` (OK | STALE | UNAVAILABLE) per layer.
- Area concept (`ebbetts-pass`, `mother-lode`), TTL caching, CORS allowlist,
  AI enhancement of road/weather alerts.

**Missing — the actual scope of this rework:**
1. **Persistence.** Everything is TTL-cached in memory. No history, no revisions,
   no replay, no archive. Restart = amnesia.
2. **Event lifecycle.** Records are per-poll snapshots; nothing tracks an incident
   across time or detects resolution.
3. **Places.** Only coarse configured areas. No towns, no evac-zone resolve, no
   point/address queries — the primitive "My Area" and zone lookup need.
4. **Cross-layer query.** Layers are fetched one GeoJSON file at a time; there's no
   "events in place X, severity ≥ Y, since T" with pagination.
5. **Subscriptions/digests.** Nothing.

---

## 2. Resolving the proto ↔ GeoJSON tension

The design doc deliberately moved hazard endpoints *off* proto/grpc-gateway because
proto3 models RFC 7946 geometry awkwardly. That decision was right **for the wire**,
and this spec doesn't reverse it. The reconciliation:

> **Proto is the canonical model — the poller output and the SQLite blob.
> GeoJSON is a projection rendered from the store. The typed `/v1` query API speaks
> protojson; the `.geojson` endpoints keep their hand-built RFC 7946 contract,
> byte-compatible, now backed by the event store instead of the TTL cache.**

Geometry inside the canonical proto is a GeoJSON byte field + promoted bbox/centroid
(§3) — proto never models coordinate polymorphism, so the awkwardness the design doc
avoided stays avoided. One projection helper owns lat/lng axis-order swapping,
5-decimal trimming, and polygon simplification, exactly as the doc centralizes today.

**Considered and rejected: geobuf** (go-geobuf as a binary geometry encoding). The
size win is marginal once coordinates are trimmed and responses are gzipped; the
ecosystem is stagnant (momentum went to vector tiles / flatgeobuf); and it breaks
direct GeoJSON-URL consumption by map clients. If payloads ever genuinely grow,
the escape hatch is vector tiles, per the original design doc.

**Conditions vs. events.** The existing API got a factoring right that v0.1 missed:
roads (travel time, congestion, chain control) and current weather are *conditions* —
continuously-valued state of a monitored resource — not events. Conditions keep their
typed services largely untouched. The event store covers discrete occurrences with
lifecycle: wildfire, evacuation, weather_alert, earthquake, road_incident, plus new
types. GeoJSON layers draw from both (event-backed: wildfire, evacuation,
weather_alert, earthquake, road_incident; condition-backed: road_segment,
chain_control, fire_weather).

---

## 3. Canonical proto model

Package `grid.v1` (naming open, §8). Field-level alignment with the shipped GeoJSON
envelope is deliberate — the projection is a rename-free mapping.

```proto
syntax = "proto3";
package grid.v1;

import "google/protobuf/timestamp.proto";

message Event {
  string id = 1;                    // "{source_id}:{native_id}" — matches shipped ids
                                    // e.g. "calfire:2026-salt-springs"
  Layer layer = 2;                  // shipped taxonomy + additions
  string category = 3;              // source sub-type slug ("active", "order", ...)
  Severity severity = 4;            // shipped 5-level scale
  EventStatus status = 5;

  string headline = 6;              // card-renderable without knowing the kind
  string summary = 7;               // AI-enhanced 2–3 sentences where applicable
  string description = 8;           // long form / original text
  string area_label = 9;            // "Hathaway Pines & Avery"
  string canonical_url = 10;        // scheme-validated (https/http only, per shipped rule)

  Geometry geometry = 11;           // may be absent: county-wide advisories
  repeated string place_ids = 12;   // precomputed intersections (§4)

  Provenance provenance = 13;

  google.protobuf.Timestamp effective = 14;
  google.protobuf.Timestamp expires = 15;
  google.protobuf.Timestamp observed_at = 16;   // upstream last-update
  google.protobuf.Timestamp ingested_at = 17;
  uint32 revision = 18;

  oneof detail {                    // mirrors the namespaced per-kind blocks
    WildfireDetail wildfire = 20;
    EvacuationDetail evacuation = 21;
    WeatherAlertDetail weather_alert = 22;
    FireWeatherDetail fire_weather = 23;
    EarthquakeDetail earthquake = 24;
    RoadIncidentDetail road_incident = 25;
    PowerDetail power = 26;         // new: PSPS + outages
    GaugeDetail gauge = 27;         // new: river/reservoir threshold events
    AirQualityDetail air_quality = 28;  // new
    NetworkDetail network = 29;     // new: mesh/repeater state changes
    AnnouncementDetail announcement = 30;  // new: human-authored
  }
}

enum Layer {
  LAYER_UNSPECIFIED = 0;
  WILDFIRE = 1; EVACUATION = 2; WEATHER_ALERT = 3; FIRE_WEATHER = 4;
  EARTHQUAKE = 5; ROAD_INCIDENT = 6;
  // condition-backed layers (ROAD_SEGMENT, CHAIN_CONTROL) are projections, not events
  POWER = 10; GAUGE = 11; AIR_QUALITY = 12; NETWORK = 13; ANNOUNCEMENT = 14;
}

// The shipped scale, unchanged. rank = enum value; INFO=0 ... EXTREME=4.
enum Severity {
  INFO = 0; MINOR = 1; MODERATE = 2; SEVERE = 3; EXTREME = 4;
}

enum EventStatus {
  EVENT_STATUS_UNSPECIFIED = 0;
  SCHEDULED = 1;   // planned PSPS, forecast closures
  ACTIVE = 2;
  RESOLVED = 3;    // upstream confirmed over
  EXPIRED = 4;     // aged out per-source policy; upstream went quiet
}

message Geometry {
  bytes geojson = 1;                // RFC 7946 geometry object, UTF-8
  BoundingBox bbox = 2;             // always populated at ingest
  LatLng centroid = 3;
}
message BoundingBox { double min_lat = 1; double min_lng = 2; double max_lat = 3; double max_lng = 4; }
message LatLng { double lat = 1; double lng = 2; }

message Provenance {
  string source_id = 1;
  string source_name = 2;           // denormalized: "CAL FIRE"
  string attribution = 3;           // "CAL FIRE / WFIGS"
  string source_url = 4;
  google.protobuf.Timestamp fetched_at = 5;
}

message Source {
  string id = 1;
  string name = 2;
  string attribution = 3;
  string homepage_url = 4;
  uint32 poll_interval_seconds = 5;
  uint32 stale_after_seconds = 10;   // per-source tunable; default 3x poll interval
  uint32 expire_after_seconds = 11;  // per-source tunable; missing-from-feed → EXPIRED
  google.protobuf.Timestamp last_success_at = 6;
  google.protobuf.Timestamp last_attempt_at = 7;
  string last_error = 8;
  SourceStatus status = 9;          // shipped semantics, now per-source not per-layer
}
enum SourceStatus { SOURCE_STATUS_UNSPECIFIED = 0; OK = 1; STALE = 2; UNAVAILABLE = 3; }

message Place {
  string id = 1;        // "area:ebbetts-pass" — existing area ids preserved under a kind
  PlaceKind kind = 2;
  string name = 3;
  string slug = 4;
  Geometry geometry = 5;
  string parent_id = 6;
}
enum PlaceKind {
  PLACE_KIND_UNSPECIFIED = 0;
  AREA = 1;             // existing configured areas; org footprints live here too
  COUNTY = 2; TOWN = 3; EVAC_ZONE = 4; CORRIDOR = 5; SITE = 6;
}
```

Detail messages: port field-for-field from the shipped namespaced blocks (wildfire:
acres/containment/behavior/personnel/cause/evac_map_url; earthquake: magnitude/depth_km;
etc.). Unit-in-name convention (`depth_km`, `wind_gust_mph`) carries over. New details
(Power, Gauge, AirQuality, Network, Announcement) defined with their pollers.

### 3.1 AI enhancement

Currently applied to road/weather alerts; expanded to any source whose raw feed has
opaque or source-specific grammar. Assessed per source against the actual feeds:

| Source | Raw grammar | Verdict |
|---|---|---|
| CHP incidents | Radio codes, abbreviations ("TC", "1141") | **Enhance** (already does; the archetype). |
| NWS alert descriptions | Hard-wrapped ALL-CAPS `* WHAT/WHERE/WHEN/IMPACTS` blocks; fire-weather-zone numbers | **Enhance**: condense + *localize* — translate zone references into named places from our directory. |
| WFIGS / CAL FIRE narratives | IC jargon: "uphill runs", "spotting", "RH recovery", 209-style remarks | **Enhance**: plain-language behavior summary. |
| CAL FIRE structured fields | Acres, containment, location string | No — template. |
| Caltrans chain control | R1/R2/R3 codes | No — static lookup table. |
| USGS earthquakes | Already human-readable ("10km NE of Arnold, CA") | No — template. |
| USGS gauges | Numeric vs. flood stage | No — arithmetic + template. |
| Genasys evacuations | Zone IDs + order level | **Prohibited** (see below). Zone ID → place names is geometric, not linguistic. |
| PG&E PSPS | Cause codes + update bulletins | Probe at poller time; likely bulletins only. |
| NASA FIRMS | Numeric detections (confidence, FRP) | Defer — narrative would be aggregation-level inference about fire behavior, i.e. asserting, not translating. |

**Policy — enhancement translates, never asserts:**

1. Output may contain no place, number, or instruction not present in the input or
   the place directory. The place directory is the one permitted external grounding —
   it's ours, and it's what makes localization safe.
2. Original text is always preserved verbatim in `description`; enhanced text lives
   in `headline`/`summary`. Clients can render the original.
3. **Deterministic-first.** If a code table or template can do the translation
   (R1/R2/R3, magnitudes, gauge stages), no LLM is involved.
4. **Directive life-safety text is never rewritten.** Evacuation orders and
   instructions render verbatim with a link; enhancement may add context *around*
   them, never a paraphrase *of* them. This is the fail-loud invariant's sibling:
   we don't paraphrase orders.
5. Triggered only on `content_hash` change; result stored in the revision (history
   preserves exactly what was shown); enhancement failure serves raw and never
   blocks ingest. Cost scales with change rate, not poll rate.
6. Enhanced events carry provenance so clients can badge AI-summarized text:

```proto
message Enhancement {
  string model = 1;                          // "gpt-4o-mini", "claude-haiku-4-5", ...
  google.protobuf.Timestamp enhanced_at = 2;
  repeated string fields = 3;                // which Event fields were generated
}
// Event: Enhancement enhancement = 19;
```

---

## 4. SQLite schema

Proto blob canonical, columns as indexes. WAL; single writer (poller scheduler).

```sql
CREATE TABLE events (
  id           TEXT PRIMARY KEY,
  layer        INTEGER NOT NULL,
  severity     INTEGER NOT NULL,
  status       INTEGER NOT NULL,
  source_id    TEXT NOT NULL REFERENCES sources(id),
  effective    INTEGER,             -- unix seconds throughout
  expires      INTEGER,
  observed_at  INTEGER NOT NULL,
  ingested_at  INTEGER NOT NULL,
  revision     INTEGER NOT NULL DEFAULT 1,
  content_hash TEXT NOT NULL,       -- normalized proto hash; unchanged => no revision
  proto        BLOB NOT NULL
);
CREATE INDEX idx_events_active ON events(status, severity DESC, observed_at DESC);
CREATE INDEX idx_events_layer  ON events(layer, status);

CREATE TABLE event_revisions (       -- full snapshots; storage trivial at this volume
  event_id TEXT NOT NULL, revision INTEGER NOT NULL,
  observed_at INTEGER NOT NULL, ingested_at INTEGER NOT NULL,
  proto BLOB NOT NULL,
  PRIMARY KEY (event_id, revision)
);

CREATE TABLE places (
  id TEXT PRIMARY KEY, kind INTEGER NOT NULL,
  name TEXT NOT NULL, slug TEXT NOT NULL UNIQUE,
  parent_id TEXT, proto BLOB NOT NULL
);

CREATE TABLE event_places (          -- computed at ingest; hot path never touches geometry
  event_id TEXT NOT NULL, place_id TEXT NOT NULL,
  PRIMARY KEY (event_id, place_id)
);
CREATE INDEX idx_event_places_place ON event_places(place_id, event_id);

CREATE VIRTUAL TABLE event_geo USING rtree(rowid, min_lat, max_lat, min_lng, max_lng);
CREATE TABLE event_geo_map (rowid INTEGER PRIMARY KEY, event_id TEXT NOT NULL UNIQUE);

CREATE TABLE sources (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, attribution TEXT NOT NULL DEFAULT '',
  poll_interval_seconds INTEGER NOT NULL,
  stale_after_seconds INTEGER,       -- NULL => 3x poll interval
  expire_after_seconds INTEGER,      -- NULL => never auto-expire
  last_success_at INTEGER, last_attempt_at INTEGER,
  last_error TEXT NOT NULL DEFAULT ''
);

CREATE TABLE subscriptions (         -- phase 2, anticipated
  id TEXT PRIMARY KEY,
  channel TEXT NOT NULL,             -- 'email' | 'ntfy' | 'rss'
  address TEXT NOT NULL,
  place_id TEXT NOT NULL,
  min_severity INTEGER NOT NULL DEFAULT 2,
  created_at INTEGER NOT NULL, confirmed_at INTEGER
);
```

**Ingest tick** (one transaction per source): fetch → normalize to Event → enhance if
`content_hash` changed → upsert `events` + insert `event_revisions` → recompute
`event_places` + rtree → update `sources`. Upstream disappearance → RESOLVED or
EXPIRED per-source policy, emitted as a revision (the all-clear is part of history).

The TTL cache doesn't disappear — read paths serve from memory as today; SQLite is
the write-behind system of record. Restart rehydrates instead of forgetting, and
`STALE` gains meaning across restarts (last-good survives).

---

## 5. API surface

First-principles redesign; backward compatibility is not a constraint (§6 describes
the migration). Everything under `/v1/`.

**Design rules:**

1. **Two idioms, one rule.** *Entities* are global collections filtered by query
   param (`/v1/events?place=...`) — they exist independent of any place and get
   canonical ids. *Place-derived documents* nest under the place path
   (`/v1/places/{place}/summary`) — they only exist relative to a place. This
   replaces the current mix of `{area}` path segments and ad-hoc scoping.
2. **Hot paths are query-string-free.** The two endpoints a browser hits every 30s —
   summary and map layers — are pure paths, so CDN cache keys are clean and
   `Cache-Control` does the cost-control work.
3. **Media types:** protojson default; `application/proto` via `Accept`; the
   `.geojson` extension marks the RFC 7946 projection (map clients can't set
   headers on source URLs).
4. **Conventions:** timestamps RFC 3339; filters `place`, `layer` (repeatable),
   `status`, `severity_min`, `since`, `from`/`to`; cursor `page_token`. Errors are
   `google.rpc.Status` protojson. ETags everywhere.
5. Places addressable by slug (`ebbetts-pass`) or id (`county:calaveras-county`); slugs
   globally unique.

| Endpoint | Returns | Notes |
|---|---|---|
| `GET /v1/places/{place}/summary` | `Summary` | The workhorse: mode (QUIET/WATCH/ACTIVE), per-domain statuses, top events, source health. One fetch renders the Now page. Carries the evac fail-loud invariant (`active_evacuations: int\|null` + status). |
| `GET /v1/places/{place}/map/{layer}.geojson` | FeatureCollection | RFC 7946 projection. Envelope carried over from the current hazards endpoints (it's good) — only the URL moves. |
| `GET /v1/events?place=&layer=&status=&severity_min=&since=&page_token=` | `EventList` | The cross-layer query. Default `status=ACTIVE,SCHEDULED`. **Subsumes** `/incidents/{area}` (`layer=road_incident`) and weather-alert listing (`layer=weather_alert`). |
| `GET /v1/events/{id}` | `Event` | Current revision. |
| `GET /v1/events/{id}/history` | `EventRevisionList` | Per-incident timeline. |
| `GET /v1/history?place=&from=&to=&layer=` | `EventRevisionList` | Cross-event archive/replay; after-action review. |
| `GET /v1/places?kind=&q=` / `/v1/places/{place}` | `PlaceList` / `Place` | Directory; feeds the location picker. |
| `GET /v1/places/resolve?lat=&lng=` \| `?address=` | `PlaceList` | Point/address → containing places. Zone lookup core. |
| `GET /v1/roads?place=` / `/v1/roads/{id}` | conditions | Travel time, congestion, chain control — state, not events. |
| `GET /v1/weather?place=` / `/v1/weather/{location}` | conditions | Observations + forecast. Alerts are gone from here — they're events. |
| `GET /v1/scanners?place=` | `ScannerList` | Feed config; entity collection, no longer riding on the summary. |
| `GET /v1/sources` | `SourceList` | Provenance/health registry behind every `source_status`. |

**Dropped from the public surface:** `GetProcessingMetrics` (AI pipeline metrics are
an ops concern → internal/admin); alerts endpoints on WeatherService and the
incidents endpoint on RoadsService (dissolved into `/events`); scanners riding in
the situation payload (fetch once, cache long).

The consolidation is the point: WeatherService and RoadsService shrink to pure
conditions, and every discrete occurrence — of any kind, present or future —
is reachable through exactly one query surface.

No streaming: the design doc's polling + `Cache-Control` non-goal stands.
`/summary` and `/events` cacheable 30–60s.

Deliberately still absent: per-source endpoints. A new poller (PSPS, FIRMS, gauges)
adds a `Layer` value and a detail message; it appears in summary domains, `/events`,
and the map namespace automatically, with zero new API surface.

---

## 6. Migration

Not compatibility — a describable cutover. All known consumers are ours (SIERRA
site, ERSN site, mesh map), so both surfaces run on the same binary over the same
store, frontends cut over per-page, and the old surface is deleted when access
logs go quiet. Target weeks, not months.

**Old → new mapping:**

| Current | Becomes | Shape change |
|---|---|---|
| `/api/v1/situation/{area}` | `/v1/places/{area}/summary` | `layers[]` → `domains[]`; adds `mode`; scanners removed. Evac fail-loud semantics identical. |
| `/api/v1/hazards/{area}/{layer}.geojson` | `/v1/places/{area}/map/{layer}.geojson` | None — envelope unchanged, URL move only. Map cutover is a source-URL swap. |
| `/api/v1/incidents/{area}` | `/v1/events?place={area}&layer=road_incident` | Typed `Incident` → Event envelope + `road_incident` detail. |
| WeatherService alerts | `/v1/events?layer=weather_alert` | Same envelope move. |
| `/api/v1/roads*` | `/v1/roads*` | Conditions shape kept; `?place=` filter added. |
| `/api/v1/weather*` | `/v1/weather*` | Same, minus alerts. |
| `/api/v1/scanners/{area}` | `/v1/scanners?place={area}` | Path → filter. |
| `GetProcessingMetrics` | *(removed)* | Internal/admin ops surface. |

**Sequence:**

1. **Land the store dark.** Event model, SQLite, ingest pipeline; pollers for
   event-shaped sources write through. Existing read paths untouched.
2. **Ship `/v1` beside `/api/v1`.** Both served from the store. Map layers cut
   over first (URL swap, zero shape risk), then Now page onto `/summary`, then
   incident/alert views onto `/events`.
3. **Places.** Areas import as places day one (slugs preserved, so `{area}` path
   segments carry over unchanged); counties, towns, zone polygons, corridors
   follow. `/places/resolve` unblocks "What's my zone?" on the SIERRA side.
4. **Delete `/api/v1`** after N quiet weeks in access logs.
5. **New pollers** (PSPS first, then FIRMS, gauges, AQI) land on the new surface
   only.

Risk to watch in step 2: GeoJSON layers are currently assembled from live typed
models; the store round-trip must not lose AI-enhanced fields or provenance
freshness. The revision history actually fixes today's behavior, where an
enhancement can be regenerated per poll cycle instead of per change.

---

## 7. Boundaries

- **Mesh telemetry stays parallel** (meshcore-land). State changes worth attention
  emit `NETWORK` events; the firehose does not enter this store.
- **Evacuation data remains reference-only.** The shipped fail-loud invariant is a
  load-bearing safety property; the store must preserve it — an UNAVAILABLE source
  renders unknown, never all-clear, and `OK`+0 is a distinct confirmed-empty.
- **No accounts.** Personalization is client-side place ids; subscriptions are
  address + confirmation, phase 2.
- **Writes internal.** Pollers in-process; `ANNOUNCEMENT` authoring admin-token'd.
- **The data site is an unprivileged client.** The nerd frontend at
  data.sierragridteam.org consumes only the public API — no backdoor queries, no
  internal endpoints, no privileged reads. If a page needs data the public API
  can't serve, that's a defect in the API, not a reason for a private path. The
  pressure is the point: the API stays complete because its most demanding
  consumer lives on it. (Full spec: `data-site-spec.md`.)

---

## 8. Decisions (formerly open questions)

1. **Naming — resolved.** The service has outgrown ERSN (which is a net; SIERRA is
   the technical org). It moves under the SIERRA project as
   **`data.sierragridteam.org`**, with `info.ersn.net` CNAME'd through the
   transition; `grid.v1` package stands (deliberately org-neutral). The ERSN
   website remains a supported consumer. `data.` also hosts a light,
   operator/nerd-facing frontend over the API — source health, raw event
   inspector, archive browser — separate from the resident-facing main site,
   plus future static artifacts (coverage tiles, geometry downloads, exports).
2. **Conditions history — yes, later.** Target use case: an MCP server over `/v1`
   enabling natural-language queries against history — "why was traffic bad
   yesterday," "which fire was closest to Avery this year." The query surface is
   already MCP-shaped (`/events`, `/history`, `/places/resolve` map 1:1 to tools);
   what's missing is a `condition_snapshots` time-series table for the
   traffic/weather questions. Phase 2+; the event store needs no changes to
   support it.
3. **Staleness — tunable per source.** `stale_after_seconds` and
   `expire_after_seconds` config per source (see §3/§4), defaulting to multiples
   of `poll_interval_seconds`. Wrong defaults look like stale alarm or premature
   all-clear, so expect per-source tuning.
4. **Geocoder — Census, with pin-drop fallback** for unincorporated foothill
   parcels it can't resolve.
5. **Zone polygons — resolved: use the Cal OES California Evacuation Aggregation
   Layer.** Cal OES publishes a statewide ArcGIS feature service on the state open
   data portal (data.ca.gov) aggregating county evacuation zones in the
   Zonehaven/Genasys schema, refreshed every 10 minutes, including zone status.
   This is the sanctioned public path — no scraping of protect.genasys.com needed.
   Its "as-is, reference only" framing matches the shipped evac invariant exactly.
   Two things to verify at poller time: (a) Calaveras and Tuolumne appear in the
   participating-counties inventory; (b) the layer serves NORMAL-status zone
   geometry, not just active zones — point→zone lookup on quiet days needs the
   full inventory. If it's active-only, fall back to the per-county public layers
   for the base zone geometry and use the aggregation layer for status.
6. **Retention — no compaction.** History is the point. The `content_hash` gate
   (no revision without change) is the mechanism that prevents unnecessary
   duplication; nothing else needed at this volume.
