-- M3: Git → isolated build (ADR-028; plan §14.1, §18.1).
--
-- A GitHub App installation belongs to one organization. A source binding
-- ties one target to one repository and branch with an editable build
-- recipe; the head it last read from the provider is kept with the target's
-- source epoch at that read. A build attempt builds exactly one commit for
-- one epoch and build configuration revision; its phase follows
-- kuben_core::ops::BuildPhase, and a terminal attempt never changes again.
-- Build slots are the atomic admission record of ADR-028: a slot row exists
-- exactly while its attempt may hold a Job.

-- Looked up by installation id before the organization is known (webhooks),
-- so it has no row-level security; request paths filter by organization.
CREATE TABLE git_installations (
  provider        TEXT NOT NULL CHECK (provider IN ('github')),
  installation_id BIGINT NOT NULL CHECK (installation_id > 0),
  org_id          TEXT NOT NULL REFERENCES organizations (id),
  account         TEXT NOT NULL,
  suspended       BOOLEAN NOT NULL DEFAULT FALSE,
  created_at      BIGINT NOT NULL,
  updated_at      BIGINT NOT NULL,
  PRIMARY KEY (provider, installation_id),
  UNIQUE (org_id, provider, installation_id)
);

CREATE TABLE source_bindings (
  id               UUID PRIMARY KEY,
  org_id           TEXT NOT NULL,
  project_id       UUID NOT NULL,
  application_id   UUID NOT NULL,
  target_id        UUID NOT NULL UNIQUE,
  provider         TEXT NOT NULL,
  installation_id  BIGINT NOT NULL,
  -- `owner/name`, lowercase; the provider's numeric id is pinned on the first
  -- verified read so a renamed or recreated repository is not confused.
  repository       TEXT NOT NULL CHECK (repository ~ '^[a-z0-9._-]+/[a-z0-9._-]+$'),
  repository_id    BIGINT,
  branch           TEXT NOT NULL CHECK (length(branch) BETWEEN 1 AND 255),
  recipe           JSONB NOT NULL CHECK (jsonb_typeof(recipe) = 'object'),
  -- Push destination without tag or digest; the digest comes from the build.
  image_repository TEXT NOT NULL CHECK (image_repository !~ '[@\s]'),
  head_sha         TEXT CHECK (head_sha ~ '^[0-9a-f]{40}$'),
  head_epoch       BIGINT NOT NULL DEFAULT 0 CHECK (head_epoch >= 0),
  created_at       BIGINT NOT NULL,
  updated_at       BIGINT NOT NULL,
  UNIQUE (org_id, project_id, id),
  UNIQUE (org_id, project_id, application_id, target_id, id),
  FOREIGN KEY (org_id, project_id, application_id, target_id)
    REFERENCES application_targets (org_id, project_id, application_id, id),
  FOREIGN KEY (org_id, provider, installation_id)
    REFERENCES git_installations (org_id, provider, installation_id)
);
CREATE INDEX source_bindings_push ON source_bindings (provider, installation_id, repository, branch);

CREATE TABLE build_attempts (
  id                    UUID PRIMARY KEY,
  org_id                TEXT NOT NULL,
  project_id            UUID NOT NULL,
  application_id        UUID NOT NULL,
  target_id             UUID NOT NULL,
  binding_id            UUID NOT NULL,
  -- An infrastructure retry is a new attempt of the same inputs.
  attempt_no            INTEGER NOT NULL DEFAULT 1 CHECK (attempt_no > 0),
  commit_sha            TEXT NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
  source_epoch          BIGINT NOT NULL CHECK (source_epoch > 0),
  build_config_revision BIGINT NOT NULL CHECK (build_config_revision >= 0),
  lifecycle_uid         UUID NOT NULL,
  repository            TEXT NOT NULL,
  recipe                JSONB NOT NULL,
  image_repository      TEXT NOT NULL,
  phase                 TEXT NOT NULL DEFAULT 'queued' CHECK (phase IN (
                          'queued', 'blocked', 'preparing', 'running', 'publishing',
                          'verifyingOutput', 'cancelRequested', 'cancelling',
                          'succeeded', 'failed', 'cancelled')),
  blocked_reason        TEXT,
  -- kuben_core::ops::BuildFailure code and a bounded human detail.
  failure               TEXT,
  failure_detail        TEXT CHECK (length(failure_detail) <= 2048),
  -- What the build pod reported (a hint), and the digest verified against
  -- the registry, set once.
  reported_digest       TEXT CHECK (reported_digest ~ '^sha256:[0-9a-f]{64}$'),
  digest                TEXT CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  job_name              TEXT,
  release_id            UUID,
  deployment_run_id     UUID REFERENCES deployment_runs (id),
  -- `deployed`, or why the verified build did not deploy (a Reject).
  deploy_decision       TEXT,
  operation_id          UUID NOT NULL UNIQUE REFERENCES operations (id),
  cancel_requested_at   BIGINT,
  created_at            BIGINT NOT NULL,
  started_at            BIGINT,
  finished_at           BIGINT,
  updated_at            BIGINT NOT NULL,
  UNIQUE (binding_id, source_epoch, build_config_revision, attempt_no),
  CHECK ((phase = 'succeeded') = (digest IS NOT NULL)),
  CHECK (phase NOT IN ('succeeded', 'failed', 'cancelled') OR finished_at IS NOT NULL),
  FOREIGN KEY (org_id, project_id, application_id, target_id, binding_id)
    REFERENCES source_bindings (org_id, project_id, application_id, target_id, id),
  FOREIGN KEY (org_id, project_id, application_id, release_id)
    REFERENCES releases (org_id, project_id, application_id, id)
);
CREATE INDEX build_attempts_binding ON build_attempts (binding_id, created_at DESC);
CREATE INDEX build_attempts_active ON build_attempts (org_id, phase)
  WHERE phase NOT IN ('succeeded', 'failed', 'cancelled');

-- Inputs never change, a terminal phase is absorbing and the digest, release
-- and run are written at most once.
CREATE FUNCTION kuben_build_attempt_rules() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.org_id IS DISTINCT FROM OLD.org_id
     OR NEW.project_id IS DISTINCT FROM OLD.project_id
     OR NEW.application_id IS DISTINCT FROM OLD.application_id
     OR NEW.target_id IS DISTINCT FROM OLD.target_id
     OR NEW.binding_id IS DISTINCT FROM OLD.binding_id
     OR NEW.attempt_no IS DISTINCT FROM OLD.attempt_no
     OR NEW.commit_sha IS DISTINCT FROM OLD.commit_sha
     OR NEW.source_epoch IS DISTINCT FROM OLD.source_epoch
     OR NEW.build_config_revision IS DISTINCT FROM OLD.build_config_revision
     OR NEW.lifecycle_uid IS DISTINCT FROM OLD.lifecycle_uid
     OR NEW.repository IS DISTINCT FROM OLD.repository
     OR NEW.recipe IS DISTINCT FROM OLD.recipe
     OR NEW.image_repository IS DISTINCT FROM OLD.image_repository
     OR NEW.operation_id IS DISTINCT FROM OLD.operation_id
     OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'the inputs of build attempt % are immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF OLD.phase IN ('succeeded', 'failed', 'cancelled') AND NEW.phase IS DISTINCT FROM OLD.phase THEN
    RAISE EXCEPTION 'build attempt % is %, which is final', OLD.id, OLD.phase
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF (OLD.digest IS NOT NULL AND NEW.digest IS DISTINCT FROM OLD.digest)
     OR (OLD.reported_digest IS NOT NULL AND NEW.reported_digest IS DISTINCT FROM OLD.reported_digest)
     OR (OLD.release_id IS NOT NULL AND NEW.release_id IS DISTINCT FROM OLD.release_id)
     OR (OLD.deployment_run_id IS NOT NULL AND NEW.deployment_run_id IS DISTINCT FROM OLD.deployment_run_id)
     OR (OLD.deploy_decision IS NOT NULL AND NEW.deploy_decision IS DISTINCT FROM OLD.deploy_decision) THEN
    RAISE EXCEPTION 'the output of build attempt % is written once', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER build_attempt_rules BEFORE UPDATE ON build_attempts
  FOR EACH ROW EXECUTE FUNCTION kuben_build_attempt_rules();

-- Admission (ADR-028): counted under an advisory lock by the claiming
-- transaction, deleted in the transaction that settles the attempt. Workers
-- count every organization's slots, so the table has no row-level security.
CREATE TABLE build_slots (
  attempt_id UUID PRIMARY KEY REFERENCES build_attempts (id) ON DELETE CASCADE,
  org_id     TEXT NOT NULL REFERENCES organizations (id),
  claimed_at BIGINT NOT NULL
);
CREATE INDEX build_slots_org ON build_slots (org_id);

ALTER TABLE source_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE source_bindings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON source_bindings
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

ALTER TABLE build_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE build_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON build_attempts
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

-- A release made by a build names it; deployment runs keep their reasons.
ALTER TABLE deployment_runs DROP CONSTRAINT deployment_runs_reason_check;
ALTER TABLE deployment_runs ADD CONSTRAINT deployment_runs_reason_check
  CHECK (reason IN ('deploy', 'rollback', 'promotion', 'restart', 'handover', 'build'));
