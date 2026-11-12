# Deploys: readers, the writer lease, and the control API

The Grid (`sierra-server`) runs as one or more identical processes on one
PostgreSQL database. **Every process serves every read from boot.** At most
one process at a time is the **writer**: it holds the writer lease and runs
everything that writes (the ingest scheduler and its pollers, the MeshCore
subscriber, push-ingest buffers, source and place seeding, schema
migrations, upstream cache warming). A deploy is therefore an ordinary
blue/green deploy: the new processes start serving reads at once, and the
writer role moves when the old writer goes away (SIGTERM) or is told to
yield.

This replaces the standby/quiesce/activate handoff the legacy stack needed
because only the database owner could serve. That handoff still exists (the
Grid remaps it onto the roles, below) so a coordinator can drive the Grid if
migration route B is taken; the native path needs none of it. The mesh relay
(`cmd/mesh-relay`) keeps the old handoff unchanged: it stays one process on
its own SQLite file until it moves (migration plan R4).

The legacy stack (SQLite on EFS, the coordinator) is frozen on
`legacy/efs-sqlite`; deploy it only from a checkout of that branch with
`make ecs-deploy` (see the top-level `AGENTS.md`).

Code: `internal/store/lease.go` (the lease and fencing),
`cmd/server/grid.go` (the reader: `Serve`, `Exit`), `cmd/server/writer.go`
(the writer role and the remap), `cmd/server/ingestroute.go` (push
forwarding), `internal/handoff` (the lifecycle and control API).

## Roles

**Reader (every process, from boot to exit).** `store.OpenReader` opens the
pool, checks the collation and that the database schema is at least the
version this build reads, and takes no lock. The production listener starts
right after. A reader writes nothing; every write path in the store refuses
with `ErrNotWriter`. ETag validators come from the `versions` table (below),
so every process returns the same ETag for the same data.

**Writer (the lease holder).** `store.AcquireLease`:

1. takes `pg_try_advisory_lock` on a dedicated connection (taken from the pool
   and hijacked out of it), polling every 250 ms;
2. bumps `writer_lease.epoch` and records `holder` (`<instanceId>@<hostname>`)
   and `address` (where other processes reach this one);
3. applies pending migrations (migrations run only here);
4. then the process seeds sources and places and starts the scheduler, the
   MeshCore registry, push ingest and cache warming (a **term**).

The holder refreshes `writer_lease.heartbeat_at` every 2 s on the lease
connection.

## Fencing

The advisory lock alone does not stop a writer whose session has died (an RDS
failover, a network cut) but who has not noticed yet: its pooled connections
would keep writing after another process took the lock. So every write
transaction (`inTx`) starts with

```sql
SELECT 1 FROM writer_lease WHERE id = 1 AND epoch = $mine FOR KEY SHARE
```

and aborts with `ErrFenced` on zero rows. A new holder bumps the epoch with
`SELECT ... FOR UPDATE`, which waits for any write transaction of the old
term that has already passed the check; that transaction commits as the old
term's last write, and every later one fails. Key-share does not conflict
with the holder's own heartbeat (a no-key `UPDATE`), so a long write never
stalls the heartbeat. Tested in `internal/store/lease_test.go`
(`TestStaleEpochIsFenced`, `TestEpochBumpWaitsForInFlightWrite`).

## Losing the lease

The heartbeat fails when the lease connection's session is gone (error) or
the epoch has moved (zero rows). Either marks the lease lost: `inTx` refuses
from that moment, the connection is closed (which releases the lock
server-side if it was not already gone), and the writer role stops the term
(scheduler, MQTT, background work), keeps serving reads, and campaigns again
after a backoff (1 s, doubling to 15 s; reset after a term that lasted a
minute). RDS failover or maintenance drops every session: every process's
reads reconnect through the pool, the writer loses its lease within about 2 s,
and whichever process wins the next campaign writes. Pollers lose nothing
(they re-fetch); pushes get 503 and retry.

## Push ingest on a reader

`POST /api/v1/ingest/{stream}` is mounted on every process (when reporters
are configured). The writer serves it from its term's registry. Any other
process reads `writer_lease` and, when a holder is recorded with a heartbeat
younger than 10 s, forwards the request (method, URI, `Authorization`,
`Content-Type`, body) to `http://<address>` within
`grid.writer.forwardTimeout` and relays the status, body and the
`Content-Type`, `Retry-After`, `WWW-Authenticate` and `Allow` headers. It
answers `503` with `Retry-After: 2` when there is no live writer, when the
forward fails, when the writer is its own process still starting its term,
and when the request already carries `X-Grid-Forwarded-By` (a forwarded
request is never forwarded again). Reporters retry 5xx already.

The reader does not authenticate the request itself: the writer does, once.

## Validators (ETags)

The validators are the `versions` counters (bumped in the writing
transaction), a short hash of the running executable (a new build may render
the same data differently; the image has no `.git`), and the place-set
version (bumped when a writer term reseeds). Each process caches the counters
and refreshes them

- after each of its own commits (the writer is exact),
- on `NOTIFY grid_versions` (a statement trigger on `versions`; delivered at
  commit) on a dedicated `LISTEN` connection, and
- on a 10 s poll, so a reader with a broken listener answers a stale `304`
  for at most 10 s after a write.

Note: the gRPC handlers set `ETag` and `Cache-Control`, but the gateway does
not copy them to the HTTP response today (pre-existing, also on the legacy
build; prefab's outgoing header matcher). Comparing validators across
processes therefore needs a gRPC client for now.

## Configuration

| key | env | default | |
|---|---|---|---|
| `grid.writer.enabled` | `PF__GRID__WRITER__ENABLED` | `true` | `false`: a reader-only process that never campaigns (a bake against a database another stack writes; extra read capacity) |
| `grid.writer.advertiseAddr` | `PF__GRID__WRITER__ADVERTISE_ADDR` | `server.host` (the hostname when it is `0.0.0.0`) + the bound port | where other processes reach this one for forwarded pushes |
| `grid.writer.forwardTimeout` | `PF__GRID__WRITER__FORWARD_TIMEOUT` | 10s | bound on one forwarded push |
| `grid.database.lockTimeout` | `PF__GRID__DATABASE__LOCK_TIMEOUT` | 90s | how long `POST /activate` (coordinator mode) waits for the lease; a boot campaign has no deadline |
| `grid.control.port` | `PF__GRID__CONTROL__PORT` | 8081 | control listener |
| `grid.control.tokenSha256` | `PF__GRID__CONTROL__TOKEN_SHA256` | empty (off) | hex SHA-256 of the control token |
| `grid.control.drainTimeout` | | 10s | in-flight request grace at exit |
| `grid.control.workerStopTimeout` | | 10s | one budget for the term's workers to stop (a running tick gets 2s); past it the release fails and `databaseHeld` stays true |

**On ECS** the address must be the task's private IP. With `awsvpc`
networking the hostname is the task's private DNS name, which the default
already uses. To be explicit, the entrypoint (or a future change) can read
the task metadata endpoint (`$ECS_CONTAINER_METADATA_URI_V4/task`,
`Containers[].Networks[0].IPv4Addresses[0]`) and set
`PF__GRID__WRITER__ADVERTISE_ADDR=<ip>:8080`. The server does not do this AWS
lookup itself. The security group must allow the tasks to reach each other
on the production port.

## Health

On the production port (and the control port):

- `GET /livez`: 200 while the process runs.
- `GET /readyz`: 200 when the store is open at a schema this build reads and
  the process is not exiting; the body says whether it is the writer
  (`{"ready": true, "writer": false}`). The writer role never enters into
  readiness: a reader is ready. 503 while the schema is older than the build
  (until a writer migrates it) and once SIGTERM has started the exit.

The Dockerfile `HEALTHCHECK` probes `/readyz` with a 15 s start period. The
load balancer's target check should use `/readyz` too.

## SIGTERM

1. The writer role stops: campaigning ends, the term's workers stop (within
   `workerStopTimeout`), and the lease is released, clearing `holder` and
   `address` so readers answer `503` at once rather than forward to a
   stopping process. Measured locally: about 250 ms, almost all of it closing
   the MeshCore subscriber.
2. `/readyz` turns 503, the admission gate closes (`503`, `Retry-After: 1`) and
   in-flight requests drain (`drainTimeout`).
3. The store is closed and the process exits 0 (1 if the release failed;
   process exit releases the lock regardless).

prefab stops its HTTP listener on SIGTERM itself, in parallel with step 1, so
a push forwarded to a stopping writer fails and is answered `503` by the
forwarding reader. A second signal exits at once with 1.

## Control API (optional)

Runs on `grid.control.port` only when `grid.control.tokenSha256` is set.
Never route it from the public load balancer.

| Endpoint | Auth | |
|---|---|---|
| `GET /status` | none | `state`, `ready` (the read path), `databaseHeld` (holds the writer lease, or is still stopping a term), identity (`instanceId`, `pid`, `hostname`, `revision`, `startedAt`), the current/last `operation`, `ecs` (task metadata, reported raw) |
| `GET /livez`, `GET /readyz` | none | as above |
| `POST /yield` | bearer | release the lease and stop campaigning until `/campaign`; keep serving reads. 200 when done |
| `POST /campaign` | bearer | resume campaigning after a yield; only in `ACTIVE` (409 otherwise) |
| `POST /activate` | bearer | coordinator mode, below |
| `POST /quiesce` | bearer | release the lease for good (`QUIESCED`); keep serving reads until SIGTERM |

Commands take `Authorization: Bearer <token>` and an optional body
`{"operationId": "...", "expectedInstanceId": "..."}`; a set
`expectedInstanceId` that is not this process is refused with 409 before
anything happens. Generate a token pair with
`t=$(openssl rand -hex 32); printf %s "$t" | sha256sum`.

**Yield** is how to move the writer before the old task set is stopped, for
example for a release that adds a migration: the new build cannot be ready
(its schema check fails) until a writer runs the migration, and only the
writer migrates. Yield the old writer (and any other old process, or one of
them may win the campaign instead); a new process takes the lease, migrates
and becomes ready; the old processes keep serving reads (additive
migrations only: they read the expanded schema fine).

## The coordinator remap (route B)

`internal/handoff`'s states are unchanged; the Grid maps them onto the roles:

| state | meaning for the Grid |
|---|---|
| `STANDBY` (`--standby`) | a reader that does not campaign; serves reads |
| `ACTIVATING` | `POST /activate`: waiting up to `lockTimeout` for the lease, then starting the term; on failure it releases and returns to `STANDBY` |
| `ACTIVE` | holds the lease, or (after a lost lease or a yield) campaigns for it; `databaseHeld` says which |
| `QUIESCING` / `QUIESCED` | `POST /quiesce` or SIGTERM: the lease released for good; still serving reads until exit |

Without `--standby` the process activates at boot, which for the Grid means
"campaign in the background": `ACTIVE` at once, holding the lease as soon as
it is free. The coordinator's sequence (standby the new task, quiesce the old,
activate the new, switch traffic, terminate the old) and its
`expectedInstanceId` contract still work. What changed for it: a standby
already serves reads (it can take traffic before activation), and quiesce no
longer stops reads (the admission gate closes only at exit). The writer lock
is the PostgreSQL lease, never a file lock.

## Rehearsal (migration gate 3)

`make rehearse-bluegreen` builds the server, starts the Docker database and
runs `scripts/rehearse-bluegreen.sh` on a throwaway database
(`REHEARSE_DB`, default `sierra_rehearsal`, dropped and recreated) with two
servers on ports `REHEARSE_PORT_A/B` (8197/8198):

1. A starts and takes the lease; B starts as a ready reader, serves reads and
   forwards a push to A.
2. Under steady load on B (reads, and preflight pushes that retry on 503 like a
   reporter), A gets SIGTERM; B must take the lease.
3. A returns as a reader; under load on A, B gets SIGKILL; A must take over.
4. The epochs must be 1, 2, 3 with one lock holder; no read may fail and no
   push may fail after its retries; each takeover must be under
   `REHEARSE_TAKEOVER_MAX_MS` (5 s).

Local results (2026-11-12, three runs): SIGTERM takeover 506-513 ms, SIGKILL
takeover 62-289 ms, no failed reads or pushes, one push retried once per
switch.

## Timing logs

Each step logs a duration: `Grid store opened (reader)`, `Serving reads`,
`Campaigning for the writer lease`, `Writer lease acquired` (`waited`),
`Seeded grid sources and places`, `Writer term started`, `Writer lease LOST`,
`Writer stop: ...` (scheduler, MeshCore, background work, lease released),
`Quiesce: writer lease released; still serving reads`, `Exit: request drain
complete`, `Shutdown complete`, `Process exiting` (`state`, `databaseHeld`,
`exitCode`). Forwarded pushes log `Push ingest: forwarded to the writer`
(`writer`, `epoch`, `status`, `duration`).
