-- M4.8: the upgrade journal (plan §17.4, S07).
--
-- Every server version that started against this database, and every run
-- of the migrations a new version brought. A run is written as `running`
-- before the migrations and settled after them, so an interrupted upgrade
-- is visible and the next start resumes it forward: each migration is its
-- own transaction, and applied ones are never repeated. Installation-wide.

CREATE TABLE server_versions (
  version          TEXT PRIMARY KEY,
  schema           BIGINT NOT NULL,
  first_started_at BIGINT NOT NULL,
  last_started_at  BIGINT NOT NULL,
  starts           BIGINT NOT NULL DEFAULT 1 CHECK (starts > 0)
);

CREATE TABLE upgrade_runs (
  id           UUID PRIMARY KEY,
  from_version TEXT,
  to_version   TEXT NOT NULL,
  from_schema  BIGINT NOT NULL,
  to_schema    BIGINT NOT NULL,
  status       TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
  detail       TEXT CHECK (char_length(detail) <= 2048),
  started_at   BIGINT NOT NULL,
  finished_at  BIGINT,
  CHECK ((status = 'running') = (finished_at IS NULL))
);
CREATE INDEX upgrade_runs_started ON upgrade_runs (started_at DESC);

-- A run only settles, once.
CREATE FUNCTION kuben_upgrade_run_rules() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' OR OLD.status <> 'running'
     OR ROW(NEW.id, NEW.from_version, NEW.to_version, NEW.from_schema, NEW.started_at)
        IS DISTINCT FROM ROW(OLD.id, OLD.from_version, OLD.to_version, OLD.from_schema, OLD.started_at) THEN
    RAISE EXCEPTION 'upgrade run % is settled once and kept', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER upgrade_run_rules BEFORE UPDATE OR DELETE ON upgrade_runs
  FOR EACH ROW EXECUTE FUNCTION kuben_upgrade_run_rules();
