---
title: Polygon events over-attach to places by bounding box instead of real overlap
date: 2026-07-29
category: logic-errors
module: internal/store (grid event store — place matching)
problem_type: logic_error
component: database
symptoms:
  - "A wildfire perimeter attaches to every county whose bounding box overlaps the perimeter's bbox, not just the ones it lies in"
  - "Dove Fire (physically in Tuolumne) tagged with county:calaveras-county and county:stanislaus-county too"
root_cause: logic_error
resolution_type: code_fix
severity: medium
tags: [place-matching, geojson, polygon-intersection, bounding-box, wildfire, geometry]
---

# Polygon events over-attach to places by bounding box instead of real overlap

## Problem
The grid event store attached a polygon event (a wildfire perimeter) to any
polygonal place whose **bounding box** merely overlapped the event's bounding box.
County bounding boxes are large interlocking rectangles that overlap each other far
from their actual borders, so a fire near a multi-county junction was tagged to
every nearby county. The Dove Fire — physically in Tuolumne — appeared under
`county:calaveras-county`, `county:stanislaus-county`, and `county:tuolumne-county`
on the public `/api/v1` place attachments and map.

## Symptoms
- A wildfire event's `placeIds` (and `/api/v1/events?place=` scoping) include
  counties the fire does not actually touch.
- The Dove perimeter bbox is tiny (~1 km: lat 37.964–37.973, lng −120.400 to
  −120.382) yet it attached to all three counties whose bboxes clip it.
- The over-attachment appeared only **once the perimeter existed**. As a bare point
  (before a perimeter was available) the same fire matched exactly one county,
  because point events use an exact point-in-polygon test.

## What Didn't Work
- The tempting wrong diagnosis is that the fire geographically straddles the three
  counties. It does not. Testing the **real** county polygons against the real Dove
  MultiPolygon showed `Intersects` = true for Tuolumne only (its centroid is inside
  Tuolumne), and false for Calaveras and Stanislaus. Those two attachments came
  purely from rectangular-bbox overlap, not from any real geometric contact — so
  "just widen/keep it, the fire is near the borders" would have been wrong.

## Solution
`matchPlaces` (`internal/store/events.go:552`) runs three checks for a polygon
event against a place; the last attached on bbox overlap alone.

Before:
```go
if pl.polygonal {
    matched = append(matched, pl.id) // permissive polygon-polygon bbox overlap
}
```

After (`internal/store/events.go:611`):
```go
if pl.polygonal && geojson.Intersects(evGeom, pl.geom) {
    matched = append(matched, pl.id) // actual polygon overlap, not just bbox
}
```

Added `geojson.Intersects` (`internal/lib/geojson/intersect.go:22`): a bbox
pre-reject, then a robust polygon-overlap test — **any vertex of one geometry
inside the other** (covers containment and partial overlap) **or any edge of one
crossing an edge of the other** (covers the plus/cross case where boundaries cross
but no vertex lands inside). It has early exits and runs off the request path, only
for polygon events that already passed the bbox prefilter. Committed to `main` as
`7c2b873`.

## Why This Works
A county's rectangular bounding box overlaps its neighbors' bboxes well away from
the actual borders, so bbox overlap is a poor proxy for "the fire is inside this
county." An actual polygon-intersection test attaches a fire only to the counties
it geometrically touches. The bbox stays as the cheap prefilter (reject fast), and
`Intersects` is the authoritative confirmation before attaching. A perimeter that
genuinely straddles a county line still attaches to both counties — a perimeter
vertex lands inside each, or the county boundary crosses the perimeter, so
edge-crossing catches it.

## Prevention
- **Separate the prefilter from the predicate.** For any "does shape A relate to
  shape B" decision on polygons, use the bounding box only to *reject* cheaply,
  then confirm with a real geometric predicate before acting. A bbox test that
  doubles as the decision over-attaches whenever the shapes' bboxes overlap more
  than the shapes do.
- **No backfill needed for a matching-logic fix.** The store recomputes place
  attachments on every upsert, including hash-equal no-ops (`refreshEventPlaces`
  rewrites `event_places` and the blob's `place_ids` when the recomputed set
  differs — see `internal/store/CLAUDE.md`), so a fix self-heals stored events on
  the next poll.
- **Test the bbox-overlaps-but-disjoint case explicitly** — a place whose bbox
  clips the event while its polygon misses it. Covered by
  `TestPolygonEventPlaceMatching` (a triangular county whose bbox overlaps the
  perimeter but whose body does not) and `geojson.TestIntersects`
  ("bbox overlaps but polygons disjoint", plus a plus-shape edge-crossing case).

## Related Issues
- Surfaced by the WFIGS → CAL FIRE/FIRIS perimeter-source swap (commit `1fdcdd2`),
  which turned wildfire events from points into polygons and so first exercised the
  polygon branch of `matchPlaces`. See `../../firis-perimeter-source-design.md`.
