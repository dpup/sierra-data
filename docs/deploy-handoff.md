# Deploys: readers, the writer lease, and the control API

How the Grid (`sierra-server`) is deployed and what it does during a
deploy. The Grid and the mesh relay run as two ECS Fargate services, each
with its own image, task definition and ECR repository. The infrastructure
code that creates them owns the services, the task definitions' content and
the deployment settings; this repository's deploy only swaps images. The
first part is this repository's deploy workflow; the second part, "What the
Grid does during a deploy", is the Grid's lifecycle (readers, the writer
lease, migrations, health, SIGTERM).

## How a deploy flows

1. **CI on main.** A push to `main` runs `CI` (`.github/workflows/ci.yml`:
   `make test` against PostgreSQL, `go vet`, the script tests).
2. **The Deploy workflow** (`.github/workflows/deploy.yml`) starts when CI
   succeeds on a push to this repository's `main` (`workflow_run`), or by
   hand (`make deploy`, below). Its `preflight` job resolves the commit and
   refuses anything not reachable from `origin/main`, and anything at or
   before the tag `legacy-efs-final` (the SQLite build, which main's history
   still reaches); a manual run also needs a successful CI push run for
   that commit.
3. **Approval.** The `deploy` job runs in the GitHub Environment
   `production`, which should have a required reviewer and allow only
   `main`. Once approved it gets the deploy role through GitHub OIDC (no AWS
   keys) and runs `scripts/deploy.py release`. Tooling comes from main's
   tip; the images are built from a second checkout of the commit being
   deployed.
4. **Images.** For each of `grid` and `relay` the workflow computes a
   *content key*: its binaries built reproducibly (`-trimpath`, no VCS
   stamp, no build id) for the task definition's architecture, plus the files
   the Dockerfile copies in and the Dockerfile itself. When ECR already has
   an image tagged `content-<key>` it is reused; otherwise the Dockerfile
   target is built and pushed. Either way the image is tagged with the
   **full commit SHA**, the **git tag** when the commit has one, and the
   content key. ECR tags are immutable.
5. **Revision.** It takes the family's latest ACTIVE task-definition
   revision (so a revision the infrastructure code registered is picked
   up), replaces only the matching container's image with
   `<repository>@<digest>`, and registers it. Environment, secrets, CPU,
   memory, roles and everything else stay as the infrastructure wrote them.
6. **UpdateService** with that revision, then it follows the ECS service
   deployment (`describe-service-deployments`): blue/green lifecycle stages,
   the bake time, and ECS's automatic rollback on failed health or alarms.
   It prints the deployment ARN, the command that rolls the running
   deployment back, and on success the command that restores the revision
   that ran before. A rollback by ECS fails the run.

A service whose image and revision are both unchanged is not redeployed, so
a docs-only commit deploys nothing and a relay-only change leaves the Grid
alone. The Grid deploys first; if it fails, the relay is not touched. One
deploy runs at a time (`concurrency: deploy-production`), never cancelled
half way.

The workflow assumes the infrastructure leaves each service's task
definition to the deploy (so applying it never reverts an image), keeps
earlier task-definition revisions for rollback, and runs the Grid service
with ECS blue/green and a `/readyz` health check. During the bake both task
sets serve reads; the writer lease moves when the old set gets SIGTERM
(about half a second, rehearsed), and the new tasks have already applied
any migration at boot. No coordinator, no yield step.

### Repository variables

Repository variables (or variables on the `production` environment) are
the workflow's inputs. `scripts/deploy.py check-config` refuses to go on
while a required one is unset or any is malformed. Automatic runs are
skipped until `DEPLOY_ROLE_ARN` is set, so the workflow is inert before the
infrastructure exists.

The three relay variables are **optional as a group**, so the Grid can be
deployed before a relay service exists. All three unset: the relay is
skipped and the run (and `make deploy`) says so in a notice line,
`relay: skipped, ... are all unset; deploying the Grid only`. Some set:
refused, naming the missing ones. All set: both are deployed.

| Variable | Required | What |
|---|---|---|
| `DEPLOY_ROLE_ARN` | yes | The IAM role the workflow assumes through GitHub OIDC |
| `DEPLOY_AWS_REGION` | yes | The services' AWS region |
| `DEPLOY_ECS_CLUSTER` | yes | Cluster holding the services |
| `DEPLOY_GRID_SERVICE` | yes | The Grid ECS service |
| `DEPLOY_GRID_TASK_FAMILY` | yes | Its task-definition family (the service must run a revision of it) |
| `DEPLOY_GRID_ECR_REPOSITORY` | yes | ECR repository name for the `grid` image |
| `DEPLOY_RELAY_SERVICE` | all three or none | The relay ECS service |
| `DEPLOY_RELAY_TASK_FAMILY` | all three or none | Its task-definition family |
| `DEPLOY_RELAY_ECR_REPOSITORY` | all three or none | ECR repository name for the `relay` image |

The container whose image is replaced is the one already using the
service's repository, or else the one named `grid` / `relay`.

The runner is `ubuntu-24.04` (x86_64) and the workflow refuses to build for
another architecture: the Dockerfile cross-compiles Go but its runtime
stages run `apk`, which would need emulation. An ARM64 task definition needs
the runner changed to `ubuntu-24.04-arm` first.

### Deploying by hand

```bash
make deploy                 # HEAD: must be main, pushed, equal to origin/main, clean, CI green
make deploy REF=v1.4.0      # an earlier tag (or commit) on main; the checkout does not matter
make deploy PUSH_ONLY=1     # build, push and tag the images only; no ECS calls
make deploy-status          # recent Deploy runs
```

`make deploy` needs `gh` logged in, not AWS credentials. It checks the
checkout (`branch_drift`: the right branch, not detached, HEAD fetched and
equal to origin; `dirty_files`: no uncommitted tracked changes,
`ALLOW_DIRTY=1` overrides) and that CI passed. When `gh` can read the
variables (repository and `production` environment) it applies the
configuration check by name, refusing missing ones and noting a skipped
relay; when it cannot, it says so and leaves the check to the workflow.
Then it runs `gh workflow run deploy.yml --ref main` and follows the run
with `gh run watch`; the exit code is the run's. Approve the `deploy` job
in the `production` environment when asked.

A change the infrastructure code made to a task definition (an env var,
CPU, a secret) registers a new revision that is not deployed until the
next Deploy run, which builds on it; `make deploy` rolls it out alone (same
image, new revision).

### Redeploying a previous release (rollback)

- **While a deployment runs**, the workflow log has the command:
  `aws ecs stop-service-deployment --service-deployment-arn <arn> --stop-type ROLLBACK`.
  ECS also rolls back on its own when the new tasks fail health or the
  configured alarms during the bake.
- **After it finished**, either `make deploy REF=<previous tag or commit>`
  (reuses the image already in ECR, registers a revision on the family's
  current settings) or the printed
  `aws ecs update-service --cluster <c> --service <s> --task-definition <previous revision>`
  (restores the exact previous revision, settings included).

Rolling the Grid back across a migration is safe: an older build accepts a
newer schema (migrations are additive; see Migrations below).

# What the Grid does during a deploy

The Grid (`sierra-server`) runs as one or more identical processes on one
PostgreSQL database. **Every process serves every read from boot.** At most
one process at a time is the **writer**: it holds the writer lease and runs
everything that writes (the ingest scheduler and its pollers, the MeshCore
subscriber, push-ingest buffers, source and place seeding, upstream cache
warming). Schema migrations are not the writer's: every process applies
them at boot (below). A deploy is therefore an ordinary
blue/green deploy: the new processes start serving reads at once, and the
writer role moves when the old writer goes away (SIGTERM) or is told to
yield.

This replaces the standby/quiesce/activate handoff the legacy stack needed
because only the database owner could serve. That handoff still exists (the
Grid remaps it onto the roles, below) so a coordinator could drive the Grid;
the native ECS path needs none of it, and since the 2026-11-13 cutover
nothing uses it: it is a candidate for removal once the old account is gone. The mesh relay
(`cmd/mesh-relay`) keeps the old handoff unchanged: it stays one process on
its own store, the SQLite file or, in the Grid account, a DynamoDB table
whose instance lease the next task takes over when the previous one stops
(`meshRelay.store`; a rolling deploy with minimum healthy 0%).

Code: `internal/store/lease.go` (the lease and fencing),
`cmd/server/grid.go` (the reader: `Serve`, `Exit`), `cmd/server/writer.go`
(the writer role and the remap), `cmd/server/ingestroute.go` (push
forwarding), `internal/handoff` (the lifecycle and control API).

## Roles

**Reader (every process, from boot to exit).** `store.OpenReader` opens the
pool, checks the collation, applies pending migrations (next section) and
takes no writer lease. The production listener starts right after. A reader writes nothing; every write path in the store refuses
with `ErrNotWriter`. ETag validators come from the `versions` table (below),
so every process returns the same ETag for the same data.

**Writer (the lease holder).** `store.AcquireLease`:

1. takes `pg_try_advisory_lock` on a dedicated connection (taken from the pool
   and hijacked out of it), polling every 250 ms;
2. bumps `writer_lease.epoch` and records `holder` (`<instanceId>@<hostname>`)
   and `address` (where other processes reach this one);
3. re-applies migrations (normally a no-op: the process already migrated at
   boot);
4. then the process seeds sources and places and starts the scheduler, the
   MeshCore registry, push ingest and cache warming (a **term**).

The holder refreshes `writer_lease.heartbeat_at` every 2 s on the lease
connection.

## Migrations: every process, at boot

Every process, reader or writer, runs `migrations.Apply` when it opens the
store, under the transaction-scoped migration advisory lock (one version per
transaction; concurrent boots queue and the later ones find nothing to do).
It does not need the writer lease. A database already newer than the build
(a later release migrated it) is accepted as it is, so an older build keeps
serving through a rollback. A migration that fails fails the boot.

This is safe because migrations are always backwards compatible: while the
new release migrates, the previous release's readers **and its writer** keep
running against the newer schema. The rules (`internal/store/migrations`
package doc, `internal/store/AGENTS.md`): append-only; additive only; every
new column nullable or with a default, so the old writer can still insert
rows; no rename or drop of anything a released reader or writer uses (that
is a later release, once nothing running uses it). So a release with a
migration deploys like any other: the new tasks migrate as they boot, turn
ready, and the writer role moves when the old writer stops.

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
version (bumped only when a place actually changes: a writer term's reseed
of an unchanged directory bumps nothing, so a handover alone moves no tag). Each process caches the counters
and refreshes them

- after each of its own commits (the writer is exact),
- on `NOTIFY grid_versions` (a statement trigger on `versions`; delivered at
  commit) on a dedicated `LISTEN` connection, and
- on a 10 s poll, so a reader with a broken listener answers a stale `304`
  for at most 10 s after a write.

HTTP clients get the `ETag` and `Cache-Control: public, max-age=30`, and a
`304` with no body on a matching `If-None-Match`, through
`gridapi.OutgoingHeaderMatcher` on the gateway mux (prefab's own outgoing
matcher dropped both headers until 2026-11-12). Two processes on one
database return the same tag, and either honours the other's.

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
- `GET /readyz`: 200 when the store is open (and therefore migrated to at
  least this build's schema) and the process is not exiting; the body says
  whether it is the writer (`{"ready": true, "writer": false}`). The writer
  role never enters into readiness: a reader is ready. 503 once SIGTERM has
  started the exit.

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
example to have the new build's ingest running before the old tasks drain.
Yield the old writer (and any other old process, or one of them may win the
campaign instead); a new process takes the lease; the old processes keep
serving reads. A release with a migration does not need it: the new tasks
migrate at boot and are ready without the writer role.

## The coordinator remap (route B, unused since the cutover)

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
5. Conditional GET: A and B return the same `ETag` for `/api/v1/places` and
   `/api/v1/events`, B answers A's tag with `304`, and the `/api/v1/places`
   tag taken before each switch still answers `304` (no body) from the
   survivor after the SIGTERM and the SIGKILL handover.

Local results (2026-11-12, three runs after the integration of the importer
and the header fix): SIGTERM takeover 455-481 ms, SIGKILL takeover 284-289 ms,
no failed reads or pushes, one push retried once per switch, ETags equal on
both processes and the pre-switch tag answered `304` after every handover.

## Timing logs

Each step logs a duration: `Grid store opened (reader)`, `Serving reads`,
`Campaigning for the writer lease`, `Writer lease acquired` (`waited`),
`Seeded grid sources and places`, `Writer term started`, `Writer lease LOST`,
`Writer stop: ...` (scheduler, MeshCore, background work, lease released),
`Quiesce: writer lease released; still serving reads`, `Exit: request drain
complete`, `Shutdown complete`, `Process exiting` (`state`, `databaseHeld`,
`exitCode`). Forwarded pushes log `Push ingest: forwarded to the writer`
(`writer`, `epoch`, `status`, `duration`).
