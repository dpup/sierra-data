-- Grid store schema version 2: the writer lease and version notifications.
--
-- writer_lease is the one-row record of which process may write. The
-- exclusion itself is a session advisory lock (internal/store/lease.go); this
-- row adds what a lock cannot carry:
--   * epoch: bumped by each new holder. Every write transaction checks it
--     (inTx: SELECT ... FOR KEY SHARE WHERE epoch = mine), so a process that
--     lost its session (RDS failover, a network cut) cannot land a write after
--     the next holder has started, even from a pooled connection that never
--     noticed. The bump takes FOR UPDATE, which waits for the previous term's
--     open write transactions and then fails their successors.
--   * holder / address: who holds it and where other tasks reach it (push
--     ingest on a reader forwards to address). Cleared on a clean release.
--   * heartbeat_at: refreshed every 2 s by the holder; a reader treats a
--     holder silent for longer than its staleness bound as gone.
-- Timestamps here are bookkeeping, not domain data, so timestamptz.

CREATE TABLE writer_lease (
  id           smallint PRIMARY KEY CHECK (id = 1),
  epoch        bigint NOT NULL DEFAULT 0,
  holder       text NOT NULL DEFAULT '',
  address      text NOT NULL DEFAULT '',
  acquired_at  timestamptz,
  heartbeat_at timestamptz
);
INSERT INTO writer_lease (id) VALUES (1);

-- Every change to versions (the ETag validators' counters) notifies the
-- grid_versions channel. Readers LISTEN and refresh their cached validators;
-- a slow poll covers a dropped listener. Statement level, and PostgreSQL
-- folds identical notifications within one transaction, so a writing
-- transaction sends one notification however many counters it bumps; it is
-- delivered at commit, never for a rolled-back write.
CREATE FUNCTION grid_versions_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('grid_versions', '');
  RETURN NULL;
END
$$;

CREATE TRIGGER versions_notify
  AFTER INSERT OR UPDATE OR DELETE ON versions
  FOR EACH STATEMENT EXECUTE FUNCTION grid_versions_notify();
