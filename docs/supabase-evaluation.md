# Supabase as the grid backend — evaluation

Companion to `docs/db-hosting-options.md` (which weighed Litestream vs managed
Postgres vs EFS). This answers the narrower question asked: **is Supabase a
viable backend, and could we stay on the free tier?**

Measured 2026-09-20 against live production (`data.sierragridteam.org`) and the
code at `internal/store` / `internal/ingest`.

**Answer in one line:** Supabase is technically viable *as plain Postgres behind
the existing Go server*, but **the free tier is not** — ingest alone would spend
the 5 GB monthly egress allowance in about **35 hours**. Fixing that is possible
(one caching change), but what remains on free — no backups, a hard 500 MB
ceiling ~1–1.5 years out, no SLA — is strictly worse than the Litestream option
already recommended, on the exact axis (durability of a system of record) that
started this discussion.

---

## 1. Two readings of "Supabase as a backend"

**A. Supabase as the database** — swap SQLite for Supabase Postgres, keep the Go
server, the gRPC/Gateway API, the ingest scheduler, everything. This is the real
option and is what the rest of this doc evaluates.

**B. Supabase as the whole backend** — drop the Go server, serve the API from
PostgREST + Edge Functions. **Not viable, don't spend time on it.** Reasons,
briefly:

- The MeshCore subscriber is a long-lived WSS/MQTT connection with an in-memory
  registry (`meshcore.Registry`, cadence-aware presence). Edge Functions are
  request-scoped; there is nowhere to put it.
- The ten pollers are thousands of lines of Go — KML parsing, ArcGIS query
  hygiene, Ed25519 advert decoding, geometry PIP, OpenAI structured outputs.
  Re-expressing those as pg_cron + pg_net is a rewrite, not a migration.
- The canonical value is a **proto blob**. PostgREST would expose base64 bytea,
  not `grid.v1.Event`. The camelCase protojson contract, keyset pagination,
  ETag/304, and the hand-built RFC 7946 GeoJSON layers would all have to be
  rebuilt — a breaking change for ersn.net and sierragridteam.org.

Everything below is option A.

## 2. The free tier, against this workload

| Free plan limit | Our number | Verdict |
|---|---|---|
| **5 GB egress/mo** | **~104 GB/mo** from ingest alone | ✗ **~20× over** |
| 500 MB database | ~100 MB today, +~25 MB/mo | ⚠ hard wall in ~1–1.5 yr |
| No backups, no PITR | system of record | ✗ **defeats the purpose** |
| Shared CPU / 500 MB RAM | small working set | ⚠ probably fine |
| Pause after 7 days idle | polls every 60 s | ✓ never idle |
| 2 active projects | 1 | ✓ |

Supabase counts Postgres traffic as **Database Egress** under the unified
metric, so this is the meter that matters. Exceeding it on free means a grace
period, then restriction.

### 2.1 The egress blocker, shown

The binding constraint is the **60-second MeshCore tick**, and it is not the
writes — it is two full-blob reads per tick.

`Scheduler.tick` calls `loadPrior` → `ActiveEventsBySource("meshcore")`, which
`SELECT proto, last_seen_at`s **every active mesh event** and unmarshals it.
Then `sweepDisappeared` calls `ActiveEventsBySource` **again**, for the same
source, in the same tick (`internal/ingest/scheduler.go:534`, `:635`, `:667` —
the comment at `:327` already flags this as the untimed cost).

Measured inputs:

- **524** active `MESH` events in production (paginated `/api/v1/events?layer=mesh_node`).
- Per-event compact JSON **mean 3,210 B**; **mean 30 gateway pubkeys** per event
  (30 × 64 hex chars ≈ 1.9 KB of string data carried verbatim into the proto).
  Estimated proto blob ≈ **2.3 KB**.

```
per tick   524 rows × 2.3 KB × 2 reads   ≈ 2.4 MB
per day    × 1,440 ticks                 ≈ 3.5 GB
per month                                ≈ 104 GB      (free tier: 5 GB)
```

The 5 GB allowance is gone in **~35 hours**. Nothing else is close: the nine
non-mesh sources poll at 2–10 min over a few dozen events (~tens of MB/month),
and public API reads are small by comparison and mostly ETag-guarded to 304s.

Note what is *not* driving it. `shouldUpsert` gates on a batched
`ContentHashes` read (~42 KB/tick), and on a steady-state tick nearly every
event is hash-equal so `UpsertEvent` never runs. `TouchSeen` is already
coalesced to a 10-minute window and usually matches nothing. The ingest write
path is already tuned. **All 104 GB is the prior/sweep double read.**

### 2.2 It is fixable — but that is a store change, not a Supabase decision

The scheduler is the only writer (enforced by the store mutex), so it can hold
the active set in memory and serve both `loadPrior` and `sweepDisappeared` from
it, refreshing on write rather than re-reading 524 blobs twice a minute. That
would cut ingest egress to roughly nothing and is worth doing **regardless of
where the DB lives** — on EFS it is exactly the cold-page re-read pattern that
`internal/store/CLAUDE.md` documents as the source of the 1.7–3.5 s latency
spikes on ~7% of requests.

With that fix, free-tier egress becomes plausible: 5 GB/month is roughly 12,500
full event-list reads, and conditional GET keeps repeat traffic at 304s
(`DataVersion` is a `MAX(rowid)` — one cheap round trip). But it is a
prerequisite, not a footnote, and it means "can we fit on free" is really "will
we also do this refactor and keep fitting under it forever."

### 2.3 What is *not* fixable on free

**No backups, no PITR.** This is the one that decides it. The whole reason
`db-hosting-options.md` exists is that the store is an irreplaceable system of
record — sources are active-only, so a lost DB does **not** rebuild resolved
events or revision history. The free plan includes no automatic backups and no
PITR; durability would rest on a `pg_dump` cron we write and monitor ourselves.
Litestream gives ~1 s RPO and restore-to-timestamp for pennies. **Free-tier
Supabase is a downgrade on the exact axis that motivated moving.**

**The 500 MB ceiling, with a dated runway.** From production, one node's history
(`/api/v1/events/{id}/history`): revision 1 at **2026-07-18**, revision 39 at
**2026-09-20** — 64 days, **~0.61 revisions/day/node**. Across 524 nodes at
2.3 KB/snapshot that is **~22 MB/month** of `event_revisions` from mesh alone,
before the other layers, `mesh_link_rollup` (2-year retention),
`mesh_observations` (48 h) and `mesh_telemetry` (1-year retention). Blobs sit
just under Postgres' 2 KB TOAST threshold, so they mostly store inline
uncompressed — call it ~25–30 MB/month with row overhead. **~1–1.5 years to the
cap**, and on free there is no way to buy disk: the project goes read-only.

**No SLA, shared instance.** This service carries evacuation and fire data. The
fail-loud design means an unreachable DB reports `UNAVAILABLE` rather than
fabricating an all-clear — the invariant holds — but running life-safety data on
a best-effort free tier with no backups is a posture call, not a cost call.

## 3. The port cost

`internal/store` is ~2,600 lines of non-test Go plus ~2,600 lines of tests, and
most of the SQL is portable — `ON CONFLICT … DO UPDATE` is already Postgres
syntax. What actually has to change:

- **R\*Tree.** `event_geo` is a SQLite virtual table with an `event_geo_map`
  rowid indirection (`internal/store/events.go:576`). Becomes PostGIS GiST, or
  plain btree on the four bbox columns. PostGIS is the better end state — it
  could eventually absorb the hand-rolled PIP in `lib/geojson`.
- **`rowid`.** `DataVersion` is `MAX(rowid)` over `event_revisions` — the ETag
  validator. Needs an explicit sequence or xid equivalent, and it must stay
  cheap and monotonic.
- **Placeholders.** `?` → `$n` throughout.
- **PRAGMAs.** `Settings`/`Analyze` (`store.go:549`, `:594`) are SQLite-specific;
  `ANALYZE` exists but the journal/cache/synchronous reporting does not.
- **`INSERT OR REPLACE`**, BLOB → bytea, the hand-rolled migration ladder (v1–v5).

`db-hosting-options.md` estimated "~a day"; with the geometry index and a full
re-run of the store suite, **1–3 days** is more honest. `pgx` is pure Go, so the
`CGO_ENABLED=0` cross-compile survives — that constraint is not a problem.

## 4. What Postgres would genuinely buy us

Being fair to the idea, two real wins, neither specific to Supabase:

- **MVCC removes reader-blocks-writer.** Today the store runs the TRUNCATE
  rollback journal (WAL is unsafe on EFS), so readers have no MVCC and wait on
  the writer's commit via `busy_timeout(5000)`. Postgres readers never block.
  This, plus killing the cold-page re-read, addresses the documented latency
  spikes at the root.
- **PostGIS** is a real spatial index with real predicates, replacing an R\*Tree
  bbox filter plus Go-side containment.

## 5. Recommendation

**Don't move to Supabase free.** It fails on egress by ~20× as the code stands,
and the version that passes still has no backups, a dated 500 MB wall, and no
SLA under a system of record.

**Supabase Pro ($25/mo) would work** — 250 GB egress absorbs even the unfixed
ingest pattern, 8 GB disk, daily backups with 7-day retention. But at that point
compare it to what `db-hosting-options.md` already recommends: Litestream keeps
the tested SQLite code, needs no port, gives ~1 s RPO with restore-to-timestamp,
and costs pennies. Supabase Pro is $300/year to get *less* PITR granularity plus
a 1–3 day port. And every Supabase differentiator — PostgREST, Auth, Realtime,
Storage, Edge Functions — goes unused here: the API is keyless, read-only,
proto-defined and already built. We would be buying plain Postgres with extra
steps, and if we ever do want plain Postgres, RDS/Aurora/Neon compete directly.

**Three things worth taking from this exercise regardless:**

1. **Fix the prior/sweep double read** (§2.2). It is ~104 GB/month of
   unnecessary reads against *any* backend and it is the mechanism behind the
   EFS latency spikes already documented in `internal/store/CLAUDE.md`.
2. **Watch `event_revisions` growth** — ~25 MB/month and compounding. Fine on
   Litestream/S3; it is what would eventually force a retention policy.
3. If a managed Postgres is ever wanted, **Supabase free is a fine place to
   prototype the port** (PostGIS, pgx, the migration ladder) without paying for
   it — just don't run production on it.
