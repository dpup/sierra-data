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
  the writer would apply it; an empty schema is not data.
- **An import refuses a database that holds Grid data** (any imported table,
  `versions`, or `import_log`), unless `--allow-nonempty` (rehearsals only:
  it truncates inside the same transaction, so a failed re-import keeps the
  previous data). It also refuses a database that does not compare text
  bytewise (create it `TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'`), and a
  source that is not a complete v14 ladder.
- **A running writer cannot interleave.** The import takes `EXCLUSIVE` locks
  on every target table (readers continue; writes wait) with a 10 s lock
  timeout, so it fails rather than queueing behind a live writer.
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

`import_log` is created by the importer inside its transaction. It becomes
migration `0003_import_log.sql` (the same `CREATE TABLE IF NOT EXISTS`) once
the writer lease's `0002` has landed on main.

## Commands

```
gridctl snapshot --db /data/grid.db --out s3://<bucket>/grid-<ts>.db   # or a local path
gridctl import   --from s3://<bucket>/grid-<ts>.db [--to <url>] [--report r.json] [--allow-nonempty]
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

### Snapshot (old account)

The legacy image does not contain `gridctl` (it is built from
`legacy/efs-sqlite`). Register a one-off task definition in the old account
that runs the new image (push it to the old account's ECR, or allow the old
account to pull from the new ECR) with:

- the existing EFS volume and access point, mounted **read-only** at `/data`;
- a task role with `s3:PutObject` (and `s3:PutObjectAcl` if the bucket keeps
  ACLs enabled) on `arn:aws:s3:::<migration-bucket>/grid-*` only;
- command `snapshot --db /data/grid.db --out s3://<bucket>/grid-<UTC ts>.db`.

`VACUUM INTO` holds a shared lock for the length of the copy. On the
rollback-journal legacy database the writer's commits wait for it (up to its
5 s busy timeout) and a poll tick may fail; the next tick recovers. Run it
off-peak, or (the cutover) after the writer is stopped. Measured on a
172 MiB fixture on local disk: 0.4-0.7 s; on EFS expect it to be bounded by
EFS read throughput (bursting mode: about 100 MiB/s per client is a
reasonable planning figure, so seconds, not minutes; measure it in the
rehearsal).

### Import (new account)

A one-off task in the new account with:

- `PF__GRID__DATABASE__URL` from the Grid's database secret (execution role:
  `secretsmanager:GetSecretValue` on it);
- a task role with `s3:GetObject` on `arn:aws:s3:::<migration-bucket>/grid-*`
  (and `kms:Decrypt` if the bucket uses a customer-managed key);
- network: the private subnets and the task security group RDS admits, and
  the S3 gateway endpoint;
- size: 0.5 vCPU / 1 GiB is plenty (the import streams; memory is bounded by
  one row);
- command `import --from s3://<bucket>/grid-<ts>.db --report /tmp/report.json`.

Run it while **no task holds the writer lease** (`grid.writer.enabled =
false`, cutover step 1); readers may serve during it and see the database
empty until the commit. Then `verify` (same task definition, command
`verify --from ...`) reads it back without writing, and must print
`RESULT: OK` before the writer is enabled. `verify` after the writer has run
reports differences by design (new revisions, swept events).

## If it fails

Nothing was imported; read the report's `problems` and the per-table `diff:`
and `check:` lines, fix the cause, run again.

| Message | Meaning | Do |
| --- | --- | --- |
| `does not match its manifest` | the downloaded file is not the snapshot that was described (truncated or replaced upload) | re-run the snapshot; never edit a manifest |
| `source schema is version N` | not a v14 store | the importer reads only `legacy-efs-final`'s schema |
| `quick_check` not ok, or SQLite says the database is read-only / malformed when opening | the file (often an EFS backup taken mid-commit, carrying a hot journal) is inconsistent; a `mode=ro` open reports it instead of repairing it | use the next recovery point, or snapshot the live file |
| `the target database is not empty` | a previous import (or a writer) left data | production: drop and recreate the database, then import; rehearsal: `--allow-nonempty` |
| `locking the target tables` | something holds a lock: a writer is running | stop the writer (lease holder), retry |
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
