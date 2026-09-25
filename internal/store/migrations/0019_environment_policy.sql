-- M4.1: environment policy, deployment approvals and scoped roles
-- (kuben_core::policy; plan §8.2, §13).
--
-- A policy is an append-only revision per environment; the newest revision
-- applies. A deployment run records the approvals it needs, the policy
-- revision it was accepted under, when its approval window closes and the
-- hash of what approvers are shown; all of these are inputs, never changed.
-- Every decision is one append-only row per approver. Role bindings may be
-- scoped to a project or an environment, one binding per subject and node.

CREATE TABLE environment_policies (
  org_id             TEXT NOT NULL,
  project_id         UUID NOT NULL,
  environment_id     UUID NOT NULL,
  revision           BIGINT NOT NULL CHECK (revision > 0),
  required_approvals SMALLINT NOT NULL CHECK (required_approvals BETWEEN 0 AND 5),
  deploy_role        TEXT NOT NULL CHECK (deploy_role IN ('developer', 'admin', 'owner')),
  approve_role       TEXT NOT NULL CHECK (approve_role IN ('admin', 'owner')),
  approval_ttl_secs  INTEGER NOT NULL CHECK (approval_ttl_secs BETWEEN 300 AND 2592000),
  created_by         TEXT NOT NULL,
  created_at         BIGINT NOT NULL,
  PRIMARY KEY (environment_id, revision),
  FOREIGN KEY (org_id, project_id, environment_id) REFERENCES environments (org_id, project_id, id)
);
CREATE TRIGGER environment_policies_append_only BEFORE UPDATE OR DELETE ON environment_policies
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();

ALTER TABLE environment_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE environment_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON environment_policies
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

-- Production environments that exist before this migration get the
-- production policy: one approval by an admin who did not ask for the change.
INSERT INTO environment_policies
  (org_id, project_id, environment_id, revision, required_approvals, deploy_role, approve_role,
   approval_ttl_secs, created_by, created_at)
SELECT org_id, project_id, id, 1, 1, 'developer', 'admin', 604800, 'system:migration-0019',
       (extract(epoch FROM clock_timestamp()) * 1000)::BIGINT
FROM environments
WHERE COALESCE(env_type, CASE WHEN protected THEN 'production' ELSE 'standard' END) = 'production';

ALTER TABLE deployment_runs
  ADD COLUMN approvals_required  SMALLINT NOT NULL DEFAULT 0 CHECK (approvals_required BETWEEN 0 AND 5),
  ADD COLUMN approval_expires_at BIGINT,
  ADD COLUMN policy_revision     BIGINT;
ALTER TABLE deployment_runs ADD CONSTRAINT deployment_runs_approval_inputs CHECK (
  (approvals_required = 0 AND approval_expires_at IS NULL)
  OR (approvals_required > 0 AND approval_expires_at IS NOT NULL AND approval_plan_hash IS NOT NULL)
);

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
     OR NEW.policy_revision IS DISTINCT FROM OLD.policy_revision THEN
    RAISE EXCEPTION 'the inputs of deployment run % are immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;

CREATE TABLE run_approvals (
  run_id     UUID NOT NULL REFERENCES deployment_runs (id),
  org_id     TEXT NOT NULL REFERENCES organizations (id),
  -- The user who decided; never the run's requester.
  approver   TEXT NOT NULL,
  decision   TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  plan_hash  BYTEA NOT NULL,
  comment    TEXT CHECK (char_length(comment) <= 1024),
  decided_at BIGINT NOT NULL,
  PRIMARY KEY (run_id, approver)
);
CREATE INDEX run_approvals_org ON run_approvals (org_id, run_id);
CREATE TRIGGER run_approvals_append_only BEFORE UPDATE OR DELETE ON run_approvals
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();

ALTER TABLE run_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON run_approvals
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

-- One binding per subject and node: an update changes the role in place.
-- Duplicates cannot have been created with different roles (every writer
-- updates all of them), so the oldest is kept.
DELETE FROM role_bindings a USING role_bindings b
WHERE a.org_id = b.org_id AND a.subject_kind = b.subject_kind AND a.subject_id = b.subject_id
  AND a.scope_kind = b.scope_kind AND COALESCE(a.scope_uid, '') = COALESCE(b.scope_uid, '')
  AND a.id > b.id;
CREATE UNIQUE INDEX role_bindings_one_per_node
  ON role_bindings (org_id, subject_kind, subject_id, scope_kind, (COALESCE(scope_uid, '')));
CREATE INDEX role_bindings_scope ON role_bindings (org_id, scope_kind, scope_uid);
