# Ingest (poller scheduler + normalizers)

Normalizes upstream hazard feeds into canonical `grid.v1.Event`s and drives them
into the store with per-source health and lifecycle. One goroutine per poller
(jittered start, panic-recovered, ticks on the poller's interval). Each normalizer
reproduces the **shipped `/api/v1/hazards` envelope semantics** — id namespaces,
headline formats, severity mappings (delegated to `internal/hazards`' exported
helpers) — so the store→GeoJSON projection (`internal/gridapi.ProjectEvents`)
stays byte-compatible with the live builders. Design:
`docs/v2-implementation-plan.md` Tier C.

## The Normalizer / Prior / PollResult contract

```go
type Normalizer interface {
    SourceIDs() []string                         // source-registry rows this poller updates (poller ≠ source)
    Poll(ctx, prior Prior) (*PollResult, error)
}
```

- **`SourceIDs`** — the source rows this one poller writes health for. A poller may
  span several (wildfire → `calfire`+`firis`; road incidents → `chp`+`caltrans`).
- **`Prior`** — a read-only view of the store's current ACTIVE/SCHEDULED events for
  this poller's sources (`ByID`, `ForSource`), built by the scheduler before each
  tick. Normalizers use it to keep **identity/state stable across ticks** — e.g.
  wildfire keeps a joined fire's id stable while one sibling feed is momentarily
  down. It is never nil when the scheduler calls; the impl is nil-safe for tests.
- **`PollResult{Events, PerSource, SweepSuppress}`**:
  - `Events` — the **full current set** for this poller's scope (that's what the
    disappearance diff is against; a partial set is a lie the sweep will act on).
  - `PerSource` — per-source partial failures: a source that failed while a sibling
    succeeded (its events still returned). Nil/absent = success.
  - `SweepSuppress` — sources that fetched cleanly but whose full current set could
    not be computed this tick (see below).
  - `Superseded` — the **inverse** of `SweepSuppress`: ids the poller *proves* are
    gone because it knows the successor (see below).

## THE fail-loud sweep invariant

> A failed **or incomputable** fetch must NEVER resolve or expire events.

"Missing from the feed" is only meaningful against a *successful, complete* poll of
that source. This is the same life-safety posture as the evacuation layer — an
error must never become an all-clear. Four mechanisms enforce it; keep all four:

1. **Hard `Poll` error** → the scheduler records the failure for every covered
   source and ends the tick. No upserts, no sweep.
2. **`PerSource[src] != nil`** → that source's disappearance sweep is skipped (its
   absence proves nothing while its fetch failed), but the tick still processes the
   sources that succeeded.
3. **`SweepSuppress`** → a source that fetched cleanly yet cannot prove
   disappearance (wildfire can't compute the standalone-perimeter set while the
   sibling CAL FIRE feed is down) is skipped by the sweep, **but `RecordAttempt`
   still records its success** — health and lifecycle are deliberately separate.
4. **`errEmptyScope`** → an empty configured scope (no hazard/incident areas) is a
   **hard Poll error**, never a success-empty `PollResult`. A "successful" empty
   poll would let the sweep RESOLVE every stored active event (a fabricated
   all-clear written into history) and mark the source healthy — all from a config
   regression, with no fetch ever made.

Corollary: `Events` is the whole truth for the scope, or you must fail/suppress.
Never return a partial `Events` as if complete.

## `Superseded` — the one way to skip the grace, and why it is still fail-loud

`SweepSuppress` says *"I can't prove this is gone."* `Superseded` says the
opposite: *"I can prove it, and here is what replaced it."* Those ids transition
to **RESOLVED immediately**, ignoring the source's disappearance policy.

This does not weaken the invariant above, because the invariant is about
**absence being ambiguous**. The `expire` grace exists for a perimeter whose
upload lagged or an alert that dropped at end-of-product — cases where "missing"
might just mean "not re-listed yet". When the poller can name the successor,
missing is not ambiguous, and holding the old id ACTIVE for the grace just draws
the same hazard twice.

Two rules keep it honest, and both are load-bearing:

- **Positive evidence only.** Populate `Superseded` from something you observed,
  never from "I didn't see it" — that is what the sweep is for. The only producer
  today is wildfire: a perimeter that was standalone (`firis:<name>`) and is now
  **adopted** by a CAL FIRE incident (`calfire:<uuid>`). The perimeter is *still
  in the feed*; we know precisely which event absorbed it
  (`supersededStandalones`).
- **Same guard as the sweep.** The scheduler only supersedes for sources whose
  fetch succeeded and wasn't suppressed, so a failed fetch still transitions
  nothing. And it is deliberately *narrow*: wildfire names only the id this exact
  candidate would have been emitted under (`standaloneContinuityID`). A sibling
  cluster that genuinely dropped out of the feed keeps its grace — absence is
  still ambiguous for that one.

Without this, every standalone→adopted transition (CAL FIRE adding a fire to its
curated list, or a scope change bringing the incident in) shows the fire twice
for the full 24h `firis` grace.

## Per-source disappearance policies (prefab.yaml `grid.sources`)

- **`resolve`** — the feed is authoritatively active-only (Cal OES, CAL FIRE, CHP,
  Caltrans). Missing from a good poll ⇒ RESOLVED immediately.
- **`expire`** — the feed going quiet proves nothing (NWS alerts drop at
  end-of-product, FIRIS/CAL FIRE perimeter uploads lag). EXPIRED only once past the event's
  own `expires`, or past the `expireAfter` grace since it was **last seen**;
  otherwise it stays active.

Either way a **failed** poll transitions nothing (mechanisms above). Every
transition is a recorded revision. Grace is anchored to `last_seen_at`
(`shouldExpire`), not `observed_at`.

## Push sources wrapped as pollers (MeshCore)

`network.go` (the `meshcore` source) is a **push** source fitted to this pull
model: a long-lived MQTT subscriber (`internal/clients/meshcore.Registry`)
accumulates node state in memory, and `NetworkNormalizer.Poll` returns a snapshot
of recently-heard, in-region nodes on each tick. This keeps single-writer
discipline, tick-based health, and the disappearance sweep unchanged. Two
mesh-specific rules:

- **All brokers down ⇒ hard `Poll` error.** An empty snapshot from *our* outage
  must never read as "every node left the mesh" (fail-loud invariant). A mesh has
  no goodbye packet, so the source uses `disappearance: expire` with a multi-day
  `expireAfter`; genuine silence expires a node, our downtime does not.
- **Volatile telemetry stays out of the content hash.** SNR/RSSI/hops/gateways
  ride in `NetworkDetail.telemetry`, which `store.ContentHash` zeroes — so the
  advert firehose refreshes `last_seen_at` without minting a revision. Only a
  node's identity, role, name, location, or status change writes history.

## Enhancement budget + carry-forward

Only the `WEATHER_ALERT` layer is enhanced here (road incidents arrive already
AI-enhanced from the `RoadsService` pipeline — do not re-enhance them).
`maybeEnhance`:

- No-op when the enhancer is nil (no OpenAI key / `grid.enhancement.enabled:
  false`) or the per-tick budget is exhausted — enhancement never gates ingest; a
  raw alert is always served.
- **`NeedsUpdate` gates the spend**: unchanged content keeps its stored `summary`
  (a hash-equal no-op upsert) and costs nothing. Because `summary`/`enhancement`
  are excluded from the content hash, they carry forward across polls untouched —
  this is exactly the "no per-poll regeneration" property.
- **Budget counts attempts, not successes** (`*budget--` before the call), so a
  failing enhancer can't loop the API within a tick. Alerts deferred by an
  exhausted budget are picked up on their next content change.
- Enhancement failure is log-and-continue (serve raw); the enhancer may localize
  only against the event's attached place **names** (grounding, not a requirement).

## Wildfire has its own, wider geography

Every other spatial poller (earthquake, evacuation) fetches over `unionBounds`
— the bare union of `hazards.areas[].bounds`. **Wildfire does not.** It uses
`wildfireScope`, that union grown by `grid.wildfire.marginDegrees` (default
0.5° ≈ 55 km), which puts the fire box just outside the CHP/Caltrans incident
box. The reason is asymmetric: every other hazard only matters where it
happens, but a fire *outside* the coverage footprint is a threat *to* it — it
moves, it closes the roads out, and an hour of warning changes what people do.
Scoping fire to the footprint meant a fire on the edge was invisible until it
crossed the line. Don't "simplify" it back onto `unionBounds`.

Two consequences to preserve when touching `wildfire.go`:

- **Geometry is resolved BEFORE the in-scope test**, because `inWildfireScope`
  consults it. CAL FIRE publishes one origin point per incident; a large fire's
  FIRIS perimeter reaches far beyond it, so a point-only test drops precisely
  the fire burning into the region — and drops the acreage/containment/URL that
  only the CAL FIRE row carries.
- **Testing the *published* geometry (not the freshly-adopted perimeter) is what
  keeps scope stable across a FIRIS outage.** On an unusable perimeter set the
  prior polygon is carried forward, which keeps a perimeter-only fire in
  `Events`. If it silently dropped out instead, the disappearance sweep would
  RESOLVE it — a fabricated all-clear, the exact failure the sweep invariant
  above exists to prevent. Scope is part of "the whole truth for the scope".

The matching store-side rule (a fire attaching to an area/town place it is
merely *near*) lives in `store.matchPlaces` — see `internal/store/CLAUDE.md`.

## Adding a poller

1. **Proto**: add a `Layer` enum value and a `<Kind>Detail` message to
   `api/grid/v1/grid.proto`; `make proto` (deterministic, committed).
2. **Normalizer**: `internal/ingest/<layer>.go` implementing `Normalizer`. Map to
   the shipped envelope — reuse `internal/hazards` severity/name helpers (or pin
   equivalence with a test), set the id namespace, geometry via
   `GeometryFromGeoJSON`/`GeometryFromPoint`, provenance via `NewProvenance` with
   per-source name/attribution constants that match the shipped `Source` block.
3. **prefab.yaml**: add a `grid.sources.<id>` entry (poll interval + `disappearance`
   policy, optional `expireAfter`).
4. **Seed registry + wiring** (`cmd/server/main.go`): add the source id to
   `gridSourceInfo` (display name + attribution) and add a `PollerSpec` to the
   scheduler's `Pollers` list. The registry constants must match the normalizer's
   provenance so `/api/v1/sources` and event provenance agree.
5. **Projection** (`internal/gridapi/project.go`): add a `case` to `ProjectEvents`
   producing the exact shipped envelope; if it's a map layer, add it to
   `eventLayers` and `layerSourceIDs` in `internal/gridapi/maplayers.go`.
6. **Docs**: `site/docs.html` (public `/api/v1` reference) and a `CHANGELOG.md` entry.

Per the spec, that's the whole surface — a new poller shows up in summary domains,
`/api/v1/events`, and the map namespace automatically; no new endpoints.
