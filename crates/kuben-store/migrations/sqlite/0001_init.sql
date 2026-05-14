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
