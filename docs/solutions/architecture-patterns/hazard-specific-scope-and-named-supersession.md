---
title: Giving one hazard its own wider geography, and ending an event by naming its successor
date: 2026-08-04
category: architecture-patterns
module: internal/ingest/wildfire, internal/store (place matching), internal/ingest/scheduler
problem_type: architecture_pattern
component: background_job
severity: high
tags: [ingest, geo-scope, wildfire, place-matching, proximity, disappearance-policy, supersession, fail-loud, life-safety]
applies_when:
  - one hazard layer needs a different (wider) geographic scope than every other source
  - an upstream gives one coordinate per record but the record's real extent is a polygon
  - an event's place attachment must include "approaching but not yet overlapping"
  - a poller stops emitting an id because something else absorbed it, and the disappearance grace holds the orphan active
  - a shared "scope" helper is quietly load-bearing for lifecycle, not just for fetching
---

# Giving one hazard its own wider geography, and ending an event by naming its successor

Shipped alongside the widened wildfire scope on `main`.
Primary files: `internal/ingest/wildfire.go`, `internal/ingest/ingest.go`
(`wildfireScope`, `PollResult.Superseded`), `internal/ingest/scheduler.go`
(`supersede`), `internal/store/events.go` (`matchPlaces`),
`internal/lib/geojson/near.go` (`WithinDistance`), `internal/config/config.go`
(`WildfireConfig`).

## Context

Every spatial poller in the grid scoped itself to one shared rectangle:
`unionBounds(cfg.Hazards.Areas)`, the union of the configured hazard areas. It
was the obvious design — one service area, one box, every source scoped to it.

It was wrong for exactly one layer, and measurably so. On 2026-08-04 the **Gann
Fire** (3,760 ac, 0% contained, Valley Springs) was burning with its CAL FIRE
origin point at `-120.766` — 46 km west of the box's `-120.72` edge. Three things
followed, none of them visible as an error:

1. The CAL FIRE incident was dropped by a point-in-box test, taking its acreage,
   containment and incident URL with it.
2. The FIRIS perimeter *was* returned (the ArcGIS envelope query is
   `esriSpatialRelIntersects`, so it caught the eastern lobe) and surfaced as a
   bare standalone `firis:gann` — no containment, no incident link.
3. That perimeter's easternmost point (`-120.693`) was still west of the coverage
   polygon's westernmost (`-120.68`), so it attached to **no area**. The Ebbetts
   Pass map layer served **zero features** for a 3,760-acre uncontained fire
   about a kilometre from the boundary.

Worse, the shared box was *narrower* than the CHP/Caltrans incident box
(0.72°×0.83° vs 1.65°×1.65°). The service watched road incidents over four times
the area it watched fire.

## Guidance

### 1. Scope per hazard, not per deployment — the asymmetry is real

The instinct to give every source one scope is a false economy. Most hazards only
matter **where they happen**: an earthquake 50 km away is someone else's
earthquake, a road incident outside the corridor is not on your route. Fire is
different in kind — it **moves toward you**, it closes the roads out, and it is
the one hazard where an hour of warning changes what people do. A fire outside
the coverage footprint is a threat *to* the footprint.

So wildfire got its own rectangle and nothing else changed:

```go
// internal/ingest/ingest.go
func wildfireScope(cfg *config.Config) (config.GeoBounds, bool) {
    minLat, minLng, maxLat, maxLng, ok := unionBounds(cfg.Hazards.Areas)
    if !ok {
        return config.GeoBounds{}, false   // still errEmptyScope at the caller
    }
    m := cfg.Grid.Wildfire.Margin()        // default 0.5° ≈ 55 km
    return config.GeoBounds{ /* union grown by m, clamped */ }, true
}
```

Two details that matter more than the margin value:

- **The default lives in code, not only in yaml** (`Margin()`,
  `DefaultWildfireMarginDegrees`). An omitted config key must not silently
  restore the narrow, footprint-only behaviour on a life-safety layer. The knob
  can widen; it cannot accidentally narrow by omission.
- **Derive it from the existing scope rather than hand-drawing a second box.** A
  margin auto-tracks any future edit to the coverage polygon; a second literal
  rectangle goes stale the first time someone moves the area and doesn't know to
  move its twin.

### 2. Test scope against the geometry you are about to publish, not the source's point

Upstreams routinely give one coordinate per record while the record's real extent
is a polygon. CAL FIRE publishes an origin point; the fire is a perimeter that can
reach tens of kilometres from it. A point-only in-scope test therefore drops
*precisely the record you most want* — the large one, encroaching.

The fix is an ordering change more than a logic change: **resolve geometry first,
then test scope against it.**

```go
// internal/ingest/wildfire.go — geometry BEFORE the scope test
var geom *gridv1.Geometry
switch {
case matched && !ambiguous[norm]:  geom = cand.geom          // adopted perimeter
case perimsUnusable:               geom = carriedForward(...) // prior polygon
default:                           geom = GeometryFromPoint(in.Lat, in.Lng)
}
if !inWildfireScope(scope, in.Lat, in.Lng, geom) {
    continue
}
```

`inWildfireScope` passes on the point **or** a geometry-bbox overlap.

The subtle, load-bearing part is *which* geometry: the one about to be
**published**, not the freshly-adopted perimeter. Those differ during a FIRIS
outage, when the prior polygon is carried forward — and that difference is what
keeps scope **stable across the outage**. Test the fresh perimeter instead and a
perimeter-only fire silently leaves `Events` the moment the perimeter feed
hiccups, the disappearance sweep sees it missing, and RESOLVES it. A fabricated
all-clear on a burning fire, caused by an upstream blip.

> Scope is part of the sweep's "whole truth for the scope" contract. Anything
> that can move an event in or out of `Events` is lifecycle-critical, even when
> it reads like a fetch-time filter.

### 3. "Near" is a real attachment relation for a hazard that moves

Widening ingest is only half the job. Events reach a place's map and summary via
the precomputed `event_places` join, so a fire that overlaps nothing attaches to
nothing and stays invisible where it matters. `matchPlaces` gained one extra
rule, layered on top of the exact geometry rules rather than replacing them:

```go
near := nearBuffer > 0 && wildfireBufferedKind(pl.kind) &&
    geojson.WithinDistance(nearGeom, pl.geom, nearBuffer)
if eg.matches(pl) || near {
    matched = append(matched, pl.id)
}
```

Scope the looseness deliberately — a proximity rule applied everywhere is just
over-attachment with extra steps:

- **Only AREA and TOWN.** Counties *tile* the map, so a nearby fire already
  attaches to some county exactly, and buffering them smears one fire across four
  (the same failure the bbox-overlap bug produced). Corridors already have a
  tuned 1.5 km point buffer; a 20 km fire buffer would pin every regional fire to
  every highway segment.
- **Only WILDFIRE.** A quake 9 km outside an area is not in it.
- **Cheap by construction.** `WithinDistance` bbox-rejects with a padded box
  before projecting anything, and the buffered kinds are small geometries (a
  10-vertex coverage polygon, a town point), so the exact pass stays trivial
  inside the write transaction.

Be honest downstream about what this means: a proximity-attached event is an
ordinary active event for that place. It counts in `totalActive`, in
`severityCounts`, and can lift the summary `mode`. That is the intent — a fire
12 km out *should* raise the mode — but it means a place can report an active
fire whose perimeter is outside its boundary, and consumers should render "in or
near".

### 4. End an event by naming its successor, instead of waiting out the grace

Richer scope surfaced a lifecycle gap that predated it. A perimeter is emitted
standalone (`firis:<name>`) only while no CAL FIRE incident claims it. The moment
one does, that id stops being emitted — and because `firis` is an `expire`
source, the sweep sees only "absent" and holds the orphan ACTIVE for its full
**24h grace**. The same fire, drawn twice, for a day.

The grace is not wrong; it is answering a different question. It exists because
absence is **ambiguous** (a perimeter upload lags, an alert drops at
end-of-product). Here nothing is ambiguous: the perimeter is still in the feed and
we know exactly which event absorbed it. So say so.

```go
// PollResult — the deliberate INVERSE of SweepSuppress
Superseded []string   // ids this poll proves are gone; RESOLVED immediately
```

`SweepSuppress` says *"I can't prove this is gone."* `Superseded` says *"I can,
and here is what replaced it."* The scheduler resolves them **before** the sweep
runs, so the sweep never sees them.

Three constraints keep this from becoming a hole in the fail-loud invariant:

- **Positive evidence only.** Populate it from something observed, never from "I
  didn't see it" — that is what the sweep is for.
- **Same guard as the sweep.** Only for sources whose fetch succeeded and wasn't
  suppressed, so a failed fetch still transitions nothing.
- **Narrow targeting.** Name only the id *this exact candidate* would have been
  emitted under — `supersededStandalones` reuses `standaloneContinuityID`, the
  same function the standalone path uses. A sibling cluster that genuinely
  dropped out keeps its grace, because absence is still ambiguous for that one.

RESOLVED is a recorded revision, so the supersession is part of history rather
than a delete: `rev 1 ACTIVE → rev 2 RESOLVED`, with the successor carrying the
fire forward under a richer id.

## Why This Matters

Verified end-to-end against the live Gann Fire, old binary vs new on the same
database:

| | before | after |
|---|---|---|
| event | `firis:gann` — bare perimeter, no URL/location | `calfire:e51208a8-…` — CAL FIRE incident + adopted perimeter |
| places | `county:calaveras-county` only | `area:ebbetts-pass` + `county:calaveras-county` |
| `ebbetts-pass/map/wildfire.geojson` | **0 features** | 1 feature |
| place summary | fire absent | top event, mode ACTIVE |
| on deploy | duplicate for up to 24h | `Ingest tick: resolved superseded events source=firis count=1` |

Each failure was silent. Nothing errored, no source went STALE, no log line
complained — the map was simply, confidently empty. That is the through-line:
**a scope filter, a place-attachment rule, and a disappearance grace all fail by
omission**, and omission is invisible. They need to be reasoned about
deliberately, because no alarm will do it for you.

## When to Apply

- **One layer's hazard model differs in kind from the others** (it moves, it
  approaches, its extent dwarfs its reported location) → give that layer its own
  scope rather than widening the shared one for everybody. Derive it from the
  shared scope so it tracks edits; default it in code so omission can't narrow it.
- **An upstream reports a point but the entity is areal** → resolve geometry
  before any spatial filter, and filter on the geometry you will publish.
- **A filter can move an event in or out of the poller's `Events`** → treat it as
  lifecycle-critical and check it is stable across a sibling-source outage. Ask:
  *if the other feed goes down for one tick, does anything silently drop out and
  get resolved?*
- **A poller stops emitting an id because something else absorbed it** → name it
  in `Superseded` instead of letting the grace expire it. Applies to any
  merge/adopt/promote transition between id namespaces, not just fire perimeters.
- **You give an entity richer geometry or wider scope** → audit every downstream
  consumer that assumed the coarser shape. This is the second time that has paid
  out here (see Related).

## Related

- `../logic-errors/polygon-event-place-matching-bbox-overattach.md` — the earlier
  case of richer geometry exposing a downstream assumption.
- `dedup-combo-feed-before-name-join.md` — the FIRIS source swap that produced
  the perimeters this work depends on, and the adoption/name-join it builds on.
- `../../../internal/ingest/CLAUDE.md` — the fail-loud sweep invariant and the
  `Superseded` contract.
