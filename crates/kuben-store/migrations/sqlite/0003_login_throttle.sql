-- Scenario 1 (login throttling): failure windows shared by every replica.
-- Buckets are SHA-256 hashes; no email address or client IP is stored.
-- Must stay column-for-column equivalent to migrations/postgres/0003_login_throttle.sql.

CREATE TABLE login_throttle (
  bucket     TEXT PRIMARY KEY,
  failures   BIGINT NOT NULL,
