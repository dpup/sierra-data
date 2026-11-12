-- Legacy SQLite schema version 7, verbatim from legacy-efs-final:internal/store/store.go (migrationV7).
-- Test fixture only: never edit (it is what production ran).
CREATE TABLE burn_readings (
  line_id      TEXT PRIMARY KEY,     -- config line id ("calaveras-apcd")
  status       TEXT NOT NULL,        -- upstream vocabulary: green | red | orange
  message      TEXT NOT NULL DEFAULT '',
  transcript   TEXT NOT NULL DEFAULT '',
  confidence   INTEGER NOT NULL DEFAULT 0,
  observed_at  INTEGER NOT NULL,     -- when the line was CALLED (the freshness signal)
  received_at  INTEGER NOT NULL      -- when we accepted the push (diagnostic only)
)
