-- M4.6: image SBOMs, vulnerability scans, exceptions and the scan gate
-- (kuben_core::scan; plan §13.3, §18.1).
--
-- Scans are append-only records per digest: the newest decides. A scan that
-- could not run is recorded as `unavailable`, never as clean. An SBOM is
-- kept once per digest (gzip of the CycloneDX document). An exception lets
-- one finding through the gate until it expires or is revoked; it names its
-- owner and reason. The gate is part of the environment policy revision.

ALTER TABLE environment_policies
  ADD COLUMN scan_mode         TEXT NOT NULL DEFAULT 'off' CHECK (scan_mode IN ('off', 'warn', 'block')),
  ADD COLUMN scan_severity     TEXT NOT NULL DEFAULT 'critical' CHECK (scan_severity IN ('high', 'critical')),
  ADD COLUMN scan_require      BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN scan_max_age_secs INTEGER NOT NULL DEFAULT 604800
    CHECK (scan_max_age_secs BETWEEN 3600 AND 7776000);

-- Production environments get a new revision that refuses known critical
-- findings, as new production environments do. Tables with forced row-level
-- security show an ordinary role nothing without an organization, so this
-- runs once per organization. It also gives production environments the
-- policy 0019 meant to give them, where its backfill saw no rows.
DO $$
DECLARE
  org TEXT;
BEGIN
  FOR org IN SELECT id FROM organizations LOOP
    PERFORM set_config('kuben.org_id', org, true);
    INSERT INTO environment_policies
      (org_id, project_id, environment_id, revision, required_approvals, deploy_role, approve_role,
       approval_ttl_secs, created_by, created_at, scan_mode)
    SELECT e.org_id, e.project_id, e.id, COALESCE(p.revision, 0) + 1,
           COALESCE(p.required_approvals, 1), COALESCE(p.deploy_role, 'developer'),
           COALESCE(p.approve_role, 'admin'), COALESCE(p.approval_ttl_secs, 604800),
           'system:migration-0024', (extract(epoch FROM clock_timestamp()) * 1000)::BIGINT, 'block'
    FROM environments e
    LEFT JOIN LATERAL (SELECT n.* FROM environment_policies n
      WHERE n.environment_id = e.id ORDER BY n.revision DESC LIMIT 1) p ON TRUE
    WHERE e.org_id = org
      AND COALESCE(e.env_type, CASE WHEN e.protected THEN 'production' ELSE 'standard' END) = 'production';
  END LOOP;
  PERFORM set_config('kuben.org_id', '', true);
END
$$;

CREATE TABLE artifact_scans (
  id               UUID PRIMARY KEY,
  org_id           TEXT NOT NULL REFERENCES organizations (id),
  repository       TEXT NOT NULL,
  digest           TEXT NOT NULL CHECK (digest ~ '^sha(256|512):[0-9a-f]+$'),
  status           TEXT NOT NULL CHECK (status IN ('ok', 'unavailable')),
  scanner          TEXT NOT NULL CHECK (char_length(scanner) <= 128),
  db_updated_at    BIGINT,
  critical         INTEGER NOT NULL DEFAULT 0 CHECK (critical >= 0),
  high             INTEGER NOT NULL DEFAULT 0 CHECK (high >= 0),
  medium           INTEGER NOT NULL DEFAULT 0 CHECK (medium >= 0),
  low              INTEGER NOT NULL DEFAULT 0 CHECK (low >= 0),
  unknown          INTEGER NOT NULL DEFAULT 0 CHECK (unknown >= 0),
  -- `severity:id`, most severe first.
  findings         TEXT[] NOT NULL DEFAULT '{}' CHECK (cardinality(findings) <= 40),
  detail           TEXT CHECK (char_length(detail) <= 2048),
  build_attempt_id UUID,
  scanned_at       BIGINT NOT NULL
);
CREATE INDEX artifact_scans_digest ON artifact_scans (org_id, digest, scanned_at DESC);
CREATE TRIGGER artifact_scans_append_only BEFORE UPDATE OR DELETE ON artifact_scans
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();

ALTER TABLE artifact_scans ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_scans FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON artifact_scans
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

CREATE TABLE artifact_sboms (
  org_id     TEXT NOT NULL REFERENCES organizations (id),
  digest     TEXT NOT NULL CHECK (digest ~ '^sha(256|512):[0-9a-f]+$'),
  format     TEXT NOT NULL CHECK (format IN ('cyclonedx+json')),
  -- gzip of the document, at most 8 MiB.
  content    BYTEA NOT NULL CHECK (octet_length(content) BETWEEN 18 AND 8388608),
  created_at BIGINT NOT NULL,
  PRIMARY KEY (org_id, digest)
);
CREATE TRIGGER artifact_sboms_append_only BEFORE UPDATE OR DELETE ON artifact_sboms
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();

ALTER TABLE artifact_sboms ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_sboms FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON artifact_sboms
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

CREATE TABLE vulnerability_exceptions (
  id            UUID PRIMARY KEY,
  org_id        TEXT NOT NULL REFERENCES organizations (id),
  -- The project it applies to; every project of the organization when null.
  project_id    UUID,
  vulnerability TEXT NOT NULL CHECK (vulnerability ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
  reason        TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1024),
  -- Who answers for it (an email or a team).
  owner         TEXT NOT NULL CHECK (char_length(owner) BETWEEN 1 AND 256),
  created_by    TEXT NOT NULL,
  created_at    BIGINT NOT NULL,
  expires_at    BIGINT NOT NULL CHECK (expires_at > created_at AND expires_at <= created_at + 7776000000),
  revoked_at    BIGINT,
  revoked_by    TEXT,
  CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id)
);
CREATE INDEX vulnerability_exceptions_live ON vulnerability_exceptions (org_id, vulnerability)
  WHERE revoked_at IS NULL;

CREATE FUNCTION kuben_exception_rules() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'exception % is revoked, never deleted', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF ROW(NEW.id, NEW.org_id, NEW.project_id, NEW.vulnerability, NEW.reason, NEW.owner,
         NEW.created_by, NEW.created_at, NEW.expires_at)
     IS DISTINCT FROM
     ROW(OLD.id, OLD.org_id, OLD.project_id, OLD.vulnerability, OLD.reason, OLD.owner,
         OLD.created_by, OLD.created_at, OLD.expires_at)
     OR OLD.revoked_at IS NOT NULL THEN
    RAISE EXCEPTION 'exception % can only be revoked, once', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER exception_rules BEFORE UPDATE OR DELETE ON vulnerability_exceptions
  FOR EACH ROW EXECUTE FUNCTION kuben_exception_rules();

ALTER TABLE vulnerability_exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE vulnerability_exceptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON vulnerability_exceptions
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());
