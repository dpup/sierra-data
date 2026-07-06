# DB hosting options (working notes — not a decision yet)

Scratch notes from the deploy discussion. The grid store is a **system of record**
(irreplaceable revision history + resolved/expired events; sources are active-only
so a lost DB does NOT fully rebuild — see `internal/store/CLAUDE.md`). So durability
matters. Options considered:

## 1. (Recommended) local-disk SQLite + Litestream → S3

Keep SQLite unchanged; run it on the task's **local ephemeral disk in WAL mode**
and stream continuously to S3 with Litestream (https://litestream.io).

- **No code change** — WAL is a config flip (`PF__GRID__JOURNALMODE=WAL`), local
  disk is a real FS where WAL works (fast, no NFS corruption risk). Undoes the EFS
  constraint rather than adding to it.
- **Durability + PITR** — Litestream ships WAL frames to versioned S3 at ~1s RPO.
  Lose the task/AZ/everything → next task restores the full DB (history and all)
  from S3 on boot. `restore -timestamp T` recovers to a past moment (corruption out).
- **Clean deploy shape** — Fargate + default 20 GB ephemeral storage; run the app
  *through* Litestream (`litestream replicate -exec "/app/ersn-server"`) so it's one
  container (restore-then-supervise), no sidecar ordering. Needs an S3 bucket + a
  task role with `s3:Put/GetObject`. Cost: pennies.
- **Caveat** — single-writer only (design already is): two tasks must never
  replicate the same S3 path at once → deploys stop-old-then-start-new.

### How a new task restores
Fresh task = empty local `/data`. Entrypoint runs restore BEFORE the app:
```sh
#!/bin/sh
set -e
litestream restore -if-db-not-exists -if-replica-exists /data/grid.db
exec litestream replicate -exec "/app/ersn-server"
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
- Litestream stores periodic **snapshots** + a stream of **WAL segments**. `restore`
  downloads the latest snapshot and replays WAL to the last frame that reached S3 →
  the complete store (every revision, resolved/expired events, all history).
- Flags: `-if-replica-exists` (empty bucket first boot = clean no-op, app makes a
  fresh DB); `-if-db-not-exists` (never clobbers an existing local DB).
- **RPO ≈ 1s** (unshipped WAL frames when the old task died) — at a poll every few
  minutes that's usually 0 writes lost; and the next poll re-fetches current state.
- **Restore gates readiness** — server can't serve until restore completes → set a
  generous ECS health-check grace. Small DB restores in seconds.
- **No two writers** — concurrent replication to one S3 path corrupts the replica;
  ECS deploy `maximumPercent:100 / minimumHealthyPercent:0` (brief redeploy
  downtime, fine here).

Env: `PF__GRID__DBPATH=/data/grid.db`, `PF__GRID__JOURNALMODE=WAL`, local `/data`.

## 2. Managed Postgres (Aurora Serverless v2 / small RDS)

Boring/bulletproof system-of-record: automated backups, PITR, Multi-AZ; stop
thinking about filesystems/corruption.
- **Cost: a real port.** `store` is SQLite-specific (modernc driver, R*Tree geometry
  index → bbox columns + GIST or PostGIS, dialect diffs). Feasible (pgx is pure-Go →
  `CGO_ENABLED=0` survives), ~a day + re-test the store suite.
- **$$** monthly floor (~$15–40) vs pennies — overkill for this volume, but
  unimpeachable durability. Choose only if you'd rather run a managed DB than a
  Litestream sidecar.

## 3. (Do-least) stay on EFS + back it up

Works for single-writer/low-traffic. Turn on EFS automatic backups (AWS Backup),
keep `synchronous=FULL` (already default), accept residual NFS-locking corruption
risk as the thing backups recover from. Simplest, but weakest durability posture.

---
**Lean:** Litestream — keeps the tested code, uses WAL as intended, sidesteps EFS,
turns S3 into durable versioned history for cents. Postgres if a managed DB is
wanted. EFS only to change nothing.
