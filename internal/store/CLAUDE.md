# Grid event store (SQLite persistence)

The system of record for the grid service: hazard **events** with full revision
history, the **place** directory, and the **source** registry. Backs the
`/api/v1` API (`internal/gridapi`, gRPC-Gateway) and the hazard event layers
(`internal/hazards`). Design: `docs/v2-api-spec.md` §4 +
`docs/v2-implementation-plan.md` §2.2. Pure-Go driver (`modernc.org/sqlite`) so
the `CGO_ENABLED=0` cross-compile keeps working — do **not** swap in a cgo driver.

## Schema philosophy — the proto blob is canonical

Every row stores the full `grid.v1` proto as a `BLOB`; the scalar columns
(`layer`, `severity`, `status`, `source_id`, `effective`, `expires`,
`observed_at`, …) exist **only as query indexes** and are always re-derivable from
the blob. Every read path (`GetEvent`, `QueryEvents`, `EventHistory`, place/source
reads) rehydrates from the blob, never from the columns — so a column and the blob
can never drift into two answers. (The two deliberate exceptions are the ETag
validators `EventVersion` — `SELECT revision` — and `DataVersion` —
`MAX(rowid)` over `event_revisions`: they read a column/rowid *on purpose*, never
rehydrating, because they must be cheap and they only ever feed an opaque
change-detection tag, never a response body. The `revision` column is written
atomically with the blob, so it can't drift from it.) When you add a filterable field, add a column
**and** keep writing it from the blob at upsert; never let a column become the only
home of a value. `event_geo` (R*Tree) + `event_geo_map` index geometry bboxes for
spatial queries; `event_places` is the precomputed event→place join so the hot
read path never touches geometry.

## Content-hash gating — what's zeroed and why

`ContentHash(ev)` is the SHA-256 of a deterministic proto marshal of a **clone
with volatile fields zeroed**. `UpsertEvent` writes a new revision only when the
hash differs from the stored one. Zeroed fields and the reason each is excluded:

- `revision`, `ingested_at`, `observed_at`, `provenance.fetched_at` — bookkeeping
  timestamps/counters. Upstream re-stamping a record it didn't actually change
  must not mint a revision.
- `place_ids` — derived at ingest (geometry match ∪ caller preset), not upstream
  content. A place seeded *after* the event first arrived must be able to attach
  without faking a content change (see the hash-equal path below).
- `enhancement` + `summary` — AI output. Excluded for **two** reasons: (1) so
  generating an enhancement never causes hash churn / a spurious revision, and (2)
  so the scheduler can call `NeedsUpdate` (a pure hash compare) to decide whether
  to spend AI-enhancement budget **before** enhancing. If enhancement were hashed,
  an enhanced event would differ from the next raw poll and loop forever, and the
  spec §6 "enhancement regenerated per poll" bug would come back.
- `network.telemetry` (NETWORK events only) — the MeshCore per-advert signal state
  (SNR/RSSI/hop count/path/gateways/last-advert time). A mesh node re-adverts
  constantly and every packet carries fresh signal metrics; hashing them would mint
  a revision per packet and blow up `event_revisions`. Grouping them into one
  telemetry sub-message means one field to zero. What still mints a revision is a
  node's **stable identity** (pubkey, role, name), its **location** (geometry is
  hashed — movement is meaningful), or a **status** flip. The volatile metrics ride
  forward untouched across polls, exactly like `summary`/`enhancement`.

`NeedsUpdate` is the read-only pre-check the scheduler uses for the budget
decision; `UpsertEvent` re-computes the hash itself from the event **as passed**
(before any store-side mutation like geometry backfill) so the stored hash always
equals what the next poll's `ContentHash` yields.

## Revision semantics — transitions are revisions; the all-clear is history

- New event → revision 1. Changed content → `old+1`. Every content write also
  inserts an `event_revisions` snapshot (full blob), recomputes `event_places`,
  refreshes the R*Tree row, and stamps `last_seen_at`.
- `TransitionEvents` (lifecycle: → RESOLVED/EXPIRED, and any status change) bumps
  the revision and writes a snapshot too. **The all-clear is part of history**, not
  a delete — an event that leaves its feed becomes a RESOLVED/EXPIRED revision, and
  a later reappearance (status differs) writes another revision. Events already in
  the target status (and unknown ids) are skipped, so sweeps are idempotent.
- `status` **is** hashed content — that's what makes a resolve→reappear cycle
  produce revisions rather than silently no-op.

## `last_seen_at` vs `observed_at`

Two different questions, do not conflate:

- `observed_at` (blob + index column, caller-owned) — *when the content was
  observed*. Moves only on a content change or a transition.
- `last_seen_at` (column only, `TouchSeen`) — *when a successful poll last
  included this event*. Written for **every** id a good poll returned, including
  hash-equal no-op upserts that write nothing else. No revision, never touches the
  content hash — liveness is not content.

The lifecycle expire grace (`ingest.shouldExpire`) is anchored to `last_seen_at`
so a stable long-lived event that drops out of a single poll is not expired
instantly. Rows predating the `last_seen_at` migration (value 0) fall back to
`observed_at`, then `ingested_at`, so the grace still terminates them.

## `event_places` recompute — including the hash-equal path

`matchPlaces` computes geometric attachments (point → PIP for polygon places, or within a ~1.5 km buffer of a corridor **LineString** via `geojson.PointNearLine`/`PointInOrNearGeometry`; polygon → bbox-intersect
+ permissive containment; over-attach beats missing a perimeter that straddles a
boundary), then `UpsertEvent` **unions** those with the caller's preset
`place_ids` (e.g. the NWS zone→area mapping) — preset ids are never dropped.

Because `place_ids` is zeroed out of the content hash, **hash-equal does not mean
place-set-equal**. On the hash-equal upsert path, `refreshEventPlaces` still
recomputes the wanted set and, if it differs from the stored rows, updates
`event_places` **and** rewrites the blob's `place_ids` — with no new revision, no
hash change. This is what lets a place polygon seeded after the event first
arrived (boot ordering, a new county polygon, an edited zone→area config) attach
retroactively. Keep `event_places` and the blob's `place_ids` in lockstep — reads
rehydrate from the blob, so a divergence would make place queries and event detail
disagree.

### The one layer with a looser rule: wildfire proximity

On top of the geometry rules, a `WILDFIRE` event also attaches to an **AREA** or
**TOWN** place it comes within `wildfireBuffer` metres of
(`WithWildfireProximity`, from `grid.wildfire.placeBufferMeters`, default 20 km;
`geojson.WithinDistance` is the predicate). Fire is the hazard that *moves toward
you*, so a fire 12 km outside the coverage polygon belongs on that area's map and
summary — waiting for the perimeter to cross the line is waiting too long.

Two boundaries on it, both deliberate:

- **Only AREA and TOWN.** Counties tile the map, so a nearby fire already
  attaches to *some* county exactly and buffering them would smear one fire
  across four (the same over-attach failure the bbox rule caused). Corridors
  already have their own tuned 1.5 km point buffer, and a 20 km fire buffer would
  pin every regional fire to every highway segment.
- **Only WILDFIRE.** Every other layer keeps strict containment/overlap. A quake
  9 km outside an area is not in it.

Downstream consequence to keep in mind: a proximity-attached fire is an ordinary
active event for that place — it counts in the summary's `totalActive`,
`severityCounts` and `mode`. That is intended (a fire 12 km out *should* raise the
mode), but it means a place can report an active fire whose perimeter is outside
its boundary.

## Single-writer discipline

Writes go through `inTx` (or `TouchSeen`), which take the store mutex — the ingest
scheduler is the **only** writer, serialized. Reads go straight to the connection
pool and serialize against the writer's short commit via `busy_timeout(5000)`
(the default TRUNCATE rollback journal has no WAL MVCC, so a reader waits up to
5s for a commit rather than erroring `SQLITE_BUSY`). Keep write transactions
small — one `UpsertEvent` per tx — so that window stays tiny; this is why
`matchPlaces` is cache-backed. `matchPlaces` reads the `places` table inside the
write transaction (consistent snapshot). Do not add a second writer; if you need
one, it must also hold `mu`.

## Journal mode / filesystem (`WithJournalMode`)

`Open` takes a journal mode; default **TRUNCATE**, config `grid.journalMode`
(`PF__GRID__JOURNAL_MODE`). It is whitelisted and paired with a `synchronous`
level in `journalModeSynchronous`:

- **TRUNCATE / DELETE / PERSIST** (rollback journals) → `synchronous=FULL`. No
  memory-mapped `-shm` file, so they work over a **network filesystem (NFS/EFS)**
  — which is why this is the default (prod runs on EFS). TRUNCATE zero-truncates
  the `-journal` instead of unlinking it (one fewer NFS metadata op per commit).
  FULL keeps the rollback journal crash-safe (NORMAL can corrupt a rollback
  journal on power loss).
- **WAL** → `synchronous=NORMAL`. Faster concurrent reads, but the `-shm` is
  memory-mapped and **breaks over NFS/EFS** — use it only on a real local disk.

**Cross-process guard — `Open` takes an exclusive `flock`.** SQLite's own
per-file locking is not honored on some shared filesystems — notably the
**virtiofs bind mount** Docker Desktop uses for `/workspace` — so two processes
writing the same DB there silently corrupt it (`database disk image is
malformed`). The classic footgun: the container's dev server *and* a host process
that mounts the workspace both open `./data/grid.db`. `Open` takes an exclusive
advisory `flock` on `<path>.lock` and returns a clear "already open by another
process" error instead of racing; the flock is bound to the fd, so the kernel
releases it on `Close` or process death (no stale lock). It waits up to
`lockAcquireTimeout` (~15s) for a contended lock first, so a rolling deploy's new
task acquires once the old one drains and exits (releasing its flock on the
shared EFS volume) rather than failing on the brief overlap. It coordinates within
one kernel only — a writer reaching the file from a separate host via the mount
can still slip past — so **for dev, keep the DB off the bind mount**: point
`PF__GRID__DB_PATH` at a container-local path outside `/workspace` (e.g.
`$HOME/.local/state/grid/grid.db`, set in `.envrc`) rather than relying on the
lock alone. A single writer is corruption-free on every filesystem; only
concurrent writers are the hazard.

**The store is a system of record, not a rehydrate-able cache.** A clean restart
with the DB intact rehydrates everything (that is the point of persistence). But
if the DB is *lost*, only the **current active snapshot** rebuilds — the pollers
re-fetch the upstreams' present state and re-insert those events (as revision 1,
freshly ingested). Everything else is **irreplaceable**, because most sources are
active-only and ephemeral (CAL FIRE `inactive=false`, Cal OES active-zones,
CHP/Caltrans current incidents, NWS active alerts): the full revision history,
every RESOLVED/EXPIRED event (things that already ended are gone from the feeds),
and the true first-seen times / revision counts of even still-active events
cannot be recovered. So the durability choices here matter — hence FULL on the
rollback journal — and the volume deserves real backups, not "it'll just
rebuild." Match the journal mode to the filesystem, and treat DB loss as data
loss of history, not a cache miss.

## Index statistics are REQUIRED, not tuning (`Analyze`)

`Store.Analyze` runs `ANALYZE`. It is called at boot (`cmd/server`) and every
`statsRefreshInterval` (6 h) by the ingest scheduler, and it is **load-bearing**:
without stats the place-scoped event query degrades without bound as the store
ages.

The query is `FROM events e JOIN event_places ep ON … ep.place_id = ? WHERE
e.status IN (ACTIVE, SCHEDULED)`. **`event_places` attachments are deliberately
never deleted on a lifecycle transition** — `TransitionEvents` updates `events`
and writes a revision but leaves `event_places` alone, because place-scoped
history (`QueryHistory`, and `place=X&status=RESOLVED`) needs them. So a place's
attachment count grows forever while its live set stays small: measured **62-96%
dead after five days** on the dev DB.

There is **no index carrying `status` on `event_places`**
(`idx_event_places_place` is `(place_id, event_id)`), so the dead rows cannot be
discarded until each candidate has been fetched out of the wide `events` table.
With no statistics SQLite assumes `ep.place_id = ?` is highly selective and picks
exactly that plan:

```
SEARCH ep USING COVERING INDEX idx_event_places_place (place_id=?)
SEARCH e USING INDEX sqlite_autoindex_events_1 (id=?)      <- one wide-row fetch per dead attachment
```

With statistics it drives from the bounded ACTIVE set instead, and probes
`event_places` through a covering index — never fetching an `event_places` row:

```
SEARCH e USING INDEX idx_events_active (status=?)
SEARCH ep USING COVERING INDEX idx_event_places_place (place_id=? AND event_id=?)
```

Measured on a synthetic reproducing production's plan (local disk, warm cache;
**prod is EFS, where every discarded fetch is an extra network round trip**, so
the real spread is far wider):

| attachments | no stats | with stats |
| ----------- | -------- | ---------- |
| 500         | 2.2 ms   | 2.0 ms     |
| 6,400       | 10.9 ms  | 4.0 ms     |
| 50,400      | 69.1 ms  | 3.9 ms     |

This shipped as `/api/v1/events?place=…` taking 1.1-3.7 s while returning 14
events, and 0.9 s to return 200 — latency **anti-correlated** with the size of
the answer, which is the signature to recognize.

Two traps:

- **`PRAGMA analysis_limit` does not work here.** At the documented 400-row
  sample the plan does not change at all (68.8 ms at the largest size) — the
  sample is far too small to see a 99% skew. Use a full `ANALYZE`; it cost
  119 ms at 50,400 attachments.
- **`PRAGMA optimize` is a no-op** on a freshly pooled connection, which is what
  `database/sql` hands you.

`TestPlaceScopedEventsDoesNotWalkDeadAttachments` pins the join order. It
reproduces the production plan exactly when `Analyze` is omitted, so it is not
vacuous. Denormalizing `status` onto `event_places` would be marginally faster
(2.8 vs 3.9 ms) but needs a migration plus an invariant maintained across two
tables — and a drift there would silently hide an ACTIVE event from a
place-scoped query, which is the one failure this service must not have.

## Migration ladder

`migrations[]` is an ordered slice; index `i` is schema version `i+1`. `Open`
creates `schema_migrations`, reads `MAX(version)`, and applies only the missing
ones, each in its own transaction, recording the version. This makes `Open`
idempotent across restarts and lets an older dev DB pick up just the new steps.
Rules:

- **Append-only.** Never edit an already-shipped migration (a deployed DB has
  recorded it as applied and will skip it) — add a new entry instead.
- `migrations[0]` is the embedded `schema.sql`; later entries are Go string DDL
  (see `migrationV2` adding `last_seen_at`). Keep `schema.sql`'s trailing comment
  noting which columns later migrations add, so the file reads as the current
  shape.
- A migration that needs a data backfill runs its DML in the same transaction.

`ErrNotFound` from point lookups (`GetEvent`, `GetPlace`) maps to HTTP 404 at the
API layer — return it, don't invent a sentinel.
