---
title: Swapping a rate-limited feed for a faster superset, and deduping its many-rows-per-entity shape
date: 2026-07-29
category: architecture-patterns
module: internal/ingest/wildfire
problem_type: architecture_pattern
component: background_job
severity: medium
tags: [ingest, dedup, arcgis, fire-perimeters, wildfire, name-join, spatial-clustering, determinism, fail-loud, source-swap]
applies_when:
  - swapping a rate-limited upstream ArcGIS/NIFC source for a faster superset combo feed
  - an upstream returns multiple rows per logical entity (successive IR flights + FIRIS mission) that must collapse to one before a downstream join
  - collapsing duplicates keyed on a normalized name without merging two genuinely-distinct same-named entities
  - an id field is only authoritative on some rows and null on others, so name + spatial proximity must be the fallback link
  - cluster/dedup output must be deterministic despite unstable upstream feature ordering (ArcGIS does not guarantee order)
---

# Swapping a rate-limited feed for a faster superset, and deduping its many-rows-per-entity shape

Shipped in commit `1fdcdd2` on `main` (`feat(ingest,firis)!: swap wildfire perimeter source WFIGS -> CAL FIRE/FIRIS combo feed`); follow-up place-matching fix in commit `7c2b873`.
Primary files: `internal/clients/firis/client.go`, `internal/ingest/wildfire.go`, `cmd/server/main.go`, `docs/firis-perimeter-source-design.md`, `internal/ingest/CLAUDE.md`.

## Context

The `wildfire` layer adopts a fire-perimeter polygon onto a CAL FIRE incident by a normalized-name match. The perimeter source used to be **NIFC WFIGS** — the interagency upload the National Interagency Fire Center hosts on ArcGIS. Two things made WFIGS a bad source:

1. **It lags real fires by hours.** WFIGS is a to-date interagency *upload*, not a live sensing feed. The Dove Fire already had a mapped perimeter on CAL FIRE's own public incident map (from a CAL FIRE Intel flight) while WFIGS still returned **zero** features for it (verified 2026-07-27, `docs/firis-perimeter-source-design.md`).
2. **It lives on NIFC's chronically 429-saturated ArcGIS org.** In fire season that org's per-org request-unit quota is hammered by every consumer of NIFC data, so the expensive feature query throttles exactly when fires are most active.

The replacement is the **CA Perimeters — CAL FIRE / NIFC / FIRIS public view** feature service (`CA_Perimeters_NIFC_FIRIS_public_view`), the same layer CAL FIRE's own public incident map draws from (`internal/clients/firis/client.go:1-16`). It:

- **Combines** CAL FIRE Intel remote-sensing + FIRIS IR-flight perimeters + WFIGS into one CA-wide layer — a strict **superset** of WFIGS, so replacing (not running both) is simplest and strictly more coverage (`docs/firis-perimeter-source-design.md`).
- Updates **~every 5 minutes** instead of hours.
- Lives on the **CAL FIRE-Forestry** org (`services1.arcgis.com/jUJYIo9tSA7EHvfZ`) — a *different, healthier* quota than NIFC's, and its metadata endpoint is CDN-cached `public, max-age=3600` (`internal/clients/firis/client.go:10-15`).

The one genuinely new problem the swap introduced: the combo feed carries **many rows per fire** — successive IR flights plus a FIRIS mission row — so the raw response has to be collapsed to one perimeter per fire *before* the existing name-join adoption runs, without accidentally merging two genuinely-distinct same-named fires.

The follow-up fix (`7c2b873`) is a second-order consequence of the swap: once the FIRIS feed gave the Dove Fire a *real perimeter polygon* (WFIGS never had), the store's place-matcher — which attached polygon events to any place whose *bounding box* merely overlapped — tagged the fire to three counties at a tri-county junction. That was replaced with an actual polygon-overlap test (`geojson.Intersects`). The lesson: giving an entity richer geometry can expose latent bugs in every downstream consumer that assumed the old, coarser shape.

## Guidance

Four reusable patterns came out of this work. Reach for them whenever you swap in, or add, an upstream feed.

### 1. Gate an expensive rate-limited query on a cheap cached metadata stamp

ArcGIS (and many feed services) expose a **metadata endpoint far cheaper than the data query** — here `FeatureServer/0?f=json` carries `editingInfo.dataLastEditDate` (the epoch-ms timestamp of the last edit to *any* row) and is CDN-cached for an hour, while the feature `query` is metered against the org's request-unit quota and can 429 (`internal/clients/firis/client.go:9-15`, `123-147`).

So don't run the expensive query every tick. Run the cheap one, and only run the expensive one when the stamp advanced past your last successful fetch:

```go
// internal/ingest/wildfire.go gatedPerimeters (condensed)
edit, err := n.firis.LastEdit(ctx)          // cheap, CDN-cached
if err != nil {
    // Metadata check failed (rare). Log it — a persistently-failing check
    // silently reverts to a fetch every tick — then fall back to a direct
    // fetch so a metadata hiccup never *blocks* perimeters. Never worse than
    // pre-gating.
    logging.Warnw(ctx, "FIRIS metadata check failed; falling back to a direct perimeter fetch", "error", err)
    return n.firis.GetPerimeters(ctx, b)
}
if n.havePerimCache && edit.Equal(n.lastPerimEdit) &&
        n.now().Sub(n.lastPerimFetch) <= maxPerimCacheAge {
    return n.cachedPerims, nil               // unchanged + fresh — skip the origin query
}
perims, perr := n.firis.GetPerimeters(ctx, b) // stamp advanced (or cache stale) → fetch
```

Two guardrails make this safe rather than merely clever:

- **A max-cache-age safety valve** (`maxPerimCacheAge = 6h`, `wildfire.go:48-52`). A stamp that stalls upstream — a stuck editor, a CDN pin — must not silently freeze the map forever, so the last-good set is force-refetched after the valve regardless of the stamp.
- **The fallback-on-metadata-failure is explicitly logged** (`wildfire.go:248-257`). A persistently failing cheap check quietly disables the gating (you're back to fetching every tick); if that's invisible you'll never notice the quota protection evaporated.

Crucially, a stamp-unchanged tick returns a **genuine success**, not a suppressed one — the data really hasn't changed, so the disappearance sweep can legitimately act on the cached set.

Caveat worth stating honestly: `dataLastEditDate` here is a **statewide** stamp, so in peak fire season it advances most ticks (a fire changed *somewhere* in CA) and the gate mainly saves the query off-season; and the metadata is CDN-cached ~1h, so perimeter freshness is bounded nearer ~1h than the 5-min poll. Both are still far better than WFIGS, but the gate's headline savings are season-dependent — the durable win is moving to a healthier-quota org.

### 2. Dedup a many-rows-per-entity feed by a stable id when present, with DETERMINISTIC fallback clustering

The combo feed has multiple rows per fire and **no single id field that works alone** (measured on the live feed, `docs/firis-perimeter-source-design.md`): `incident_number` is a stable per-fire uuid but is null on FIRIS mission rows (149/254 null); `incident_name` is null on the FIRIS mission rows too (110/254). The only thing linking a fire's CAL FIRE Intel rows to its FIRIS rows is the **name** (`incident_name` when present, else parsed from the mission id).

The dedup pipeline (`dedupePerimeters`, `wildfire.go:306`):

1. **Derive a name for every row**, drop rows with neither a name nor drawable geometry.
2. **Group by normalized name**, preserving first-seen order.
3. **Cluster within a name into per-fire groups** (`clusterByCentroid` + `sameFire`) so two distinct same-named fires stay separate.
4. **Keep the freshest row per cluster** (`perimFresher`).

The identity rule (`sameFire`, `wildfire.go:404`) uses the stable id when both rows have one, and falls back to spatial proximity only when at least one is absent:

```go
func sameFire(a, b perimCandidate) bool {
    ai, bi := a.perim.IncidentNumber, b.perim.IncidentNumber
    if ai != "" && bi != "" {
        return ai == bi          // shared uuid = one fire; different uuids = never merge
    }
    return centroidDistSq(a.geom, b.geom) < perimClusterThresholdSq  // ~0.15° ≈ 15 km
}
```

**The determinism is the load-bearing part.** ArcGIS does not guarantee stable feature order across polls, and the greedy single-pass clustering's output *count* can depend on input order. So the group is sorted into a canonical order — id-bearing rows first, then by id, then by centroid — before clustering (`clusterByCentroid`, `wildfire.go:364`), and the query also asks for `orderByFields=OBJECTID` as belt-and-suspenders (`client.go:89`). Sorting id-bearing rows first also guarantees each cluster's *representative* (element `[0]`, the one `sameFire` compares against) carries an `incident_number` whenever any member has one, so the "different uuids never merge" split is reliable.

The representative pick is a **total order** so it's deterministic under ties (`perimFresher`, `wildfire.go:416`): latest `poly_DateCurrent`, then Active over Inactive, then source priority (CAL FIRE Intel > FIRIS > WFIGS), then larger acreage.

### 3. Treat an empty-but-successful upstream response as non-authoritative

A working combo feed with *any* active fire in-bbox returns at least one perimeter, so a zero-feature HTTP 200 is far more likely a transient ArcGIS glitch (backend load-shedding, a momentarily-empty spatial index) than every fire's perimeter vanishing at once. Two mechanisms encode that (this is the ingest fail-loud posture, `internal/ingest/CLAUDE.md`):

**Never cache an empty set as last-good** (`gatedPerimeters`, `wildfire.go:269`):

```go
if len(perims) == 0 {
    // Return it for THIS tick, but leave the last-good cache + stamp untouched
    // so a good set is never overwritten by empty and the next tick re-fetches.
    return perims, nil
}
n.cachedPerims, n.lastPerimEdit, n.lastPerimFetch, n.havePerimCache = perims, edit, n.now(), true
```

**Carry adopted geometry forward on a wholesale-empty deduped set** (`Poll`, `wildfire.go:146`, `183`). `perimsUnusable := perr != nil || len(deduped) == 0` treats a wholesale-empty result exactly like a hard outage: an incident that held a perimeter last tick keeps its **prior geometry and `has_perimeter`** rather than being downgraded to a bare point — which would write a false "perimeter gone" revision across the whole map and throw away real spatial extent. Scalar fields (acres, containment, headline) still update from the sibling CAL FIRE feed, because those are genuine revisions.

The asymmetry is deliberate and important: a **non-empty** feed that simply omits *one* fire **is** authoritative — that one fire genuinely downgrades. Only *wholesale* emptiness is treated as an outage.

### 4. Retire a renamed source's orphaned events + registry row at boot

The disappearance sweep only runs for a *live* poller's `SourceIDs()`. So when you rename a source (`wfigs` → `firis`), the old ACTIVE standalone `wfigs:` events already in the store have no poller to sweep them — they'd persist ACTIVE forever as stale duplicates beside the fresh `firis:` events. A one-time boot migration fixes it (`retireOrphanedSources`, `cmd/server/main.go:272`):

```go
var retiredSourceIDs = []string{"wfigs"}

func retireOrphanedSources(ctx context.Context, st *store.Store) {
    for _, src := range retiredSourceIDs {
        evs, _ := st.ActiveEventsBySource(ctx, src)
        // ... TransitionEvents(ids, EXPIRED, now)  — a proper recorded revision, history kept
        // ... st.DeleteSource(ctx, src)            — so /api/v1/sources stops listing it
    }
}
```

It's **idempotent** (once drained, `ActiveEventsBySource` returns none and the delete is a no-op) and **best-effort** (a failure logs and continues — it must never block startup). The `retiredSourceIDs` entry can be dropped a deploy cycle later once it has drained everywhere.

## Why This Matters

Each pattern averts a concrete, already-observed failure:

- **Stale fire perimeters** on a life-safety map — the original reason for the swap (WFIGS lagged the Dove Fire by hours).
- **429 storms** that throttle the perimeter query in peak fire season — avoided by both moving off NIFC's saturated org *and* gating the expensive query behind the cheap cached stamp, cutting origin calls to only when data changed.
- **Phantom duplicate events** — non-deterministic clustering would flap the cluster count across polls, which flaps the standalone `-2`/`-3` id suffixes, which mints and expires duplicate fire events on every tick. Deterministic sorting kills this.
- **Dropping a real perimeter** — naive spatial-only clustering would collapse two same-named fires ~10 km apart into one, silently discarding a real fire's perimeter. The "different non-empty uuids never merge" rule prevents it.
- **False "gone" revisions** — a transient empty response, if trusted, would write a fabricated "perimeter gone" revision across every fire on the map and cache the empty state for replay. The non-authoritative-empty handling prevents both.
- **Orphaned ACTIVE events** — a renamed source's leftovers would linger ACTIVE forever with no sweep to retire them, showing stale duplicates in `/api/v1/sources` and on the map.
- (Follow-up) **Mis-attributed places** — richer geometry surfaced a latent bbox-overlap bug that tagged one fire to three counties.

## When to Apply

Reach for these patterns when you are **swapping or adding an upstream feed** and any of the following hold:

- **The feed is rate-limited / quota-metered** and exposes a cheaper metadata or last-modified endpoint → gate the expensive query on it (pattern 1). Also worth checking: is the *org / tenant* the feed lives on itself saturated? Moving to a healthier-quota host of the *same data* can matter as much as caching.
- **The feed returns multiple rows per logical entity** (successive observations, versions, sub-parts) → dedup to one-per-entity by a stable id when present, spatial/name heuristics as fallback, and make the clustering **deterministic** so downstream counts and ids don't flap (pattern 2).
- **The feed's emptiness is ambiguous** — a working feed almost never returns zero for a live situation → treat empty-but-successful as non-authoritative: never cache it, carry prior state forward, but keep *partial* omissions authoritative (pattern 3).
- **You are renaming or removing a source id** whose events live in a store swept per-live-poller → add a one-time, idempotent boot retirement of the orphaned events and registry row (pattern 4).
- **You are giving an entity richer geometry than before** (point → polygon) → audit every downstream consumer that may have assumed the coarser shape (the `7c2b873` place-matching bug — see Related).

## Examples

**The `incident_number`-aware `sameFire` rule** (`wildfire.go:404`). Two `DOVE` CAL FIRE Intel rows share one `incident_number` → merged even though successive flights' centroids drifted apart. A FIRIS `CA-TCU-DOVE-N57B` mission row has a null id → falls back to centroid proximity, co-located, joins the same cluster. Two *different* same-named fires ~10 km apart with distinct non-empty ids → never merge, so neither perimeter is dropped. Worked end-to-end (`docs/firis-perimeter-source-design.md`): three `dove` rows collapse to one cluster, latest `poly_DateCurrent` (CAL FIRE Intel 225 ac) wins, and the single `dove` perimeter adopts cleanly onto the `calfire:` Dove incident.

**The wholesale-empty carry-forward** (`wildfire.go:146`, `183`). `perimsUnusable := perr != nil || len(deduped) == 0`. On a zero-feature success, each incident that had `hasPerimeter` last tick reuses `pe.GetGeometry()` and stays `hasPerimeter=true`, while acres/containment/headline still update from CAL FIRE — no false "perimeter gone" revision, no lost extent. Standalones need no equivalent guard: their `expire` disappearance grace already absorbs a transient empty.

**The `nameFromMission` parse** (`wildfire.go:463`). A FIRIS mission id `CA-<UNIT>-<NAME…>[-<FLIGHTID>]` yields the fire name by dropping the `CA` + unit tokens and a trailing flight-id token (`N57B`/`N50X`/`N42Z` — an `N` followed by alphanumerics with at least one digit): `CA-TCU-DOVE-N57B` → `DOVE`, `CA-FKU-PARAMOUNT` → `PARAMOUNT`. The subtle case is a mission that is *only* unit + flight id (`CA-TCU-N57B`): after dropping tokens a **lone flight-id token** remains, which names the aircraft, not a fire — so the row is treated as unattributable and returns `""` rather than minting a phantom fire named `N57B` (which would also merge two of that plane's unrelated unnamed missions).

## Related

- `../logic-errors/polygon-event-place-matching-bbox-overattach.md` — the follow-up place-matching bug this swap exposed. Once a fire gets a real perimeter polygon, correct place attachment depends on that geometry fix.
- `../../firis-perimeter-source-design.md` — the full design (source details, the measured field-null rates, the dedup worked example).
