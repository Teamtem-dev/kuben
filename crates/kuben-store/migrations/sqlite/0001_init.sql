-- Kuben identity / audit schema (SQLite dialect).
-- Timestamps are unix milliseconds (BIGINT). IDs are UUIDv7 strings.

CREATE TABLE organizations (
  id         TEXT PRIMARY KEY,
  slug       TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL,
  created_at BIGINT NOT NULL
);

CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  display_name  TEXT,
  password_hash TEXT,
  is_active     BOOLEAN NOT NULL DEFAULT TRUE,
  created_at    BIGINT NOT NULL
);

CREATE TABLE identities (
  id       TEXT PRIMARY KEY,
  user_id  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  subject  TEXT NOT NULL,
  UNIQUE (provider, subject)
);

CREATE TABLE memberships (
  org_id  TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (org_id, user_id)
);

CREATE TABLE role_bindings (
  id           TEXT PRIMARY KEY,
  org_id       TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  subject_kind TEXT NOT NULL,
  subject_id   TEXT NOT NULL,
  role         TEXT NOT NULL,
  scope_kind   TEXT NOT NULL,
  scope_uid    TEXT,
  created_at   BIGINT NOT NULL
);
CREATE INDEX rb_subject ON role_bindings (subject_kind, subject_id);

CREATE TABLE sessions (
  id_hash      BLOB PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   BIGINT NOT NULL,
  expires_at   BIGINT NOT NULL,
  last_seen_at BIGINT,
  ip           TEXT,
  ua_hash      BLOB,
  revoked_at   BIGINT
);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE api_tokens (
  id            TEXT PRIMARY KEY,
  org_id        TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  owner_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
  name          TEXT NOT NULL,
  prefix        TEXT NOT NULL,
  secret_hash   BLOB NOT NULL UNIQUE,
  scopes        TEXT NOT NULL,
  expires_at    BIGINT,
  last_used_at  BIGINT,
  revoked_at    BIGINT,
  created_at    BIGINT NOT NULL
);

CREATE TABLE revocations (
  subject_hash BLOB PRIMARY KEY,
