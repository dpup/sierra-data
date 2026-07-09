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
(`PF__GRID__JOURNALMODE`). It is whitelisted and paired with a `synchronous`
level in `journalModeSynchronous`:

- **TRUNCATE / DELETE / PERSIST** (rollback journals) → `synchronous=FULL`. No
  memory-mapped `-shm` file, so they work over a **network filesystem (NFS/EFS)**
  — which is why this is the default (prod runs on EFS). TRUNCATE zero-truncates
  the `-journal` instead of unlinking it (one fewer NFS metadata op per commit).
  FULL keeps the rollback journal crash-safe (NORMAL can corrupt a rollback
  journal on power loss).
- **WAL** → `synchronous=NORMAL`. Faster concurrent reads, but the `-shm` is
  memory-mapped and **breaks over NFS/EFS** — use it only on a real local disk.

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
