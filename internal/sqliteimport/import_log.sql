-- import_log: one row per committed SQLite import (gridctl import).
--
-- Created by the importer inside its own transaction, so a failed import
-- leaves no trace. Written to become migration 0003_import_log.sql verbatim
-- once the writer lease's 0002 has landed; IF NOT EXISTS keeps a database
-- that an importer already created it in valid under the migration.
CREATE TABLE IF NOT EXISTS import_log (
  id                    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source                text NOT NULL,         -- path or s3:// URL as given
  source_sha256         text NOT NULL,
  source_size           bigint NOT NULL,
  source_schema_version integer NOT NULL,      -- the legacy ladder's version (14)
  snapshot_created_at   timestamptz,           -- from the snapshot manifest, when there was one
  rows_per_table        jsonb NOT NULL,
  report                jsonb NOT NULL,        -- the verification report (hashes, checks)
  replaced              boolean NOT NULL DEFAULT false, -- --allow-nonempty replaced existing data
  gridctl_version       text NOT NULL,
  started_at            timestamptz NOT NULL,
  finished_at           timestamptz NOT NULL
);
