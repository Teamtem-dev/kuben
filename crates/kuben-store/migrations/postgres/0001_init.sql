-- Kuben identity / audit schema (PostgreSQL dialect).
-- Must stay column-for-column equivalent to migrations/sqlite/0001_init.sql
-- (enforced by kuben-store::tests::schema_parity).

CREATE TABLE organizations (
  id         TEXT PRIMARY KEY,
  slug       TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL,
  created_at BIGINT NOT NULL
);

CREATE TABLE users (
