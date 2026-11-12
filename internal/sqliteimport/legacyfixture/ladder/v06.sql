-- Legacy SQLite schema version 6, verbatim from legacy-efs-final:internal/store/store.go (migrationV6).
-- Test fixture only: never edit (it is what production ran).
CREATE TABLE mesh_telemetry (
  pubkey      TEXT NOT NULL,
  reported_at INTEGER NOT NULL,       -- the monitor's generated_at, clamped on arrival
  received_at INTEGER NOT NULL,       -- our clock: the observed/ingested split, kept
  reporter    TEXT NOT NULL DEFAULT '',
  -- The monitor's own attempt/success stamps AT THAT REPORT. Stored so an
  -- archived row rehydrates grid.v1.MeshAdminTelemetry exactly: the event holds
  -- the latest reading, this table holds all of them, and they are the same
  -- message rather than two drifting descriptions of one thing.
  last_success_at INTEGER,
  last_attempt_at INTEGER,

  -- GAUGES. NULL = the monitor could not read it. Never 0.
  battery_volts      REAL,
  battery_pct        REAL,
  battery_pct_source TEXT NOT NULL DEFAULT '',  -- "measured" | "estimated"
  temperature_c      REAL,
  humidity           REAL,
  pressure           REAL,
  noise_floor_dbm    INTEGER,
  last_snr_db        REAL,
  last_rssi_dbm      INTEGER,
  tx_queue_len       INTEGER,

  -- COUNTERS, lifetime since the node booted. Reset detection is uptime_s
  -- going backwards; the read path segments on it so a reboot breaks the line
  -- instead of plunging it.
  uptime_s      INTEGER NOT NULL DEFAULT 0,
  airtime_ms    INTEGER NOT NULL DEFAULT 0,
  rx_airtime_ms INTEGER NOT NULL DEFAULT 0,
  packets_sent  INTEGER NOT NULL DEFAULT 0,
  packets_recv  INTEGER NOT NULL DEFAULT 0,
  sent_flood    INTEGER NOT NULL DEFAULT 0,
  sent_direct   INTEGER NOT NULL DEFAULT 0,
  recv_flood    INTEGER NOT NULL DEFAULT 0,
  recv_direct   INTEGER NOT NULL DEFAULT 0,
  direct_dups   INTEGER NOT NULL DEFAULT 0,
  flood_dups    INTEGER NOT NULL DEFAULT 0,
  full_evts     INTEGER NOT NULL DEFAULT 0,
  recv_errors   INTEGER NOT NULL DEFAULT 0,

  -- (pubkey, reported_at) is what makes the write idempotent: the mesh poller
  -- ticks every 60s and reports arrive every ~15 minutes, so it re-offers the
  -- same sample ~15 times. Keying on OUR receive time instead would store 15
  -- copies of one reading.
  PRIMARY KEY (pubkey, reported_at)
) WITHOUT ROWID;
CREATE INDEX idx_mesh_telem_time ON mesh_telemetry(reported_at);
