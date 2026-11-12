-- Legacy SQLite schema version 10, verbatim from legacy-efs-final:internal/store/store.go (migrationV10).
-- Test fixture only: never edit (it is what production ran).
ALTER TABLE events ADD COLUMN refreshes INTEGER NOT NULL DEFAULT 0
