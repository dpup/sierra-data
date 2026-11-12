-- Legacy SQLite schema version 11, verbatim from legacy-efs-final:internal/store/store.go (migrationV11).
-- Test fixture only: never edit (it is what production ran).
ALTER TABLE sources ADD COLUMN kind INTEGER NOT NULL DEFAULT 1
