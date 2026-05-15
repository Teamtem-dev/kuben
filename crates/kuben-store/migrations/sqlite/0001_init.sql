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
