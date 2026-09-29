-- 2.1: integrations — Git connections, organization registries and build
-- logs that outlive the build pod.
--
-- A Git connection is an organization's token for one Git provider account
-- (GitHub by personal or fine-grained token, GitLab, Gitea/Forgejo; the
-- GitHub App keeps its installations). Its token and the secret its push
-- webhooks are verified with are sealed by the caller with the keyring, as
-- secret revisions are: the database never holds them in clear. A push
-- webhook names its connection before the organization is known, so, like
-- git_installations, the table has no row-level security and request paths
-- filter by organization.
--
-- A source binding reads its repository through either a GitHub App
-- installation or a connection, never both.
--
-- An organization registry is a login for one registry host that every
-- environment of the organization pulls with, unless the environment has its
-- own login for that host (migration 0023), which wins.
--
-- A build attempt keeps the tail of its build pod's log and the timings of
-- its stages when it settles, so both survive the pod.

CREATE TABLE git_connections (
  id                        UUID PRIMARY KEY,
  org_id                    TEXT NOT NULL REFERENCES organizations (id),
  provider                  TEXT NOT NULL CHECK (provider IN ('github', 'gitlab', 'gitea')),
  name                      TEXT NOT NULL CHECK (name ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
  -- The provider's API root: `https://api.github.com`, `https://gitlab.com`,
  -- a self-hosted URL. No trailing slash, no credentials.
  base_url                  TEXT NOT NULL CHECK (base_url ~ '^https?://[^/@\s]+(/[^@\s]*[^/@\s])?$'
                                                 AND char_length(base_url) <= 2048),
  auth_kind                 TEXT NOT NULL CHECK (auth_kind IN ('token')),
  token_ciphertext          BYTEA NOT NULL,
  token_wrapped_key         BYTEA NOT NULL,
  token_key_version         INTEGER NOT NULL CHECK (token_key_version > 0),
  -- The last four characters of the token, to recognize it; never more.
  token_hint                TEXT NOT NULL CHECK (char_length(token_hint) <= 4),
  -- The account the token belongs to, as the provider named it.
  username                  TEXT CHECK (char_length(username) BETWEEN 1 AND 255),
  webhook_secret_ciphertext BYTEA NOT NULL,
  webhook_secret_wrapped_key BYTEA NOT NULL,
  webhook_secret_key_version INTEGER NOT NULL CHECK (webhook_secret_key_version > 0),
  default_branch            TEXT CHECK (char_length(default_branch) BETWEEN 1 AND 255),
  created_by                TEXT NOT NULL,
  created_at                BIGINT NOT NULL,
  updated_at                BIGINT NOT NULL,
  last_checked_at           BIGINT,
  last_error                TEXT CHECK (char_length(last_error) <= 1024),
  UNIQUE (org_id, name),
  UNIQUE (org_id, provider, id)
);
CREATE INDEX git_connections_org ON git_connections (org_id, created_at);

-- What a connection is never changes; its token, webhook secret and checks
-- do.
CREATE FUNCTION kuben_git_connection_rules() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF ROW(NEW.id, NEW.org_id, NEW.provider, NEW.auth_kind, NEW.created_by, NEW.created_at)
     IS DISTINCT FROM
     ROW(OLD.id, OLD.org_id, OLD.provider, OLD.auth_kind, OLD.created_by, OLD.created_at) THEN
    RAISE EXCEPTION 'the identity of git connection % is immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER git_connection_rules BEFORE UPDATE ON git_connections
  FOR EACH ROW EXECUTE FUNCTION kuben_git_connection_rules();

ALTER TABLE source_bindings
  ADD COLUMN connection_id UUID,
  ALTER COLUMN installation_id DROP NOT NULL,
  ADD CONSTRAINT source_bindings_provider_check CHECK (provider IN ('github', 'gitlab', 'gitea')),
  -- Exactly one way to read the repository; only GitHub has installations.
  ADD CONSTRAINT source_bindings_one_reader
    CHECK ((installation_id IS NULL) <> (connection_id IS NULL)),
  ADD CONSTRAINT source_bindings_installation_provider
    CHECK (installation_id IS NULL OR provider = 'github'),
  -- The connection's provider is the binding's; a connection in use is not
  -- deleted.
  ADD CONSTRAINT source_bindings_connection_fkey FOREIGN KEY (org_id, provider, connection_id)
    REFERENCES git_connections (org_id, provider, id) ON DELETE RESTRICT;
-- GitLab nests groups: a connection's repository may have more than two
-- segments. An installation's stays `owner/name`.
ALTER TABLE source_bindings DROP CONSTRAINT source_bindings_repository_check;
ALTER TABLE source_bindings ADD CONSTRAINT source_bindings_repository_check
  CHECK (repository ~ '^[a-z0-9._-]+/[a-z0-9._-]+$'
         OR (connection_id IS NOT NULL AND repository ~ '^[a-z0-9._-]+(/[a-z0-9._-]+){1,20}$'));
CREATE INDEX source_bindings_connection_push ON source_bindings (connection_id, repository, branch)
  WHERE connection_id IS NOT NULL;

CREATE TABLE org_registries (
  id                  UUID PRIMARY KEY,
  org_id              TEXT NOT NULL REFERENCES organizations (id),
  name                TEXT NOT NULL CHECK (name ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
  preset              TEXT NOT NULL CHECK (preset IN ('dockerhub', 'ghcr', 'gitlab', 'quay', 'harbor', 'custom')),
  -- The registry as image references name it, as in migration 0023.
  server              TEXT NOT NULL CHECK (server ~ '^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?(:[0-9]{1,5})?$'),
  username            TEXT NOT NULL CHECK (char_length(username) BETWEEN 1 AND 255),
  password_ciphertext BYTEA NOT NULL,
  password_wrapped_key BYTEA NOT NULL,
  password_key_version INTEGER NOT NULL CHECK (password_key_version > 0),
  created_by          TEXT NOT NULL,
  created_at          BIGINT NOT NULL,
  updated_at          BIGINT NOT NULL,
  last_checked_at     BIGINT,
  last_error          TEXT CHECK (char_length(last_error) <= 1024),
  UNIQUE (org_id, name),
  -- One login per host: pulls must know which one to use.
  UNIQUE (org_id, server)
);

CREATE FUNCTION kuben_org_registry_rules() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF ROW(NEW.id, NEW.org_id, NEW.created_by, NEW.created_at)
     IS DISTINCT FROM
     ROW(OLD.id, OLD.org_id, OLD.created_by, OLD.created_at) THEN
    RAISE EXCEPTION 'the identity of registry % is immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER org_registry_rules BEFORE UPDATE ON org_registries
  FOR EACH ROW EXECUTE FUNCTION kuben_org_registry_rules();

ALTER TABLE org_registries ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_registries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON org_registries
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

ALTER TABLE build_attempts
  -- The end of the build pod's log (at most 1 MiB; the server keeps less).
  ADD COLUMN log_tail TEXT CHECK (octet_length(log_tail) <= 1048576),
  -- [{name, status, startedAt, finishedAt, detail}], in order.
  ADD COLUMN stages   JSONB CHECK (jsonb_typeof(stages) = 'array');
-- The event stream reads the organization's builds that changed since the
-- newest update it saw (store/build_feed.go).
CREATE INDEX build_attempts_changed ON build_attempts (org_id, updated_at);
