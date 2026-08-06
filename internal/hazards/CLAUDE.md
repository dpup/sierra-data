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
   runtime entry point now. `internal/gridapi` calls it for the three
   **condition** layers only: `road_segment`, `chain_control`, `fire_weather`
   (see `conditionLayers` in `internal/gridapi/maplayers.go`). The five
   **event** layers (wildfire, evacuation, weather_alert, earthquake,
   road_incident) are projected from the grid store by
   `internal/gridapi.ProjectEvents` / the per-kind `project*` helpers — **not
   here**. The live event builders and the store-backed path that used to live
   in this package were deleted with the endpoints.

## The model (don't break the envelope)

- `geojson.go` — RFC 7946 types + geometry constructors. **Coordinates are
  `[longitude, latitude]`** (the inverse of the service's internal
  `{latitude, longitude}`); build geometry via `PointGeom` / `LineStringGeom`
  (or `RawGeom` to pass upstream `[lon,lat]` GeoJSON through), which do the swap
  and trim to 5 decimals.
- `properties.go` — the common `Properties` envelope shared by every layer, plus
  a namespaced per-kind block (`incident`, `road`, `chain_control`, `weather`,
  `fire_weather`, `earthquake`, `wildfire`, `evacuation`). The envelope is
  identical across layers — that's the unification; a client renders any card
  from `headline/severity/source`. `gridapi`'s projection builds these same
  structs.
- `severity.go` — the one severity scale (`INFO..EXTREME`, rank 0–4) every source
  maps onto. Editorial response-urgency, not magnitude. Use `setSeverity` so
  `severity_rank` stays in sync. The `SeverityFrom*` / `NormFireName` /
  `NormalizeEvacLevel` wrappers in `severity_export.go` are the exported seam the
  ingest normalizers (`internal/ingest`) use.

## Fail-loud (still enforced for the condition layers)

`buildLayer` applies the fail-loud rules for the three condition layers. On a
source **error** it returns `source_status = UNAVAILABLE` with empty features —
never a fabricated clear state. The load-bearing property is **"an error never
becomes a 0"**: `UNAVAILABLE` means the source genuinely failed (consumer shows
"unknown / check the official source"), distinct from `OK` + 0 features (source
healthy, currently reports nothing). Status resolution:

- fresh cache hit → `OK` (no upstream call)
- builder OK (incl. a clean empty) → `OK`; non-empty results cached for `layerTTL`
- builder returns `partialData(err)` → `STALE`, features kept
- builder hard error **with** a cached value → `STALE`, last-good features served
- builder hard error, nothing cached → `UNAVAILABLE`, empty

Empty results are **never** cached — a later fetch error then falls through to
`UNAVAILABLE` instead of replaying a stale "0". Caching uses the shared
`internal/cache`; `layerTTL` returns 0 for layers already cached by their
underlying service (no double-caching).

## Served through prefab's HTTP security wrapper

The package no longer registers any handler itself; `gridapi` serves `/api/v1`
(via prefab's gRPC-Gateway) and calls `BuildLayer` in-process. CORS is applied by
prefab's `securityMiddleware` on the mounted handlers from `prefab.yaml`
(`server.security`) — now open (`corsOrigins: ["*"]`, GET-only; see the CORS note
in the root `CLAUDE.md`) — do not add manual `SecurityHeaders` calls.

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
