-- M5.2: domain claims and DNS providers (plan §14.3).
--
-- An organization claims a domain before its apps may serve it. A claim is
-- verified by a TXT record (`_kuben-challenge.<domain>` = token) or by a DNS
-- provider account that holds the domain's zone. A verified claim covers
-- the domain and every name below it.
--
-- Verified domains are copied to `verified_domains`, which has no row-level
-- security: the uniqueness and overlap check must see every organization's
-- claims. It holds names and organization ids only, and is changed under an
-- advisory lock on the domain's last two labels, so two organizations never
-- verify overlapping names at the same time.

CREATE TABLE domain_claims (
  id              UUID PRIMARY KEY,
  org_id          TEXT NOT NULL REFERENCES organizations (id),
  -- Lower case, IDNA (punycode), no trailing dot.
  domain          TEXT NOT NULL CHECK (
                    char_length(domain) BETWEEN 3 AND 253
                    AND domain ~ '^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
  -- The TXT value that proves the claim.
  token           TEXT NOT NULL CHECK (char_length(token) BETWEEN 32 AND 128),
  status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'verified', 'revoked')),
  -- `txt`, or the provider that proved it.
  method          TEXT CHECK (method IN ('txt', 'cloudflare')),
  provider_id     UUID,
  created_by      TEXT NOT NULL,
  created_at      BIGINT NOT NULL,
  verified_at     BIGINT,
  revoked_at      BIGINT,
  revoked_by      TEXT,
  last_checked_at BIGINT,
  last_error      TEXT CHECK (char_length(last_error) <= 1024),
  CHECK ((status = 'verified') = (verified_at IS NOT NULL AND revoked_at IS NULL)),
  CHECK ((status = 'revoked') = (revoked_at IS NOT NULL)),
  CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)),
  UNIQUE (org_id, id)
);
-- One open claim per domain in an organization.
CREATE UNIQUE INDEX domain_claims_open ON domain_claims (org_id, domain) WHERE status <> 'revoked';

ALTER TABLE domain_claims ENABLE ROW LEVEL SECURITY;
ALTER TABLE domain_claims FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON domain_claims
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

-- Verified domains of every organization: the global uniqueness check.
CREATE TABLE verified_domains (
  domain   TEXT PRIMARY KEY,
  org_id   TEXT NOT NULL REFERENCES organizations (id),
  claim_id UUID NOT NULL UNIQUE,
  since    BIGINT NOT NULL
);
CREATE INDEX verified_domains_org ON verified_domains (org_id);

-- A DNS provider account of an organization: its API token is sealed with
-- the secret keyring (like webhook secrets).
CREATE TABLE dns_providers (
  id          UUID PRIMARY KEY,
  org_id      TEXT NOT NULL REFERENCES organizations (id),
  name        TEXT NOT NULL CHECK (name ~ '^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$'),
  kind        TEXT NOT NULL CHECK (kind IN ('cloudflare')),
  secret      BYTEA NOT NULL,
  wrapped_key BYTEA NOT NULL,
  key_version INTEGER NOT NULL CHECK (key_version > 0),
  created_by  TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  deleted_at  BIGINT,
  UNIQUE (org_id, id)
);
CREATE UNIQUE INDEX dns_providers_name ON dns_providers (org_id, name) WHERE deleted_at IS NULL;

ALTER TABLE dns_providers ENABLE ROW LEVEL SECURITY;
ALTER TABLE dns_providers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON dns_providers
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());

ALTER TABLE domain_claims
  ADD CONSTRAINT domain_claims_provider FOREIGN KEY (org_id, provider_id) REFERENCES dns_providers (org_id, id);

-- DNS records Kuben wrote through a provider: deleted only when the provider
-- still reports the same record id (plan §14.3, delete safety).
CREATE TABLE dns_records (
  id           UUID PRIMARY KEY,
  org_id       TEXT NOT NULL REFERENCES organizations (id),
  provider_id  UUID NOT NULL,
  target_id    UUID,
  name         TEXT NOT NULL,
  record_type  TEXT NOT NULL CHECK (record_type IN ('A', 'AAAA', 'CNAME', 'TXT')),
  content      TEXT NOT NULL,
  zone_id      TEXT NOT NULL,
  -- The provider's record id, once created.
  provider_ref TEXT,
  created_at   BIGINT NOT NULL,
  updated_at   BIGINT NOT NULL,
  UNIQUE (provider_id, name, record_type, content),
  FOREIGN KEY (org_id, provider_id) REFERENCES dns_providers (org_id, id)
);
CREATE INDEX dns_records_target ON dns_records (org_id, target_id);

ALTER TABLE dns_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE dns_records FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON dns_records
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());
