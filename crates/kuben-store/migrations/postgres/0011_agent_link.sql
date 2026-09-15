-- AgentLink (ADR-027, plan §12): enrollment tokens and the agent linked to
-- each cluster.
--
-- An enrolling agent is anonymous until its token is redeemed, so these
-- tables are found by token hash and cluster id, not through the tenant
-- setting (like operations and the outbox). Every row still names its
-- organization, bound to the cluster by a foreign key, and the tenant-scoped
-- store functions filter by it.

-- A bootstrap token, kept as its SHA-256 only. It is bound to one cluster,
-- expires, and is redeemed once: by one device, which may resume it.
CREATE TABLE agent_tokens (
  token_hash  BYTEA PRIMARY KEY CHECK (octet_length(token_hash) = 32),
  org_id      TEXT NOT NULL,
  cluster_id  UUID NOT NULL,
  expires_at  BIGINT NOT NULL,
  device_id   TEXT,
  redeemed_at BIGINT,
  created_by  TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  CHECK ((device_id IS NULL) = (redeemed_at IS NULL)),
  FOREIGN KEY (org_id, cluster_id) REFERENCES clusters (org_id, id) ON DELETE CASCADE
);
CREATE INDEX agent_tokens_cluster ON agent_tokens (cluster_id);

CREATE FUNCTION kuben_agent_token_is_redeemed_once() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.token_hash IS DISTINCT FROM OLD.token_hash
     OR NEW.org_id IS DISTINCT FROM OLD.org_id
     OR NEW.cluster_id IS DISTINCT FROM OLD.cluster_id
     OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
     OR NEW.created_by IS DISTINCT FROM OLD.created_by
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR (OLD.device_id IS NOT NULL
         AND (NEW.device_id IS DISTINCT FROM OLD.device_id
              OR NEW.redeemed_at IS DISTINCT FROM OLD.redeemed_at)) THEN
    RAISE EXCEPTION 'agent token % is redeemed once, by one device', encode(OLD.token_hash, 'hex')
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER agent_token_is_redeemed_once BEFORE UPDATE ON agent_tokens
  FOR EACH ROW EXECUTE FUNCTION kuben_agent_token_is_redeemed_once();

-- The agent linked to a cluster: its device, its certificate's expiry, the
-- latest link and whether it was revoked. A fresh enrollment with another
-- device replaces a revoked one; the same device stays revoked.
CREATE TABLE cluster_agents (
  cluster_id            UUID PRIMARY KEY,
  org_id                TEXT NOT NULL,
  device_id             TEXT NOT NULL,
  certificate_not_after BIGINT NOT NULL,
  protocol_version      INTEGER,
  features              JSONB NOT NULL DEFAULT '[]'::jsonb,
  agent_version         TEXT,
  linked_at             BIGINT,
  last_seen_at          BIGINT,
  revoked_at            BIGINT,
  updated_at            BIGINT NOT NULL,
  FOREIGN KEY (org_id, cluster_id) REFERENCES clusters (org_id, id) ON DELETE CASCADE
);

CREATE FUNCTION kuben_agent_revocation_holds() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.org_id IS DISTINCT FROM OLD.org_id
     OR (OLD.revoked_at IS NOT NULL
         AND NEW.revoked_at IS NULL
         AND NEW.device_id IS NOT DISTINCT FROM OLD.device_id) THEN
    RAISE EXCEPTION 'the revoked agent of cluster % stays revoked', OLD.cluster_id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER agent_revocation_holds BEFORE UPDATE ON cluster_agents
  FOR EACH ROW EXECUTE FUNCTION kuben_agent_revocation_holds();
