# Ingest (poller scheduler + normalizers)

Normalizes upstream hazard feeds into canonical `grid.v1.Event`s and drives them
into the store with per-source health and lifecycle. One goroutine per poller
(jittered start, panic-recovered, ticks on the poller's interval). Each normalizer
reproduces the **shipped `/api/v1/hazards` envelope semantics** — id namespaces,
headline formats, severity mappings (delegated to `internal/hazards`' exported
helpers) — so the store→GeoJSON projection (`internal/gridapi.ProjectEvents`)
stays byte-compatible with the live builders. Design:
`docs/design/v2-implementation-plan.md` Tier C.

## The Normalizer / Prior / PollResult contract

```go
type Normalizer interface {
    SourceIDs() []string                         // source-registry rows this poller updates (poller ≠ source)
    Poll(ctx, prior Prior) (*PollResult, error)
}
```

- **`SourceIDs`** — the source rows this one poller writes health for. A poller may
  span several (wildfire → `calfire`+`firis`; PG&E → `pge`+`psps`). The reverse
  is forbidden: **a source has exactly one poller.** Two pollers sweeping one
  source against different event sets resolve each other's events every tick.
  That is why road incidents cover `chp`+`caltrans` only while closures still
  come from `lcs2way.kml`, and `chp` alone once CWWP2 owns `caltrans`.
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

**A restart is not a change of source.** `meshProvenance` derives the hashed
`attribution` and `source_url` from the brokers in the current snapshot, and
`Registry.Seed` rehydrates everything about a node except its broker set — so
every deploy re-attributed each not-yet-re-heard node to nobody, then back when
it next adverted. Measured across 45 live nodes: 505 attribution flips, 11.2 per
node, 26 inside one minute. `keepPriorAttribution` carries the two naming fields
forward when this tick learned no broker; `fetched_at` stays fresh, because we
did observe the node, we just cannot say through whom.

**GPS noise is not movement.** A mesh node's geometry is hashed (movement is
meaningful), and a node's self-reported fix wanders tens of metres between
adverts while it sits still — so every wobble was a revision, and one stationary
companion reached revision 75 that way. `stablePosition` keeps the STORED
geometry, byte for byte, until an advert lands more than
`meshPositionEpsilonMeters` (150 m) from it.

Quantization was the first attempt and cannot work: the jitter is wider than any
sane grid (245 m across 18 positions on one still node, against an 11 m grid),
and rounding has no hysteresis — a node parked on a cell boundary flips forever
however coarse the cells are. A threshold measured from the last stored position
does have hysteresis, the same shape as the reachability window's: noise changes
nothing, a real move registers once. `quantizeCoord` stays, but as an output
precision convention, not a damping mechanism.

**The event holds the latest reading; the archive holds all of them.** A
monitor's sample rides on the event (hash-excluded, so it mints no revision) AND
is appended to `mesh_telemetry` via `PollResult.MeshTelemetry`, batch-inserted by
the scheduler in the same writer context as `MeshObservations`. The projection
runs over the tick's EVENTS rather than the raw reports, because that is where a
key prefix has already been resolved — filing one node's samples under two keys
is the failure mode, and `PollResult.MeshTelemetryRenames` (paired with
`Superseded`) is what carries the history across a promotion. Only the archive
can be graphed; see `internal/store/CLAUDE.md`.

**A missing shape is not a smaller shape.** PG&E's polygon layer periodically
answers with zero rows (200, no error envelope — see `internal/clients/CLAUDE.md`),
which left every outage redrawn as its own centre point and redrawn back on the
next poll: 25 revisions on one 7-customer planned outage in an afternoon, none of
them news, each one also flipping its corridor place attachment. `GetOutages`
now reports that as `pge.ErrPolygonLayerBlank` — a PARTIAL failure, so the source
degrades and its sweep is skipped while its events still land — and
`attachOutageGeometry` carries each outage's last-known footprint forward rather
than believing the blank.

This is the **wildfire perimeter rule, second instance** (see the FIRIS section
below): a wholesale-empty response from a feed that should have returned
something is a glitch, and carrying the prior polygon is how we decline to
publish a downgrade we do not believe. A NON-empty response that omits one row is
authoritative in both places — that one genuinely has no shape this poll. If a
third source needs this, it is a pattern and not a coincidence; give it a name
before copying it a third time.

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

## The mesh poller has TWO inputs, and the rules that keeps honest

Since 2026-09 the same normalizer also merges reports pushed by operator-run
monitors (`internal/pushingest`, `POST /api/v1/ingest/mesh.repeater`) — a
Raspberry Pi that logs into repeaters and reads their admin interface. The two
inputs are not symmetrical and the asymmetry drives every rule below:

| | MQTT adverts | Operator monitor |
|---|---|---|
| Evidence | positive only — silence is ambiguous | positive AND negative ("I tried and failed") |
| Identity | signed, self-declared, with location | a name and a pubkey PREFIX, no coordinates |
| Carries | signal metrics a listener observed | battery/airtime/counters only the node knows |

**One node is one event.** A monitor identifies a node by a prefix of its public
key (8 bytes in practice; our ids are the full 32). `Poll` resolves that prefix
against the catalog of full keys — this tick's adverts, plus the store's existing
mesh events — on a UNIQUE-match rule, the same rule `meshcore.resolvePath` uses
for relay hops. Attaching a repeater's battery reading to the WRONG repeater is
worse than leaving it unattached, so an ambiguous prefix resolves to nothing and
keeps its own `meshcore:<prefix>` id. When an advert later supplies the full key,
`supersededPrefixIDs` retires the prefix event via `Superseded` — positive
evidence naming the successor, the same shape as a standalone fire perimeter
being adopted by a CAL FIRE incident.

**Precedence is by input class, never by recency.** Identity fields are hashed.
A rule like "most recent wins" would let two inputs that disagree about a node's
name flip the winner every tick, minting a revision each time. A signed advert
beats a monitor's reading; among monitors, configured `priority` then reporter id.

**Reachability is the deliberate exception to "telemetry is not hashed."** A
repeater going down IS history — "how often is Lilac Park down?" is a question
the hash-excluded telemetry block can never answer. So `MeshDetail.reachability`
is hashed, and to make that affordable it is derived from the AGE of the
monitor's last success (`grid.ingest.unreachableAfter`, 45m) rather than from the
reporter's own per-poll `online` flag, which flaps on any marginal node. One
missed poll changes nothing; a sustained outage transitions exactly once.

**Everything else a monitor reports is hash-excluded — which means persisting it
needs `PollResult.ForceWrite`.** Counters move on every report, so hashing them
would mint ~288 revisions per node per day. But hash-excluded content is skipped
by `shouldUpsert`, so without an explicit force the sample would only ever be
written on the ticks where something ELSE about the node changed — for a fixed
repeater, never, leaving a battery reading frozen at whatever was current the
last time anyone renamed it. `shouldPersistTelemetry` marks the id, and coalesces
on `grid.ingest.telemetryPersistInterval` (10m) for the same reason `TouchSeen`
coalesces: each forced write is a transaction, and on EFS every commit
invalidates every reader's page cache. The store side is the `telChanged` branch
of `refreshEventPlaces`, which rewrites the blob WITHOUT bumping the revision.

**Pushed nodes are not geofenced.** The fence scopes an anonymous global
broadcast feed; a reporter is an authenticated operator asserting facts about its
own equipment, and the nodes that most need this path — quiet backbone repeaters
— are exactly the ones that advertise no location to test. The cost is that a
node known ONLY from a monitor has no geometry and therefore no geometric place
attachment, until an advert supplies one or the operator configures `placeIds`
on the reporter.

**A report-only tick keeps the stored position.** A node drops out of the
registry snapshot when its last advert is older than its presence window — for
a once-a-day backbone repeater, 14h (`graceFloor`) of every day — while the
monitor goes on reaching it, so for most of the day the monitor is the only
input. `buildEvent` carries the prior geometry forward on that path, byte for
byte, the same way `stablePosition` carries it across GPS wobble: a report says
nothing about where the node is, and "nothing new" is not "nowhere". Shipped
without this from 2026-09-15 to 2026-10-09: the event was rebuilt with nil
geometry, which detached it from every place (dropping it from every
place-scoped map and summary), minted a revision, and minted another when the
next advert put the position back. Lilac Park reached revision 40 that way,
and the Ebbetts Pass mesh map showed one of nine SIERRA repeaters — the one a
distant gateway had happened to hear in the last 14 hours. The visible
symptom is a node's history alternating geometry/no-geometry with nothing else
changing; `TestPushOnlyTickKeepsStoredPosition` pins the rule on the content
hash, not just the geometry. Recovery after the fix is per node, on its next
advert: a node already stored WITHOUT geometry has nothing to carry until a
bridge hears it again. See `docs/solutions/logic-errors/push-only-mesh-tick-wiped-stored-position.md`.

**A node the monitor has never reached carries NO telemetry block.** Zeroed
counters would assert that it has sent and received nothing. "Never read" and
"read as zero" are different facts.

### The fail-loud rule, generalized

The old rule was "all brokers down ⇒ hard `Poll` error". With two inputs it
splits, and both halves matter:

- **Every input dead** (no broker connected AND no reporter fresh) ⇒ hard error,
  exactly as before.
- **Some input degraded** ⇒ emit what we have, plus `SweepSuppress["meshcore"]`.
  The nodes missing from this snapshot are missing for OUR reason.

A silent reporter contributes NOTHING to the snapshot (replaying its last set
would refresh `last_seen_at` and fabricate liveness for nodes nobody has checked
in hours) while suppressing the sweep (its silence must not read as departure).
Those two are not in tension — they are the same honesty applied to the two
different questions.

Suppression is **bounded**: `pushingest` ages a reporter OK → STALE (suppress,
this is probably a blip) → DEAD (stop suppressing, at `4 × staleAfter`). A
monitor that never comes back must not freeze the layer's lifecycle forever. The
bound is acceptable only because `meshcore` is an `expire` source — nodes reach
EXPIRED, the "we lost track of this" terminus, not the fabricated all-clear that
RESOLVED would be — and because mesh presence is ambient INFO. **Do not copy this
bound onto a life-safety layer.**

### A second door into the registry: `mesh.packet` (2026-10)

The two inputs above are the MQTT registry and the monitor's REPORTS. There is
now a third way data arrives, but it is not a third input: the `mesh.packet`
push stream forwards raw advert frames a companion radio heard, and the push
handler hands each one to `meshcore.Registry.IngestEnvelope` — the same decode,
signature policy, spam floor and presence update an MQTT reception gets. By the
time `Poll` runs, a forwarded advert is already in the registry's `Snapshot`,
indistinguishable from one a broker delivered except for its provenance: the
reporter occupies a broker's slot (`NodeState.Brokers` and
`Observation.Broker` carry `reporter:<id>`, see `pushingest.PacketSource`),
and `meshProvenance` names the reporter from config the way it names a
broker's operator.

Why it exists: the radios that hear the SIERRA backbone at zero hops are
companions in Arnold and Dorrington, and the community brokers hear that
backbone only through a distant gateway, every few days per repeater
(measured 2026-10-09 against map.meshcore.io, which hears the same adverts
daily through its uploader bot — a companion-attached script reading the
radio's RX log). The stream accepts exactly what that bot produces. The trust
rule is unchanged: the node signed the advert, the forwarder is the courier,
and `requireValidSignature` still applies.

What it changes in `Poll`:

- **The snapshot is current while EITHER door is open.** `connected > 0 ||
  snap.PacketLive > 0` is the gate on reading `Snapshot`; with every broker
  down and no forwarder live the snapshot is only what was heard before we
  went deaf, and is left out as before.
- **Fail-loud counts three things**: no broker connected, no monitor live, no
  forwarder live ⇒ hard error.
- **A STALE forwarder suppresses the sweep**, like a stale monitor: the nodes
  only it heard are about to age out of the snapshot for OUR reason. Bounded
  the same way (DEAD stops suppressing).
- **A forwarder contributes no `Reports`** — its packets are already in the
  registry — so `buildEvent`'s report-side rules (prefix resolution,
  reachability, admin telemetry) never see it.
- **One reporter on both streams is one source row**: `SourceIDs` and
  `reporterHealth` dedupe by id, and `pushingest` rate-limits per stream so a
  forwarder and a monitor on one token do not 429 each other.

The registry is constructed whenever either door is configured
(`cmd/server/main.go`): MQTT enabled with brokers, OR a reporter authorized for
`mesh.packet`. Only the brokers need `Connect`.

Each reporter also gets its own **health-only source row** (`SourceIDs` returns
`meshcore` plus every reporter id), so `/api/v1/sources` answers "is that monitor
still reporting?" the way it answers that for every other feed. No event is ever
stored with a reporter as its source: mesh events stay on `meshcore` whichever
input observed them, because a `source_id` that flipped as inputs came and went
would both mint a revision and confuse the per-source sweep, which diffs
`polled[src]` against `prior.ForSource(src)`. A reporter that has never reported
records an ERROR, not a success — `RecordAttempt(nil)` would paint a monitor that
was never set up as a healthy feed.

## Lane closures: one event per window, from CWWP2 (`lane_closure.go`)

With `roads.caltransFeeds.cwwp2.laneClosureDistricts` set, Caltrans closures come
from the CWWP2 portal through `LaneClosureNormalizer` (source `caltrans`, its own
10m poller because the D3 + D10 files are ~5 MB uncompressed). The incidents
pipeline stops reading `lcs2way.kml`, and `RoadIncidentNormalizer` drops
`caltrans` from its `SourceIDs` (see "a source has exactly one poller" above).
Per-road segment status (the roads service) reads the same districts' ACTIVE
windows, so it agrees with these events (`internal/services/CLAUDE.md`).
Empty districts fall back to the old KML path for both.

**The unit is the window, because Caltrans has no other one.** A multi-day job is
one row per day, and neither identifier upstream names a job: `C50KB` log 8 is an
alternating closure on US-50 at Sly Park Rd one week and a lane closure at Point
View Dr the next, and `C88EA` issues a new log number every day at the same spot
(logs 4/5, 9/10, 14/15, 19/20). A per-job event's geometry would walk between
windows: the history-walks-30-km bug from `services.incidentID`, again. Only 31
of 203 (closure, log) groups had more than one window anyway, so grouping would
not have saved much.

- **Id**: `caltrans:d{district}-{index}` with the index's colons dropped. The
  index embeds the planned start, so a rescheduled window is a new id upstream
  too. The district is there because the index is only unique per district file.
- **Status** is `PhaseAt(now)`: SCHEDULED until the 10-97 call, then ACTIVE.
  COMPLETED and CANCELLED rows linger in the file but are NOT emitted; their
  absence from a successful poll is what resolves them. The planned end never
  becomes `expires` (the PG&E ETOR rule): crews overrun, and an overrunning
  closure stays ACTIVE until the 10-98 — **bounded** by
  `roads.caltransFeeds.cwwp2.laneClosureOverrunGrace` (default 12h) past the
  planned end. Crews sometimes never radio the pickup and the row lingers for
  days, so past the grace `PhaseAt` presumes it COMPLETED, the window drops
  out of the poll, and the `resolve` sweep writes RESOLVED. That is a clock
  inference, the same kind the no-show case (window over, never set up) has
  always made; it is a resolve, not an EXPIRED, because the source's policy is
  `resolve` and the planned window is the upstream's own statement of when the
  work ends. Indefinite windows are exempt. The bound lives in `PhaseAt`, not
  in the sweep, so the scheduler stays source-agnostic.
- **Text and severity are deterministic** (`hazards.SeverityFromLaneClosure`),
  never AI. ~10x the closure volume would have starved CHP of the incidents
  pipeline's 5-per-refresh budget, and the AI headline was hashed, so every
  re-wording minted a revision. Two KML-era closures reached revisions 40 and 56
  that way. The headline is what and why, the area label is where, and the
  description is the lanes. Times are typed fields and are not repeated in text.
- **Geometry is the begin point**, not a begin→end LineString. A straight chord
  across a winding mountain road attaches to places the road never touches.

**Fail-loud.** Every district down is a hard error. One district down (HTTP, a
frozen file, an EMPTY file; the client refuses all three, see
`internal/clients/CLAUDE.md`) is a `PerSource` error: the healthy district's events
land, and nothing is swept. An in-scope row the client marks `Unrecognized` (a
code flag that is not exactly true/false, no start, no end, no position) degrades
the source the same way, and is not emitted, because its phase is unknown. A row
with NO position counts as in scope, because it might be. This follows the
chain-control precedent and is deliberately strict: one bad row holds every
closure's lifecycle until it is fixed, and `/api/v1/sources` names the row.

**The feed switch adopts KML-era ids** (`adoptLegacyClosureIDs`). Without it the
first tick after deploy resolves every closure physically in place (a fabricated
all-clear in ~20 histories) and opens a duplicate for each. A legacy
`caltrans:{closureId}-{log}-{hash}` event is matched to an ACTIVE window with the
same closure id and log number and an endpoint within 250 m, unique in both
directions. On 2026-10-01 all 23 live closures matched exactly one window, at
0 m. The match recurs each tick until the window is picked up, and no new legacy
ids are minted, so the shim retires itself. **Delete it after 2026-11-09**, when
the longest KML-era closure (C26EA, SR-26 at Mokelumne Hill) is planned to end.

**SCHEDULED closures are not counted in the place summary** (`totalActive`, top
events, the roads domain). That carve-out lives in `internal/gridapi/summary.go`
with the other two. CWWP2 publishes a week of windows, 200+ in the incident
box, and planned roadwork is a calendar, not a hazard.

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

Only the `WEATHER_ALERT` layer is enhanced here (CHP road incidents arrive already
AI-enhanced from the `RoadsService` pipeline — do not re-enhance them; CWWP2 lane
closures are deliberately never enhanced, see above).
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

## Burn status: ambient state whose VALUE is the revision history

`burn_status.go` is the first layer that is not about a hazard at all. It reports
whether residential burning is currently legal in a county, and it is shaped by
three decisions worth keeping.

**A LINE is the unit, not a county.** `grid.burn.lines` is a list of recorded
phone lines, each naming the counties it speaks for, because the mapping is not
1:1 in EITHER direction: a county can have more than one relevant recording, and
one air-district line routinely covers several counties (listed once, dialed
once, attached to each). Modelling a phone number as a field on a county forces
you to either duplicate a shared line per county — and dial it N times, paying
N calls for one answer — or flatten two real lines into one field.

`mergeBurnDays` collapses a county's lines into `burn_day`, taking the MOST
RESTRICTIVE: any NO wins outright, else any MARGINAL (an elevation-restricted
burn day is a restriction and YES is not), and YES requires that every DIALED
line produced a usable answer. That last clause is the same asymmetry
`derivePermission` applies across facets — a line we expected to read but could
not is a gap, and a gap must never render as a green light. Each line's own
answer stays visible in `burn_lines[]`, so a consumer can show the disagreement
rather than only the merge.

**`enabled` governs what we DIAL, not what we BELIEVE.** A configured line that
is not dialed is still published on the event (with its number, and burn_day
UNKNOWN) so a reader can call it, and it cannot block a YES — we never asked it
anything. But a fresh reading that *does* arrive for it is used: the push
endpoint accepts readings for any configured line, so accepting one and then
silently ignoring it would be the surprising behaviour, and it would make a
manual one-off push useless. Only DIALED lines are expected to report, so only
they degrade the source.

**One of the two facets is PUSHED, not polled — the only one in the service.**
`cmd/burn-line` calls the county line from CI and POSTs the reading to
`POST /api/v1/ingest/burn.line` — the `burn.line` stream on the shared
push-ingest endpoint (`internal/pushingest`) — which lands it in the
`burn_readings` staging table. `Poll` reads the latest row on the tick. The push
handler deliberately does NOT write events: the scheduler stays the single owner
of event writes, and this is the same push-source-wrapped-as-a-poller shape
`network.go` uses for MeshCore. Reading from the STORE rather than an in-memory
buffer is what makes a reading survive a restart — it keeps its own
`observed_at`, so a rehydrated reading is re-judged by the freshness gate rather
than resurrected as current.

**Two authorities, carried separately, never merged.** A legal burn needs BOTH
the county air district's permissive-burn-day call (daily; flips weekly in
winter/spring) and the absence of a CAL FIRE suspension on SRA land (moves about
twice a year). They are separate facets on one event because they fail
independently — the county line can be unreachable while CAL FIRE's page is
fine. `derivePermission` is the only place they combine, and it is deliberately
asymmetric: **PROHIBITED is conclusive from either facet alone, ALLOWED requires
both to be known and permissive.** Someone acts on this holding a match, so an
unreadable authority must never render as a green light.

**The event is ambient and permanently ACTIVE.** One per configured county,
severity INFO, excluded from the summary hazard rollup exactly like mesh-node
presence (`totalActive`, `severityCounts`, `topEvents`, `mode`). Its value is
`/api/v1/events/{id}/history` — "when did it change" is the question the layer
exists to answer. That is also why:

- **The per-reading fields live in `BurnObservation`, which `store.ContentHash`
  zeroes.** The county line's message NAMES THE DATE ("Today, September 10th, is
  not a burn day"), so the text differs every single day even when the answer has
  not. Hashed, it would mint a revision daily and bury the handful of real
  transitions in 365 rows of noise a year. Same mechanism as `MeshTelemetry`.
- **The headline is composed deterministically** (`burnHeadline`). `ContentHash`
  does NOT zero `Headline`, so a generated or reworded one would differ every
  tick and mint a revision each time — the same rule, and the same reason, as the
  NWS alert headline above.
- **Provenance keys off CONFIGURATION, not on whether the fetch succeeded.**
  Provenance is hashed (only `fetched_at` is zeroed), so flipping `source_id`
  when the burn line blips would mint a spurious revision pair on an event that
  never changed. `TestBurnPoll_ProvenanceIsStableAcrossABurnLineOutage` pins it.
- **The id is `burn:<county place slug>`** — nothing but an immutable identifier.
  The id trap above applies with full force here: deriving it from a status field
  would mint a new id on the very transition this layer records, and the sweep
  would RESOLVE the old one.

**One facet carries forward, the other must not.** On a CAL FIRE failure the
stored suspension is carried forward from `Prior`: that page being down is no
evidence the suspension lifted, and it changes twice a year. The burn-day facet
is deliberately NOT carried forward — it is a statement about TODAY, and
yesterday's answer is exactly what the freshness gate exists to reject. Carrying
it would reintroduce the freeze through the back door.

### A sixth freeze case (`burnline`) — and why a PUSH source needs one MORE

Every other freeze case is about an upstream that keeps answering 200 with stale
data. A push source has the same problem in a starker form: **there is no fetch
to fail at all.** A pipeline that silently stops looks exactly like one that has
not run yet — the staging row simply sits there. Age is the ONLY signal.

`grid.burn.burnDayStaleAfter` (36h — one missed daily run plus slack) turns an
old `observed_at` into a `PerSource` failure AND blanks the facet to UNKNOWN.
Both halves matter: a stale "no-burn" is merely over-cautious, but a stale "burn
day" tells someone today is fine when the district may since have said
otherwise. A dialed county with NO row at all is the same failure, reported as
"no reading has been pushed". A negative value disables the gate verbatim, the
same explicit-opt-out rule as `grid.power.outageStaleAfter`.

This is also why the burn-day facet is never carried forward from `Prior`:
durability is the staging row's job (which keeps a real timestamp the gate can
judge), never the last published answer's.

**`BurnLine.Dialed()` is what "we expect a reading" means**, and it is
deliberately `enabled && phone != "" && id != ""` rather than just the presence
of a number. Publishing a county's number so readers can call it must not, by
itself, flip that county's source unhealthy for a reading we never ask for.
Tuolumne is the live example.

### Not a map layer, and place attachment without geometry

These events carry **no geometry** — burn status is an administrative fact about
a county, not a footprint — so `burn_status` is absent from `eventLayers` and
there is no `.geojson` for it. Attachment instead presets `ev.PlaceIds` (the
mechanism NWS zone alerts already use; `UpsertEvent` unions preset ids with
geometric matches and never drops the preset ones), resolved through the
`PlaceIndex` interface: the county, its towns, and any AREA overlapping it, so a
query for a town sees its county's status. Writing the county polygon instead
would put 18-32 KB into the event *and every revision of it* to express something
true of the county by definition.

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

### Why burn.line stages in the store when mesh.repeater buffers in memory

Both streams live on the same endpoint and both honour the same invariant — the
scheduler is the only thing that writes EVENTS — but they persist differently,
and the reason is cadence, not taste.

A mesh monitor re-reports every few minutes, so an in-memory buffer losing a
restart costs nothing: the next report refills it. **A burn line is called once a
day.** An in-memory buffer would leave `burn_day` UNKNOWN until the next morning
after any deploy, on the one layer whose entire job is to answer "can I burn
today". So `ingestBurn` writes a staging row (through the same store mutex as
every other writer), the row keeps the reading's own `observed_at`, and the
freshness gate re-judges a rehydrated one rather than resurrecting it as current.

The same cadence drives two more settings worth not "tidying":

- **The reporter batches every line into ONE report.** The endpoint rate-limits
  per reporter against the last ACCEPTED report, so a push per line would 429
  everything after the first.
- **A bad reading in a report is a WARNING, not a rejection.** The next attempt
  is tomorrow, so one malformed line must not discard the ones that were read.
