-- Legacy SQLite schema version 13, verbatim from legacy-efs-final:internal/store/store.go (migrationV13).
-- Test fixture only: never edit (it is what production ran).
CREATE TABLE road_samples (
  road_id     TEXT NOT NULL,
  at          INTEGER NOT NULL,  -- the roads service's refresh time (unix s)
  status      INTEGER NOT NULL,  -- api.RoadStatus
  congestion  INTEGER NOT NULL,  -- api.CongestionLevel
  duration_m  INTEGER,           -- travel time, minutes; NULL = not reported
  delay_m     INTEGER,
  PRIMARY KEY (road_id, at)
) WITHOUT ROWID
