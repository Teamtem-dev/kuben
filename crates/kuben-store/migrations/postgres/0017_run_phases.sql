-- M2.16: the timeline of a deployment run. Every phase a run enters is kept
-- with the time it entered it, so the console can show how a deploy went
-- (and where it stopped), not only where it stands now.
--
-- A trigger writes the rows, so every writer of `deployment_runs` (the API,
-- the materializer, the agent link, recovery) is covered without knowing of
-- this table. Rows are only ever added.
CREATE TABLE deployment_run_phases (
  run_id     UUID NOT NULL REFERENCES deployment_runs (id) ON DELETE CASCADE,
  org_id     TEXT NOT NULL,
  seq        BIGINT GENERATED ALWAYS AS IDENTITY,
  phase      TEXT NOT NULL,
  entered_at BIGINT NOT NULL,
  PRIMARY KEY (run_id, seq)
);

-- Like `deployment_runs`, whose writers include workers that act for every
-- organization: tenants read their rows through `org_id` in every query.
CREATE INDEX deployment_run_phases_org ON deployment_run_phases (org_id, run_id);

CREATE FUNCTION kuben_record_run_phase() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    INSERT INTO deployment_run_phases (run_id, org_id, phase, entered_at)
    VALUES (NEW.id, NEW.org_id, NEW.phase, NEW.created_at);
  ELSIF NEW.phase IS DISTINCT FROM OLD.phase THEN
    INSERT INTO deployment_run_phases (run_id, org_id, phase, entered_at)
    VALUES (NEW.id, NEW.org_id, NEW.phase, NEW.updated_at);
  END IF;
  RETURN NULL;
END;
$$;

CREATE TRIGGER deployment_runs_phase_history
  AFTER INSERT OR UPDATE OF phase ON deployment_runs
  FOR EACH ROW EXECUTE FUNCTION kuben_record_run_phase();

CREATE FUNCTION kuben_run_phases_are_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'deployment_run_phases rows are never changed';
END;
$$;

CREATE TRIGGER deployment_run_phases_append_only
  BEFORE UPDATE ON deployment_run_phases
  FOR EACH ROW EXECUTE FUNCTION kuben_run_phases_are_append_only();

-- Runs that existed before this migration start their timeline where they
-- stand now.
INSERT INTO deployment_run_phases (run_id, org_id, phase, entered_at)
SELECT id, org_id, phase, updated_at FROM deployment_runs ORDER BY created_at;
