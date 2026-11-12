-- Legacy SQLite schema version 12, verbatim from legacy-efs-final:internal/store/store.go (migrationV12).
-- Test fixture only: never edit (it is what production ran).
CREATE TABLE event_lite (
  event_id TEXT PRIMARY KEY,
  proto    BLOB NOT NULL
) WITHOUT ROWID
