# Deploy handoff: standby startup and explicit database ownership

The Grid (`sierra-server`) and the mesh relay (`mesh-relay`) each hold an
exclusive lock on their own SQLite file (`grid.db`, `relay.db`). Without
handoff, a rolling deploy is stop-then-start: the replacement cannot open the
database until the old task has drained, been signalled, exited and been
deprovisioned, and all of that is downtime (measured ~1m35s, most of it
platform orchestration rather than our code).

With handoff, a deployment coordinator:

1. starts the replacement with `--standby` (config loaded and validated,
   nothing opened),
2. tells the old process to **quiesce** (stop admitting, stop every database
   user, close the database, release the lock) and confirms the release,
3. tells the replacement to **activate**,
4. confirms readiness and switches load-balancer traffic,
5. terminates the old task afterwards, at leisure.

Provisioning and deprovisioning both move outside the downtime window. What
remains is quiescence plus activation (milliseconds to a few seconds locally;
dominated in production by the in-flight drain and the store open over the
network filesystem).

The code is `internal/handoff` (state machine, control API, admission gate),
`cmd/server/grid.go` and `cmd/mesh-relay/main.go`.

**Which branch deploys.** This stack is frozen on `legacy/efs-sqlite`
(`main` is the PostgreSQL stack and cannot run here). `make ecs-deploy`
builds only from a checkout of that branch whose HEAD matches
`origin/legacy/efs-sqlite`; it refuses `main`, a detached HEAD, unpushed
commits and a stale checkout, before any image is built. Hotfix on the
legacy branch, merge, pull, deploy, then forward-port to `main`.

## Flags and configuration

```
sierra-server [--standby]
mesh-relay [--standby]
mesh-relay token -name <admin>     # unchanged
```

`--standby` defaults to false: without it a process activates at boot exactly
as before, and SIGTERM runs the same quiescence described below before exit.

| key | env | default | |
|---|---|---|---|
| `grid.control.port` | `PF__GRID__CONTROL__PORT` | 8081 | Grid control listener |
| `grid.control.tokenSha256` | `PF__GRID__CONTROL__TOKEN_SHA256` | empty (off) | hex SHA-256 of the deployment token |
| `meshRelay.control.port` | `PF__MESH_RELAY__CONTROL__PORT` | 8091 | relay control listener (must differ from the Grid's whenever the two processes share a network namespace) |
| `meshRelay.control.tokenSha256` | `PF__MESH_RELAY__CONTROL__TOKEN_SHA256` | empty (off) | |
| `*.control.drainTimeout` | | 10s | in-flight request grace before cancellation; this is downtime |
| `*.control.workerStopTimeout` | | 10s | one budget for every background database user to stop (a running ingest tick gets 2s, then is cancelled); past it the release FAILS. With `drainTimeout`, a quiesce takes at most ~20s, inside ECS's default 30s stop timeout |

The control listener runs only when `tokenSha256` is set, with or without
`--standby`. `--standby` without it refuses to start (a standby nobody can
activate). Only the hash is configured; the token stays with the coordinator.
Generate a pair with `t=$(openssl rand -hex 32); printf %s "$t" | sha256sum`.

**The control ports must never be reachable from the public load balancer.**

## Lifecycle

```
STANDBY ──activate──▶ ACTIVATING ──ok──▶ ACTIVE ──quiesce──▶ QUIESCING ──ok──▶ QUIESCED
   │                      │ failed                              │ failed
   │                      ▼                                     ▼
   │                   STANDBY                      QUIESCING (database counted as HELD)
   └────────────quiesce (fence: never opened anything)────────────────────────▶ QUIESCED
```

- **STANDBY**: configuration loaded and validated (including the push-ingest
  reporter hashes and relay admin hashes), read-path services constructed. No
  database file is opened, created, migrated, seeded or analyzed; no ingest
  scheduler, MQTT subscriber, upstream cache warming or relay session runs;
  the production port is not bound.
- **ACTIVATING**: opening the database (the file lock is taken here, and waits
  up to `lockTimeout` for a holder to let go), seeding, starting background
  work, binding the production listener. ACTIVE is reported only once the
  production port accepts connections.
- **ACTIVE**: serving production. The faster-boot behaviour applies here:
  persisted index statistics are reused (the refresh runs 10 minutes later),
  and the MeshCore neighbour graph is rebuilt in the background.
- **QUIESCING / QUIESCED**: see the contract below. QUIESCED is terminal: the
  process stays alive (so the platform can clean it up whenever) and never
  reopens its database.

A failed activation (including a panic during it) releases what it opened
and returns to STANDBY. If it cannot release, it goes to QUIESCING with a
failed operation and `databaseHeld: true` instead. Activation also refuses to
start when its production port is already bound, so it can never report
ACTIVE on the strength of another process's listener; and the lock wait ends
as soon as the activation is cancelled (SIGTERM), rather than after
`lockTimeout`.

## Quiescence contract

A successful quiescence (state QUIESCED, `databaseHeld: false`) means, in
order:

1. **New work is not admitted.** Every production route is behind an
   admission gate that now answers `503` with `Retry-After: 1` (gRPC
   `UNAVAILABLE`). prefab owns the production listener and cannot stop it
   short of a signal, so the port stays bound in QUIESCED; it just refuses.
   Load balancer health checks against it fail, which is what you want.
2. **In-flight requests drained or were cancelled.** They get `drainTimeout`
   to finish, then their contexts are cancelled. A handler that ignores
   cancellation is logged; step 5 still cuts it off from the database.
3. **Background database users have stopped and their goroutines have
   returned**: the ingest scheduler's pollers, mesh maintenance and the
   statistics refresh (a running tick gets 2s, then is cancelled; a cancelled
   fetch is a failed poll, which writes nothing), the MeshCore subscriber, and
   the neighbour-graph rebuild.
4. **(Relay) every session is closed and the audit queue is flushed.** Leases
   end with `relay_shutdown`, bridges get close code 1001, every websocket
   handler has returned, and no lease or bridge can attach behind the sweep.
5. **The SQLite connections are closed and the file lock released.** Any
   straggler gets `sql: database is closed`, never a query.

If any step cannot complete, the operation fails, the state stays QUIESCING,
and `databaseHeld` stays true. **That is an ambiguous handoff**: do not
activate the replacement. Either retry the quiesce with a new operation id or
terminate the old task and confirm it is gone.

## Control API

| Endpoint | Auth | |
|---|---|---|
| `GET /status` | none | state, `prepared`, `ready`, `databaseHeld`, `standby`, identity (`instanceId`, `pid`, `hostname`, `revision`, `startedAt`), the current/last `operation`, `activatedBy`, `quiescedBy`, and `ecs` (below) |
| `GET /livez` | none | 200 whenever the process and its control plane are up, standby included |
| `GET /readyz` | none | 200 only when ACTIVE; 503 otherwise |
| `POST /activate` | bearer | body `{"operationId": "...", "expectedInstanceId": "..."}` |
| `POST /quiesce` | bearer | body `{"operationId": "...", "expectedInstanceId": "..."}` |

Mutating calls take `Authorization: Bearer <token>` (401 without it). Both run
asynchronously: `202 Accepted` with the operation, then poll `/status` until
the operation's `status` is `succeeded` or `failed` (`error` says why). Every
response carries `state`, `instanceId` and `operation`; conflicts carry
`error: {code, message}` with HTTP 409.

**`expectedInstanceId` addresses one process incarnation.** Every process
gets a random `instanceId` at start; a container restart comes back at the
same IP and port with a new one. When the field is set and does not match,
the command is refused with 409 `instance_mismatch` **before** anything else
is considered, in every state, idempotent replays included, so a delayed
command meant for a previous incarnation can never activate or quiesce its
successor. Read `instanceId` from `/status` and send it on every command; it
is optional only so a hand-run command still works.

| state | `POST /activate` | `POST /quiesce` |
|---|---|---|
| STANDBY | 202, starts | 200, **fences**: QUIESCED without ever opening anything |
| STANDBY after a failed activation | same id: 409 `operation_failed`; new id: 202, retries | 200, fences |
| ACTIVATING | same id or none: 202; other id: 409 `activation_in_progress` | 409 `activation_in_progress` |
| ACTIVE | same id or none: 200; other id: 409 `already_active` | 202, starts |
| QUIESCING (running) | 409 `released` | same id or none: 202; other id: 409 `quiescence_in_progress` |
| QUIESCING (failed) | 409 `released` | same id: 409 `operation_failed`; new id: 202, retries |
| QUIESCED | 409 `released` | 200 (release is a terminal fact) |

Use one operation id per deployment step (`<deploy>-release`,
`<deploy>-activate`), and reuse it on retries of the same request.

### ECS identity (`/status.ecs`)

On ECS the process reads the task metadata endpoint (v4,
`ECS_CONTAINER_METADATA_URI_V4`) every 10 seconds for its whole life and
reports the raw values: `taskArn`, `cluster`, `family`, `revision`,
`availabilityZone`, `containerName`, `containerArn`, `dockerId`,
`containerStartedAt`, `restartCount`, with `fetchedAt` (when those values
were read), `lastAttemptAt` and `lastError`. Nothing is inferred from them.

**The metadata does not identify the process; `instanceId` does.** On a
Fargate in-place container restart (restart policy), ECS kept
`containerStartedAt`, `dockerId` and `containerArn` unchanged, and only
`restartCount` advanced (0 → 1; observed in a Fargate restart test on
2026-11-04). An earlier version derived a
`boundToProcess` flag from start-time proximity; that accepted a
predecessor's metadata and has been removed. So the binding contract is the
coordinator's, using three pieces of evidence together:

1. **Which task and container answered:** `taskArn` + `containerName` match
   the target the coordinator addressed.
2. **Whether ECS restarted it:** `restartCount` compared with the value the
   coordinator recorded for the previous owner. A larger count means an
   in-place restart since. It is only as fresh as `fetchedAt`, and ECS may lag
   a restart, so read it again rather than trusting one sample.
3. **Which process it is:** `instanceId`, pinned on every command with
   `expectedInstanceId`. This is the only process-generation proof.

None of these authorizes activation on its own, and none replaces a confirmed
release (or confirmed termination) of the previous owner.

- `restartCount` is `null` when ECS does not report it (no restart policy):
  unknown, never 0.
- A failed refresh keeps the last good values with `lastError` set and an
  `lastAttemptAt` later than `fetchedAt`; treat them as that old.
- Off ECS (local runs) or before any successful read: `available: false` with
  a `reason`. Nothing else depends on it.

## SIGTERM

The same quiescence runs on SIGTERM in every state, then the process exits:

| state | on SIGTERM |
|---|---|
| STANDBY | exit 0; nothing was opened |
| ACTIVATING | the activation is cancelled and releases what it opened; exit 0 |
| ACTIVE | quiesce, then exit 0 (or 1 if the release failed) |
| QUIESCING | wait for it; retry once if it failed; exit 0/1 |
| QUIESCED | exit 0 |

If the production server stops on its own after activation (prefab's
`Start` returns an error), the process quiesces and exits 1, the same as a
fatal server error before handoff, but with the database released first.

A second signal exits immediately with 1. The exit code is 0 only when the
database ended released (process exit releases the lock regardless).

## Coordinator sequence

```
start replacement task with --standby
  wait: GET new:/livez == 200 and /status.state == STANDBY
  record each process's /status.instanceId (old and new, Grid and relay)
POST old:/quiesce  {"operationId": "D-release", "expectedInstanceId": OLD}   (Grid AND relay)
  wait: old /status.state == QUIESCED and databaseHeld == false   (both)
  on failure or timeout: do NOT activate; terminate the old task and confirm
  it has stopped, then continue
POST new:/activate {"operationId": "D-activate", "expectedInstanceId": NEW}  (both)
  409 instance_mismatch: the process restarted since it was recorded; re-read
  /status and decide again (never retry blindly with the new id)
  wait: new /status.state == ACTIVE (operation succeeded), /readyz == 200
  on failure: the new task is back in STANDBY, released; see rollback
switch load-balancer traffic to the new task; verify production health
terminate the old task
```

The server never infers ownership. Nothing activates because a predecessor
timed out, went unhealthy or stopped answering; activation is the
coordinator's explicit decision, made only after a confirmed release or a
confirmed termination of the previous owner. On an ambiguous handoff
everything stays inactive.

**Rollback is another handoff.** A released (QUIESCED) task never
reactivates, so rolling back means starting the previous version in standby,
quiescing the current owner, and activating the previous one. A version that
predates this feature has no control plane and cannot do that; rolling back
to it is a classic stop-then-start.

## Health checks

Liveness and readiness are deliberately separate, and a standby fails the
existing production-port check by design (nothing listens there yet):

- **Container health** during the whole task life: `GET :8081/livez` (Grid)
  and `GET :8091/livez` (relay). The Dockerfile `HEALTHCHECK`s still probe the
  production ports, which is correct for a normal deployment and wrong for a
  standby; the task definition's checks must use `/livez` once `--standby` is
  in use.
- **Production readiness**: `GET /readyz` on the control port, or the
  existing load-balancer target check on the production port (which also goes
  503 once quiesced).

## First migration

Binaries before this change have no control plane, so the first rollout
cannot be a handoff:

1. Deploy this version the classic way (no `--standby`), with
   `control.tokenSha256` configured so the running task has a control plane.
   It activates at boot, exactly as before.
2. Every deploy after that can be a handoff, because the old task now
   answers `POST /quiesce`.

Until the coordinated infrastructure is in place, leave `--standby` off: the
default path is unchanged.

## The file lock is a safeguard, not the protocol

`store.Open` and the relay's `OpenStore` (sierra-meshrelay) still take an exclusive `flock` on a
sibling `.lock` file and wait up to `lockTimeout` for it. Locally that makes a
premature activation fail (verified: activating a standby while the old
process held the database failed with the lock error after the timeout, and
the standby returned to STANDBY, released). **Whether `flock` is enforced
between two separate Fargate tasks on the shared filesystem has not been
verified.** Before relying on it, run the same check there: with the old task
ACTIVE, activate a standby replacement and confirm the activation fails on the
lock and the replacement reports STANDBY, `databaseHeld: false`. Either way,
the coordinator's confirmed release is what prevents two writers.

## Timing logs

Each step logs a duration (`duration` field):

- preparation: `Grid prepared (no database opened)` / `Relay prepared`,
  `Standby preparation complete; waiting for activation` (`sinceProcessStart`)
- activation: `Activation started`, `Grid store opened`, `Seeded grid sources
  and places`, `Using persisted grid store statistics`, `MeshCore: rehydrated
  node presence`, `Server initialization complete`, `Activation complete;
  serving production` (with `sinceProcessStart`); relay: `Relay store opened`,
  `Relay serving`
- quiescence: `Quiesce: request drain complete` (`inFlight`, `cancelled`,
  `remaining`), `Quiesce: ingest scheduler stopped`, `Quiesce: MeshCore
  subscriber closed`, `Quiesce: background work stopped`, relay `Relay
  sessions closed and audit queue flushed` (`sessionDrain`, `auditFlush`),
  `Quiesce: grid store closed and lock released`, `Quiesce: grid.db ownership
  released`, `Quiescence complete; database ownership released`
- exit: `Shutdown requested`, `Shutdown complete`, `Process exiting`
  (`state`, `databaseHeld`, `exitCode`)
