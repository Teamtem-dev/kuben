-- M4.9: owners, change freezes, paused delivery, alert silences and
-- emergency rollbacks (plan §15, S05).
--
-- Each control has one effect and nothing else:
-- * an owner names who answers for a project or an application;
-- * a freeze refuses new changes to an environment for a while;
-- * a pause holds delivery of a target: runs are accepted but not written
--   until it is resumed, and then only the newest is (older ones are
--   superseded);
-- * a silence keeps alerts about an environment or target quiet for a
--   while, without changing what is detected or recorded;
-- * an emergency rollback is a run that passes approvals, freezes, the scan
--   gate and a pause, by a person with a reason, audited.

CREATE TABLE owners (
  org_id      TEXT NOT NULL REFERENCES organizations (id),
  project_id  UUID NOT NULL,
  -- The application, or NULL for the project itself.
  application_id UUID,
  owner       TEXT NOT NULL CHECK (char_length(owner) BETWEEN 1 AND 256),
  contact     TEXT CHECK (char_length(contact) <= 512),
  runbook_url TEXT CHECK (runbook_url ~ '^https?://' AND char_length(runbook_url) <= 2048),
  updated_by  TEXT NOT NULL,
  updated_at  BIGINT NOT NULL,
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id),
  FOREIGN KEY (org_id, project_id, application_id) REFERENCES applications (org_id, project_id, id)
);
CREATE UNIQUE INDEX owners_subject ON owners (project_id, (COALESCE(application_id, '00000000-0000-0000-0000-000000000000'::uuid)));

ALTER TABLE owners ENABLE ROW LEVEL SECURITY;
ALTER TABLE owners FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON owners
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

CREATE TABLE environment_freezes (
  id             UUID PRIMARY KEY,
  org_id         TEXT NOT NULL,
  project_id     UUID NOT NULL,
  environment_id UUID NOT NULL,
  reason         TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1024),
  created_by     TEXT NOT NULL,
  created_at     BIGINT NOT NULL,
  starts_at      BIGINT NOT NULL,
  ends_at        BIGINT NOT NULL CHECK (ends_at > starts_at),
  lifted_at      BIGINT,
  lifted_by      TEXT,
  CHECK ((lifted_at IS NULL) = (lifted_by IS NULL)),
  FOREIGN KEY (org_id, project_id, environment_id) REFERENCES environments (org_id, project_id, id)
);
CREATE INDEX environment_freezes_active ON environment_freezes (environment_id, ends_at) WHERE lifted_at IS NULL;

CREATE TABLE silences (
  id             UUID PRIMARY KEY,
  org_id         TEXT NOT NULL,
  project_id     UUID NOT NULL,
  environment_id UUID NOT NULL,
  -- One app of the environment, or all of it when NULL.
  target_id      UUID REFERENCES application_targets (id),
  reason         TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1024),
  created_by     TEXT NOT NULL,
  created_at     BIGINT NOT NULL,
  ends_at        BIGINT NOT NULL CHECK (ends_at > created_at),
  lifted_at      BIGINT,
  lifted_by      TEXT,
  CHECK ((lifted_at IS NULL) = (lifted_by IS NULL)),
  FOREIGN KEY (org_id, project_id, environment_id) REFERENCES environments (org_id, project_id, id)
);
CREATE INDEX silences_active ON silences (environment_id, ends_at) WHERE lifted_at IS NULL;

-- A freeze or silence keeps what it said; it is only lifted, once.
CREATE FUNCTION kuben_lift_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' OR OLD.lifted_at IS NOT NULL
     OR (to_jsonb(NEW) - 'lifted_at' - 'lifted_by') IS DISTINCT FROM (to_jsonb(OLD) - 'lifted_at' - 'lifted_by') THEN
    RAISE EXCEPTION '% % can only be lifted, once', TG_TABLE_NAME, OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER environment_freezes_lift_only BEFORE UPDATE OR DELETE ON environment_freezes
  FOR EACH ROW EXECUTE FUNCTION kuben_lift_only();
CREATE TRIGGER silences_lift_only BEFORE UPDATE OR DELETE ON silences
  FOR EACH ROW EXECUTE FUNCTION kuben_lift_only();

ALTER TABLE environment_freezes ENABLE ROW LEVEL SECURITY;
ALTER TABLE environment_freezes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON environment_freezes
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());
ALTER TABLE silences ENABLE ROW LEVEL SECURITY;
ALTER TABLE silences FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON silences
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

ALTER TABLE application_targets
  ADD COLUMN paused_at    BIGINT,
  ADD COLUMN paused_by    TEXT,
  ADD COLUMN pause_reason TEXT CHECK (char_length(pause_reason) <= 1024),
  ADD CONSTRAINT application_targets_pause CHECK ((paused_at IS NULL) = (paused_by IS NULL));

ALTER TABLE deployment_runs ADD COLUMN emergency_reason TEXT CHECK (char_length(emergency_reason) BETWEEN 1 AND 1024);
ALTER TABLE deployment_runs DROP CONSTRAINT deployment_runs_reason_check;
ALTER TABLE deployment_runs ADD CONSTRAINT deployment_runs_reason_check
  CHECK (reason IN ('deploy', 'rollback', 'promotion', 'restart', 'handover', 'build', 'rotation', 'emergency'));
ALTER TABLE deployment_runs ADD CONSTRAINT deployment_runs_emergency_reason
  CHECK ((reason = 'emergency') = (emergency_reason IS NOT NULL));

-- The reason of an emergency is an input of its run, like the others.
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
     OR NEW.restarted_at IS DISTINCT FROM OLD.restarted_at
     OR NEW.approvals_required IS DISTINCT FROM OLD.approvals_required
     OR NEW.approval_expires_at IS DISTINCT FROM OLD.approval_expires_at
     OR NEW.policy_revision IS DISTINCT FROM OLD.policy_revision
     OR NEW.emergency_reason IS DISTINCT FROM OLD.emergency_reason THEN
    RAISE EXCEPTION 'the inputs of deployment run % are immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
