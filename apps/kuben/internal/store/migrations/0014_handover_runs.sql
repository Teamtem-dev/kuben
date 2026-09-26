-- M1.9: an app the App controller delivers is handed over to its cluster's
-- agent on request (application_targets.delivery moves to `agent`, migration
-- 0012). The handover is a deployment run of the same release and
-- configuration that goes through the agent, which adopts the workloads.

ALTER TABLE deployment_runs DROP CONSTRAINT deployment_runs_reason_check;
ALTER TABLE deployment_runs ADD CONSTRAINT deployment_runs_reason_check
  CHECK (reason IN ('deploy', 'rollback', 'promotion', 'restart', 'handover'));
