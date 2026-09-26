-- M4.3: single sign-on with an OpenID Connect provider (kuben_core::sso).
--
-- A started sign-in is one row, found by the hash of its `state` and taken
-- once; it carries the nonce and PKCE verifier the callback needs and dies
-- after ten minutes. Identities link a provider subject to a user and
-- record the last sign-in.

CREATE TABLE sso_logins (
  state_hash BYTEA PRIMARY KEY CHECK (octet_length(state_hash) = 32),
  nonce      TEXT NOT NULL CHECK (char_length(nonce) BETWEEN 1 AND 128),
  verifier   TEXT NOT NULL CHECK (char_length(verifier) BETWEEN 43 AND 128),
  return_to  TEXT NOT NULL CHECK (char_length(return_to) BETWEEN 1 AND 512),
  created_at BIGINT NOT NULL,
  expires_at BIGINT NOT NULL CHECK (expires_at > created_at)
);
CREATE INDEX sso_logins_expiry ON sso_logins (expires_at);

ALTER TABLE identities
  ADD COLUMN email         TEXT,
  ADD COLUMN created_at    BIGINT,
  ADD COLUMN last_login_at BIGINT;
CREATE INDEX identities_user ON identities (user_id);
