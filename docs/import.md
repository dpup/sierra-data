# Importing the legacy SQLite store into PostgreSQL (`gridctl`)

Runbook for carrying the legacy Grid store (`grid.db`, SQLite schema v14, the
`legacy-efs-final` tag) into the PostgreSQL store. Design and cutover sequence:
the workspace note `notes/2026-11-11-rds-postgres-migration-plan.md` (D11, D12,
sections 4, 5 and 8). Code: `cmd/gridctl` (CLI) over `internal/sqliteimport`.

`grid.db` is the system of record: revision history, resolved and expired
events, true first-seen times. The pollers cannot rebuild any of it. Treat
every step below as handling the only copy.

## What the tool guarantees

- **The source is never written.** Every SQLite connection is opened
  `mode=ro` through a URI, with no `journal_mode` pragma: SQLite cannot roll
  back a hot journal on a read-only connection (it reports one instead), never
  creates a `-journal` or `-wal` file, and never creates the database file.
  Mount the source read-only as well when you can.
- **A snapshot is consistent.** `VACUUM INTO` reads the whole database inside
  one read transaction, so the copy is a single state of a live file.
- **An import is all or nothing.** The data, the derived columns, `versions`,
  the SQLite ladder in `schema_migrations` and the `import_log` row are written
  in one PostgreSQL transaction, verified in that transaction, and committed
  only if verification passed. A failure (verification, a crash, a killed
  task, a dropped connection) leaves no data. The schema itself (the
  PostgreSQL migrations) is applied first and committed on its own, exactly as
  every Grid process applies it at boot; an empty schema is not data.
- **An import refuses a database that holds Grid data** (any imported table,
  `versions`, or `import_log`), unless `--allow-nonempty` (rehearsals only:
  it truncates inside the same transaction, so a failed re-import keeps the
  previous data). It also refuses a database that does not compare text
  bytewise (create it `TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'`), and a
  source that is not a complete v14 ladder.
- **The import is the writer while it runs.** It takes the Grid writer lease
  through the same `store.AcquireLease` a server's campaign uses (holder
  `gridctl-import@<host>/<pid>`, no address, so readers answer pushes with
  503 meanwhile), and its transaction carries the lease's epoch fence like
  every write. If a server holds the lease for `--lock-timeout` (default
  10 s), the import is **refused**, naming the holder; `--allow-nonempty`
  does not override that. Taking the lease bumps the epoch, so a writer that
  lost its session without noticing is fenced out too. The import releases
  the lease when it ends; a server's campaign then takes it in a newer term.
  Inside the transaction it also takes `EXCLUSIVE` locks on every target
  table (readers continue). `verify` takes no lease and no lock.
- **Verification** (inside the import, and on demand with `verify`):
  - row counts per table equal the snapshot manifest and the source;
  - a SHA-256 over each table's rows in primary-key order, computed in Go on
    both sides from typed canonical values (so engine formatting never
    matters), is equal; both sides are read in lockstep and the first
    differing rows are named, with the column;
  - **every** event, revision, lite and place blob unmarshals as its
    `grid.v1` message, and its index columns agree with it (id, layer,
    severity, status, source, effective, expires, observed/ingested times,
    revision, bbox; for places id, kind, name, slug, parent);
  - `events.first_ingested_at` equals revision 1's `ingested_at`; every
    revision has its event; `versions.revisions` equals the revision count
    and `places`/`mesh_activity` are positive; the `mesh_observations`
    identity is past both `MAX(id)` and the compaction watermark; the
    PostgreSQL baseline is recorded.
  - Informational (not failures): events whose stored `content_hash`
    differs from `store.ContentHash` of the blob (the stored hash is of the
    event as polled, so such an event may mint one revision on its next
    poll), and events without a revision 1.

## What moves, and how

In dependency order, one `COPY` each: `sources`, `places`, `events`,
`event_revisions`, `event_places`, `event_lite`, `mesh_observations`,
`mesh_link_rollup`, `mesh_meta`, `mesh_telemetry`, `mesh_activity`,
`burn_readings`, `road_samples`, then the SQLite ladder.

| Target | From the source |
| --- | --- |
| `events.first_ingested_at` | revision 1's `ingested_at` (fallback: earliest revision, then the event's own `ingested_at`; counted in the report) |
| `events.min_lat`…`max_lng` | the blob's `geometry.bbox` (NULL without one), the store's `bboxArgs` mapping |
| `event_revisions.id` | generated (`GENERATED ALWAYS`), in the source's insertion (rowid) order |
| `mesh_observations.id` | preserved; the identity is then set past `MAX(id)` and the `compaction_watermark` |
| `versions` | `revisions` = imported revision count; `places`, `mesh_activity` = their row counts (at least 1) |
| `schema_migrations` | the SQLite rows as `source = 'sqlite'`, versions 1..14 with their `applied_at` |
| `import_log` | one row: source, its SHA-256 and size, snapshot time, rows per table, the full report, gridctl version, start and finish |

Not carried: `event_geo` and its shadow tables, `event_geo_map` (the R*Tree
is replaced by the bbox columns) and `subscriptions` (never used; a
non-empty one fails the import so it is noticed). A table the importer does
not know fails the import too.

`import_log` is schema migration `0003_import_log.sql` (empty on a database
that was never imported into); the importer writes its row in the import
transaction, so a failed import leaves none.

## Commands

```
gridctl snapshot --db /data/grid.db --out s3://<bucket>/grid-<ts>.db   # or a local path
gridctl import   --from s3://<bucket>/grid-<ts>.db [--to <url>] [--report r.json] [--allow-nonempty] [--lock-timeout 10s]
gridctl verify   --from s3://<bucket>/grid-<ts>.db [--to <url>] [--report r.json]
gridctl version
```

- The manifest travels beside the snapshot as `<out>.manifest.json`; the
  database is uploaded first and the manifest last, so a manifest means a
  complete upload. `import`/`verify` require it unless `--manifest none` (a
  file that never went through `snapshot`; run `snapshot` on it first
  instead when you can, it also runs `quick_check`).
- `--to` defaults to `$PF__GRID__DATABASE__URL`, so the one-off task gets
  the URL from the same secret as the Grid. The URL is never printed.
- Exit status: 0 success, 1 failure (any difference; an import has rolled
  back), 2 usage. The report prints to stdout; `--report` also writes the
  JSON.
- S3 credentials come from the default AWS chain (the task role). Uploads
  carry `bucket-owner-full-control` and a SHA-256 checksum; downloads
  validate it.
- Scratch space for an `s3://` location: `--tmp` or `$TMPDIR`, about the
  snapshot's size (import downloads one file; snapshot writes one before
  uploading). Fargate's default 20 GiB ephemeral storage is ample.

Locally: `make gridctl` builds `bin/gridctl`; the image ships `/app/gridctl`.

## Running it in ECS

The Grid image's entry point is the server, and `run-task` overrides only the
command, so the one-off tasks use a small task definition of their own:
same image, `entryPoint: ["/app/gridctl"]`, the command as the arguments.
ECS Exec is off in both accounts; nothing here needs it (the output is read
from CloudWatch Logs).

### The migration bucket (old account)

The Grid organization's resource control policy refuses every principal
outside the organization on Grid-account (083901560296) buckets and ECR, and
the old project-hub account is outside it. So the bucket lives in the **old**
account, created by hand in `us-west-2` (the organization's region policy
treats `us-east-1` as global services only, so a Grid principal may not read
a bucket there): private, SSE-S3, Object Ownership "bucket owner enforced",
deleted after the migration is accepted. Its policy lets the Grid account's
import task role read the snapshots and nothing else:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "GridImportReadsSnapshots",
    "Effect": "Allow",
    "Principal": {"AWS": "arn:aws:iam::083901560296:role/<gridctl task role>"},
    "Action": "s3:GetObject",
    "Resource": "arn:aws:s3:::<migration-bucket>/grid-*"
  }]
}
```

The snapshot task writes to its own account's bucket; the
`bucket-owner-full-control` header gridctl sends is harmless there.

**Fallback** if the Grid account cannot read outside the organization (its
SCPs, or the S3 gateway endpoint policy, refuse a bucket in another
account; the import then fails with `AccessDenied`): copy the snapshot and
its manifest down with the operator's old-account credentials and up into a
Grid-account bucket with Grid credentials (an in-organization principal),
database first and manifest last, and import from there.
`scripts/rehearse_import.py --new-bucket <grid bucket>` does exactly that
(`transfer` step). The copy passes through the operator's disk: use native
storage outside the workspace and bind mounts, and delete it after.

### Snapshot (old account)

The legacy image does not contain `gridctl` (it is built from
`legacy/efs-sqlite`), and the RCP stops the old account pulling from Grid
ECR, so push the new Grid image to an old-account ECR repository (a separate
one, e.g. `sierra-gridctl`, so nothing touches the tags the coordinator
deploys from `sierra-data`). Register a one-off task definition (family
`sierra-gridctl-snapshot`) in the old account with:

- that image, `entryPoint ["/app/gridctl"]`, 0.25 vCPU / 512 MiB, default
  ephemeral storage;
- the EFS file system **read-only**. For the cutover (the live file) the
  existing access point (root `/info-ersn`) at `/data` is enough:
  `--db /data/grid.db`. For a rehearsal from an AWS Backup restore the
  restored copy lands outside that root (below), so mount the file system
  root instead: `efsVolumeConfiguration` with `rootDirectory "/"`, no access
  point, transit encryption on, at `/efs`, and `user "10001:10001"` on the
  container (the access point normally supplies that identity; the files are
  owned by 10001);
- a task role with `s3:PutObject` on `arn:aws:s3:::<migration-bucket>/grid-*`
  only;
- awslogs to the existing `sierra-data` log group, stream prefix `gridctl`;
- network: the `sierra-data` service's public subnets and its security
  group `info-ersn` (the EFS mount targets admit NFS only from that group);
  public IP on, as the service has;
- command `snapshot --db <path> --out s3://<bucket>/grid-<UTC ts>.db`, with
  `AWS_REGION=us-west-2` (the bucket's region) in the container overrides:
  gridctl's S3 client takes the task's region and does not follow a
  cross-region redirect.

`VACUUM INTO` holds a shared lock for the length of the copy. On the
rollback-journal legacy database the writer's commits wait for it (up to its
5 s busy timeout) and a poll tick may fail; the next tick recovers. Run it
off-peak, or (the cutover) after the writer is stopped. Measured on a
172 MiB fixture on local disk: 0.4-0.7 s; on EFS expect it to be bounded by
EFS read throughput (bursting mode: about 100 MiB/s per client is a
reasonable planning figure, so seconds, not minutes; measure it in the
rehearsal). A snapshot of a restored copy takes no lock anyone waits on.

### Import (new account)

A one-off task in the Grid account (family `sierra-gridctl`) with:

- `PF__GRID__DATABASE__URL` from the Grid's database secret (execution role:
  `secretsmanager:GetSecretValue` on it);
- a task role with `s3:GetObject` on
  `arn:aws:s3:::<migration-bucket>/grid-*` (the old account's bucket, which
  must also grant it, above; or the fallback bucket);
- network: the private subnets and the gridctl security group RDS admits,
  and the S3 gateway endpoint (its policy must allow the old account's
  bucket, or use the fallback);
- size: 0.5 vCPU / 1 GiB is plenty (the import streams; memory is bounded by
  one row);
- command `import --from s3://<bucket>/grid-<ts>.db --report /tmp/report.json`.

**Order: the writer stays disabled until the import says `RESULT: OK`.**
The import takes the writer lease and refuses while any server holds it, so:

1. the Grid service runs with `PF__GRID__WRITER__ENABLED=false` (readers
   only; pushes are answered 503) from its first deploy;
2. `import` (readers may serve meanwhile and see an empty database until the
   commit);
3. `verify` (same task definition, command `verify --from ...`) reads it
   back without writing and must print `RESULT: OK`;
4. only then deploy the Grid with the writer enabled.

`verify` after the writer has run reports differences by design (new
revisions, swept events).

## Rehearsal from an AWS Backup restore (gate 5)

The migration plan (section 8, gate 5) wants two full rehearsals before the
cutover, on an earlier copy of production: one from scratch, one proving the
empty check refuses a re-run. Each is one command,
`scripts/rehearse_import.py` (`make rehearse-import REHEARSE_ARGS=...`),
which never touches the live `grid.db`: it restores a recovery point and
snapshots the restored copy.

| step | account | what it does |
| --- | --- | --- |
| `restore` | old | AWS Backup item-level restore of `/info-ersn/grid.db` (vault `info-ersn-backups`, latest completed recovery point or `--recovery-point-arn`) into the same EFS file system; waits; the `sierra-efs-admin` task lists `aws-backup-restore_*` and takes the newest |
| `snapshot` | old | `sierra-gridctl-snapshot` with `snapshot --db /efs/aws-backup-restore_<ts>/info-ersn/grid.db --out s3://<bucket>/grid-rehearsal-<run>.db`; then reads the manifest back (size, SHA-256, quick_check, counts) |
| `transfer` | both | only with `--new-bucket`: the fallback copy into a Grid-account bucket |
| `import` | new | checks the Grid service's task definition says `PF__GRID__WRITER__ENABLED=false`, then `sierra-gridctl` with `import --from ... --report /tmp/report.json`; fails unless the task exits 0 and its log says `RESULT: OK` |
| `verify` | new | the same task with `verify`; `RESULT: OK` required |
| `refuse` | new | `import` again without `--allow-nonempty`; must fail with "the target database is not empty" |
| `apidiff` | - | with `--new-base-url`: `scripts/apidiff.py` between `https://data.sierragridteam.org` and a new-stack server on the imported database. Production has moved on since the recovery point, so differences are expected and recorded, not fatal (`--apidiff-strict` makes them fatal); for the strict gate 2, run a legacy build on the same snapshot ("API diff gate" below) |
| `cleanup` | both | removes the restore directory through `sierra-efs-admin` (or prints the command when `--efs-admin-family ''`) and deletes the snapshot objects; `--keep-restore`, `--keep-snapshot` |

Each task's log is read from its awslogs stream (group and prefix from the
task definition); the counts, timings and verify outcome go into a Markdown
report, with a resumable state file beside it, in the workspace's
`notes/local/` (ignored; the script refuses any other directory under
`notes/` and the repository). A failure stops the run at that step and the
report says where.

```
# look first: every AWS command, nothing run, nothing written
python3 scripts/rehearse_import.py --dry-run --old-profile <old> --new-profile <grid>

# rehearsal 1, on a freshly created empty database
make rehearse-import REHEARSE_ARGS="--old-profile <old> --new-profile <grid> \
  --new-subnets <private subnets> --new-security-groups <gridctl sg> --new-base-url <new-stack url>"

# rehearsal 2, a later recovery point: recreate the database first, or replace in place
make rehearse-import REHEARSE_ARGS="... --allow-nonempty"
```

Re-running with the same `--run-id` resumes: finished steps are skipped, the
restore job is started with the run's idempotency token (and an existing job
is waited on, not repeated), and an uploaded snapshot is not taken again.
`--from-step <step>` and `--only a,b` run steps even if they are done; `--skip`
leaves steps out. Every name is a flag with the defaults above (`--help`);
the new account's identity is checked against 083901560296 before anything
runs.

### Before the first rehearsal (by hand)

Old account (project-hub, `us-east-1`):

1. The migration bucket in `us-west-2` and its policy (above).
2. An ECR repository with the new Grid image (`make ecr-push-grid` with that
   registry and repository).
3. Task definition `sierra-gridctl-snapshot` (above), with the file system
   root mounted read-only at `/efs` and `user "10001:10001"`.
4. Task definition `sierra-efs-admin`: the same image, `entryPoint
   ["/bin/sh", "-c"]`, `user "0"`, the file system root mounted
   **read-write** at `/efs`, no task role, awslogs to `sierra-data` (prefix
   `efs-admin`). It deletes restore directories, so deregister it after the
   migration.
5. The operator's role needs `backup:StartRestoreJob`, `iam:PassRole` on
   `info-ersn-backup` (the backup role, which already has
   `AWSBackupServiceRolePolicyForRestores`), `ecs:RunTask`/`DescribeTasks`,
   `logs:GetLogEvents`, and S3 read and delete on the bucket.

Grid account (`us-west-2`): the `sierra-gridctl` task definition and its
security group (sierra-infra `grid/compute` and `grid/data`), an empty
database created `TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'` owned by
`grid`, the Grid service (if it exists yet) deployed with the writer
disabled, and the import task role named in the bucket policy.

### Assumptions to verify on the first run

- **Where the restore lands.** An item-level EFS restore to the same file
  system is documented to create `aws-backup-restore_<timestamp>/` at the
  file system root; the tooling assumes the item keeps its path below it
  (`/aws-backup-restore_<ts>/info-ersn/grid.db`, with a fallback glob for
  `/aws-backup-restore_<ts>/grid.db`) and that the names sort by time. If
  the listing finds nothing, look on the file system and pass
  `--restored-path`.
- **The access point root.** The services' access point is rooted at
  `/info-ersn` and cannot see `/aws-backup-restore_*`, hence the
  file-system-root volume in the two old task definitions. That works only
  if no file system policy requires an access point or IAM authorization
  (project-hub sets none) and if root is not squashed for the admin task.
- **Ownership.** Restored files are expected to keep uid 10001; the snapshot
  task runs as 10001 to read them.
- **Log groups.** Read from each task definition's awslogs options
  (`--old-log-group`/`--new-log-group` override); the stream is
  `<prefix>/<container>/<task id>`.
- **Region override.** `AWS_REGION` in the container overrides is assumed to
  win over the value ECS injects for Fargate tasks; if the upload fails with
  a redirect (`PermanentRedirect`/301), gridctl needs an explicit S3 region.
- **Torn copies.** AWS Backup copies a live file without SQLite's lock, and
  only `grid.db` is restored (no journal). `quick_check` (in `snapshot`)
  and the full import verification catch most damage; if either fails, use
  the next recovery point.
- **Names in the new account** (`sierra-grid` cluster and service,
  `sierra-gridctl`) follow the sierra-infra plan and do not exist yet.

### Decisions for review

| decision | options | choice | why |
| --- | --- | --- | --- |
| How the script drives AWS | boto3; AWS CLI v2 through `subprocess` | AWS CLI, Python stdlib only | the precedent of `scripts/deploy.py` (and its stubbed-subprocess tests); no new dependency or virtualenv; `--dry-run` prints the exact commands an operator could run by hand |
| Where the restore goes | the same EFS file system; a new file system per rehearsal | the same file system | cheapest and fastest: no new file system, mount targets or security group rules, and the restore directory is deleted at the end. The cost is a second copy of the system of record beside the live one for the length of the run and a root-mounted admin task; a fresh file system would isolate it (and could be deleted whole) but needs mount targets in the task subnets and its own access point |
| Finding the restored path | guess from the restore job; list it from a task | list it from a one-off task (`sierra-efs-admin`) | the restore job reports only the file system, not the directory; the same task also deletes it afterwards. Without that task, `--restored-path` and printed cleanup instructions |
| Where the report goes | notes/; notes/local/; the repository | the workspace's `notes/local/` (ignored) | the working agreement keeps rehearsal reports there: they carry the hashes and counts of the production store |
| API diff target | production; a legacy build on the snapshot | production, informational | no extra legacy deployment for a rehearsal; the strict comparison stays the gate 2 run below |

## If it fails

Nothing was imported; read the report's `problems` and the per-table `diff:`
and `check:` lines, fix the cause, run again.

| Message | Meaning | Do |
| --- | --- | --- |
| `does not match its manifest` | the downloaded file is not the snapshot that was described (truncated or replaced upload) | re-run the snapshot; never edit a manifest |
| `source schema is version N` | not a v14 store | the importer reads only `legacy-efs-final`'s schema |
| `quick_check` not ok, or SQLite says the database is read-only / malformed when opening | the file (often an EFS backup taken mid-commit, carrying a hot journal) is inconsistent; a `mode=ro` open reports it instead of repairing it | use the next recovery point, or snapshot the live file |
| `the target database is not empty` | a previous import (or a writer) left data | production: drop and recreate the database, then import; rehearsal: `--allow-nonempty` |
| `another process holds the Grid writer lease` | a server (named in the message) is the writer | stop it, `yield` it, or run it with `PF__GRID__WRITER__ENABLED=false`; retry |
| `locking the target tables` | something else holds a table lock (a long query, a manual session) | find it in `pg_locks`, end it, retry |
| `does not use byte-order collation` | the database was created with a glibc/ICU locale | recreate it with `TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'` |
| `rows fail the blob check` | an index column disagrees with its blob in the source (historical drift) or a blob does not unmarshal | stop and investigate the named rows on the source; do not patch around it |
| `row stream SHA-256 differs` | the data changed in transit; the first differing rows and column are listed | a bug in the importer: keep the report, do not proceed |
| `subscriptions holds N rows` / unknown table | the source has data the new schema has no home for | decide explicitly; the importer will not drop it silently |

Keep reports and snapshots out of the workspace, notes and bind mounts: they
contain the system of record. Reports from production rehearsals go in
`notes/local/` only. The snapshot in S3 is also the cutover's rollback
artifact; delete it after the migration is accepted.

## Timings

Measured 2026-11-12 in the Moat sandbox (10 cores, arm64, PostgreSQL 18.6 in
Docker with `fsync=off`) on a generated fixture sized like production
(`TestImportAtProductionScale`: 172.5 MiB snapshot; 6,000 events, 29,810
revisions, 200,000 mesh observations, 40,000 telemetry rows, 30,000 rollups,
70,000 road samples), three runs:

| step | time |
| --- | --- |
| snapshot (VACUUM INTO, quick_check, counts, SHA-256) | 0.42-0.46 s |
| import, copy | 1.5-1.7 s |
| import, in-transaction verify | 1.1-1.2 s |
| import total (CLI, end to end) | 2.7-2.9 s |
| separate `verify` | 1.3 s |
| import with `GOMAXPROCS=1` | 3.1 s |

On RDS (`db.t4g.small`, gp3, fsync on, a network hop) and a 0.5 vCPU task,
budget an order of magnitude more: under a minute. The rehearsal measures the
real number; record it in the handoff note, not here.

Re-run the measurement with
`GRIDCTL_SCALE_TEST=1 go test -run AtProductionScale -v ./internal/sqliteimport/`
(`GRIDCTL_SCALE_DIR` to place the fixture on native storage).

## API diff gate

After a rehearsal import, run the legacy server (a `legacy/efs-sqlite`
build on the same snapshot) and the new server on the imported database,
then `make apidiff LEGACY=<url> NEW=<url>` (see `scripts/apidiff.py` for the
endpoint list and what is normalised). Keep both servers from changing the
data during the run (no upstream credentials, or the writer disabled). Every
difference is a finding to explain.
