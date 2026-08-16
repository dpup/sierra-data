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

### A fifth case: the source that fails by FREEZING (`power`)

Every mechanism above keys off a fetch that *failed*. The PG&E outage feed can
fail without failing: its ETL stalls while the endpoint keeps answering 200 with
the last set, so a restored outage stays listed and a new one never appears.
Nothing in a successful fetch reveals this. (It is not hypothetical — the Cal
OES statewide mirror of this same data was measured 26 h stale while reporting
every row as `Active`.)

PG&E publishes its own ETL stamp, so `PowerNormalizer.freshnessError` compares
it to now and, past `grid.power.outageStaleAfter`, records the `pge` source as
**failing for the tick** — a `PerSource` error, not `SweepSuppress`. That choice
is deliberate and both halves matter: `PerSource` skips the sweep *and* the
`TouchSeen` refresh *and* degrades health, which is right, because unlike the
wildfire case the fetch is not honestly healthy — the data behind it is a day
old and `/api/v1/sources` must say so. The events still upsert: last-known data
is the best available, and `DegradeStoreStatus` serves them as STALE rather than
disowning data the response carries.

Two boundaries to preserve:

- **The gate is outage-only.** The PSPS service's stamp legitimately sits idle
  for weeks between shutoff events (observed a month stale with zero active
  rows), so gating it would permanently flag a healthy source.
- **It fails OPEN when the stamp itself is unreadable.** The gate is an EXTRA
  signal layered on an already-successful fetch; losing it leaves us exactly
  where every other source here already sits (none publish an ETL stamp at all),
  whereas failing the source on a flaky metadata table would flap the layer for
  no gain in truth.

### Corollary: an event id may only be built from IMMUTABLE fields

Under `resolve`, the id IS the lifecycle handle. If a poller derives an id from a
field the upstream mutates, the next poll emits a different id, the old one is
"missing from a successful poll", and the sweep RESOLVES it — a fabricated
all-clear plus a history that restarts from scratch.

`pspsGroupKey` is the worked example. PSPS rows carry `Stage`, which is mutable
*by design* — Watch escalating to Warning is the single most important thing
this feed reports — and `DeEngEnd`, which PG&E revises as a shutoff runs long.
Keying on either meant a shutoff read as CANCELLED at the moment it got more
serious. The key is `EventID:TimePeriod` (PG&E's own stable identifiers), and
where those are missing it deliberately **collapses** rather than reaching for a
mutable field: under-reporting how many windows a shutoff has is recoverable, a
fabricated all-clear is not. `TestPowerPoll_PSPSIDSurvivesStageEscalation` pins
this.

The same rule covers geometry, which is hashed: `combineGeometry` sorts its
members because ArcGIS promises no row ordering, and an order flip would
otherwise mint a revision on an event that never changed.

### The other direction: upstream staleness is surfaced, never acted on

The freeze gate above degrades a SOURCE. The evacuation layer has the same
problem one level down — a single ORPHANED ROW, a zone the county lifted that
Cal OES never retracted — and it is handled deliberately differently.

`warnIfOrphaned` logs it and `observed_at` carries the row's true age, but the
event stays **ACTIVE**. Cal OES still lists the zone, and `caloes` is a `resolve`
source: retiring the event would publish an all-clear that no authority issued,
off nothing but our inference about upstream's bookkeeping. That is the fail-loud
invariant read in the direction people forget — it forbids fabricating an
all-clear from OUR failure, and equally from our guess about THEIRS.

The rule of thumb: a freshness signal may degrade a source's health and suppress
a sweep, and it may make an event's age visible. It may not, on its own, end a
life-safety event.

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

## A tick only writes what changed (`shouldUpsert`)

The tick does **not** call `UpsertEvent` for every polled event. It calls
`NeedsUpdate` (a lock-free, transaction-free `SELECT content_hash`) and skips
events whose content is unchanged.

**Why.** `UpsertEvent` always opens a transaction, even when it writes nothing.
On the mesh poller that meant ~194 transactions every 60s where the great
majority were no-ops — mesh telemetry (SNR/RSSI/hops/path) is zeroed out of the
content hash by design, so a node merely re-advertising is hash-equal. Each
no-op still paid `BEGIN` + 3 `SELECT`s + `COMMIT`, and on EFS every one of those
is a network round trip. Measured in production: the tick's write phase spanned
7-9 seconds, and because a rollback journal has no WAL MVCC, readers blocking on
the writer's EXCLUSIVE commit saw 1.7-3.5s spikes on ~7% of `/api/v1/events`
requests, plus the occasional 503.

**Three cases still take the write path, and all three are load-bearing:**

- **`fullReconcile`** — see below.
- **A carried `Enhancement`.** `enhancement` and `summary` are EXCLUDED from the
  content hash, so the hash-equal upsert (`refreshEventPlaces`' `enhChanged`
  branch) is the ONLY thing that persists them. Road incidents arrive already
  enhanced from the `RoadsService` pipeline and are routinely hash-equal —
  skipping those would silently drop AI text that had just been regenerated.
  (Weather alerts are safe either way: `maybeEnhance` only sets `Enhancement`
  when the content changed.)
- **A failed `NeedsUpdate`.** Fail TOWARD doing the work. A check that errors
  must never silently skip a write; being wrong costs one transaction we would
  have paid anyway.

### `fullReconcile` — why place attachments still work

Place attachments are DERIVED state, recomputed by the hash-equal upsert path.
Skipping that path would mean an event which arrived BEFORE a place was seeded
never attaches to it — vanishing from that place's map and summary permanently,
with nothing in the logs. So `Store.PlacesVersion()` increments whenever the
place set changes (same mutex, same place as the `placesGeoValid` invalidation),
and a tick that sees a new version upserts **every** event once. A poller's first
tick also reconciles, so a restart re-derives attachments.

Cross-tick state lives in `pollerState`, created in `run` and never shared, so it
is goroutine-local by construction — **do not hoist it onto `Scheduler`**, which
every poller shares. `TestTickReconcilesWhenPlacesChange` pins this and fails if
the version check is removed.

### `TouchSeen` is coalesced — and the cache-invalidation model behind it

The read-latency work of 2026-08 converged on one mechanism wearing different
costumes: **cold reads over EFS**. Warm SQLite page caches serve a place query
in ~60 ms; any commit makes every other connection discard its cache wholesale,
and the changed pages come back over the network. The per-tick `TouchSeen` —
rewriting `last_seen_at` for the entire polled set, ~400 blob-carrying rows
every 60 s — was the standing invalidator: the first read after each tick paid
1.7-3.5 s (~7% of requests, clustered on the tick cadence). Shortening the tick
16x (the skip gate + batched pre-check) did not move that rate, because
invalidations-per-tick was unchanged — the tell that it was never lock
contention (commits measured 0.3-1.4 s, too short to explain 3 s waits).

So the scheduler passes `TouchSeen` a staleness cutoff (`touchSeenCoalesce`,
10 min): only rows whose stamp is older get rewritten, and a tick where nothing
is stale commits nothing at all. One ~400-row burst per window replaces sixty
per hour. The stamp may lag the last confirmed appearance by up to the window —
bounded, and safe because it feeds graces measured in hours (smallest: 2 h
meshcore; keep the window far below the smallest `expireAfter`). The write-phase
log's `touched` field shows it working: most ticks log `touched=0`.

If per-tick spikes survive this, the remaining every-tick committer is the
mesh-observations insert; the escape hatch is moving `mesh_observations` (pure
derived telemetry) into a separate/ATTACHed database file with its own change
counter, so its writes stop invalidating the events cache.

### The write-phase log line

Every tick logs `Ingest tick: write phase` with `events / upserted / skipped /
fullReconcile / priorMs / pollMs / upsertMs / touchSeenMs / touched /
observationsMs / observations / sweepMs / healthMs / totalMs` — the whole tick,
loadPrior through recordAttempt. (The first version timed only the middle and
reported ~600 ms while the tick owned multi-second windows; measure everything
or the next hypothesis is a guess.) This
exists because the cost was previously observable only from OUTSIDE the process,
as latency on unrelated reads — a slow tick showed up as p99 on `/api/v1/events`
with nothing in the logs connecting the two. `skipped` is the headline: those are
transactions not opened.

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

## Weather-alert headline: deterministic, never AI

`nws.Alert.ShortHeadline` composes `<Event> — <reason>` from the product name
and the reason clause in `parameters.NWSheadline`. CAP's own
`properties.headline` is issuance boilerplate — its every token is already
`category`, `effective`, `expires` and `provenance.sourceName` — so shipping it
verbatim repeated the record back at the reader in four places.

**Do not move this to the enhancer.** `store.ContentHash` zeroes `Enhancement`
and `Summary` but *not* `Headline`, so an AI headline would differ from the
normalizer's on every tick: `NeedsUpdate` would fire forever (288 calls/day/alert
against a `budgetPerTick` of 5) and each wording drift would mint a revision.
Deterministic composition also makes it structurally impossible for a model to
render a Watch as a Warning — the product name is copied from `Event`.

Both consumers use it: the event card (`weather_alert.go`) and the fire-weather
banner (`nws.fireWeatherFromAlert`, which does not go through the store).

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
- **The summary is a regional summary, capped at 2 sentences / 320 characters.**
  Policies 4-7 of `nwsSystemPrompt` exist because the unbounded version produced
  865-character summaries whose bulk was a roster of out-of-area forecast zones
  and a restatement of the timestamps the card already shows. It must not repeat
  the headline it sits under, name zone identifiers, or state the office or the
  times — all of those are typed fields on the same record. Prompt policy cannot
  be unit-tested; `TestNWSEnhancerLive` (skipped unless `NWS_ENHANCE_LIVE=1`)
  runs the real prompt against a real product and asserts these.

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

Power is the explicit counter-example: an outage or shutoff outside the
footprint is *not* a threat to it (the grid does not move), so `power.go` uses
the bare `unionBounds`. Don't generalize the wildfire margin to new layers.

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
