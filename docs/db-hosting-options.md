# DB hosting options (working notes — not a decision yet)

Scratch notes from the deploy discussion. The grid store is a **system of record**
(irreplaceable revision history + resolved/expired events; sources are active-only
so a lost DB does NOT fully rebuild — see `internal/store/CLAUDE.md`). So durability
matters. Two goals now drive this: **durability**, and **blue/green (zero-downtime)
deploys** — today every deploy is a few minutes of downtime.

## Why deploys go down today

Not SQLite as such: **the only process allowed to serve reads is the one holding
the writer lock.** `store.Open` takes an exclusive `flock` before the listener
opens, so a deploy is strictly serial:

1. ECS deregisters the old task; the ALB drains it for the deregistration delay
   (AWS default **300s**) while it still holds the lock.
2. SIGTERM; prefab exits in ~2s; the lock is released.
3. The new task acquires, listens, and must pass the target group's healthy
   threshold (default 5 × 30s ≈ **150s**) before it gets traffic.

**Quick win, no code:** deregistration delay → ~10s, health check 5–10s interval /
threshold 2. Cuts the outage to ~20–30s; worth doing whatever else is chosen.
True zero downtime needs the read path to stop depending on the writer lock.

## What actually writes, and what a write gap costs

| Write path | During a write pause |
|---|---|
| Pollers (most data) | Nothing lost — pull-based snapshots; the next tick re-fetches current state |
| MQTT adverts | A few `mesh_observations` rows; nodes re-advert |
| `POST /ingest/mesh.repeater` | Buffered in memory (lost on any restart); reporters retry 5xx |
| `POST /ingest/burn.line` | Once a day from CI, and `cmd/burn-line` does **not** retry |

So a pause of seconds is harmless for pollers. Only push ingest needs buffering,
and it is the one path where a request can land on any task — that is where SQS
earns a place (below). SQS does nothing for reads.

## Option A — shared EFS file, writer lease moves at cutover

Every task opens the EFS database for reading at boot and serves immediately;
the scheduler, MQTT and push ingest run only in the task holding a **writer
lease** (the existing `flock`, waited on with no timeout). Green signals readiness,
blue yields the lease but keeps serving reads until ECS stops it; if green dies,
blue re-acquires. No new infrastructure.

**Spike: two OS processes, one file, production DSN** (TRUNCATE, `synchronous=FULL`,
`busy_timeout(5000)`, modernc driver). The reader checks invariants inside each read
transaction (balances sum, no torn multi-page row, counter == row count), so wrong
data without an error is detectable.

| Scenario | OK reads | Silently wrong | Errors | DB after |
|---|---|---|---|---|
| `mode=ro` reader vs writer at ~1,200 commits/s | 83 | 0 | 0 | intact |
| `query_only` reader, same | 145 | 0 | 0 | intact |
| Writer `ALTER TABLE` + `CREATE INDEX` mid-run | 161 | 0 | 0 | intact |
| Writer SIGKILLed mid-txn, `mode=ro` reader | 6 | 0 | 528k × `readonly database (776)` | intact once a writer reopened |
| Writer SIGKILLed mid-txn, `query_only` reader | 5,746 | 0 | 0 | intact (reader rolled the hot journal back) |
| **Locks not honoured** (`vfs=unix-none`), `mode=ro` | 7,308 | **1,208** | 91k | intact |
| **Locks not honoured**, `query_only` | ~0 | some | 890k × `malformed (11)` | **corrupt, 3/3 runs** |

Findings:

- **With working locks, reads fail loud** — no silent wrong data under load, a
  migration, or a writer crash.
- **Readers must be read-write connections with `query_only=1`, not `mode=ro`.** A
  writer killed mid-transaction leaves a hot journal that a `mode=ro` connection
  may not roll back: every read errors 776 until a writer reopens the file.
- **With broken locks, reads are NOT loud** (~14% silently wrong), and a
  self-healing `query_only` reader rolls back a live writer's journal and corrupts
  the database. Everything rests on EFS honouring locks across tasks — the
  existing `flock` guard already assumes this, but it is unverified on EFS, and the
  virtiofs dev mount is a filesystem that did not.
- **Retries:** only `SQLITE_BUSY` (past `busy_timeout`) is worth retrying. Never
  silently retry `CORRUPT (11)` or `READONLY_ROLLBACK (776)` — with working locks
  those mean real corruption or a misconfigured reader; fail the request and the
  health check.
- **Lock canary:** the writer stamps a heartbeat row; a task that acquires the
  writer `flock` *instantly* while the heartbeat is fresh has proven cross-task
  locking broken and must refuse the writer role.

Unverified: the same binary run across two Fargate tasks on the production EFS
mount. Keeps EFS and its latency problems (`internal/store/CLAUDE.md`).

## Option B — (leaning) local-disk SQLite + Litestream, SQS for push, a lease

Each task has its **own local copy**. The writer runs SQLite in WAL on local disk
under `litestream replicate`; every other task runs `litestream restore -f`
(follow mode, v0.5.9+, crash-safe resume since v0.5.10) and reads its copy. Removes
EFS entirely — the cold-reads-over-NFS saga, the WAL-on-EFS constraint, the lock
dependency — and S3 gives durability + point-in-time restore.

- **Push ingest → SQS** from any task; only the lease holder consumes. Delete a
  message only after the tick that persists it. Standard queues are at-least-once
  and unordered: burn readings are already last-write-wins on `observed_at`; mesh
  reports must key on the monitor's `reported_at` (or use FIFO). Local dev needs an
  in-process queue behind the same interface.
- **A lease picks the single writer** — the `flock` cannot, each task has its own
  disk. S3 conditional writes (`If-Match`) or DynamoDB, with a TTL; a writer that
  cannot renew stops writing. Two tasks replicating to one S3 path corrupt the
  replica, so this is load-bearing. Litestream provides none.
- **Deploy = pause writes and hand over:** blue stops the scheduler and SQS
  consumption, Litestream ships its last frames, blue releases the lease; green's
  follower catches up to blue's final TXID, stops following, takes the lease, and
  starts `replicate` on its copy. Pushes wait in SQS; pollers skip a tick.
- **Ruled out: the Litestream VFS** (query S3 directly) — needs CGO and a CGO SQLite
  driver, which breaks the `CGO_ENABLED=0` build.

**Spike: Litestream v0.5.17, `file` replica backend standing in for S3**, same
invariant checker and driver.

| Test | Result |
|---|---|
| Read the follower copy while follow mode updates it (paced and ~3,400 commits/s writers) | ~55k reads, **0 wrong, 0 errors**, worst 39ms; follower ended at exactly the writer's final count |
| Handover: blue SIGTERM, green catches up, promotes, writes | blue's SIGTERM shipped its final frames; green caught up in **35ms–1.1s**; a fresh restore got green's full final state |
| A third task following blue, then green | carried on across the handover with no gap or error |
| Lag | ~1–2s at the default 1s follow interval |

Follow mode writes pages directly, but takes SQLite's own PENDING + SHARED lock
bytes first (`internal/lock_unix.go`) and rewrites the header to DELETE mode, which
is why readers see whole transactions.

**The trap — a follower can serve frozen data with no error.** A freshly restored
file's header says **WAL** (bytes 18–19 = 2). A reader that opens it switches to WAL
and creates `-wal`/`-shm`; in WAL mode every open connection holds a shared lock
for its lifetime, and our pool keeps connections forever (idle == open). Follow
mode then never gets its exclusive lock: the reader served one snapshot for 12s
with no error, even at 50 reads/s. **Fix (tested):** before the server opens the
file, set header bytes 18–19 to `1,1` under SQLite's PENDING + SHARED locks — the
same edit follow mode makes itself — after which updates applied every ~1s under
steady reads. Not reported upstream as of 2026-10-14.

Rules this surfaced:

- **Never open the follower DB before the initial restore finishes** — wait for the
  `-txid` sidecar. An early open made SQLite create an empty file, and follow mode
  then refused to resume (`database exists but no -txid file`).
- **Reader connections: read-write with `query_only=1`**, as in option A.
- **Freshness is fail-loud, not assumed.** Follow mode has open bugs where it stops
  progressing silently (litestream#1515, #1570). The writer stamps a heartbeat row
  each tick; readers check its age, report STALE on `/sources`, and fail the health
  check past a threshold — the PG&E / burn-line freshness-gate pattern.

Cost: the writer uploads only on change (a few commits a minute — under $1/month).
Each follower polls S3 once per interval: several dollars a month per reader at 1s,
a fifth of that at `-follow-interval 5s`, which is plenty for this API. SQS at our
volume is inside the free tier.

Unverified: real S3 latency and request counts, the handover under ECS
(deregistration ordering, SIGTERM timing), and the lease.

### Restoring on boot (writer, or a fresh follower)
```sh
#!/bin/sh
set -e
litestream restore -if-db-not-exists -if-replica-exists /data/grid.db
exec litestream replicate -exec "/app/sierra-server"
```
```yaml
# /etc/litestream.yml
dbs:
  - path: /data/grid.db
    replicas:
      - type: s3
        bucket: sierra-grid-db
        path: grid.db
        region: us-west-2
```
- Litestream stores periodic **snapshots** + a stream of LTX files. `restore`
  downloads the latest snapshot and replays to the last transaction that reached
  S3 → the complete store (every revision, resolved/expired events, all history).
- `-if-replica-exists` (empty bucket on first boot = clean no-op, app makes a fresh
  DB); `-if-db-not-exists` (never clobbers an existing local DB).
- **RPO ≈ 1s** (the sync interval). Restore gates readiness — give the health check
  a generous grace. Needs an S3 bucket and a task role with `s3:Put/Get/List`.
- Env: `PF__GRID__DB_PATH=/data/grid.db`, `PF__GRID__JOURNAL_MODE=WAL`, local `/data`.

## Considered and set aside

- **Managed Postgres (Aurora Serverless v2 / small RDS)** — makes zero-downtime
  trivial (many readers, `pg_advisory_lock` for the writer) and durability boring.
  Rejected on **hosting cost** (~$15–40/month floor vs pennies) plus a real port of
  `store` (R*Tree → GIST/PostGIS, dialect differences).
- **LiteFS** — built for exactly this handoff, but needs FUSE, which Fargate does
  not support.
- **SQS for everything** — pollers are snapshots and lose nothing in a pause, so a
  queue in front of them buffers nothing; it is only worth it for push ingest.
- **A DB per color (copy at deploy, both colors ingest during the bake)** — true
  isolation and clean rollback, but doubles AI-enhancement and upstream API spend
  for every deploy and needs a "which DB is canonical" pointer. Not worth it for a
  bake of seconds to minutes.
- **Do-least: stay on EFS + AWS Backup** — keeps the downtime and the EFS latency;
  durability only.

---
**Lean:** B (Litestream + SQS for push + a lease), with the header fix and the
heartbeat freshness check built in from the start. A if a faster route is wanted
and the EFS multi-reader test passes on Fargate. Next step either way: run the
spike against a real S3 bucket / the EFS mount from Fargate before building.
