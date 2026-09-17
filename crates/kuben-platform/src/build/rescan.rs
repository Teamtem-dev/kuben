//! Rescans of the images apps run (M4.6; plan §18.1): an image scanned
//! yesterday may have a new finding today. Every image a live target runs
//! whose newest scan is older than the interval is scanned again with a
//! fresh feed, by a scan-only Job in the build namespace with the same
//! budgets and isolation as a build (no service-account token, no source).
//! A rescan that cannot run is recorded as unavailable, which also waits for
//! the next interval. New findings gate future deployments; running apps
//! are never stopped by them.

use std::time::Duration;

use k8s_openapi::api::{batch::v1::Job, core::v1::Pod};
use kube::{
    Api, Client,
    api::{DeleteParams, PostParams, PropagationPolicy},
};
use kuben_core::{ids::OrgId, scan::ScanReport, time::now_ms};
use kuben_store::{Store, repo::NewScan};
use serde_json::json;
use sha2::{Digest as _, Sha256};
use tokio_util::sync::CancellationToken;

use super::{
    evidence,
    job::{BuildSettings, scan_container},
};
use crate::health::Health;

const HEALTH: &str = "rescans";
/// Label of a rescan Job and its pod.
pub const RESCAN_LABEL: &str = "kuben.dev/rescan";
/// Images rescanned per organization per pass.
const BATCH: i64 = 10;
/// Pause between passes.
const PASS_EVERY: Duration = Duration::from_mins(30);
const POLL: Duration = Duration::from_secs(5);
/// Longest one rescan may take.
const DEADLINE: Duration = Duration::from_mins(15);

/// Rescans running images of every organization.
#[derive(Clone)]
pub struct Rescanner {
    store: Store,
    client: Client,
    settings: BuildSettings,
    interval: Duration,
}

impl std::fmt::Debug for Rescanner {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Rescanner")
            .field("namespace", &self.settings.namespace)
            .field("interval", &self.interval)
            .finish_non_exhaustive()
    }
}

/// The deterministic Job name of a rescan of `digest`.
#[must_use]
pub fn job_name(digest: &str) -> String {
    let hash = Sha256::digest(digest.as_bytes());
    let hex = hash.iter().take(8).fold(String::new(), |mut out, b| {
        use std::fmt::Write as _;
        let _ = write!(out, "{b:02x}");
        out
    });
    format!("kscan-{hex}")
}

/// The Job that scans `repository@digest`.
///
/// # Errors
///
/// When the rendered object does not deserialize (a bug), or without a
/// scanner.
pub fn rescan_job(settings: &BuildSettings, repository: &str, digest: &str) -> Result<Job, String> {
    let scan = scan_container(settings, repository, Some(digest)).ok_or("scanning is not configured")?;
    let name = job_name(digest);
    let labels = json!({ RESCAN_LABEL: name, "app.kubernetes.io/managed-by": "kuben" });
    let mut volumes =
        vec![json!({ "name": "workspace", "emptyDir": { "sizeLimit": settings.ephemeral_storage } })];
    if let Some(secret) = &settings.push_secret {
        volumes.push(json!({ "name": "push", "secret": {
            "secretName": secret, "defaultMode": 0o440,
            "items": [{ "key": ".dockerconfigjson", "path": "config.json" }],
        } }));
    }
    let value = json!({
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": { "name": name, "namespace": settings.namespace, "labels": labels },
        "spec": {
            "backoffLimit": 0,
            "activeDeadlineSeconds": DEADLINE.as_secs(),
            "ttlSecondsAfterFinished": 3600,
            "template": {
                "metadata": { "labels": labels },
                "spec": {
                    "restartPolicy": "Never",
                    "automountServiceAccountToken": false,
                    "enableServiceLinks": false,
                    "securityContext": { "runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000 },
                    "containers": [scan],
                    "volumes": volumes,
                },
            },
        },
    });
    serde_json::from_value(value).map_err(|e| e.to_string())
}

/// Run `rescanner` until `token` is cancelled.
pub async fn run(rescanner: Rescanner, health: Health, token: CancellationToken) -> anyhow::Result<()> {
    health.ok(HEALTH);
    while !token.is_cancelled() {
        if let Err(e) = rescanner.pass(&token).await {
            tracing::warn!(error = %e, "a rescan pass failed");
            health.degraded(HEALTH, &e.to_string());
        } else {
            health.ok(HEALTH);
        }
        tokio::select! {
            () = token.cancelled() => {}
            () = tokio::time::sleep(PASS_EVERY) => {}
        }
    }
    Ok(())
}

impl Rescanner {
    #[must_use]
    pub fn new(store: Store, client: Client, settings: BuildSettings, interval: Duration) -> Self {
        Self {
            store,
            client,
            settings,
            interval,
        }
    }

    /// Rescan what is due in every organization, one image at a time.
    pub async fn pass(&self, token: &CancellationToken) -> anyhow::Result<usize> {
        let before = now_ms() - i64::try_from(self.interval.as_millis()).unwrap_or(i64::MAX);
        let mut done = 0;
        for org in self.store.org_ids().await? {
            let due = {
                let mut t = self.store.tenant(org).await?;
                t.rescan_due(before, BATCH).await?
            };
            for (repository, digest) in due {
                if token.is_cancelled() {
                    return Ok(done);
                }
                self.rescan(org, &repository, &digest, token).await?;
                done += 1;
            }
        }
        if done > 0 {
            tracing::info!(images = done, "running images rescanned");
        }
        Ok(done)
    }

    async fn rescan(
        &self,
        org: OrgId,
        repository: &str,
        digest: &str,
        token: &CancellationToken,
    ) -> anyhow::Result<()> {
        let jobs: Api<Job> = Api::namespaced(self.client.clone(), &self.settings.namespace);
        let pods: Api<Pod> = Api::namespaced(self.client.clone(), &self.settings.namespace);
        let name = job_name(digest);
        let job = rescan_job(&self.settings, repository, digest).map_err(anyhow::Error::msg)?;
        match jobs.create(&PostParams::default(), &job).await {
            Ok(_) => {}
            // Another replica or a previous pass started it: follow it.
            Err(kube::Error::Api(s)) if s.code == 409 => {}
            Err(e) => return Err(e.into()),
        }
        let started = tokio::time::Instant::now();
        let (report, sbom) = loop {
            let finished = jobs.get_opt(&name).await?.is_none_or(|j| {
                j.status
                    .and_then(|s| s.conditions)
                    .unwrap_or_default()
                    .iter()
                    .any(|c| (c.type_ == "Complete" || c.type_ == "Failed") && c.status == "True")
            });
            if finished {
                break evidence::collect(&pods, &format!("{RESCAN_LABEL}={name}")).await?;
            }
            if started.elapsed() > DEADLINE + POLL {
                break (ScanReport::unavailable("the rescan did not finish"), None);
            }
            tokio::select! {
                () = token.cancelled() => return Ok(()),
                () = tokio::time::sleep(POLL) => {}
            }
        };
        let scan = NewScan {
            repository: repository.to_owned(),
            digest: digest.to_owned(),
            summary: evidence::summary_of(&report, now_ms()),
            detail: report.detail.as_deref().map(|d| d.chars().take(2048).collect()),
            build_attempt: None,
        };
        let mut t = self.store.tenant(org).await?;
        t.record_scan(&scan).await?;
        if let Some(sbom) = &sbom {
            t.put_sbom(digest, sbom).await?;
        }
        t.commit().await?;
        let background = DeleteParams {
            propagation_policy: Some(PropagationPolicy::Background),
            ..DeleteParams::default()
        };
        match jobs.delete(&name, &background).await {
            Ok(_) => {}
            Err(kube::Error::Api(s)) if s.code == 404 => {}
            Err(e) => return Err(e.into()),
        }
        let counts = scan.summary.counts;
        tracing::info!(%org, digest, status = scan.summary.status.as_str(), critical = counts.critical, high = counts.high, "image rescanned");
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::build::job::tests::settings;

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    #[test]
    fn rescan_jobs_scan_one_digest_in_isolation() {
        assert_eq!(job_name(DIGEST), job_name(DIGEST));
        assert_ne!(job_name(DIGEST), job_name("sha256:1"));
        assert!(job_name(DIGEST).len() <= 63);
        let job =
            serde_json::to_value(rescan_job(&settings(), "registry.local/acme/shop", DIGEST).expect("job"))
                .expect("json");
        let pod = &job["spec"]["template"]["spec"];
        assert_eq!(pod["automountServiceAccountToken"], false);
        assert_eq!(job["spec"]["backoffLimit"], 0);
        let containers = pod["containers"].as_array().expect("containers");
        assert_eq!(containers.len(), 1);
        let env = containers[0]["env"].as_array().expect("env");
        assert!(
            env.iter()
                .any(|e| e["name"] == "KUBEN_DIGEST" && e["value"] == DIGEST)
        );
        assert!(
            pod["volumes"]
                .as_array()
                .expect("volumes")
                .iter()
                .any(|v| v["name"] == "push")
        );
        assert!(pod.get("initContainers").is_none(), "no source, no build");
        let off = BuildSettings {
            scanner_image: None,
            ..settings()
        };
        assert!(rescan_job(&off, "r", DIGEST).is_err());
    }
}
