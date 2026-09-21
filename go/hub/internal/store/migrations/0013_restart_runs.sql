-- M1.9: a rolling restart of an app its cluster's agent delivers has no App
-- object to annotate. It is a deployment run of the same release and
-- configuration that stamps every pod template.

ALTER TABLE deployment_runs DROP CONSTRAINT deployment_runs_reason_check;
ALTER TABLE deployment_runs ADD CONSTRAINT deployment_runs_reason_check
  CHECK (reason IN ('deploy', 'rollback', 'promotion', 'restart'));

-- The restart stamp the run renders with (Unix milliseconds): the time of a
-- `restart` run, carried forward unchanged by every later run of the target,
-- so the next deploy does not restart the pods a second time. Set when the
-- run is inserted, an input like the others.
ALTER TABLE deployment_runs ADD COLUMN restarted_at BIGINT;

CREATE OR REPLACE FUNCTION kuben_run_inputs_are_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.org_id IS DISTINCT FROM OLD.org_id
     OR NEW.project_id IS DISTINCT FROM OLD.project_id
     OR NEW.application_id IS DISTINCT FROM OLD.application_id
     OR NEW.target_id IS DISTINCT FROM OLD.target_id
     OR NEW.release_id IS DISTINCT FROM OLD.release_id
     OR NEW.config_revision_id IS DISTINCT FROM OLD.config_revision_id
     OR (OLD.render_plan_id IS NOT NULL AND NEW.render_plan_id IS DISTINCT FROM OLD.render_plan_id)
     OR NEW.generation IS DISTINCT FROM OLD.generation
     OR NEW.lifecycle_uid IS DISTINCT FROM OLD.lifecycle_uid
     OR NEW.reason IS DISTINCT FROM OLD.reason
     OR NEW.strategy IS DISTINCT FROM OLD.strategy
     OR NEW.requested_by IS DISTINCT FROM OLD.requested_by
     OR NEW.approval_plan_hash IS DISTINCT FROM OLD.approval_plan_hash
     OR NEW.operation_id IS DISTINCT FROM OLD.operation_id
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.restarted_at IS DISTINCT FROM OLD.restarted_at THEN
    RAISE EXCEPTION 'the inputs of deployment run % are immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
