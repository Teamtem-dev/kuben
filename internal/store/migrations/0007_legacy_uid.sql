-- The Kubernetes UID of the resource a row was imported from (ADR-032):
--   Project → projects
--   Environment → environments
--   App → application_targets (an App is one application in one
--         environment namespace, which is a target)
--
-- While the resource model and SQL coexist, scope resolution finds a row by
-- this UID first, and role bindings made on the UID keep applying. The
-- importer fills it; rows created through SQL leave it empty.

ALTER TABLE projects ADD COLUMN legacy_uid UUID;
ALTER TABLE environments ADD COLUMN legacy_uid UUID;
ALTER TABLE application_targets ADD COLUMN legacy_uid UUID;

CREATE UNIQUE INDEX projects_legacy_uid ON projects (org_id, legacy_uid)
  WHERE legacy_uid IS NOT NULL;
CREATE UNIQUE INDEX environments_legacy_uid ON environments (org_id, legacy_uid)
  WHERE legacy_uid IS NOT NULL;
CREATE UNIQUE INDEX application_targets_legacy_uid ON application_targets (org_id, legacy_uid)
  WHERE legacy_uid IS NOT NULL;
