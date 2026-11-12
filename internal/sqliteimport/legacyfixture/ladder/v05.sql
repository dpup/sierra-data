-- Legacy SQLite schema version 5, verbatim from legacy-efs-final:internal/store/store.go (migrationV5).
-- Test fixture only: never edit (it is what production ran).
ALTER TABLE sources ADD COLUMN homepage_url TEXT NOT NULL DEFAULT ''
