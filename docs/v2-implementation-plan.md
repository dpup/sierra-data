# Grid Info Service v2 — Implementation Plan

Implements `docs/v2-api-spec.md` (API + persistence) and `docs/data-sites-spec.md`
(data.sierragridteam.org). This document is both the human-readable plan and the
**coordination contract for subagent-driven development**: every task lists its
file ownership, its public contracts (signatures), and its verification. Agents
must not edit files owned by another task.

Status legend: each task is sized for one focused subagent. Dependencies are
explicit; tasks at the same tier run in parallel on disjoint files.

---

## 0. Context (what the explorers established)

- Module `github.com/dpup/sierra-data`, Go 1.24+, Prefab v0.2.2
  (gRPC + grpc-gateway + hand-built `http.Handler`s via `prefab.WithHTTPHandler`).
- Existing surface: `RoadsService`/`WeatherService` (grpc-gateway, `/api/v1/...`),
  hand-built GeoJSON hazards endpoints (`internal/hazards`, 8 layers, fail-loud
  `source_status`, `/situation/{area}`, `/scanners/{area}`).
- Docker builds with `CGO_ENABLED=0` cross-compile ⇒ **pure-Go SQLite only**.
  Verified: `modernc.org/sqlite v1.53.0` supports R*Tree + WAL (tested in sandbox).
- Clients (`internal/clients/*`) are thin fetch+parse with `HTTPDoer` injection.
  Native ID quality: strong (`usgs.Quake.ID`, `caloes.EvacZone.ZoneID`,
  `calfire.Incident.UniqueID`, `nws.Alert.ID`), weak (caltrans — identity derived
  from CHP log numbers in `services/incidents.go:incidentID()`; wfigs — name only).
- AI enhancement: `internal/lib/alerts` (OpenAI structured outputs, content-hash
  24h cache, budget 5 calls/refresh for incidents).
- Tests: colocated `*_test.go`, testify, fixture files under `tests/testdata/`,
  fake `HTTPDoer`s (no httptest). `make test` = `go test ./...`.
- No CI; deploys from `main` via make targets. Generated protos are committed.

## 1. Architecture decisions (locked)

1. **SQLite driver: `modernc.org/sqlite`** (pure Go). WAL mode, single writer
   (the ingest scheduler), `busy_timeout=5000`. DB path from config
   `grid.dbPath` (default `./data/grid.db`; prod points at the EFS mount via
   `PF__GRID__DB_PATH`). `data/` is git-ignored.
2. **`grid.v1` protos are messages-only** (`api/grid/v1/grid.proto`, go package
   `.../api/grid/v1;gridv1`). No gRPC service: every `/v1` endpoint is a
   hand-built `net/http` handler (the pattern the hazards endpoints established,
   and the only way to serve `.geojson` paths, explicit `null` for the evac
   invariant, and clean ETags). Entity endpoints marshal `grid.v1` messages with
   `protojson` (canonical model on the wire, per spec §2); `application/proto`
   served on `Accept`. Summary is a hand-shaped JSON document (place-derived
   doc) so `active_evacuations: null` is an explicit null, exactly like
   `/situation` today.
3. **Event identity keeps the shipped id namespaces**: `chp:`, `wx:`, `usgs:`,
   `calfire:`, `wfigs:`, `evac:`. `provenance.source_id` identifies the source
   registry row; the id prefix is the stable namespace. One deliberate deviation:
   standalone WFIGS perimeter ids change from `wfigs:{name}:{sliceIndex}`
   (unstable across polls) to `wfigs:{normname}` (+`-2`, `-3` disambiguator by
   centroid order) — an id-stability fix, called out in CHANGELOG.
4. **Sources registry** (drives `/v1/sources` and per-source health):
   `nws`, `usgs`, `calfire`, `wfigs`, `caloes`, `chp` (CHP incidents), `caltrans`
   (lane closures; also attributed for chain-control conditions). A poller may
   update multiple source rows (wildfire tick → `calfire` + `wfigs`; incidents
   tick → `chp` + `caltrans`); poller ≠ source.
5. **Conditions vs events** (spec §2): event-backed layers = wildfire,
   evacuation, weather_alert, earthquake, road_incident. Condition-backed =
   road_segment, chain_control, fire_weather — these stay live projections of
   the roads/weather services, on both `/api/v1` and `/v1` map endpoints.
6. **Ingest reuses the existing enhanced pipelines** rather than duplicating
   parsing: road_incident events consume `RoadsService.ListIncidents` (already
   AI-enhanced, budgeted, cached); weather_alert events consume the cached NWS
   fetch via `WeatherService.ListWeatherAlerts`. Wildfire/evac/earthquake ingest
   uses the clients directly (as `internal/hazards` builders do today).
7. **Enhancement in the store** (spec §3.1): `Event.enhancement` records
   model/enhanced_at/fields. Road incidents carry it from the existing pipeline.
   A new NWS weather-alert enhancer (condense + localize using the place
   directory; translate-never-assert prompt) is implemented behind an interface,
   budget-capped, disabled without an OpenAI key, and mocked in all tests.
   CAL FIRE narrative enhancement is deferred (client doesn't fetch narratives).
8. **Lifecycle**: per-source `disappearance` policy. `resolve` (authoritative
   active-only feeds: caloes, calfire, chp, caltrans) → missing-from-feed ⇒
   RESOLVED. `expire` (nws, wfigs) → missing AND past `expires` (or
   `expire_after_seconds` grace) ⇒ EXPIRED. Every transition writes a revision
   (the all-clear is part of history). SCHEDULED when `effective` is in the
   future (NWS watches).
9. **`/api/v1` compatibility**: untouched during foundation. After `/v1` map
   projection passes the byte-compat harness, the five event-backed `/api/v1`
   hazards layers are re-backed by the store through the same projection code
   path (strangler step 2), gated on that harness. Conditions layers unchanged.
10. **Site is embedded** (`go:embed site/`) and served at `/` (replaces the old
    homepage handler; Docker healthcheck on `GET /` keeps working). MapLibre GL
    vendored into `site/lib/` — the site stays self-contained, no CDN.
11. **Places seed**: configured hazard areas import as `area:{id}` (slug
    preserved ⇒ `{area}` URL segments carry over); counties (Calaveras,
    Tuolumne, Amador, Alpine, Stanislaus, San Joaquin, El Dorado, Mariposa) from
    Census TIGERweb simplified polygons checked in as `data/places/counties.geojson`
    (fetched once at dev time, not at runtime); towns = the 7 configured weather
    locations (point places); corridors = the 4 monitored roads (linestring).
    Evac-zone inventory import is **deferred**: the Cal OES aggregation view is
    active-events-only (spec §8.5's fallback path is a follow-up).
12. **Geocoder**: Census onelineaddress geocoder (keyless) behind
    `internal/clients/census` with `HTTPDoer` injection; `/v1/places/resolve`
    accepts `lat/lng` (pure PIP, no network) or `address` (geocode → PIP).

## 2. Wire contracts

### 2.1 grid.proto (api/grid/v1/grid.proto)

As spec §3, with these concretizations:
- `Event.detail` oneof implements: `WildfireDetail{acres double, containment int32,
  county string, cause string, has_perimeter bool}`, `EvacuationDetail{zone_id,
  level, event_type, county string}` (level ORDER|WARNING|ADVISORY|SHELTER_IN_PLACE
  as string, matching shipped), `WeatherAlertDetail{event, nws_severity, certainty,
  urgency, instruction, sender_name, area_desc string, zones repeated string}`,
  `EarthquakeDetail{magnitude double, depth_km double, felt int32, url string}`,
  `RoadIncidentDetail{log_number, incident_type, location_description string,
  impact, duration string, condensed_summary string, metadata map<string,string>}`.
  Field numbers 20–25 per spec; 26–30 reserved for Power/Gauge/AirQuality/
  Network/Announcement (defined with their pollers).
- `Enhancement` message + `Event.enhancement = 19`.
- List/response messages: `EventList{repeated Event events = 1; string
  next_page_token = 2;}`, `EventRevision{uint32 revision; google.protobuf.Timestamp
  observed_at, ingested_at; Event event;}`, `EventRevisionList{revisions,
  next_page_token}`, `PlaceList{places}`, `SourceList{sources}`.
- Layer/Severity/EventStatus/Geometry/BoundingBox/LatLng/Provenance/Source/Place/
  PlaceKind exactly as spec §3.

### 2.2 SQLite schema

Exactly spec §4 (events, event_revisions, places, event_places, event_geo rtree +
event_geo_map, sources, subscriptions) plus:
- `schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER)`.
- `sources.status INTEGER NOT NULL DEFAULT 0` and `sources.disappearance TEXT
  NOT NULL DEFAULT 'resolve'` (policy per decision 8).
- Pragmas at open: `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON`,
  `synchronous=NORMAL`.
- `content_hash` = SHA-256 of `proto.MarshalOptions{Deterministic:true}` over a
  normalized clone (zeroed: `revision`, `ingested_at`, `observed_at`,
  `provenance.fetched_at`, `place_ids`). Upstream re-stamps without content
  change ⇒ no revision.

### 2.3 /v1 endpoints (all hand-built, mounted at prefix `/v1/`)

| Endpoint | Backing | Notes |
|---|---|---|
| `GET /v1/places/{place}/summary` | store + conditions | mode QUIET/WATCH/ACTIVE; `domains[]`; evac `int\|null`; source health sidecar. Cache-Control 30s. |
| `GET /v1/places/{place}/map/{layer}.geojson` | store (events) / live (conditions) | envelope byte-identical to `/api/v1/hazards`. Cache-Control 60s. |
| `GET /v1/events` | store | filters `place,layer(rep),status(rep),severity_min,since,page_token,page_size`; default status ACTIVE,SCHEDULED; keyset pagination (severity DESC, observed_at DESC, id) encoded base64. |
| `GET /v1/events/{id}` | store | current revision, protojson. |
| `GET /v1/events/{id}/history` | store | revisions descending. |
| `GET /v1/history` | store | `place,from,to,layer` over revisions. |
| `GET /v1/places` / `/v1/places/{place}` | store | `kind`, `q` filters. |
| `GET /v1/places/resolve` | store (+census for address) | `lat,lng` or `address`. |
| `GET /v1/roads` / `/v1/roads/{id}` | RoadsService | conditions passthrough, `?place=` bbox filter, alerts field kept. |
| `GET /v1/weather` / `/v1/weather/{location}` | WeatherService | conditions, **alerts stripped** (events own them). |
| `GET /v1/scanners` | config | `?place=`. |
| `GET /v1/sources` | store | registry + health. |

Conventions: RFC 3339 timestamps; errors as `google.rpc.Status` protojson
(`{"code":5,"message":"..."}` + HTTP status); ETag = strong hash of body,
If-None-Match → 304; CORS comes free from prefab middleware.

### 2.4 Wire format details (locked)

- **JSON casing: snake_case everywhere on `/v1`** — entity endpoints marshal
  with `protojson.MarshalOptions{UseProtoNames: true}` so proto field names
  (`severity_rank`-style) hit the wire as-is, matching the shipped GeoJSON
  envelope and the hand-built summary. Query params snake_case
  (`severity_min`, `page_token`, `page_size`). Enum values render as their proto
  names (`WILDFIRE`, `ACTIVE`, `SEVERE`); query params accept them
  case-insensitively and layer params also accept the shipped lowercase layer
  slugs (`wildfire`).
- **Severity color ramp** (canonical, from hazard-aggregation-design §4.2):
  EXTREME `#7f1d1d`, SEVERE `#c2410c`, MODERATE `#b45309`, MINOR `#a16207`,
  INFO `#6b7280`. Pair color with the label — color is never the only signal.
- Canonical client sort: `severity_rank` desc, then `observed_at` desc.

Mode rules (pure function, unit-tested):
- ACTIVE: any active evacuation (ORDER/WARNING/SHELTER_IN_PLACE), or any active
  event EXTREME, or wildfire SEVERE.
- WATCH: any active SEVERE, or evac ADVISORY, or fire_weather elevated/red-flag,
  or any layer UNAVAILABLE while another signal ≥ MODERATE.
- QUIET: otherwise. (UNAVAILABLE evac forces mode ≥ WATCH — unknown is not quiet.)

Domains: `fire` (wildfire + fire_weather), `evacuation`, `weather`
(weather_alert), `roads` (road_incident + chain_control + road_segment),
`seismic` (earthquake). Each: `status` (worst source_status), `highest_severity`,
`active_count`, `headlines` (top 3).

Summary response shape (hand-built JSON, snake_case — the site codes against
this):
```json
{
  "place": "ebbetts-pass", "place_id": "area:ebbetts-pass",
  "place_name": "Calaveras County", "generated_at": "RFC3339",
  "mode": "QUIET|WATCH|ACTIVE",
  "summary": {
    "highest_severity": "SEVERE", "highest_severity_rank": 3,
    "severity_counts": {"SEVERE": 1}, "total_active": 4,
    "active_evacuations": null, "evacuation_status": "UNAVAILABLE",
    "top_events": [{"id","layer","severity","severity_rank","headline","source"}]
  },
  "domains": [{"domain","status","highest_severity","active_count",
               "headlines":[{"id","severity","headline"}]}],
  "sources": [{"id","status","last_success_at"}]
}
```
Other /v1 shapes are the protojson of the grid.v1 list messages (snake_case).
Note `Event.geometry.geojson` is a proto `bytes` field ⇒ **base64 in protojson**;
clients decode (`atob`) before `JSON.parse`. Map rendering should prefer the
`.geojson` endpoints; the base64 field is for detail views. `/v1/places/resolve`
returns places ordered most-specific-first: SITE, EVAC_ZONE, TOWN, CORRIDOR,
COUNTY, AREA.

## 3. Task breakdown

### Tier A — foundation scaffolding (serial, one agent)

**T1. Protos + deps + build plumbing.**
Files: `api/grid/v1/grid.proto` (+ generated `grid.pb.go` committed), `Makefile`
(extend `proto` target with a second protoc invocation: `--proto_path=api/grid/v1
--go_out=api/grid/v1 --go_opt=paths=source_relative`; no gateway/openapi for
grid), `go.mod`/`go.sum` (`go get modernc.org/sqlite`), `.gitignore` (`data/`).
Verify: `make proto` idempotent (no diff on second run), `go build ./...`.

### Tier B — parallel after T1

**T2. Store package.** Files: `internal/store/{store.go,schema.sql,events.go,
places.go,sources.go,query.go}` + tests.
Contracts:
```go
func Open(path string) (*Store, error)   // migrates, pragmas
func (s *Store) Close() error
// events — write side (single writer: the scheduler)
func (s *Store) UpsertEvent(ctx, ev *gridv1.Event) (UpsertResult, error)
  // UpsertResult{Changed bool, Revision uint32}; content-hash gate; on change:
  // events row + event_revisions insert + event_places recompute + rtree update
func (s *Store) TransitionEvents(ctx, ids []string, to gridv1.EventStatus, observedAt time.Time) error // revision per event
func (s *Store) ActiveEventsBySource(ctx, sourceID string) ([]*gridv1.Event, error)
// events — read side
func (s *Store) GetEvent(ctx, id string) (*gridv1.Event, error)
func (s *Store) QueryEvents(ctx, q EventQuery) ([]*gridv1.Event, string, error)
func (s *Store) EventHistory(ctx, id string, pageSize int, token string) ([]*gridv1.EventRevision, string, error)
func (s *Store) QueryHistory(ctx, q HistoryQuery) ([]*gridv1.EventRevision, string, error)
// places
func (s *Store) UpsertPlace(ctx, p *gridv1.Place) error
func (s *Store) ListPlaces(ctx, kind gridv1.PlaceKind, q string) ([]*gridv1.Place, error)
func (s *Store) GetPlace(ctx, slugOrID string) (*gridv1.Place, error)
func (s *Store) PlacesContaining(ctx, lat, lng float64) ([]*gridv1.Place, error) // bbox + PIP via lib/geojson
// sources
func (s *Store) SeedSources(ctx, seeds []SourceSeed) error // insert-or-update config-owned fields
func (s *Store) RecordAttempt(ctx, id string, err error) error // sets last_attempt/error(/success), derives status vs stale_after
func (s *Store) ListSources(ctx) ([]*gridv1.Source, error)
```
`EventQuery{PlaceID string; Layers []gridv1.Layer; Statuses []gridv1.EventStatus;
MinSeverity gridv1.Severity; Since time.Time; PageSize int; PageToken string}`;
place filter joins `event_places`. Tests: temp-dir DB; revision gating (same
content twice ⇒ 1 revision; changed headline ⇒ 2), transition revisions,
pagination stability, PIP correctness, restart-reopen rehydration.

**T3. GeoJSON geometry lib.** Files: `internal/lib/geojson/{geojson.go,pip.go}`
+ tests. Owns: parse RFC 7946 geometry bytes → typed rings; `Bbox(geom)`,
`Centroid(geom)`, `PointInGeometry(lat,lng,geom)` (Polygon/MultiPolygon,
even-odd rule, bbox fast path), `BboxPolygonGeoJSON(minLat,minLng,maxLat,maxLng)
[]byte`, `PointGeoJSON(lat,lng) []byte`, `LineStringGeoJSON(pts) []byte`.
Consumed by store (bbox/centroid at ingest), places seed, resolve.

**T4. Census geocoder client.** Files: `internal/clients/census/client.go` +
test. `NewClient()`, `NewClientWithHTTPDoer(baseURL, d)`,
`Geocode(ctx, oneline string) (lat, lng float64, matched string, err error)`
against `https://geocoding.geo.census.gov/geocoder/locations/onelineaddress`
(benchmark `Public_AR_Current`, `format=json`), body cap, no-match ⇒ typed
`ErrNoMatch`. Fixture-based test with fake HTTPDoer (repo convention).

### Tier C — ingest (parallel after T2; each owns one file pair)

Shared scaffolding (written with T2 by the same agent, file
`internal/ingest/ingest.go`):
```go
type Normalizer interface {
  SourceIDs() []string                       // source rows this poller updates
  Poll(ctx context.Context) (*PollResult, error)
}
type PollResult struct {
  Events   []*gridv1.Event      // full current set for this poller's scope
  PerSource map[string]error    // partial failures (source id → err), nil entries OK
}
// helpers: NewEvent(id, layer, sev, status, headline) *gridv1.Event;
// GeometryFromGeoJSON(raw []byte) *gridv1.Geometry (bbox+centroid via lib/geojson);
// GeometryFromPoint(lat,lng); ProvenanceFor(sourceID) — registry-driven.
```
Each normalizer maps to the **shipped envelope semantics** (same severity
mappings as `internal/hazards/severity.go` — import and reuse those functions by
exporting them or duplicating the pure mappings into ingest with tests pinning
equivalence).

**T5. `internal/ingest/earthquake.go`** — usgs client, statewide-bounds union of
configured areas, id `usgs:{ID}`, severity fromMagnitude, observed_at=Updated,
effective=Time. Detail: magnitude/depth_km/felt/url.

**T6. `internal/ingest/wildfire.go`** — calfire + wfigs concurrent fetch, name
join (port `normFireName` + ambiguity rules from `hazards/service.go` — move the
pure helpers to a shared spot or reimplement with equivalence tests), ids
`calfire:{uid|slug}` / `wfigs:{normname}[-N]` (decision 3), partial-failure ⇒
`PerSource`, geometry: perimeter polygon else point.

**T7. `internal/ingest/evacuation.go`** — caloes, id `evac:{zone}`,
level normalization (reuse `normalizeEvacLevel` semantics), category=level
lowercase, severity fromEvacLevel, PublicInfo → description **verbatim**
(life-safety: never rewritten), geometry raw polygon.

**T8. `internal/ingest/weather_alert.go`** — via `WeatherService.ListWeatherAlerts`
(cached NWS), id `wx:{id}`, severity from NWS severity, SCHEDULED when
effective>now, expires → EXPIRED policy input, zones into detail; enhancement
hook (decision 7) with `Enhancer` interface + budget; place_ids from zone→area
mapping (areas' `zones` config) — zone-carrying events attach to area places.

**T9. `internal/ingest/road_incident.go`** — via `RoadsService.ListIncidents`
per configured incident area, id `chp:{id}`, sources `chp`/`caltrans` split by
alert type, carries AI-enhanced description/condensed/impact/metadata into
detail + Enhancement provenance, geometry point.

**T10. Scheduler + lifecycle + wiring.** (after T5–T9) Files:
`internal/ingest/scheduler.go`, `internal/ingest/lifecycle.go`, edits to
`cmd/server/main.go`, `internal/config/config.go` (add `GridConfig{DBPath string;
Sources map[string]SourceTuning}`), `prefab.yaml` (grid section with per-source
poll intervals: usgs 5m, wildfire 5m, evac 2m, weather_alert 5m, road_incident
5m; stale_after defaults 3×; disappearance per decision 8).
Tick per poller: `Poll` → for each event `UpsertEvent` → compute disappeared =
ActiveEventsBySource − polled ids → apply policy transitions → `RecordAttempt`
per source. One SQL transaction per tick is NOT required across pollers; the
store serializes writes. Jittered start, panic-recovery (mirror
periodic_refresh.go), context cancellation. main.go: open store, seed sources +
places (T11 seeder), start scheduler, graceful close.

### Tier D — API + places (parallel after T2/T3; T12 needs T10 only at wiring time)

**T11. Places seed + directory data.** Files: `internal/places/seed.go` + test,
`data/places/counties.geojson` (checked in; fetched once from Census TIGERweb
during this task — document provenance in a header comment), `data/places/README.md`.
Seeds: areas (`area:{id}`, slug `{id}`, bbox polygon), counties
(`county:{slug}`), towns (`town:{slug}` from weather locations, point), corridors
(`corridor:{road-id}` linestring). Parent links: town→county, area→none.
Idempotent upsert at boot.

**T12. `/v1` API service.** Files: `internal/gridapi/{service.go,events.go,
places.go,summary.go,maplayers.go,conditions.go,sources.go,etag.go}` + tests
(httptest against seeded temp store + stub roads/weather interfaces — reuse the
`roadsAPI`/`weatherAPI` interface trick from `internal/hazards/service.go`).
Mounted in main.go as `prefab.WithHTTPHandler("/v1/", gridapiService)` with an
internal `http.ServeMux`-free router (path switch, hazards-style). Endpoints per
§2.3. Map layers: event-backed from store via the **shared projection**
(T13), condition-backed by delegating to the existing hazards builders (export
what's needed or construct hazards.Service internally). Summary per §2.3 rules.

**T13. Store→GeoJSON projection + byte-compat harness.** Files:
`internal/gridapi/project.go`, `internal/gridapi/project_test.go`. One function
per event layer: `[]*gridv1.Event → []hazards.Feature` reproducing the shipped
envelope exactly (field order irrelevant — JSON compare). Harness: golden test
that (a) builds features via today's hazards builders from fixture inputs,
(b) normalizes fixtures → Events → store → project, (c) asserts JSON-equal
envelopes (modulo the documented wfigs-id change and `source.fetched_at`).
**Gate:** only after this passes may T14 land.

**T14. Re-back `/api/v1` hazards event layers onto the store.** (after T13)
Files: `internal/hazards/service.go` (builder swap behind a `store != nil`
constructor arg), keeping conditions layers + situation/scanner endpoints
untouched. Fail-loud mapping: source row UNAVAILABLE ⇒ layer UNAVAILABLE; STALE
⇒ STALE with `last_source_update`; store always serves last-good (that's the
point) — the evac invariant (`error never becomes 0`) now derives from source
health, with tests proving UNAVAILABLE evac still yields `null` in situation.

### Tier E — site (parallel after T12 exists in rough form; pages are disjoint files)

Site root `site/` (embedded). Shared first (one agent, **T15**): `site/index.html`
(Home: status strip from `/v1/sources` + per-area `/summary` mode, links),
`site/assets/app.css` (dark instrument-panel default + light via
`prefers-color-scheme`, monospace-leaning, severity chip classes using the
canonical ramp), `site/assets/api.js` (fetch wrapper surfacing the request URL
for the "view the request" footer, ETag-friendly), `site/assets/format.js`
(relative+absolute timestamps, severity chips, protojson helpers),
`site/lib/maplibre-gl.js` + `.css` (vendored), footer partial convention.
Then parallel:
- **T16** `site/sources.html` — the ops board (unhealthy-first sort, poll
  intervals, last error text).
- **T17** `site/events.html` + `site/event.html` — explorer (filter bar mapping
  1:1 to `/v1/events` params, dense table, URL state) + detail (envelope +
  detail dl, small map, provenance, revision timeline with client-side
  protojson field diff, AI badge with verbatim original, raw toggle).
- **T18** `site/places.html` — directory by kind + place page + resolve tester
  (click map / address input → `/v1/places/resolve`).
- **T19** `site/map.html` — MapLibre layer previews, layer+place pickers,
  `metadata` block (source_status/generated_at/attribution) prominent, shows the
  exact `.geojson` URL. OSM raster basemap with attribution.
- **T20** `site/history.html` — time-range + place + layer over `/v1/history`,
  chronological revision feed, permalinkable.
- **T21** `site/docs.html` — hand-authored `/v1` reference (endpoints, params,
  severity scale + color ramp, place id scheme, evac fail-loud contract, media
  types), every example a live link; links to legacy swagger JSON.
- **T22** Server wiring for the site: `cmd/server/site.go` (go:embed FS, serve
  at `/`, correct content-types, `Cache-Control: no-cache` for HTML /
  `max-age=86400` for lib assets), replace homepageHandler.

### Tier F — verification + docs (serial)

**T23. Full-suite green + adversarial review.** `make proto && make build &&
make test` clean; multi-agent code review (find→verify) over `git diff main`;
fix confirmed findings.

**T24. E2E against real upstreams.** `make run-bg` with `.envrc` keys: assert
`/v1/sources` all OK within ~2 min; events flowing (usgs/nws at minimum);
`/v1/events` filters + pagination; `/v1/places/resolve` for Arnold lat/lng and
one address; summary modes sane; `.geojson` parity `/api/v1/hazards/...` vs
`/v1/places/.../map/...`; site pages render (curl HTML + fetch the JS-called
endpoints); **restart rehydration** (stop, start, events persist, revisions
intact); evac fail-loud probe (point caloes URL at a black-hole host in a dev
config run ⇒ situation `active_evacuations: null`).
OpenAI: allow the incident/NWS enhancement path to run normally (budget caps
apply); this is the one real-API cost and it is cents.

**T25. Docs.** CHANGELOG.md (new dated section: /v1 surface, persistence,
site, id-stability deviation, deprecation plan for /api/v1 per spec §6),
README.md, CLAUDE.md (commands/env/config), ARCHITECTURE.md refresh,
`internal/store/CLAUDE.md` + `internal/ingest/CLAUDE.md` (conventions for the
next agent), site deploy notes (Docker: EFS volume at `/data`,
`PF__GRID__DB_PATH=/data/grid.db`).

## 4. Sequencing & checkpoints

```
T1 ──┬─ T2 ──┬─ T5..T9 ── T10 ──┐
     ├─ T3 ──┤                  ├─ T12 ── T13 ── T14 ──┐
     └─ T4 ──┘   T11 ───────────┘                      ├─ T23 ── T24 ── T25
                 T15 ── T16..T22 ──────────────────────┘
```
Git checkpoints on `v2-grid` after: T1; T2–T4; T10 (store ingesting dark);
T12–T14 (/v1 live); T22 (site); T25 (final). Conventional commits, no push.

## 5. Risks & fallbacks

- **R*Tree/driver**: retired (verified in sandbox). Fallback was bbox columns.
- **Byte-compat**: if T13 finds unavoidable envelope drift, T14 does NOT land;
  `/api/v1` hazards stay on live models (spec step 1 posture) and the drift is
  documented for a follow-up. `/v1` still ships.
- **Cal OES active-only inventory**: zone-place import deferred (decision 11);
  resolve still works via county/area/town polygons.
- **wfigs id change**: documented breaking-ish detail in CHANGELOG (ids were
  never stable).
- **Byte-compat exclusion list** (T13 harness compares modulo exactly these,
  each CHANGELOG'd): (1) wfigs standalone ids (stability fix); (2)
  `source.fetched_at` freshness; (3) weather_alert severity — the store maps
  NWS "Extreme"→EXTREME (shipped path collapsed it to SEVERE via the api enum;
  accuracy fix); (4) earthquake `updated_at` — projection omits it when
  `observed_at == effective` (matches shipped omit-when-zero behavior); (5)
  road_incident `source` block — projection emits the shipped per-layer
  constant `{id:"chp", name:"CHP / Caltrans", attribution:"quickmap.dot.ca.gov"}`
  for ALL road incidents (store provenance keeps the chp/caltrans split for
  source health; the envelope block is a projection constant, as are the other
  layers' source blocks — derive them from the layer, not stored provenance,
  except URL/name fields that genuinely vary per event: usgs/calfire event
  URLs come from `Event.canonical_url`, wildfire standalone perimeters emit
  the wfigs block, evacuation emits the caloes block verbatim).
- **Prefab route precedence**: `/v1/` and `/` are distinct ServeMux prefixes;
  `/api/` (gateway) keeps precedence by longer-prefix match. Verified in T12
  tests via the mounted mux.
- **Cost control**: all tests offline (fixtures/mocks); OpenAI calls only in
  T24's single live run, under existing budget caps (≤5/refresh) + 24h hash cache.
