-- Releases, target configuration revisions, render plans and deployment runs
-- (ADR-026; plan §8.1, §8.5, §8.7, §8.9).
--
-- A release is portable and immutable (I03): an OCI digest per process, the
-- process contract and non-secret portable config, and no rollout status. A
-- deployment run is one attempt to make a release effective on one target.
-- It owns exactly one generation of that target, and a newer run supersedes
-- it (I05, I11). Keys are tenant-aware as in 0004, and a run can only pair a
-- release and a target of the same application.
--
-- Deployment workers read these tables across organizations, so, like the
-- operations of 0005, they have no row-level security; request paths filter
-- by organization explicitly.

ALTER TABLE application_targets ADD UNIQUE (org_id, project_id, application_id, id);
-- Numbers target_config_revisions in one transaction; never max() + 1.
ALTER TABLE application_targets ADD COLUMN config_revision_seq BIGINT NOT NULL DEFAULT 0;

CREATE TABLE releases (
  id               UUID PRIMARY KEY,
  org_id           TEXT NOT NULL,
  project_id       UUID NOT NULL,
  application_id   UUID NOT NULL,
  -- Process name → OCI digest; several processes may share one image.
  artifacts        JSONB NOT NULL
                   CHECK (jsonb_typeof(artifacts) = 'object' AND artifacts <> '{}'::jsonb),
  process_contract JSONB NOT NULL,
  portable_config  JSONB NOT NULL,
  renderer_schema  INTEGER NOT NULL CHECK (renderer_schema > 0),
  source           JSONB,
  -- sha256 of the canonical content: finds a duplicate, never the identity.
  content_hash     BYTEA NOT NULL,
  created_by       TEXT NOT NULL,
  created_at       BIGINT NOT NULL,
  UNIQUE (application_id, content_hash),
  UNIQUE (org_id, project_id, application_id, id),
  FOREIGN KEY (org_id, project_id, application_id) REFERENCES applications (org_id, project_id, id)
);
CREATE TRIGGER releases_append_only BEFORE UPDATE OR DELETE ON releases
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();

CREATE TABLE target_config_revisions (
  id          UUID PRIMARY KEY,
  org_id      TEXT NOT NULL,
  project_id  UUID NOT NULL,
  target_id   UUID NOT NULL,
  revision    BIGINT NOT NULL CHECK (revision > 0),
  -- Environment config, replicas, probes, routes and the security snapshot.
  -- Secret values never appear here, only references (I10).
  config      JSONB NOT NULL,
  config_hash BYTEA NOT NULL,
  created_by  TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  UNIQUE (target_id, revision),
  UNIQUE (org_id, project_id, target_id, id),
  FOREIGN KEY (org_id, project_id, target_id) REFERENCES application_targets (org_id, project_id, id)
);
CREATE TRIGGER target_config_revisions_append_only BEFORE UPDATE OR DELETE ON target_config_revisions
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();

-- Frozen at acceptance and addressed by content: a renderer upgrade never
-- re-renders an existing generation (I22).
CREATE TABLE render_plans (
  id                  UUID PRIMARY KEY,
  org_id              TEXT NOT NULL REFERENCES organizations (id),
  content_digest      BYTEA NOT NULL,
  renderer_version    TEXT NOT NULL,
  capability_snapshot JSONB NOT NULL,
  resources           JSONB NOT NULL,
  created_at          BIGINT NOT NULL,
  UNIQUE (org_id, content_digest),
  UNIQUE (org_id, id)
);
CREATE TRIGGER render_plans_append_only BEFORE UPDATE OR DELETE ON render_plans
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();

CREATE TABLE deployment_runs (
  id                 UUID PRIMARY KEY,
  org_id             TEXT NOT NULL,
  project_id         UUID NOT NULL,
  application_id     UUID NOT NULL,
  target_id          UUID NOT NULL,
  release_id         UUID NOT NULL,
  config_revision_id UUID NOT NULL,
  -- Set once, when the plan is frozen.
  render_plan_id     UUID,
  -- The target generation this run owns and the target's lifecycle UID.
  generation         BIGINT NOT NULL CHECK (generation > 0),
  lifecycle_uid      UUID NOT NULL,
  reason             TEXT NOT NULL CHECK (reason IN ('deploy', 'rollback', 'promotion')),
  strategy           TEXT NOT NULL DEFAULT 'rolling',
  requested_by       TEXT NOT NULL,
  approval_plan_hash BYTEA,
  -- kuben_core::ops::RunPhase; outcome and recovery_outcome stay separate so a
  -- failed deploy is never renamed "succeeded" by a successful rollback.
  phase              TEXT NOT NULL DEFAULT 'planned' CHECK (phase IN (
                       'planned', 'awaitingApproval', 'pendingDelivery', 'acceptedByCluster',
                       'preflight', 'blocked', 'applying', 'verifying', 'succeeded', 'failed',
                       'superseded', 'cancelRequested', 'cancelled', 'recoveryRequested',
                       'recovering', 'recovered', 'recoveryFailed', 'manualActionRequired')),
  outcome            TEXT,
  recovery_outcome   TEXT,
  operation_id       UUID NOT NULL UNIQUE REFERENCES operations (id),
  created_at         BIGINT NOT NULL,
  updated_at         BIGINT NOT NULL,
  UNIQUE (target_id, generation),
  FOREIGN KEY (org_id, project_id, application_id, target_id)
    REFERENCES application_targets (org_id, project_id, application_id, id),
  FOREIGN KEY (org_id, project_id, application_id, release_id)
    REFERENCES releases (org_id, project_id, application_id, id),
  FOREIGN KEY (org_id, project_id, target_id, config_revision_id)
    REFERENCES target_config_revisions (org_id, project_id, target_id, id),
  FOREIGN KEY (org_id, render_plan_id) REFERENCES render_plans (org_id, id)
);
CREATE INDEX deployment_runs_target ON deployment_runs (target_id, generation DESC);

-- A run's inputs never change; only its phase, outcomes and the one-time
-- render plan do.
CREATE FUNCTION kuben_run_inputs_are_immutable() RETURNS trigger
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
     OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'the inputs of deployment run % are immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER run_inputs_are_immutable BEFORE UPDATE ON deployment_runs
  FOR EACH ROW EXECUTE FUNCTION kuben_run_inputs_are_immutable();
