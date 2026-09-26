-- M4.10: incidents, signed webhooks and the events behind them (plan §15).
--
-- Every settled operation now also leaves an outbox message
-- (`<kind>.settled`), next to the `…accepted`/`…queued` one it had. The
-- notifier consumes the outbox: it opens and resolves incidents, fans events
-- out to the organization's webhook endpoints and reports commit statuses.
-- A delivery is retried with backoff and given up after a day, so a long
-- outage never ends in a storm of stale deliveries.

CREATE TABLE incidents (
  id              UUID PRIMARY KEY,
  org_id          TEXT NOT NULL REFERENCES organizations (id),
  project_id      UUID,
  environment_id  UUID,
  target_id       UUID,
  kind            TEXT NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_.]{0,63}$'),
  severity        TEXT NOT NULL CHECK (severity IN ('critical', 'warning', 'info')),
  -- One open incident per key: a repeat raises `occurrences`.
  dedupe_key      TEXT NOT NULL CHECK (char_length(dedupe_key) BETWEEN 1 AND 256),
  title           TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 256),
  detail          TEXT CHECK (char_length(detail) <= 2048),
  opened_at       BIGINT NOT NULL,
  last_seen_at    BIGINT NOT NULL,
  occurrences     BIGINT NOT NULL DEFAULT 1 CHECK (occurrences > 0),
  acknowledged_at BIGINT,
  acknowledged_by TEXT,
  resolved_at     BIGINT,
  resolved_by     TEXT,
  CHECK ((acknowledged_at IS NULL) = (acknowledged_by IS NULL)),
  CHECK ((resolved_at IS NULL) = (resolved_by IS NULL))
);
CREATE UNIQUE INDEX incidents_open ON incidents (org_id, dedupe_key) WHERE resolved_at IS NULL;
CREATE INDEX incidents_recent ON incidents (org_id, opened_at DESC);

ALTER TABLE incidents ENABLE ROW LEVEL SECURITY;
ALTER TABLE incidents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON incidents
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

CREATE TABLE webhook_endpoints (
  id          UUID PRIMARY KEY,
  org_id      TEXT NOT NULL REFERENCES organizations (id),
  name        TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
  url         TEXT NOT NULL CHECK (url ~ '^https?://' AND char_length(url) <= 2048),
  -- Event types, or `*` for all.
  events      TEXT[] NOT NULL CHECK (cardinality(events) BETWEEN 1 AND 32),
  -- The signing secret, sealed like a secret revision (revision 0).
  secret      BYTEA NOT NULL,
  wrapped_key BYTEA NOT NULL,
  key_version INTEGER NOT NULL CHECK (key_version > 0),
  created_by  TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  disabled_at BIGINT,
  -- Deliveries given up in a row; reset by a success.
  failures    INTEGER NOT NULL DEFAULT 0 CHECK (failures >= 0),
  UNIQUE (org_id, name)
);

ALTER TABLE webhook_endpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_endpoints FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON webhook_endpoints
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

-- Taken by delivery workers of any organization, like `outbox`: no row-level
-- security; every request path filters by organization.
CREATE TABLE webhook_deliveries (
  id              UUID PRIMARY KEY,
  org_id          TEXT NOT NULL REFERENCES organizations (id),
  endpoint_id     UUID NOT NULL REFERENCES webhook_endpoints (id),
  event_id        UUID NOT NULL,
  event           TEXT NOT NULL,
  payload         JSONB NOT NULL,
  status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at BIGINT NOT NULL,
  last_status     INTEGER,
  last_error      TEXT CHECK (char_length(last_error) <= 1024),
  created_at      BIGINT NOT NULL,
  finished_at     BIGINT,
  UNIQUE (endpoint_id, event_id)
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_endpoint ON webhook_deliveries (endpoint_id, created_at DESC);

-- Nothing consumed the outbox before: what is in it is history, not news.
UPDATE outbox SET delivered_at = (extract(epoch FROM clock_timestamp()) * 1000)::BIGINT
WHERE delivered_at IS NULL;
