-- Product schema (ADR-025, ADR-026):
--   organization → project → environment → placement (cluster + namespace)
--                          → application → target (application + placement)
--
-- Every child row carries its tenant and references its parent through
-- (org_id, project_id, id), so no row can point into another organization or
-- project (I21, I23). Row-level security is the second line of defence:
-- queries see only the organization named by the transaction-local setting
-- `kuben.org_id`, and no rows at all without it. Superusers and roles with
-- BYPASSRLS skip these policies, so the server must not connect as one.
--
-- Organization ids are still TEXT like the rest of the identity schema; the
-- product tables use native UUIDs for their own ids.

CREATE TABLE projects (
  id         UUID PRIMARY KEY,
  org_id     TEXT NOT NULL REFERENCES organizations (id),
  slug       TEXT NOT NULL,
  name       TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  UNIQUE (org_id, slug),
  UNIQUE (org_id, id)
);

-- A logical policy and configuration boundary (staging, production).
CREATE TABLE environments (
  id         UUID PRIMARY KEY,
  org_id     TEXT NOT NULL,
  project_id UUID NOT NULL,
  slug       TEXT NOT NULL,
  name       TEXT NOT NULL,
  -- Protection (approvals, pinned rollbacks) is policy, never inferred from a name.
  protected  BOOLEAN NOT NULL DEFAULT FALSE,
  created_at BIGINT NOT NULL,
  UNIQUE (project_id, slug),
  UNIQUE (org_id, project_id, id),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id)
);

CREATE TABLE clusters (
  id         UUID PRIMARY KEY,
  org_id     TEXT NOT NULL REFERENCES organizations (id),
  name       TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  UNIQUE (org_id, name),
  UNIQUE (org_id, id)
);

-- Binds an environment to one cluster and namespace. The binding never
-- changes: moving is a new placement and a cutover, not a rename.
CREATE TABLE environment_placements (
  id             UUID PRIMARY KEY,
  org_id         TEXT NOT NULL,
  project_id     UUID NOT NULL,
  environment_id UUID NOT NULL,
  cluster_id     UUID NOT NULL,
  namespace      TEXT NOT NULL,
  -- Recorded once the namespace exists; a recreated namespace is a new binding.
  namespace_uid  TEXT,
  failure_domain TEXT,
  state          TEXT NOT NULL DEFAULT 'provisioning'
                 CHECK (state IN ('provisioning', 'ready', 'draining', 'retired')),
  created_at     BIGINT NOT NULL,
  UNIQUE (org_id, project_id, id),
  FOREIGN KEY (org_id, project_id, environment_id) REFERENCES environments (org_id, project_id, id),
  FOREIGN KEY (org_id, cluster_id) REFERENCES clusters (org_id, id)
);
CREATE UNIQUE INDEX placements_namespace ON environment_placements (cluster_id, namespace)
  WHERE state <> 'retired';

CREATE TABLE applications (
  id         UUID PRIMARY KEY,
  org_id     TEXT NOT NULL,
  project_id UUID NOT NULL,
  slug       TEXT NOT NULL,
  name       TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  UNIQUE (project_id, slug),
  UNIQUE (org_id, project_id, id),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id)
);

-- One application on one placement, with the control state of
-- kuben_core::ops::target::TargetState.
CREATE TABLE application_targets (
  id                    UUID PRIMARY KEY,
  org_id                TEXT NOT NULL,
  project_id            UUID NOT NULL,
  application_id        UUID NOT NULL,
  placement_id          UUID NOT NULL,
  -- A recreated target gets a new lifecycle UID; work for the old one is refused.
  lifecycle_uid         UUID NOT NULL,
  desired_generation    BIGINT NOT NULL DEFAULT 0 CHECK (desired_generation >= 0),
  source_epoch          BIGINT NOT NULL DEFAULT 0 CHECK (source_epoch >= 0),
  build_config_revision BIGINT NOT NULL DEFAULT 0 CHECK (build_config_revision >= 0),
  deploy_policy         TEXT NOT NULL DEFAULT 'auto'
                        CHECK (deploy_policy IN ('auto', 'manual', 'pinned')),
  deleting              BOOLEAN NOT NULL DEFAULT FALSE,
  created_at            BIGINT NOT NULL,
  UNIQUE (application_id, placement_id),
  UNIQUE (org_id, project_id, id),
  FOREIGN KEY (org_id, project_id, application_id) REFERENCES applications (org_id, project_id, id),
  FOREIGN KEY (org_id, project_id, placement_id) REFERENCES environment_placements (org_id, project_id, id)
);

CREATE FUNCTION kuben_placement_binding_is_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.org_id IS DISTINCT FROM OLD.org_id
     OR NEW.project_id IS DISTINCT FROM OLD.project_id
     OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
     OR NEW.cluster_id IS DISTINCT FROM OLD.cluster_id
     OR NEW.namespace IS DISTINCT FROM OLD.namespace
     OR (OLD.namespace_uid IS NOT NULL AND NEW.namespace_uid IS DISTINCT FROM OLD.namespace_uid) THEN
    RAISE EXCEPTION 'placement % is bound to its cluster and namespace; create a new placement instead', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER placement_binding_is_immutable BEFORE UPDATE ON environment_placements
  FOR EACH ROW EXECUTE FUNCTION kuben_placement_binding_is_immutable();

-- I05: identity never changes, counters never go back, a deleting target
-- stays deleting.
CREATE FUNCTION kuben_target_only_moves_forward() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.org_id IS DISTINCT FROM OLD.org_id
     OR NEW.project_id IS DISTINCT FROM OLD.project_id
     OR NEW.application_id IS DISTINCT FROM OLD.application_id
     OR NEW.placement_id IS DISTINCT FROM OLD.placement_id
     OR NEW.lifecycle_uid IS DISTINCT FROM OLD.lifecycle_uid THEN
    RAISE EXCEPTION 'the identity of target % is immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.desired_generation < OLD.desired_generation
     OR NEW.source_epoch < OLD.source_epoch
     OR NEW.build_config_revision < OLD.build_config_revision
     OR (OLD.deleting AND NOT NEW.deleting) THEN
    RAISE EXCEPTION 'target % only moves forward', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER target_only_moves_forward BEFORE UPDATE ON application_targets
  FOR EACH ROW EXECUTE FUNCTION kuben_target_only_moves_forward();

-- The organization of the current transaction, or NULL. A transaction-local
-- setting reads back as '' once its transaction ended, hence NULLIF.
CREATE FUNCTION kuben_current_org() RETURNS TEXT
LANGUAGE sql STABLE AS $$ SELECT NULLIF(current_setting('kuben.org_id', true), '') $$;

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON projects
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

ALTER TABLE environments ENABLE ROW LEVEL SECURITY;
ALTER TABLE environments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON environments
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

ALTER TABLE clusters ENABLE ROW LEVEL SECURITY;
ALTER TABLE clusters FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON clusters
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

ALTER TABLE environment_placements ENABLE ROW LEVEL SECURITY;
ALTER TABLE environment_placements FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON environment_placements
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

ALTER TABLE applications ENABLE ROW LEVEL SECURITY;
ALTER TABLE applications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON applications
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

ALTER TABLE application_targets ENABLE ROW LEVEL SECURITY;
ALTER TABLE application_targets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON application_targets
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());
