-- M5.5: hourly usage of apps (plan §14.5).
--
-- Each replica keeps the last hour of CPU and memory in memory and writes
-- one row per app and hour. Rows are kept for a week (the retention pass
-- removes older ones). Replicas that write the same hour merge: the
-- averages and peaks of the fuller sample win.

CREATE TABLE usage_rollups (
  target_id   UUID NOT NULL,
  org_id      TEXT NOT NULL REFERENCES organizations (id),
  hour        BIGINT NOT NULL CHECK (hour % 3600000 = 0),
  cpu_avg     BIGINT NOT NULL CHECK (cpu_avg >= 0),
  cpu_max     BIGINT NOT NULL CHECK (cpu_max >= 0),
  memory_avg  BIGINT NOT NULL CHECK (memory_avg >= 0),
  memory_max  BIGINT NOT NULL CHECK (memory_max >= 0),
  samples     INTEGER NOT NULL CHECK (samples > 0),
  PRIMARY KEY (target_id, hour)
);
CREATE INDEX usage_rollups_age ON usage_rollups (org_id, hour);

ALTER TABLE usage_rollups ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_rollups FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON usage_rollups
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());
