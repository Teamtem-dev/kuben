-- M4.4: managed secrets with encrypted, immutable revisions (ADR-030).
--
-- A secret belongs to one environment. Every change of its values is a new
-- revision, sealed by the application (AES-256-GCM under a per-revision data
-- key, itself sealed under a versioned key-encryption key that never enters
-- the database). A revision's identity, keys and ciphertext never change; its
-- data key may be sealed again under a newer key-encryption key, and it may
-- be revoked once, never reinstated. A deployment run binds the revision of
-- every secret its configuration references when it is accepted, so a
-- retried or rolled-back run renders the values it was accepted with.
-- Promotion copies no secret: bindings name secrets of the run's environment.

-- The fingerprint of every key-encryption key the installation has used.
-- A replica whose keyring holds another key under a known version refuses to
-- start instead of sealing what the others cannot open.
CREATE TABLE secret_key_fingerprints (
  version     INTEGER PRIMARY KEY CHECK (version > 0),
  fingerprint BYTEA NOT NULL CHECK (octet_length(fingerprint) = 32),
  first_seen  BIGINT NOT NULL
);
CREATE TRIGGER secret_key_fingerprints_append_only BEFORE UPDATE OR DELETE ON secret_key_fingerprints
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();

CREATE TABLE secrets (
  id               UUID PRIMARY KEY,
  org_id           TEXT NOT NULL REFERENCES organizations (id),
  project_id       UUID NOT NULL,
  environment_id   UUID NOT NULL,
  -- A DNS label; the Kubernetes object of a revision is `<name>.r<revision>`.
  name             TEXT NOT NULL CHECK (name ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
  current_revision BIGINT NOT NULL DEFAULT 0 CHECK (current_revision >= 0),
  created_by       TEXT NOT NULL,
  created_at       BIGINT NOT NULL,
  updated_at       BIGINT NOT NULL,
  deleted_at       BIGINT,
  FOREIGN KEY (org_id, project_id, environment_id) REFERENCES environments (org_id, project_id, id)
);
CREATE UNIQUE INDEX secrets_live_name ON secrets (environment_id, name) WHERE deleted_at IS NULL;
CREATE INDEX secrets_org ON secrets (org_id, environment_id);

-- Revisions only move forward, and a deleted secret stays deleted.
CREATE FUNCTION kuben_secret_rules() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'secret % is deleted by marking it, never removed', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF ROW(NEW.id, NEW.org_id, NEW.project_id, NEW.environment_id, NEW.name, NEW.created_by, NEW.created_at)
     IS DISTINCT FROM
     ROW(OLD.id, OLD.org_id, OLD.project_id, OLD.environment_id, OLD.name, OLD.created_by, OLD.created_at)
     OR NEW.current_revision < OLD.current_revision
     OR (OLD.deleted_at IS NOT NULL AND ROW(NEW.deleted_at, NEW.current_revision)
                                       IS DISTINCT FROM ROW(OLD.deleted_at, OLD.current_revision)) THEN
    RAISE EXCEPTION 'secret % can only gain revisions or be deleted', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER secret_rules BEFORE UPDATE OR DELETE ON secrets
  FOR EACH ROW EXECUTE FUNCTION kuben_secret_rules();

ALTER TABLE secrets ENABLE ROW LEVEL SECURITY;
ALTER TABLE secrets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON secrets
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

CREATE TABLE secret_revisions (
  secret_id   UUID NOT NULL REFERENCES secrets (id),
  org_id      TEXT NOT NULL REFERENCES organizations (id),
  revision    BIGINT NOT NULL CHECK (revision > 0),
  -- Key names only, for listings; the values are in `ciphertext`.
  keys        TEXT[] NOT NULL CHECK (cardinality(keys) BETWEEN 1 AND 256),
  -- Nonce, then the sealed JSON object of the values and its tag.
  ciphertext  BYTEA NOT NULL CHECK (octet_length(ciphertext) BETWEEN 29 AND 400000),
  -- Nonce, then the sealed data key and its tag.
  wrapped_key BYTEA NOT NULL CHECK (octet_length(wrapped_key) = 60),
  key_version INTEGER NOT NULL CHECK (key_version > 0),
  created_by  TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  revoked_at  BIGINT,
  revoked_by  TEXT,
  PRIMARY KEY (secret_id, revision),
  CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);
CREATE INDEX secret_revisions_key_version ON secret_revisions (org_id, key_version);

CREATE FUNCTION kuben_secret_revision_rules() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'revision % of secret % is revoked, never deleted', OLD.revision, OLD.secret_id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF ROW(NEW.secret_id, NEW.org_id, NEW.revision, NEW.keys, NEW.ciphertext, NEW.created_by, NEW.created_at)
     IS DISTINCT FROM
     ROW(OLD.secret_id, OLD.org_id, OLD.revision, OLD.keys, OLD.ciphertext, OLD.created_by, OLD.created_at)
     OR (OLD.revoked_at IS NOT NULL
         AND ROW(NEW.revoked_at, NEW.revoked_by) IS DISTINCT FROM ROW(OLD.revoked_at, OLD.revoked_by))
     OR NEW.key_version < OLD.key_version THEN
    RAISE EXCEPTION 'revision % of secret % can only be revoked or sealed under a newer key',
      OLD.revision, OLD.secret_id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER secret_revision_rules BEFORE UPDATE OR DELETE ON secret_revisions
  FOR EACH ROW EXECUTE FUNCTION kuben_secret_revision_rules();

ALTER TABLE secret_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON secret_revisions
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

CREATE TABLE run_secret_bindings (
  run_id    UUID NOT NULL REFERENCES deployment_runs (id),
  org_id    TEXT NOT NULL REFERENCES organizations (id),
  secret_id UUID NOT NULL,
  revision  BIGINT NOT NULL,
  -- The name the configuration referenced.
  name      TEXT NOT NULL,
  PRIMARY KEY (run_id, secret_id),
  UNIQUE (run_id, name),
  FOREIGN KEY (secret_id, revision) REFERENCES secret_revisions (secret_id, revision)
);
CREATE INDEX run_secret_bindings_revision ON run_secret_bindings (secret_id, revision);
CREATE TRIGGER run_secret_bindings_append_only BEFORE UPDATE OR DELETE ON run_secret_bindings
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();

ALTER TABLE run_secret_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_secret_bindings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON run_secret_bindings
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

-- A rotation rolls the new revision out with the release and configuration
-- the target runs.
ALTER TABLE deployment_runs DROP CONSTRAINT deployment_runs_reason_check;
ALTER TABLE deployment_runs ADD CONSTRAINT deployment_runs_reason_check
  CHECK (reason IN ('deploy', 'rollback', 'promotion', 'restart', 'handover', 'build', 'rotation'));
