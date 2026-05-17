-- Scenario 4 (team): invited users must replace their temporary password.
-- Scenario 5 (releases): every spec change of an App is a numbered revision.
-- Must stay column-for-column equivalent to migrations/sqlite/0002_team_releases.sql.

ALTER TABLE users ADD COLUMN must_change_password BOOLEAN NOT NULL DEFAULT FALSE;

