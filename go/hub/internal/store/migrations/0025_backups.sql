-- M4.7: backups and restores of the installation (plan §17, S03).
--
-- `kuben backup` records every backup it writes, so the server can tell
-- when the newest good one is too old; `kuben restore` records every
-- restore and the generation jump that fences whatever the clusters saw
-- after the backup was taken. Both belong to the installation, not to an
-- organization: no row-level security.

CREATE TABLE backup_runs (
  id           UUID PRIMARY KEY,
  kind         TEXT NOT NULL CHECK (kind IN ('manual', 'scheduled')),
  status       TEXT NOT NULL CHECK (status IN ('succeeded', 'failed')),
  location     TEXT NOT NULL CHECK (char_length(location) <= 1024),
  bytes        BIGINT CHECK (bytes >= 0),
  sha256       TEXT CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  kuben        TEXT NOT NULL,
  schema       BIGINT NOT NULL,
  detail       TEXT CHECK (char_length(detail) <= 2048),
  started_at   BIGINT NOT NULL,
  finished_at  BIGINT NOT NULL CHECK (finished_at >= started_at)
);
CREATE INDEX backup_runs_finished ON backup_runs (status, finished_at DESC);
CREATE TRIGGER backup_runs_append_only BEFORE UPDATE OR DELETE ON backup_runs
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();

CREATE TABLE restore_events (
  id                UUID PRIMARY KEY,
  backup_created_at BIGINT NOT NULL,
  backup_kuben      TEXT NOT NULL,
  kuben             TEXT NOT NULL,
  generation_jump   BIGINT NOT NULL CHECK (generation_jump > 0),
  restored_at       BIGINT NOT NULL
);
CREATE TRIGGER restore_events_append_only BEFORE UPDATE OR DELETE ON restore_events
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();
