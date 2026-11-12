-- Legacy SQLite schema version 14, verbatim from legacy-efs-final:internal/store/store.go (migrationV14).
-- Test fixture only: never edit (it is what production ran).
CREATE INDEX idx_road_samples_status ON road_samples (road_id, status, at)
