-- Grid store, PostgreSQL baseline (schema version 1).
--
-- Equal to the SQLite schema at v14 (the legacy-efs-final tag), minus:
--   * event_geo (R*Tree) and event_geo_map: written but never read in
--     production. The bbox moves onto events as plain columns below.
--   * subscriptions: an anticipated table nothing ever used.
--   * every WITHOUT ROWID (a SQLite storage option).
-- plus:
--   * events.first_ingested_at, so /stats no longer joins revision 1 per event;
--   * events.min_lat/max_lat/min_lng/max_lng, the bbox from the blob;
--   * an events (source_id, status) index;
--   * event_revisions.id and mesh_observations.id as identity columns;
--   * versions: change counters bumped in the same transaction as the write
--     (ETag validators; they used to be per-process memory).
--
-- Conventions, unchanged from SQLite: the proto blob (bytea) is canonical and
-- every scalar column is a query index re-derived from it on every write; all
-- domain timestamps are bigint unix seconds (never timestamptz).
--
-- COLLATION: the database MUST be created with byte-order collation:
--   CREATE DATABASE grid TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C';
-- Keyset cursors (id > $1), golden orderings and Go-side sorting all assume
-- byte order; a glibc en_US database orders 'a-z' after 'a:a' and silently
-- reorders ids containing ':' and '-'. store.Open refuses a database that
-- does not compare bytewise.
--
-- Schema policy: additive only (expand/contract). A process accepts a
-- database whose schema version is >= its own, so a reader one release
-- behind keeps working while a newer writer migrates. Never drop or rename a
-- column an older release still reads in the same release that stops reading
-- it; never edit a shipped migration file.

CREATE TABLE sources (
  id                    text PRIMARY KEY,
  name                  text NOT NULL,
  attribution           text NOT NULL DEFAULT '',
  homepage_url          text NOT NULL DEFAULT '',
  kind                  integer NOT NULL DEFAULT 1,      -- gridv1.SourceKind
  poll_interval_seconds bigint NOT NULL,
  stale_after_seconds   bigint,                          -- NULL => 3x poll interval
  expire_after_seconds  bigint,                          -- NULL => never auto-expire
  last_success_at       bigint,
  last_attempt_at       bigint,
  last_error            text NOT NULL DEFAULT '',
  status                integer NOT NULL DEFAULT 0,
  disappearance         text NOT NULL DEFAULT 'resolve'  -- 'resolve' | 'expire'
);

CREATE TABLE events (
  id                text PRIMARY KEY,
  layer             integer NOT NULL,
  severity          integer NOT NULL,
  status            integer NOT NULL,
  source_id         text NOT NULL REFERENCES sources(id),
  effective         bigint,
  expires           bigint,
  observed_at       bigint NOT NULL,
  ingested_at       bigint NOT NULL,
  -- When revision 1 was stored. Written once, on insert; never updated.
  first_ingested_at bigint NOT NULL,
  -- When a successful poll last included the event (TouchSeen, coalesced).
  last_seen_at      bigint NOT NULL DEFAULT 0,
  revision          bigint NOT NULL DEFAULT 1,
  -- In-place rewrites of the current blob without a revision (hash-equal path).
  refreshes         bigint NOT NULL DEFAULT 0,
  content_hash      text NOT NULL,   -- normalized proto hash; unchanged => no revision
  -- Geometry bbox from the blob (NULL without geometry). No reader yet; it
  -- replaces the write-only R*Tree. A GiST index on a box can come later.
  min_lat           double precision,
  max_lat           double precision,
  min_lng           double precision,
  max_lng           double precision,
  proto             bytea NOT NULL
);
CREATE INDEX idx_events_active        ON events (status, severity DESC, observed_at DESC);
CREATE INDEX idx_events_layer         ON events (layer, status);
CREATE INDEX idx_events_source_status ON events (source_id, status);

CREATE TABLE event_revisions (        -- full snapshots, append-only
  id          bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
  event_id    text NOT NULL,
  revision    bigint NOT NULL,
  observed_at bigint NOT NULL,
  ingested_at bigint NOT NULL,
  proto       bytea NOT NULL,
  PRIMARY KEY (event_id, revision)
);
-- Matches QueryHistory's ORDER BY so the cross-event archive walks an index.
CREATE INDEX idx_revisions_observed
  ON event_revisions (observed_at DESC, event_id ASC, revision DESC);

CREATE TABLE event_lite (             -- current blob without GeoJSON (list reads)
  event_id text PRIMARY KEY,
  proto    bytea NOT NULL
);

CREATE TABLE places (
  id        text PRIMARY KEY,
  kind      integer NOT NULL,
  name      text NOT NULL,
  slug      text NOT NULL UNIQUE,
  parent_id text,
  proto     bytea NOT NULL
);

CREATE TABLE event_places (           -- computed at ingest; hot path never touches geometry
  event_id text NOT NULL,
  place_id text NOT NULL,
  PRIMARY KEY (event_id, place_id)
);
CREATE INDEX idx_event_places_place ON event_places (place_id, event_id);

-- MeshCore relay topology: derived telemetry, not proto-canonical.
CREATE TABLE mesh_observations (       -- Tier 0 reception firehose (short-lived)
  -- BY DEFAULT, not ALWAYS: the SQLite importer preserves ids (the compaction
  -- watermark in mesh_meta refers to them) and resets the sequence after.
  id         bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
  pubkey     text NOT NULL,
  heard_at   bigint NOT NULL,          -- OUR receive time (unix s)
  broker     text NOT NULL DEFAULT '',
  gateway    text NOT NULL DEFAULT '',
  snr        double precision,
  rssi       integer,
  hop_count  bigint NOT NULL DEFAULT 0,
  path       text NOT NULL DEFAULT '', -- comma-joined hop prefix hashes (hex)
  path_nodes text NOT NULL DEFAULT ''  -- resolved pubkeys ('' where unresolved)
);
CREATE INDEX idx_mesh_obs_heard  ON mesh_observations (heard_at);
CREATE INDEX idx_mesh_obs_pubkey ON mesh_observations (pubkey, heard_at);

CREATE TABLE mesh_link_rollup (        -- Tier 1 per-link-per-day topology
  a_pubkey     text NOT NULL,          -- canonical a < b
  b_pubkey     text NOT NULL,
  bucket       bigint NOT NULL,        -- heard_at truncated to the UTC day
  observations bigint NOT NULL DEFAULT 0,
  best_snr     double precision,
  first_seen   bigint NOT NULL,
  last_seen    bigint NOT NULL,
  PRIMARY KEY (a_pubkey, b_pubkey, bucket)
);
CREATE INDEX idx_mesh_link_bucket ON mesh_link_rollup (bucket);

CREATE TABLE mesh_meta (key text PRIMARY KEY, value bigint NOT NULL);

CREATE TABLE mesh_activity (           -- per node: our receive times (0 = never)
  pubkey          text PRIMARY KEY,
  last_heard_at   bigint NOT NULL DEFAULT 0,
  last_relayed_at bigint NOT NULL DEFAULT 0
);

CREATE TABLE mesh_telemetry (          -- one row per monitor read; irreplaceable
  pubkey      text NOT NULL,
  reported_at bigint NOT NULL,         -- the monitor's clock, clamped on arrival
  received_at bigint NOT NULL,         -- our clock
  reporter    text NOT NULL DEFAULT '',
  last_success_at bigint,
  last_attempt_at bigint,
  -- GAUGES. NULL = the monitor could not read it. Never 0.
  battery_volts      double precision,
  battery_pct        double precision,
  battery_pct_source text NOT NULL DEFAULT '',
  temperature_c      double precision,
  humidity           double precision,
  pressure           double precision,
  noise_floor_dbm    bigint,
  last_snr_db        double precision,
  last_rssi_dbm      bigint,
  tx_queue_len       bigint,
  -- COUNTERS since the node booted.
  uptime_s      bigint NOT NULL DEFAULT 0,
  airtime_ms    bigint NOT NULL DEFAULT 0,
  rx_airtime_ms bigint NOT NULL DEFAULT 0,
  packets_sent  bigint NOT NULL DEFAULT 0,
  packets_recv  bigint NOT NULL DEFAULT 0,
  sent_flood    bigint NOT NULL DEFAULT 0,
  sent_direct   bigint NOT NULL DEFAULT 0,
  recv_flood    bigint NOT NULL DEFAULT 0,
  recv_direct   bigint NOT NULL DEFAULT 0,
  direct_dups   bigint NOT NULL DEFAULT 0,
  flood_dups    bigint NOT NULL DEFAULT 0,
  full_evts     bigint NOT NULL DEFAULT 0,
  recv_errors   bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (pubkey, reported_at)
);
CREATE INDEX idx_mesh_telem_time ON mesh_telemetry (reported_at);

CREATE TABLE burn_readings (            -- pushed burn-line readings (staging)
  line_id     text PRIMARY KEY,
  status      text NOT NULL,            -- green | red | orange
  message     text NOT NULL DEFAULT '',
  transcript  text NOT NULL DEFAULT '',
  confidence  integer NOT NULL DEFAULT 0,
  observed_at bigint NOT NULL,          -- when the line was CALLED
  received_at bigint NOT NULL,          -- when we accepted the push
  reporter_id text NOT NULL DEFAULT ''
);

CREATE TABLE road_samples (             -- road segment status per refresh
  road_id    text NOT NULL,
  at         bigint NOT NULL,
  status     integer NOT NULL,          -- api.RoadStatus
  congestion integer NOT NULL,          -- api.CongestionLevel
  duration_m integer,                   -- NULL = not reported
  delay_m    integer,
  PRIMARY KEY (road_id, at)
);
CREATE INDEX idx_road_samples_status ON road_samples (road_id, status, at);

-- Change counters for ETag validators, bumped in the SAME transaction as the
-- write they describe, so every process reading the database agrees on them.
-- Keys: 'revisions' (event revisions written), 'refresh:<LAYER>' (in-place
-- blob rewrites in a layer), 'mesh_activity', 'places'. A missing key reads
-- as 0.
CREATE TABLE versions (
  key   text PRIMARY KEY,
  value bigint NOT NULL
);
