-- Legacy SQLite schema version 4, verbatim from legacy-efs-final:internal/store/store.go (migrationV4).
-- Test fixture only: never edit (it is what production ran).
CREATE INDEX idx_revisions_observed
  ON event_revisions(observed_at DESC, event_id ASC, revision DESC);
