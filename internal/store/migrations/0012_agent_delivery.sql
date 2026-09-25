-- M1.9 (ADR-027): targets the cluster's agent carries out, and what the
-- agent observed of them.

-- How a target's runs reach its cluster. `controller`: the materializer
-- writes the target's App and the App controller carries it out.
-- `agent`: the cluster's agent carries the target's execution envelopes
-- out. Chosen when the target is made (an agent linked with the
-- applicationRuntime feature means `agent`); a later migration may move a
-- target to the agent, nothing moves one back.
ALTER TABLE application_targets
  ADD COLUMN delivery TEXT NOT NULL DEFAULT 'controller' CHECK (delivery IN ('controller', 'agent'));

CREATE FUNCTION kuben_delivery_stays_with_the_agent() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.delivery = 'agent' AND NEW.delivery IS DISTINCT FROM OLD.delivery THEN
    RAISE EXCEPTION 'target % is delivered by its agent and stays so', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER delivery_stays_with_the_agent BEFORE UPDATE OF delivery ON application_targets
  FOR EACH ROW EXECUTE FUNCTION kuben_delivery_stays_with_the_agent();

-- The latest observation the agent reported for a target. Only an agent of
-- the target's own cluster writes it, and never with an older generation.
CREATE TABLE runtime_observations (
  target_id   UUID PRIMARY KEY,
  org_id      TEXT NOT NULL,
  project_id  UUID NOT NULL,
  generation  BIGINT NOT NULL CHECK (generation >= 0),
  phase       TEXT NOT NULL
              CHECK (phase IN ('accepted', 'applying', 'ready', 'failed', 'rejected', 'unknown')),
  reason      TEXT,
  message     TEXT,
  observed_at BIGINT NOT NULL,
  FOREIGN KEY (org_id, project_id, target_id)
    REFERENCES application_targets (org_id, project_id, id) ON DELETE CASCADE
);

ALTER TABLE runtime_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE runtime_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON runtime_observations
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());
