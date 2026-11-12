# Grid event store (PostgreSQL persistence)

The system of record for the grid service: hazard **events** with full revision
history, the **place** directory, and the **source** registry. Backs the
`/api/v1` API (`internal/gridapi`, gRPC-Gateway) and the hazard event layers
(`internal/hazards`). Design: `docs/design/v2-api-spec.md` §4 +
`docs/design/v2-implementation-plan.md` §2.2. Driver: pgx v5 (pure Go), so
the `CGO_ENABLED=0` cross-compile keeps working — do **not** add a cgo driver.
Until 2026-11-11 this was SQLite on EFS; that store is frozen on the
`legacy/efs-sqlite` branch (tag `legacy-efs-final`), and the migration plan is
the workspace note `notes/2026-11-11-rds-postgres-migration-plan.md`.

## Schema philosophy — the proto blob is canonical

Every row stores the full `grid.v1` proto as a `bytea`; the scalar columns
(`layer`, `severity`, `status`, `source_id`, `effective`, `expires`,
`observed_at`, …) exist **only as query indexes** and are always re-derivable from
the blob. Every read path (`GetEvent`, `QueryEvents`, `EventHistory`, place/source
reads) rehydrates from the blob, never from the columns — so a column and the blob
can never drift into two answers. (The deliberate exceptions are the ETag
validators: `EventVersion` — `SELECT revision, refreshes` — and the `versions`
counters (`DataVersion`, `RefreshVersion`, …): they read a column *on purpose*,
never rehydrating, because they must be cheap and they only ever feed an opaque
change-detection tag, never a response body. The `revision` column is written
atomically with the blob, so it can't drift from it.) When you add a filterable field, add a column
**and** keep writing it from the blob at upsert; never let a column become the only
home of a value. The geometry bbox is four plain columns on `events`
(`min_lat`…`max_lng`, NULL without geometry; no reader yet — the SQLite R*Tree
they replace was never read in production); `event_places` is the precomputed
event→place join so the hot read path never touches geometry.

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
- `mesh.telemetry` (MESH events only) — the MeshCore per-advert signal state
  (SNR/RSSI/hop count/path/gateways/last-advert time). A mesh node re-adverts
  constantly and every packet carries fresh signal metrics; hashing them would mint
  a revision per packet and blow up `event_revisions`. Grouping them into one
  telemetry sub-message means one field to zero. What still mints a revision is a
  node's **stable identity** (pubkey, role, name), its **location** (geometry is
  hashed — movement is meaningful), or a **status** flip. The volatile metrics ride
  forward untouched across polls, exactly like `summary`/`enhancement`.
- `burn_status` per-line `observation` + `calfire_observed_at` (BURN_STATUS
  only) — the burn line's message names the date, so it changes daily while the
  ANSWER does not; hashed, it would mint a revision a day. The lines' `burn_day`
  and the facets stay hashed: a changed answer IS the transition.

**Every hash-excluded field needs a way back into the blob**, or it freezes at
whatever was current the last time something hashed changed. The hash-equal
path (`refreshEventPlaces`) persists, without a revision: place_ids, a fresh
enhancement/summary, mesh telemetry, and burn status's observations (plus the
burn event's `observed_at`/`fetched_at`, which follow the reading). The
scheduler skips hash-equal events entirely unless the poller lists them in
`PollResult.ForceWrite`, so a poller with such a field must list them — mesh
does (coalesced), burn does (every event; two an hour). Burn status shipped
without this and served its first observation from 2026-10-09 to 10-12 while
daily readings landed; burnday.ersn.net read the frozen call time and told
residents the data was stale. If you exclude a new field from the hash, add it
to that path and to the poller's ForceWrite in the same change.

**That path changes the served body without a revision, so it must move the
validators.** Each rewrite bumps the row's `refreshes` column and, in the same
transaction, the layer's `refresh:<LAYER>` row in `versions`. `EventVersion`
returns `EventStamp{Revision, Refreshes}` for the event-detail ETag, and
`ListEvents` folds `RefreshVersion(layers)` in beside `DataVersion`. Keyed on
revision alone, a conditional GET answered 304 with the old enhancement,
telemetry or burn observation. Per layer so a mesh node's telemetry refresh
leaves a road-incident list's 304 alone; in the writing transaction so a reader
can never pair the new version with the old blob (and every process serving
reads sees the same counter). A hash-equal poll that rewrites nothing moves
neither. History is untouched: a revision snapshot never changes.

`NeedsUpdate` is the read-only pre-check the scheduler uses for the budget
decision; `UpsertEvent` re-computes the hash itself from the event **as passed**
(before any store-side mutation like geometry backfill) so the stored hash always
equals what the next poll's `ContentHash` yields.

## Revision semantics — transitions are revisions; the all-clear is history

- New event → revision 1. Changed content → `old+1`. Every content write also
  inserts an `event_revisions` snapshot (full blob; bumps the `revisions`
  counter behind `DataVersion`), recomputes `event_places`, writes the bbox
  columns and the lite copy, and stamps `last_seen_at`. `first_ingested_at` is
  written on insert only (`/stats`' "since", `FirstIngested`).
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
  observed*. Moves only on a content change or a transition — except burn
  status, where it follows each new reading (see the hash-equal path above).
- `last_seen_at` (column only, `TouchSeen`) — *when a successful poll last
  included this event*, **coalesced**: the scheduler passes a staleness cutoff
  and only rows whose stamp is older than the coalescing window
  (`ingest.touchSeenCoalesce`, 10 min) are rewritten. The stamp may therefore
  lag the true last appearance by up to the window — fine, because it only
  feeds expire graces measured in hours (smallest configured: 2 h). No
  revision, never touches the content hash — liveness is not content.

  **The cutoff is a performance invariant, not a tidy-up.** Unconditional
  stamping rewrote ~400 blob-carrying rows every 60 s mesh tick. On the legacy
  SQLite/EFS store each commit made every reader discard its page cache (the
  measured 1.7-3.5 s spikes on ~7% of requests); on PostgreSQL every UPDATE
  writes a new row version, WAL and later vacuum work. With the cutoff, a tick
  where nothing is stale matches zero rows and writes nothing. Keep the window
  far below the smallest `expireAfter`; the worst case is an event expiring
  earlier by the lag.

The lifecycle expire grace (`ingest.shouldExpire`) is anchored to `last_seen_at`
so a stable long-lived event that drops out of a single poll is not expired
instantly. Rows predating the `last_seen_at` migration (value 0) fall back to
`observed_at`, then `ingested_at`, so the grace still terminates them.

## Resolve (`PlacesContaining`) and towns

`PlacesContaining` (behind `GET /api/v1/places:resolve`) uses the same
polygon/corridor tests as event attachment, plus one rule of its own: a
**TOWN** is seeded as a point (its weather location), which no
point-in-polygon test can match, so a town contains a location within
`townRadius` metres of it (`WithTownRadius`, from
`grid.places.townRadiusMeters`, default `DefaultTownRadiusMeters` 3 km). Towns
within range are returned nearest first. The radius is **resolve only**:
`matchPlaces` does not use it, so events still attach to a town only through
the wildfire proximity rule or a preset `place_ids` (burn status). Putting it
into attachment would change every point event's `place_ids`, and so what
`/events?place=<town>` returns: a separate decision.

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

Every write goes through `inTx`, which takes the store mutex — the ingest
scheduler is the **only** writer, serialized — and refuses unless this
process holds a live writer lease (below), so exactly one process writes a
database. A reader store (`OpenReader`) answers every write with
`ErrNotWriter`. Reads go straight to the pool under PostgreSQL MVCC and never
wait on the writer. Write
transactions run at READ COMMITTED; their read-modify-write steps (the upsert's
revision lookup, `TransitionEvents`, `RecordAttempt`, compaction's watermark)
are safe only because of the single writer — say so in a comment if you add
one. Keep write transactions small (one `UpsertEvent` per tx); `matchPlaces`
is cache-backed for the same reason, and that cache (`placesGeo`) is valid only
inside a fenced write of the lease term that built it (`loadPlaceGeoms`
checks `txEpoch`; a reader cannot reach it). Do not add a second writer or a
write that bypasses `inTx`: the epoch fence lives there.

## PostgreSQL

- **Driver and pool.** `Open(url, …)` builds a `pgxpool.Pool`
  (`grid.database.maxConns`/`minConns`, defaults 8/1) and exposes it as
  `*sql.DB` via `stdlib.OpenDBFromPool`, so the store is plain `database/sql`
  with `$n` placeholders. `Pool()` returns the native pool for what
  `database/sql` cannot do (COPY, LISTEN, a dedicated connection). Dynamic
  lists bind one array: `= ANY($n)` with a Go slice. `TestNoQuestionMarkPlaceholders`
  fails on a SQLite `?` left in any SQL literal.
- **Reader and writer (`lease.go`).** `OpenReader` opens the pool, checks
  collation and the schema version, and takes no lock; a database older than
  the build opens with `SchemaReady()` false and the version watch re-checks
  it. `AcquireLease` makes the store the writer: a session
  `pg_try_advisory_lock` on a connection taken with `Acquire` and then
  `Hijack()`ed out of the pool (never a pooled connection, which would carry
  the session lock into unrelated queries), then an epoch bump in
  `writer_lease` (`SELECT ... FOR UPDATE`, then `UPDATE`, recording holder
  and address), then migrations. A 2 s heartbeat updates `heartbeat_at WHERE
  epoch = mine`; an error or zero rows means the lease is lost (`Lease.Lost`,
  `OnLost`). `ReleaseLease` waits for an in-flight write, clears holder and
  address, unlocks. `Open` is `OpenReader` plus `AcquireLease` (waiting
  `lockTimeout`): the one-process path for tools and tests.
- **Fencing.** `inTx` first runs `SELECT 1 FROM writer_lease WHERE id = 1 AND
  epoch = $mine FOR KEY SHARE` and fails with `ErrFenced` (wrapping
  `ErrNotWriter`) on zero rows, marking the lease lost. The bump's `FOR
  UPDATE` waits for a write already past the check (it commits as the old
  term's last write) and fails every later one; key-share does not block the
  heartbeat's no-key update, so a long write never stalls it. This is what
  an advisory lock alone could not do: stop a writer whose session died while
  its pooled connections still work. `lease_test.go` pins the stale epoch,
  the wait, the terminated backend and the release.
- **Collation.** Keyset cursors (`id > $1`), golden orderings and Go-side
  sorting assume byte order. The database must be created
  `TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'`; `Open` refuses one that
  does not compare bytewise (`checkCollation`), and
  `TestKeysetPagingFollowsByteOrder` walks ids with `:` and `-`.
- **Validators (`versions.go`).** `DataVersion`, `RefreshVersion`,
  `MeshActivityVersion` and `PlacesVersion` read rows of the `versions` table,
  bumped in the same transaction as the write. They used to be process memory
  (and a `COUNT(*)`), which gives wrong 304s as soon as two processes serve
  reads. Each store caches the table (`versionCache`) and re-reads it after
  its own commits, on `NOTIFY grid_versions` (a statement trigger on
  `versions`, migration 0002; a dedicated `LISTEN` connection) and on a 10 s
  poll (`WithVersionPoll`), so a reader's stale 304 is bounded by the poll
  even with the listener down. A test that writes `versions` with raw SQL
  must account for the cache.
- **Read snapshots.** Reads that span statements (`MeshLinks`,
  `RoadStatusSince`, `Totals`) run in one REPEATABLE READ READ ONLY
  transaction (`readTx`), so a commit landing between them cannot produce an
  answer that never existed.
- **Statistics.** PostgreSQL's autovacuum keeps planner statistics; there is
  no `Analyze` any more. Plan assertions use `EXPLAIN (FORMAT JSON)` with
  sequential scans, bitmap scans and sorts disabled (`plan_test.go`), which
  pins that an index CAN serve a statement's filter and ORDER BY on test-sized
  tables.
- **Timestamps** stay `bigint` unix seconds; never `timestamptz` for domain
  times (`schema_migrations.applied_at` is bookkeeping, not domain data).
- **Tests.** `internal/storetest.URL(t)` gives each test its own database,
  copied from a migrated `sierra_template_<hash of the migrations>` (so
  worktrees with different migration sets on one Docker server do not drop
  each other's template) and dropped afterwards;
  without `TEST_DATABASE_URL` the test is skipped with a `SKIP:` line.
  `make test` starts the Docker database (`make db-up`) and sets it.

**The store is a system of record, not a rehydrate-able cache.** A clean
restart rehydrates everything. But if the database is *lost*, only the
**current active snapshot** rebuilds — the pollers re-fetch the upstreams'
present state and re-insert those events (as revision 1, freshly ingested).
Everything else is **irreplaceable**, because most sources are active-only and
ephemeral (CAL FIRE `inactive=false`, Cal OES active-zones, CHP/Caltrans current
incidents, NWS active alerts): the full revision history, every
RESOLVED/EXPIRED event, and the true first-seen times / revision counts of even
still-active events cannot be recovered. Backups and point-in-time restore are
part of the deployment, not an afterthought; treat database loss as loss of
history, not a cache miss.

## `QueryEvents` sorts keys, then fetches blobs

`idx_events_active` orders by severity within ONE status, so any status list
(the default `ACTIVE,SCHEDULED` included) sorts. When the sorted `SELECT`
carried `e.proto`, the legacy SQLite store copied the blob of every matching
row into the sorter to keep 201: an all-statuses page took 25-40 s in
production whatever its `page_size`. `eventsPageSQL` sorts
`(id, severity, observed_at)` in a subquery `k` with the LIMIT, and joins back
by primary key for the page's rows alone. `TestQueryEventsSortsKeysNotBlobs`
pins that the key query never carries the blob. Do not add the blob back to
the inner select.

Every page read returns a `Page[T]` with a resume token per row (`After[i]`),
so a caller can serve a prefix (gridapi cuts at a byte budget) and the walk
stays complete. The tokens are the same keyset cursors as before.

## `event_lite` — the list copy without shapes

`event_lite` holds each event's CURRENT blob with `geometry.geojson` (and
`object`) removed; bbox and centroid stay. `QueryEvents` reads it when
`EventQuery.Lite` is set (`/api/v1/events?geometryFormat=NONE`). It exists
because GeoJSON is ~96% of an all-statuses page: on the legacy EFS store page 1
of `place=ebbetts-pass` took 15.6 s cold and 1.7 s warm (2026-11-10), and
in PostgreSQL every shape is still a TOAST fetch and bytes on the wire.

It is derived state, like `event_places`, and the rule is the same: **every
write of `events.proto` writes it in the same transaction** (`putLite`: the
upsert, `TransitionEvents`, the hash-equal refresh). A new write path that
skips it serves a stale list row. A missing row is not an error: the read
`COALESCE`s to `events.proto` and strips its shape in Go (correct, just slow;
the body is the same before and after the backfill, so its ETag is too). The
scheduler's `runLiteBackfill` fills rows for events stored before the
lite copy existed (today only rows imported from the legacy store can lack
one): it walks `events` by id from a cursor (reads outside the writer
lock, never rescanning from the start), inserts 50 missing rows per short
transaction (`DO NOTHING`, so a concurrent upsert's newer copy wins), and skips
a blob that will not unmarshal rather than stopping on it. Revisions are never
lite.

## The mesh tables are measurements, not records

Four tables hang off the mesh subsystem and none of them is proto-blob
canonical, because none of them holds event content:

| table | holds | if lost |
| --- | --- | --- |
| `mesh_observations` | one row per received advert (Tier 0) | re-accumulates from the live MQTT feed |
| `mesh_link_rollup` | per-link-per-day topology (Tier 1) | recompute from Tier 0 |
| `mesh_activity` | per node: last advert received, last relay seen | re-accumulates within minutes for any node that is talking |
| `mesh_telemetry` | one row per monitor read (a report repeating the last read, or re-sending it as new with unchanged uptime and counters, is skipped) | **gone forever** |

That last row is why `mesh_telemetry` gets a generous retention default (a year)
while raw observations get 48 hours. A monitor reports the present and never
replays: anything pruned there is a battery curve nobody can reconstruct, where
a pruned observation costs at most some freshness.

Its columns mirror `grid.v1.MeshAdminTelemetry` field for field **including the
nullable/plain split** — gauges nullable because an unread one is not a zero,
counters plain because they are only written alongside a successful read. The
read path rebuilds the proto message itself (`gridapi.adminReading`), so the
archive and the live event cannot describe one reading two ways.

Two details that look optional and are not:

- **`PRIMARY KEY (pubkey, reported_at)`** — the monitor's clock, not ours. The
  mesh poller ticks every 60s while reports arrive every ~15 minutes, so it
  re-offers the same sample ~15 times; keying on our receive time would store
  fifteen copies of one reading and draw a staircase.
- **`RenameMeshTelemetryPubKey`** — a node known only by a key prefix is filed
  under that prefix until an advert supplies the full key. The promotion retires
  the provisional event, and without the rename the samples are orphaned under a
  key nothing refers to any more: silently, once per node, with no way to notice.

No Tier 1 rollup here yet, deliberately: ~864 rows/day for nine nodes is 315k
rows a year. `mesh_link_rollup` exists because the advert firehose is orders of
magnitude larger. Adding one is additive when node count × cadence earns it.

### `mesh_activity` is the one thing the API reads from outside a blob

`telemetry.last_heard_at` and `last_relayed_at` are served on every current
MESH event but never stored in it. They move every few seconds; carrying them
in the blob would rewrite every node's proto on every tick, which is the
write cost `TouchSeen`'s coalescing exists to avoid, and the "every
hash-excluded field needs a way back into the blob" rule above would then mean
a ForceWrite of ~600 events a minute. So the ingest tick upserts one tiny row
per node whose clocks moved (`RecordMeshTick`, in the SAME transaction as the
observation insert: one commit, not two), and `gridapi.overlayMeshActivity`
stamps them on at read time.

Three rules keep that honest:

- **Merge by MAX** (`GREATEST(mesh_activity.col, excluded.col)` — qualify the
  target columns; unqualified they are ambiguous in PostgreSQL). A write never
  moves a clock backwards, so a replay after a restart, or a late drain, is a
  no-op rather than a regression.
- **Validators must see it.** The overlay changes a response body without a
  revision, so `DataVersion` and `EventVersion` cannot. `MeshActivityVersion`
  (the `mesh_activity` row in `versions`, bumped in the writing transaction) is folded into the `ListEvents` tag
  whenever the query can return MESH events, and `GetEvent` folds in the node's
  own row. Leaving it out serves a 304 with a stale "last relayed".
- **History is not overlaid.** A revision is a snapshot of what was stored.

## Migration ladder

The schema is SQL files embedded in `internal/store/migrations`
(`NNNN_slug.sql`, NNNN = schema version; `0001_baseline.sql` is the SQLite v14
schema translated, with the changes listed in its header). The runner applies
each pending version in its own transaction under `pg_advisory_xact_lock`, and
records it in `schema_migrations (source, version, applied_at)`; `source` is
`postgres` for these and `sqlite` for the legacy ladder the importer records.
Only the writer migrates (`AcquireLease`, holding the advisory lock and
after fencing the previous term). A reader on an older schema waits, not
ready, until the writer has migrated; a release that adds a migration
therefore needs the writer role to move to the new build (SIGTERM or `yield`
the old writer, docs/deploy-handoff.md). Rules:

- **Append-only.** Never edit a shipped file (a deployed database has recorded
  it and will skip it) — add a new one.
- **Additive only (expand/contract).** A process accepts a database at a
  schema version `>=` its own (`migrations.CheckVersion`), so a reader one
  release behind keeps working while a newer writer migrates. Never drop or
  rename what the previous release still reads in the same release.
- A migration that needs a data backfill runs its DML in the same file; DDL is
  transactional, so a failed migration leaves nothing behind.

### The SQLite importer depends on this schema

`internal/sqliteimport` (`gridctl import`, `docs/import.md`) loads the legacy
SQLite store into these tables and verifies them column by column. Its table
list (`tables.go`) names the columns it copies and compares, and it derives
the columns SQLite never had (`first_ingested_at`, the bbox, `versions`, the
`mesh_observations` identity). So:

- A new column on an imported table must either have a default that is right
  for every legacy row, or be derived in the importer (and checked there).
- A new table that holds data carried from SQLite goes into the importer's
  list; one that does not is simply empty after an import.
- A new `versions` key, identity column or derived index column needs the
  same treatment as `revisions` and `first_ingested_at`: seeded by the import
  and asserted by its database checks.
- `import_log` is migration `0003_import_log.sql`; the importer only writes
  its row, inside the import transaction.

`ErrNotFound` from point lookups (`GetEvent`, `GetPlace`) maps to HTTP 404 at the
API layer — return it, don't invent a sentinel.

## `burn_readings` — the one table an HTTP request writes

Every other table here is written by the ingest scheduler. `burn_readings`
is written by the `burn.line` stream of the push-ingest endpoint
(`internal/pushingest`), and it is the landing spot for a pushed burn-day
reading. It is the only table an HTTP request writes: the sibling `mesh.repeater`
stream buffers in memory instead, because it reports every few minutes whereas a
burn line is called once a day and must survive a restart.

It is a staging table on purpose. The push handler validates a reading and lands
a row; it does **not** write events. `BurnStatusNormalizer.Poll` reads the latest
row on the scheduler's tick, so the scheduler remains the single owner of event
writes and the fail-loud lifecycle rules are unchanged. Single-writer discipline
still holds because it was never "one caller" — it is `inTx` taking `mu`, which
`PutBurnReading` goes through like everything else.

Two properties worth keeping:

- **It is keyed by LINE, not by county.** One air-district line routinely speaks
  for several counties; keying by county would store (and dial) the same reading
  once per county. Which counties a line answers for is config
  (`grid.burn.lines[].counties`), not a property of the reading.
- **The row keeps the upstream's own `observed_at`** (when the phone line was
  called), separate from `received_at` (when we accepted the push). The former is
  the freshness signal the ingest gate judges; the latter is diagnostic. Storing
  only a receive time would make every restart-rehydrated reading look current.
- **A write is last-write-wins on `observed_at`, not blind replacement.** The
  `WHERE excluded.observed_at >= burn_readings.observed_at` clause means a retry
  or duplicate delivery can never move a county BACKWARDS to an older reading,
  which would hand the freshness gate a stale stamp and blank a facet that was
  fine. `TestPush_OlderReadingDoesNotOverwriteNewer` pins it.

`line_id` is effectively immutable: it is the row's key, so renaming a line in
config orphans that line's staged reading (and its history under the old id).
