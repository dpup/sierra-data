-- Legacy SQLite schema version 9, verbatim from legacy-efs-final:internal/store/store.go (migrationV9).
-- Test fixture only: never edit (it is what production ran).
CREATE TABLE mesh_activity (
  pubkey          TEXT PRIMARY KEY,           -- full public key, lowercase hex
  last_heard_at   INTEGER NOT NULL DEFAULT 0, -- our receive time of its last advert
  last_relayed_at INTEGER NOT NULL DEFAULT 0  -- our receive time of the last packet it relayed
) WITHOUT ROWID
