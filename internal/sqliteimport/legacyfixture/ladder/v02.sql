-- Legacy SQLite schema version 2, verbatim from legacy-efs-final:internal/store/store.go (migrationV2).
-- Test fixture only: never edit (it is what production ran).
ALTER TABLE events ADD COLUMN last_seen_at INTEGER NOT NULL DEFAULT 0
