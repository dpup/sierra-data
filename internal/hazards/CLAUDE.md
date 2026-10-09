# Hazard model + condition-layer projection

Originally this package served the unified GeoJSON hazard feed at
`GET /api/v1/hazards/{area}/{layer}.geojson` (plus `/api/v1/situation` and
`/api/v1/scanners`). **That HTTP surface was removed on 2026-07-08** along with
the rest of `/api/v1`. What remains, and what this package is now, is two things:

1. **The shared hazard model** (`geojson.go`, `properties.go`, `severity.go`) —
   RFC 7946 GeoJSON types, the common `Properties` envelope + per-kind blocks,
   and the one severity scale. `internal/gridapi` reuses these to project the
   grid store's events into map layers, so the envelope stays identical to what
   the old feed emitted.
2. **The live condition-layer builder** (`(*Service).BuildLayer`) — the only
   runtime entry point now. `internal/gridapi` calls it for the four
   **condition** layers only: `road_segment`, `chain_control`, `fire_weather`,
   `message_sign` (see `conditionLayers` in `internal/gridapi/maplayers.go`).
   The five **event** layers (wildfire, evacuation, weather_alert, earthquake,
   road_incident) are projected from the grid store by
   `internal/gridapi.ProjectEvents` / the per-kind `project*` helpers — **not
   here**. Two layers are neither: `mesh_link` and `camera` are built by
   `internal/gridapi` itself (`mesh.go`, `cameras.go`) and only borrow this
   package's envelope (`MeshLinkProps`, `CameraProps`). The live event builders and the store-backed path that used to live
   in this package were deleted with the endpoints.

## The model (don't break the envelope)

- `geojson.go` — RFC 7946 types + geometry constructors. **Coordinates are
  `[longitude, latitude]`** (the inverse of the service's internal
  `{latitude, longitude}`); build geometry via `PointGeom` / `LineStringGeom`
  (or `RawGeom` to pass upstream `[lon,lat]` GeoJSON through), which do the swap
  and trim to 5 decimals.
- `properties.go` — the common `Properties` envelope shared by every layer, plus
  a namespaced per-kind block (`incident`, `road`, `chain_control`, `weather`,
  `fire_weather`, `earthquake`, `wildfire`, `evacuation`, `mesh`, `meshLink`,
  `power`, `camera`). The envelope is
  identical across layers — that's the unification; a client renders any card
  from `headline/severity/source`. `gridapi`'s projection builds these same
  structs.
- `severity.go` — the one severity scale (`INFO..EXTREME`, rank 0–4) every source
  maps onto. Editorial response-urgency, not magnitude. Use `setSeverity` so
  `severity_rank` stays in sync. The `SeverityFrom*` / `NormFireName` /
  `NormalizeEvacLevel` wrappers in `severity_export.go` are the exported seam the
  ingest normalizers (`internal/ingest`) use. `SeverityFromLaneClosure` grades
  a CWWP2 closure from its fields alone (no AI). The closure TYPE wins over the
  lane list: "Alternating Lanes" lists every lane it will ever close but closes
  them in turn, so counting lanes would call it a full closure.

## Fail-loud (still enforced for the condition layers)

`buildLayer` applies the fail-loud rules for the condition layers. On a
source **error** it returns `source_status = UNAVAILABLE` with empty features —
never a fabricated clear state. The load-bearing property is **"an error never
becomes a 0"**: `UNAVAILABLE` means the source genuinely failed (consumer shows
"unknown / check the official source"), distinct from `OK` + 0 features (source
healthy, currently reports nothing). Status resolution:

- fresh cache hit → `OK` (no upstream call)
- builder OK (incl. a clean empty) → `OK`; non-empty results cached for `layerTTL`
- builder returns `partialData(err)` → `STALE`, features kept; a non-empty
  partial is cached for `layerTTL` tagged partial (cache `Source`
  `hazard:<layer>:partial`), so a fresh hit stays `STALE` + its fetch time and
  a degraded upstream is refetched at most once per TTL; the next clean fetch
  overwrites it
- builder hard error **with** a cached value → `STALE`, last-good features served
- builder hard error, nothing cached → `UNAVAILABLE`, empty

Caching uses the shared `internal/cache`; `layerTTL` returns 0 for layers already
cached by their underlying service (no double-caching — `road_segment` is the
example: `ListRoads` is cached by the roads service, so a layer TTL here would be
a second copy of the same data).

**Clean empties ARE cached, except for `evacuation`.** Non-empty successes are
always cached; `cacheEmptyResults` governs the "source healthy, reports nothing"
case:

- The invariant "an error never becomes a 0" is enforced on the **error** path,
  by the `len(stale) > 0` guard in `buildLayer` — an upstream failure can never
  be answered from a cached empty, whatever is stored. **That guard is
  load-bearing; do not simplify it away.** What gets cached is a *success*, not
  an absence of information, and a fresh cached empty is served as `OK` + 0
  ("healthy, currently reports nothing"), never as `STALE` or `OK` after an error.
- Not caching empties cost a full upstream fetch on **every request** for any
  layer that is legitimately empty most of the time. `chain_control` outside snow
  season re-parsed the Caltrans KML on every `/summary` and every
  `chain_control.geojson` — 36-49 ms each, indefinitely, for a 212-byte answer
  that never changed. Caching empties took `chain_control` to ~0.7 ms and
  `/summary` from ~26-40 ms to ~2-4 ms.
- **`evacuation` is deliberately excluded.** Caching an empty means the FIRST
  evacuation order takes up to the TTL to appear, and that is the one transition
  on the service where delay is least acceptable — the layer tolerates 2 minutes
  of staleness once zones exist but keeps "nothing → something" instant. It is
  also not a performance problem (Cal OES answers in ~1 ms), so there is nothing
  to buy and something real to lose. `TestBuildLayer_EvacEmptyIsOK` pins it.

## Served through prefab's HTTP security wrapper

The package no longer registers any handler itself; `gridapi` serves `/api/v1`
(via prefab's gRPC-Gateway) and calls `BuildLayer` in-process. CORS is applied by
prefab's `securityMiddleware` on the mounted handlers from `prefab.yaml`
(`server.security`) — now open (`corsOrigins: ["*"]`, GET-only; see the CORS note
in the root `CLAUDE.md`) — do not add manual `SecurityHeaders` calls.

## `chain_control` sources

Caltrans CWWP2 (every checkpoint, explicit `R-0`) MERGED with cc.kml — neither
source overrides the other's controls; see the `cwwp2` section of
`internal/clients/CLAUDE.md` for the rules. On a quiet day `OK` + 0 features is
a **confirmed** "no chain controls", not an empty KML taken on faith. Degraded
cases keep features and return `partialData` (→ `STALE`): either source down
(`caltrans.PartialError`), an in-area checkpoint whose status CWWP2 sent as
garbage and cc.kml doesn't explain (`Unrecognized` — dropped from the map, never
shown as a level), or a row with no position. Controls cc.kml reports where
CWWP2 says R-0 are SHOWN and logged ("cc.kml reports chain controls CWWP2 does
not"). Road closures (`Closed`) render as `"<highway> road closed"`, category
`closed`, severity `MINOR` — one notch above a clear checkpoint, below R-1,
because the seasonal gates hold it all winter (and `/summary`'s roads domain
counts it as active). Feature ids are `cc:<checkpoint index>` for CWWP2 (e.g.
`cc:10-ALP-4-0.65-W-14W`, Bear Valley WB), `cc:<message id>` for cc.kml, with a
coordinate fallback; `source.attribution` names the host.

## `message_sign` — context, never a hazard

What each Caltrans changeable message sign inside the area is showing, from
the CWWP2 `cms` feed (`messagesign.go`; the parser's rules are in the `cwwp2`
section of `internal/clients/CLAUDE.md`). Wired by `UseMessageSigns` from
`roads.caltransFeeds.cwwp2.messageSignDistricts`. Without districts the layer
is `UNAVAILABLE`, since it has no other source.

**Every feature is `INFO` and `/summary` does not read the layer.** Keep it
that way until a storm capture shows how real messages look. On 2026-10-01, 82
of District 10's 107 signs showed one statewide safety campaign, and the
campaign rotates, so filtering by a fixed list would go stale and ranking by
text would let boilerplate move the summary. A "CHAINS REQUIRED" sign stays
`INFO` too: `chain_control` carries the requirement, and two layers ranking
the same fact would double-count it. `TestSummary_IgnoresMessageSigns`
(gridapi) pins the summary half.

- `category` is `message` | `blank` | `unknown`. A sign the portal can't read
  (out of service, `Not Reported`) is LISTED as `unknown` and does not degrade
  the layer: the layer still knows every sign in the area and says which one
  it can't read. That differs from `chain_control`, where an unreadable status
  could hide a requirement.
- A fetch failure in any configured district is a hard error (all or nothing,
  like `chain_control`). A sign with no position degrades to `STALE`.
- Ids are `cms:<district>:<index>`, falling back to the sign's position when
  the index isn't unique in its district's file (D12 numbers every sign `1`).
- Scoped by `area.Bounds` like the other condition layers, so `ebbetts-pass`
  gets the six D10 signs in its fetch rectangle. Only two of them (Hwy 108 at
  Soulsbyville, Hwy 4 west of Murphys) are inside the hand-drawn area polygon.
  The Hwy 88 signs (Pine Grove, Dew Drop and its "Virtual CMS" twin) and
  `43 - EB 120 W/O YOSEMITE` are outside it.

## Changing a condition layer

1. If it needs a new per-kind block, add it in `properties.go` and the severity
   mapping in `severity.go` (cover every enum value; use `setSeverity`).
2. Write/adjust the `builder` method `func (s *Service) <layer>(ctx, area)
   ([]Feature, error)` and its `layerRegistry()` entry (the single source of
   truth for the dispatch map). Give it a `layerTTL` if it hits a new upstream.
   **Scope to the area** — `area.Bounds.Contains` for geocoded sources,
   `zonesMatch(area.Zones, …)` for zone-based data — or a second configured area
   inherits the first's data.
3. **Event layers do not live here.** A new event-shaped source is a normalizer
   in `internal/ingest` + a projection case in `internal/gridapi` (see those
   packages' guides), not a builder in this package.
