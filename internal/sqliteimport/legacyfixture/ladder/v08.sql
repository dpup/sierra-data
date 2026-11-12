-- Legacy SQLite schema version 8, verbatim from legacy-efs-final:internal/store/store.go (migrationV8).
-- Test fixture only: never edit (it is what production ran).
ALTER TABLE burn_readings ADD COLUMN reporter_id TEXT NOT NULL DEFAULT ''
