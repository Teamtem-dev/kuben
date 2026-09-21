-- M5.1: preview environments (plan §9, C14; failure matrix "PR reopen /
-- preview delete race").
--
-- A preview is an environment of type `preview` for one pull request. Its
-- identity is the provider, the repository, the pull request number and an
-- epoch: a reopened pull request gets a new epoch, so a new environment and
-- namespace, and nothing of the old one can come back. The preview's apps
-- are copies of the apps of the project's source environment that build
-- from the same repository, bound to the pull request's head.
--
-- A preview of a fork is untrusted: it never binds a secret or a registry
-- login, and only projects that allow forks get one.

CREATE TABLE preview_policies (
  project_id            UUID PRIMARY KEY,
  org_id                TEXT NOT NULL REFERENCES organizations (id),
  enabled               BOOLEAN NOT NULL DEFAULT FALSE,
  -- The environment whose Git-built apps a preview copies.
  source_environment_id UUID NOT NULL,
  ttl_hours             INTEGER NOT NULL DEFAULT 72 CHECK (ttl_hours BETWEEN 1 AND 720),
  max_active            INTEGER NOT NULL DEFAULT 10 CHECK (max_active BETWEEN 1 AND 100),
  allow_forks           BOOLEAN NOT NULL DEFAULT FALSE,
  updated_by            TEXT NOT NULL,
  updated_at            BIGINT NOT NULL,
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id),
  FOREIGN KEY (org_id, project_id, source_environment_id) REFERENCES environments (org_id, project_id, id)
);

ALTER TABLE preview_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE preview_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON preview_policies
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

CREATE TABLE previews (
  environment_id    UUID PRIMARY KEY,
  org_id            TEXT NOT NULL REFERENCES organizations (id),
  project_id        UUID NOT NULL,
  provider          TEXT NOT NULL CHECK (provider IN ('github')),
  installation_id   BIGINT NOT NULL,
  repository        TEXT NOT NULL,
  pr_number         BIGINT NOT NULL CHECK (pr_number > 0),
  preview_epoch     BIGINT NOT NULL CHECK (preview_epoch > 0),
  -- The pull request's head as last reported.
  head_repository   TEXT NOT NULL,
  branch            TEXT NOT NULL,
  commit_sha        TEXT NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
  -- A fork's preview never gets secrets.
  trusted           BOOLEAN NOT NULL,
  state             TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'closed')),
  auto_delete       BOOLEAN NOT NULL DEFAULT TRUE,
  expires_at        BIGINT NOT NULL,
  -- The provider's `updated_at` of the newest event applied: older events
  -- are ignored.
  last_event_at     BIGINT NOT NULL,
  -- When the provider last confirmed the pull request is open.
  last_verified_at  BIGINT NOT NULL,
  created_by        TEXT NOT NULL,
  created_at        BIGINT NOT NULL,
  updated_at        BIGINT NOT NULL,
  closed_at         BIGINT,
  close_reason      TEXT CHECK (close_reason IN ('closed', 'expired', 'manual', 'deleted', 'limit')),
  CHECK ((state = 'closed') = (closed_at IS NOT NULL)),
  CHECK ((closed_at IS NULL) = (close_reason IS NULL)),
  UNIQUE (org_id, project_id, provider, repository, pr_number, preview_epoch),
  FOREIGN KEY (org_id, project_id, environment_id) REFERENCES environments (org_id, project_id, id)
);
-- One active preview per pull request and project.
CREATE UNIQUE INDEX previews_active ON previews (org_id, project_id, provider, repository, pr_number)
  WHERE state = 'active';
CREATE INDEX previews_due ON previews (org_id, expires_at) WHERE state = 'active';

ALTER TABLE previews ENABLE ROW LEVEL SECURITY;
ALTER TABLE previews FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON previews
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

-- A binding of a preview's app follows a pull request's head instead of a
-- branch; pushes do not sync it.
ALTER TABLE source_bindings
  ADD COLUMN pull_request BIGINT CHECK (pull_request > 0);
