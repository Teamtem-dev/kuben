-- Scenario 4 (team): invited users must replace their temporary password.
-- Scenario 5 (releases): every spec change of an App is a numbered revision.
-- Must stay column-for-column equivalent to migrations/postgres/0002_team_releases.sql.

ALTER TABLE users ADD COLUMN must_change_password BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE app_releases (
  id         TEXT PRIMARY KEY,
  org_id     TEXT,
  namespace  TEXT NOT NULL,
  app        TEXT NOT NULL,
  revision   BIGINT NOT NULL,
  image      TEXT,
