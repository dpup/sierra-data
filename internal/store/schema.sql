-- Migration v1: full grid schema per docs/v2-api-spec.md §4, plus
-- sources.status and sources.disappearance (implementation plan §2.2).
-- Proto blob is canonical; scalar columns exist only as indexes and are
-- always derivable from the blob. All timestamps are unix seconds.

CREATE TABLE events (
  id           TEXT PRIMARY KEY,
  layer        INTEGER NOT NULL,
  severity     INTEGER NOT NULL,
  status       INTEGER NOT NULL,
  source_id    TEXT NOT NULL REFERENCES sources(id),
  effective    INTEGER,
  expires      INTEGER,
  observed_at  INTEGER NOT NULL,
  ingested_at  INTEGER NOT NULL,
  revision     INTEGER NOT NULL DEFAULT 1,
  content_hash TEXT NOT NULL,       -- normalized proto hash; unchanged => no revision
  proto        BLOB NOT NULL
);
-- v2 (store.go migrationV2) adds: last_seen_at INTEGER NOT NULL DEFAULT 0

CREATE INDEX idx_events_active ON events(status, severity DESC, observed_at DESC);
CREATE INDEX idx_events_layer  ON events(layer, status);

CREATE TABLE event_revisions (       -- full snapshots; storage trivial at this volume
  event_id    TEXT NOT NULL,
  revision    INTEGER NOT NULL,
  observed_at INTEGER NOT NULL,
  ingested_at INTEGER NOT NULL,
  proto       BLOB NOT NULL,
  PRIMARY KEY (event_id, revision)
);

CREATE TABLE places (
  id        TEXT PRIMARY KEY,
  kind      INTEGER NOT NULL,
  name      TEXT NOT NULL,
  slug      TEXT NOT NULL UNIQUE,
  parent_id TEXT,
  proto     BLOB NOT NULL
);

CREATE TABLE event_places (          -- computed at ingest; hot path never touches geometry
  event_id TEXT NOT NULL,
  place_id TEXT NOT NULL,
  PRIMARY KEY (event_id, place_id)
);
CREATE INDEX idx_event_places_place ON event_places(place_id, event_id);

CREATE VIRTUAL TABLE event_geo USING rtree(rowid, min_lat, max_lat, min_lng, max_lng);
CREATE TABLE event_geo_map (rowid INTEGER PRIMARY KEY, event_id TEXT NOT NULL UNIQUE);

CREATE TABLE sources (
  id                    TEXT PRIMARY KEY,
  name                  TEXT NOT NULL,
  attribution           TEXT NOT NULL DEFAULT '',
  poll_interval_seconds INTEGER NOT NULL,
  stale_after_seconds   INTEGER,     -- NULL => 3x poll interval
  expire_after_seconds  INTEGER,     -- NULL => never auto-expire
  last_success_at       INTEGER,
  last_attempt_at       INTEGER,
  last_error            TEXT NOT NULL DEFAULT '',
  status                INTEGER NOT NULL DEFAULT 0,
  disappearance         TEXT NOT NULL DEFAULT 'resolve'  -- 'resolve' | 'expire'
);

CREATE TABLE subscriptions (         -- phase 2, anticipated
  id           TEXT PRIMARY KEY,
  channel      TEXT NOT NULL,        -- 'email' | 'ntfy' | 'rss'
  address      TEXT NOT NULL,
  place_id     TEXT NOT NULL,
  min_severity INTEGER NOT NULL DEFAULT 2,
  created_at   INTEGER NOT NULL,
  confirmed_at INTEGER
);

-- v3 (store.go migrationV3) adds the MeshCore relay-topology tables (derived
-- telemetry, NOT proto-canonical — see docs/mesh-topology-design.md):
--   mesh_observations  Tier 0 append-only reception firehose (short-lived)
--   mesh_link_rollup   Tier 1 per-link-per-day topology history
--   mesh_meta          KV for the compaction watermark
