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
  span several (wildfire → `calfire`+`wfigs`; road incidents → `chp`+`caltrans`).
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

## Per-source disappearance policies (prefab.yaml `grid.sources`)

- **`resolve`** — the feed is authoritatively active-only (Cal OES, CAL FIRE, CHP,
  Caltrans). Missing from a good poll ⇒ RESOLVED immediately.
- **`expire`** — the feed going quiet proves nothing (NWS alerts drop at
  end-of-product, WFIGS perimeter uploads lag). EXPIRED only once past the event's
  own `expires`, or past the `expireAfter` grace since it was **last seen**;
  otherwise it stays active.

Either way a **failed** poll transitions nothing (mechanisms above). Every
transition is a recorded revision. Grace is anchored to `last_seen_at`
(`shouldExpire`), not `observed_at`.

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
   provenance so `/v1/sources` and event provenance agree.
5. **Projection** (`internal/gridapi/project.go`): add a `case` to `ProjectEvents`
   producing the exact shipped envelope; if it's a map layer, add it to
   `eventLayers` and `layerSourceIDs` in `internal/gridapi/maplayers.go`.
6. **Docs**: `site/docs.html` (public `/v1` reference) and a `CHANGELOG.md` entry.

Per the spec, that's the whole surface — a new poller shows up in summary domains,
`/v1/events`, and the map namespace automatically; no new endpoints.
