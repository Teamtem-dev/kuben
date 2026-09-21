-- M4.2: trust for external CI (kuben_core::ci; plan §13, S04).
--
-- A trust policy lets one GitHub repository, named by its immutable numeric
-- ids, exchange its Actions OIDC tokens for short-lived API tokens capped at
-- the policy's role and scoped to its project (and environment). The
-- exchange names the policy, so policies are looked up by id before the
-- organization is known: like git_installations, the table has no row-level
-- security and request paths filter by organization. Every provider token
-- is accepted once (its jti) until it expires. Revoking a policy revokes the
-- tokens issued under it.

CREATE TABLE ci_trust_policies (
  id                  UUID PRIMARY KEY,
  org_id              TEXT NOT NULL REFERENCES organizations (id),
  project_id          UUID NOT NULL,
  environment_id      UUID,
  name                TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
  provider            TEXT NOT NULL CHECK (provider IN ('github-actions')),
  repository_id       BIGINT NOT NULL CHECK (repository_id > 0),
  repository_owner_id BIGINT NOT NULL CHECK (repository_owner_id > 0),
  -- `owner/name` when the policy was made; informational only.
  repository          TEXT NOT NULL,
  refs                TEXT[] NOT NULL CHECK (cardinality(refs) BETWEEN 1 AND 20),
  environments        TEXT[] NOT NULL DEFAULT '{}' CHECK (cardinality(environments) <= 20),
  events              TEXT[] NOT NULL CHECK (cardinality(events) BETWEEN 1 AND 20),
  role                TEXT NOT NULL CHECK (role IN ('developer', 'admin')),
  token_ttl_secs      INTEGER NOT NULL CHECK (token_ttl_secs BETWEEN 60 AND 3600),
  -- The user whose authority exchanged tokens act under (capped at `role`).
  created_by          TEXT NOT NULL REFERENCES users (id),
  created_at          BIGINT NOT NULL,
  revoked_at          BIGINT,
  UNIQUE (org_id, name),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id),
  FOREIGN KEY (org_id, project_id, environment_id) REFERENCES environments (org_id, project_id, id)
);
CREATE INDEX ci_trust_policies_org ON ci_trust_policies (org_id, created_at);

-- A policy's scope and identity never change; only its revocation does.
CREATE FUNCTION kuben_ci_policy_rules() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'CI trust policy % is revoked, never deleted', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF ROW(NEW.id, NEW.org_id, NEW.project_id, NEW.environment_id, NEW.name, NEW.provider,
         NEW.repository_id, NEW.repository_owner_id, NEW.repository, NEW.refs, NEW.environments,
         NEW.events, NEW.role, NEW.token_ttl_secs, NEW.created_by, NEW.created_at)
     IS DISTINCT FROM
     ROW(OLD.id, OLD.org_id, OLD.project_id, OLD.environment_id, OLD.name, OLD.provider,
         OLD.repository_id, OLD.repository_owner_id, OLD.repository, OLD.refs, OLD.environments,
         OLD.events, OLD.role, OLD.token_ttl_secs, OLD.created_by, OLD.created_at)
     OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
    RAISE EXCEPTION 'CI trust policy % can only be revoked', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER ci_policy_rules BEFORE UPDATE OR DELETE ON ci_trust_policies
  FOR EACH ROW EXECUTE FUNCTION kuben_ci_policy_rules();

CREATE TABLE ci_token_uses (
  issuer     TEXT NOT NULL,
  jti        TEXT NOT NULL CHECK (char_length(jti) BETWEEN 1 AND 256),
  policy_id  UUID NOT NULL REFERENCES ci_trust_policies (id),
  -- Kept until the provider token could no longer be presented.
  expires_at BIGINT NOT NULL,
  used_at    BIGINT NOT NULL,
  PRIMARY KEY (issuer, jti)
);
CREATE INDEX ci_token_uses_expiry ON ci_token_uses (expires_at);

ALTER TABLE api_tokens ADD COLUMN ci_policy_id UUID REFERENCES ci_trust_policies (id);
CREATE INDEX api_tokens_ci_policy ON api_tokens (ci_policy_id) WHERE ci_policy_id IS NOT NULL;
