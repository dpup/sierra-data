-- Legacy SQLite schema version 3, verbatim from legacy-efs-final:internal/store/store.go (migrationV3).
-- Test fixture only: never edit (it is what production ran).
CREATE TABLE mesh_observations (
  id         INTEGER PRIMARY KEY,       -- rowid, monotonic
  pubkey     TEXT NOT NULL,             -- advertising node
  heard_at   INTEGER NOT NULL,          -- OUR receive time (unix sec) — the trustworthy clock
  broker     TEXT NOT NULL DEFAULT '',  -- MQTT server heard on
  gateway    TEXT NOT NULL DEFAULT '',  -- origin_id / gateway that reported it
  snr        REAL,
  rssi       INTEGER,
  hop_count  INTEGER NOT NULL DEFAULT 0,
  path       TEXT NOT NULL DEFAULT '',  -- comma-joined hop prefix-hashes (hex), as received
  path_nodes TEXT NOT NULL DEFAULT ''   -- resolved pubkeys ('' where a hop was unresolved)
);
CREATE INDEX idx_mesh_obs_heard  ON mesh_observations(heard_at);
CREATE INDEX idx_mesh_obs_pubkey ON mesh_observations(pubkey, heard_at);

CREATE TABLE mesh_link_rollup (
  a_pubkey     TEXT NOT NULL,           -- canonical a < b
  b_pubkey     TEXT NOT NULL,
  bucket       INTEGER NOT NULL,        -- heard_at truncated to UTC day
  observations INTEGER NOT NULL DEFAULT 0,
  best_snr     REAL,
  first_seen   INTEGER NOT NULL,
  last_seen    INTEGER NOT NULL,
  PRIMARY KEY (a_pubkey, b_pubkey, bucket)
);
CREATE INDEX idx_mesh_link_bucket ON mesh_link_rollup(bucket);

CREATE TABLE mesh_meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
