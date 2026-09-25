-- M4.4: credentials for private registries (ADR-030).
--
-- A registry credential is a managed secret of kind `registry` for one
-- registry of an environment; its sealed values are a username and a
-- password. A deployment run binds the credential of its release's registry
-- next to the secrets its configuration references, and its pods pull with
-- the revision it was accepted with. An environment has at most one live
-- credential per registry.

ALTER TABLE secrets
  ADD COLUMN kind     TEXT NOT NULL DEFAULT 'opaque' CHECK (kind IN ('opaque', 'registry')),
  -- The registry as image references name it: `ghcr.io`, `docker.io`,
  -- `registry.example.com:5000`.
  ADD COLUMN registry TEXT CHECK (registry ~ '^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?(:[0-9]{1,5})?$');
ALTER TABLE secrets ADD CONSTRAINT secrets_registry_of_kind
  CHECK ((kind = 'registry') = (registry IS NOT NULL));
CREATE UNIQUE INDEX secrets_live_registry ON secrets (environment_id, registry)
  WHERE deleted_at IS NULL AND kind = 'registry';

-- What a secret is never changes either.
CREATE OR REPLACE FUNCTION kuben_secret_rules() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'secret % is deleted by marking it, never removed', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF ROW(NEW.id, NEW.org_id, NEW.project_id, NEW.environment_id, NEW.name, NEW.kind, NEW.registry,
         NEW.created_by, NEW.created_at)
     IS DISTINCT FROM
     ROW(OLD.id, OLD.org_id, OLD.project_id, OLD.environment_id, OLD.name, OLD.kind, OLD.registry,
         OLD.created_by, OLD.created_at)
     OR NEW.current_revision < OLD.current_revision
     OR (OLD.deleted_at IS NOT NULL AND ROW(NEW.deleted_at, NEW.current_revision)
                                       IS DISTINCT FROM ROW(OLD.deleted_at, OLD.current_revision)) THEN
    RAISE EXCEPTION 'secret % can only gain revisions or be deleted', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
