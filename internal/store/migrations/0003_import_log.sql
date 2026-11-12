-- Grid store schema version 3: import_log, one row per committed SQLite
-- import (gridctl import, internal/sqliteimport).
--
-- The table is empty on every database that was not imported into; the
-- importer writes its row in the same transaction as the data, so a failed
-- import leaves no row. IF NOT EXISTS because rehearsal databases imported
-- before this migration existed (the importer then created the table itself,
-- with this exact definition) must still migrate.
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
