-- M4.11: apps detached from Kuben. A detach is a deletion that orphans the
-- app's objects instead of removing them: the target row is deleted as
-- usual, and this row keeps what was handed over (the export, frozen at the
-- request) until an operator releases it. While an unreleased detached app
-- lives in an environment, deleting the environment keeps its namespace.

CREATE TABLE detached_apps (
  target_id      UUID PRIMARY KEY,
  org_id         TEXT NOT NULL REFERENCES organizations (id),
  project_id     UUID NOT NULL,
  environment_id UUID NOT NULL,
  app            TEXT NOT NULL,
  namespace      TEXT NOT NULL,
  reason         TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 1024),
  requested_by   TEXT NOT NULL,
  requested_at   BIGINT NOT NULL,
  -- Set once the objects are orphaned and the target is deleted.
  completed_at   BIGINT,
  -- The export (portable release, configuration, manifests, references,
  -- inventory and runbook); never secret values.
  export         JSONB NOT NULL CHECK (jsonb_typeof(export) = 'object'),
  released_at    BIGINT,
  released_by    TEXT,
  CHECK ((released_at IS NULL) = (released_by IS NULL)),
  FOREIGN KEY (org_id, project_id, environment_id) REFERENCES environments (org_id, project_id, id)
);
CREATE INDEX detached_apps_environment ON detached_apps (org_id, environment_id);

ALTER TABLE detached_apps ENABLE ROW LEVEL SECURITY;
ALTER TABLE detached_apps FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON detached_apps
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());
