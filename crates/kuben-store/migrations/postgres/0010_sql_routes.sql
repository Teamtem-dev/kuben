-- The API as the only writer of projects, environments and apps (ADR-032,
-- M1.8b step 2): what it needs beyond 0004.
--
-- * Projects keep their description; environments keep their type and quota,
--   which the materializer renders.
-- * Deletion is soft. A row is marked `deleting`, the materializer removes
--   its resources, and then the row gets `deleted_at`. Deleted rows keep their
--   history (runs, releases and revisions are append-only and reference
--   them) and free their slug for a new row, so slugs are unique among live
--   rows only.

ALTER TABLE projects ADD COLUMN description TEXT;
ALTER TABLE projects ADD COLUMN deleting BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE projects ADD COLUMN deleted_at BIGINT;

-- NULL for rows written before this migration: read as `production` when
-- protected, else `standard`. (A backfill UPDATE would run under forced
-- row-level security without a tenant and silently change nothing.)
ALTER TABLE environments ADD COLUMN env_type TEXT
  CHECK (env_type IN ('standard', 'production', 'preview'));
ALTER TABLE environments ADD COLUMN quota JSONB
  CHECK (quota IS NULL OR jsonb_typeof(quota) = 'object');
ALTER TABLE environments ADD COLUMN deleting BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE environments ADD COLUMN deleted_at BIGINT;

ALTER TABLE applications ADD COLUMN deleted_at BIGINT;
ALTER TABLE application_targets ADD COLUMN deleted_at BIGINT;

ALTER TABLE projects DROP CONSTRAINT projects_org_id_slug_key;
CREATE UNIQUE INDEX projects_live_slug ON projects (org_id, slug) WHERE deleted_at IS NULL;
ALTER TABLE environments DROP CONSTRAINT environments_project_id_slug_key;
CREATE UNIQUE INDEX environments_live_slug ON environments (project_id, slug) WHERE deleted_at IS NULL;
ALTER TABLE applications DROP CONSTRAINT applications_project_id_slug_key;
CREATE UNIQUE INDEX applications_live_slug ON applications (project_id, slug) WHERE deleted_at IS NULL;
ALTER TABLE application_targets DROP CONSTRAINT application_targets_application_id_placement_id_key;
CREATE UNIQUE INDEX application_targets_live ON application_targets (application_id, placement_id)
  WHERE deleted_at IS NULL;

-- A deleted row stays deleted, and a row being deleted stays so (for the
-- tables that have `deleting`; application_targets keeps its own rule).
CREATE FUNCTION kuben_deletion_is_final() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS DISTINCT FROM OLD.deleted_at THEN
    RAISE EXCEPTION '% % is deleted', TG_TABLE_NAME, OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF (to_jsonb(OLD) ->> 'deleting')::boolean AND NOT (to_jsonb(NEW) ->> 'deleting')::boolean THEN
    RAISE EXCEPTION '% % is being deleted', TG_TABLE_NAME, OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER projects_deletion_is_final BEFORE UPDATE ON projects
  FOR EACH ROW EXECUTE FUNCTION kuben_deletion_is_final();
CREATE TRIGGER environments_deletion_is_final BEFORE UPDATE ON environments
  FOR EACH ROW EXECUTE FUNCTION kuben_deletion_is_final();
CREATE TRIGGER applications_deletion_is_final BEFORE UPDATE ON applications
  FOR EACH ROW EXECUTE FUNCTION kuben_deletion_is_final();
CREATE TRIGGER application_targets_deletion_is_final BEFORE UPDATE ON application_targets
  FOR EACH ROW EXECUTE FUNCTION kuben_deletion_is_final();
