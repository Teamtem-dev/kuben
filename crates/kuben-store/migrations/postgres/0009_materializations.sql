-- What the materializer (ADR-032) last wrote for each target, and the drift
-- it found since.
--
-- The materializer renders a target's `App` resource from an accepted
-- deployment run and writes it with server-side apply. It records here the
-- run's target generation and the App object it wrote: its Kubernetes UID and
-- the `metadata.generation` the write produced. A live object whose
-- `metadata.generation` is higher was changed by someone else: that is drift,
-- reported on the row and replaced by the next write.
--
-- Workers read this table across organizations, so, like 0005 and 0006, it
-- has no row-level security; request paths filter by organization explicitly.

CREATE TABLE target_materializations (
  target_id           UUID PRIMARY KEY,
  org_id              TEXT NOT NULL,
  project_id          UUID NOT NULL,
  -- The target generation written, and the operation whose run it came from.
  generation          BIGINT NOT NULL CHECK (generation > 0),
  operation_id        UUID NOT NULL REFERENCES operations (id),
  -- The App object: a recreated object has a new UID.
  resource_uid        TEXT NOT NULL UNIQUE,
  resource_generation BIGINT NOT NULL CHECK (resource_generation > 0),
  written_at          BIGINT NOT NULL,
  -- Changes made by anyone else, counted, with the last one described.
  drift_count         BIGINT NOT NULL DEFAULT 0 CHECK (drift_count >= 0),
  drift_detected_at   BIGINT,
  drift               JSONB,
  FOREIGN KEY (org_id, project_id, target_id) REFERENCES application_targets (org_id, project_id, id)
);

-- The generation fence (ADR-027) in SQL as well: a stale worker never records
-- a lower generation over a newer one, and the row never changes owner.
CREATE FUNCTION kuben_materialization_only_moves_forward() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.org_id IS DISTINCT FROM OLD.org_id OR NEW.project_id IS DISTINCT FROM OLD.project_id THEN
    RAISE EXCEPTION 'the materialization of target % belongs to its target', OLD.target_id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.generation < OLD.generation OR NEW.drift_count < OLD.drift_count THEN
    RAISE EXCEPTION 'the materialization of target % only moves forward', OLD.target_id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER materialization_only_moves_forward BEFORE UPDATE ON target_materializations
  FOR EACH ROW EXECUTE FUNCTION kuben_materialization_only_moves_forward();
