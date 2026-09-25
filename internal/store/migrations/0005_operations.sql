-- Durable operations (ADR-025, ADR-027; plan §8.2, §9.2, §9.3).
--
-- An accepted request is one transaction: its operation, audit record,
-- outbox message and idempotency receipt commit together or not at all
-- (I01). Workers claim operations with FOR UPDATE SKIP LOCKED under a short
-- lease; every claim raises the fence, and every write by a worker is
-- conditional on the fence it claimed with, so a paused worker that wakes up
-- after its lease expired writes nothing (I06). Outbox delivery is
-- at-least-once and receivers are idempotent by id (I17).
--
-- Workers read these tables across organizations, so they carry
-- tenant-aware keys but no row-level security; request paths filter by
-- organization explicitly.

-- The generic tables of 0001 were never used.
DROP TABLE outbox;
DROP TABLE idempotency_keys;

-- The database clock in unix milliseconds: leases and retries never trust a
-- worker's clock.
CREATE FUNCTION kuben_now_ms() RETURNS BIGINT
LANGUAGE sql VOLATILE AS $$ SELECT (extract(epoch FROM clock_timestamp()) * 1000)::BIGINT $$;

CREATE TABLE operations (
  id                  UUID PRIMARY KEY,
  org_id              TEXT NOT NULL REFERENCES organizations (id),
  project_id          UUID,
  target_id           UUID,
  kind                TEXT NOT NULL,
  schema_version      INTEGER NOT NULL DEFAULT 1,
  -- The target's lifecycle UID and generation the operation was accepted for.
  lifecycle_uid       UUID,
  generation          BIGINT CHECK (generation >= 0),
  input_hash          BYTEA NOT NULL,
  payload             JSONB NOT NULL,
  requested_by        TEXT NOT NULL,
  requested_at        BIGINT NOT NULL,
  deadline_at         BIGINT,
  phase               TEXT NOT NULL DEFAULT 'queued',
  -- A finished operation is never claimed again.
  done                BOOLEAN NOT NULL DEFAULT FALSE,
  attempt             INTEGER NOT NULL DEFAULT 0,
  next_attempt_at     BIGINT NOT NULL,
  lease_owner         TEXT,
  lease_until         BIGINT,
  fence               BIGINT NOT NULL DEFAULT 0,
  cancel_requested_at BIGINT,
  last_progress_at    BIGINT,
  error_code          TEXT,
  -- The last operation_events.seq; events number themselves from it.
  event_seq           BIGINT NOT NULL DEFAULT 0,
  CHECK ((project_id IS NULL) = (target_id IS NULL)),
  FOREIGN KEY (org_id, project_id, target_id) REFERENCES application_targets (org_id, project_id, id)
);
CREATE INDEX operations_ready ON operations (next_attempt_at, id) WHERE NOT done;
CREATE INDEX operations_target ON operations (target_id, requested_at) WHERE target_id IS NOT NULL;

-- Append-only history of an operation.
CREATE TABLE operation_events (
  operation_id UUID NOT NULL REFERENCES operations (id),
  seq          BIGINT NOT NULL,
  kind         TEXT NOT NULL,
  data         JSONB,
  at           BIGINT NOT NULL,
  PRIMARY KEY (operation_id, seq)
);

CREATE FUNCTION kuben_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% is append-only', TG_TABLE_NAME
    USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER operation_events_append_only BEFORE UPDATE OR DELETE ON operation_events
  FOR EACH ROW EXECUTE FUNCTION kuben_append_only();

CREATE TABLE outbox (
  id           UUID PRIMARY KEY,
  org_id       TEXT NOT NULL REFERENCES organizations (id),
  operation_id UUID REFERENCES operations (id),
  topic        TEXT NOT NULL,
  payload      JSONB NOT NULL,
  created_at   BIGINT NOT NULL,
  -- Not handed out again before this time: a delivery in progress or a retry.
  available_at BIGINT NOT NULL,
  attempts     INTEGER NOT NULL DEFAULT 0,
  delivered_at BIGINT,
  last_error   TEXT
);
CREATE INDEX outbox_pending ON outbox (available_at, id) WHERE delivered_at IS NULL;

-- Provider deliveries (webhooks), deduplicated by the provider's delivery id.
CREATE TABLE inbox (
  provider    TEXT NOT NULL,
  delivery_id TEXT NOT NULL,
  org_id      TEXT NOT NULL REFERENCES organizations (id),
  body_sha256 BYTEA NOT NULL,
  receipt     UUID NOT NULL UNIQUE,
  received_at BIGINT NOT NULL,
  PRIMARY KEY (provider, delivery_id)
);

-- Idempotency-Key receipts: the same key and request return the same
-- operation; the same key with another request is a conflict. A receipt
-- expires; the operation it names stays.
CREATE TABLE idempotency_receipts (
  org_id       TEXT NOT NULL REFERENCES organizations (id),
  actor        TEXT NOT NULL,
  operation    TEXT NOT NULL,
  key          TEXT NOT NULL,
  request_hash BYTEA NOT NULL,
  -- Deferred: the receipt is written before its operation, in one transaction.
  operation_id UUID NOT NULL REFERENCES operations (id) DEFERRABLE INITIALLY DEFERRED,
  created_at   BIGINT NOT NULL,
  expires_at   BIGINT NOT NULL,
  PRIMARY KEY (org_id, actor, operation, key)
);
CREATE INDEX idempotency_receipts_expiry ON idempotency_receipts (expires_at);
