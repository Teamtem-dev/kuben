-- M5.4: image update policies.
--
-- An app deployed from an image may follow a tag pattern of its repository:
-- a watcher lists the repository's tags, resolves the chosen one to a
-- digest and, when it changed, deploys a new release. The environment's
-- policy still applies: a protected environment waits for approval.

CREATE TABLE image_policies (
  target_id      UUID PRIMARY KEY,
  org_id         TEXT NOT NULL REFERENCES organizations (id),
  project_id     UUID NOT NULL,
  -- The repository without tag or digest, e.g. `ghcr.io/acme/web`.
  repository     TEXT NOT NULL CHECK (char_length(repository) BETWEEN 1 AND 512),
  -- `semver:<range>`, `tag:<glob>` or a tag.
  pattern        TEXT NOT NULL CHECK (char_length(pattern) BETWEEN 1 AND 256),
  enabled        BOOLEAN NOT NULL DEFAULT TRUE,
  interval_secs  INTEGER NOT NULL DEFAULT 300 CHECK (interval_secs BETWEEN 60 AND 86400),
  next_check_at  BIGINT NOT NULL,
  failures       INTEGER NOT NULL DEFAULT 0 CHECK (failures >= 0),
  last_checked_at BIGINT,
  last_tag       TEXT,
  last_digest    TEXT CHECK (last_digest ~ '^sha256:[0-9a-f]{64}$'),
  last_error     TEXT CHECK (char_length(last_error) <= 1024),
  -- The run the watcher started last.
  last_run_id    UUID,
  updated_by     TEXT NOT NULL,
  updated_at     BIGINT NOT NULL,
  FOREIGN KEY (org_id, project_id, target_id) REFERENCES application_targets (org_id, project_id, id)
);
CREATE INDEX image_policies_due ON image_policies (org_id, next_check_at) WHERE enabled;

ALTER TABLE image_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE image_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON image_policies
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());
