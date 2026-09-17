//! Image scans, SBOMs, vulnerability exceptions and the scan gate (M4.6,
//! migration 0024).
//!
//! [`Tenant::scan_verdict`] applies [`kuben_core::scan::evaluate`] to a
//! release on a target: the environment's gate, the newest scan of every
//! digest of the release, and the unexpired exceptions of the project.

use std::collections::{BTreeMap, BTreeSet};

use kuben_core::{
    ids::{BuildAttemptId, ProjectId, ReleaseId, TargetId},
    scan::{Counts, GateVerdict, ScanStatus, ScanSummary, Severity, evaluate},
    time::now_ms,
};
use uuid::Uuid;

use super::Tenant;
use crate::StoreError;

const INSERT_SCAN: &str = "INSERT INTO artifact_scans \
     (id, org_id, repository, digest, status, scanner, db_updated_at, critical, high, medium, low, unknown, \
      findings, detail, build_attempt_id, scanned_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)";
const LATEST_SCANS: &str = "SELECT DISTINCT ON (digest) digest, status, scanner, db_updated_at, critical, high, \
     medium, low, unknown, findings, scanned_at FROM artifact_scans \
     WHERE org_id = $1 AND digest = ANY($2) ORDER BY digest, scanned_at DESC";
const SCAN_HISTORY: &str = "SELECT digest, status, scanner, db_updated_at, critical, high, medium, low, unknown, \
     findings, scanned_at FROM artifact_scans \
     WHERE org_id = $1 AND digest = $2 ORDER BY scanned_at DESC LIMIT $3";
const INSERT_SBOM: &str = "INSERT INTO artifact_sboms (org_id, digest, format, content, created_at) \
     VALUES ($1, $2, 'cyclonedx+json', $3, $4) ON CONFLICT (org_id, digest) DO NOTHING";
const SBOMS_KEPT: &str = "SELECT digest FROM artifact_sboms WHERE org_id = $1 AND digest = ANY($2)";
const SBOM: &str = "SELECT content FROM artifact_sboms WHERE org_id = $1 AND digest = $2";
const INSERT_EXCEPTION: &str = "INSERT INTO vulnerability_exceptions \
     (id, org_id, project_id, vulnerability, reason, owner, created_by, created_at, expires_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)";
const EXCEPTIONS: &str = "SELECT id, project_id, vulnerability, reason, owner, created_by, created_at, \
     expires_at, revoked_at, revoked_by FROM vulnerability_exceptions \
     WHERE org_id = $1 AND ($2 OR (revoked_at IS NULL AND expires_at > $3)) \
     ORDER BY created_at DESC LIMIT 500";
const REVOKE_EXCEPTION: &str = "UPDATE vulnerability_exceptions SET revoked_at = $3, revoked_by = $4 \
     WHERE id = $1 AND org_id = $2 AND revoked_at IS NULL";
const EXCEPTED: &str = "SELECT DISTINCT vulnerability FROM vulnerability_exceptions \
     WHERE org_id = $1 AND (project_id IS NULL OR project_id = $2) \
       AND revoked_at IS NULL AND expires_at > $3";
const RELEASE_DIGESTS: &str = "SELECT artifacts::text FROM releases WHERE id = $1 AND org_id = $2";
/// The images live targets run (their newest successful run's release)
/// whose newest scan is older than `$2`.
const RESCAN_DUE: &str = "SELECT x.repository, x.digest FROM ( \
       SELECT DISTINCT rel.source ->> 'image_repository' AS repository, a.value AS digest \
       FROM application_targets t \
       JOIN LATERAL (SELECT d.release_id FROM deployment_runs d \
         WHERE d.target_id = t.id AND d.org_id = t.org_id AND d.phase = 'succeeded' \
         ORDER BY d.generation DESC LIMIT 1) r ON TRUE \
       JOIN releases rel ON rel.id = r.release_id AND rel.org_id = t.org_id \
       CROSS JOIN LATERAL jsonb_each_text(rel.artifacts) AS a (key, value) \
       WHERE t.org_id = $1 AND NOT t.deleting AND rel.source ->> 'image_repository' IS NOT NULL) x \
     WHERE NOT EXISTS (SELECT 1 FROM artifact_scans s \
       WHERE s.org_id = $1 AND s.digest = x.digest AND s.scanned_at > $2) \
     ORDER BY x.digest LIMIT $3";
const TARGET_RELEASE: &str = "SELECT t.project_id, r.artifacts::text FROM application_targets t \
     JOIN releases r ON r.id = $2 AND r.org_id = t.org_id AND r.application_id = t.application_id \
     WHERE t.id = $1 AND t.org_id = $3";

/// A scan to record.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NewScan {
    pub repository: String,
    pub digest: String,
    pub summary: ScanSummary,
    pub detail: Option<String>,
    pub build_attempt: Option<BuildAttemptId>,
}

/// An exception to grant.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NewException {
    /// Every project when `None`.
    pub project: Option<ProjectId>,
    pub vulnerability: String,
    pub reason: String,
    pub owner: String,
    pub created_by: String,
    pub expires_at: i64,
}

/// A granted exception.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct VulnException {
    pub id: Uuid,
    pub project: Option<ProjectId>,
    pub vulnerability: String,
    pub reason: String,
    pub owner: String,
    pub created_by: String,
    pub created_at: i64,
    pub expires_at: i64,
    pub revoked_at: Option<i64>,
    pub revoked_by: Option<String>,
}

#[derive(sqlx::FromRow)]
struct ScanRow {
    digest: String,
    status: String,
    scanner: String,
    db_updated_at: Option<i64>,
    critical: i32,
    high: i32,
    medium: i32,
    low: i32,
    unknown: i32,
    findings: Vec<String>,
    scanned_at: i64,
}

#[derive(sqlx::FromRow)]
struct ExceptionRow {
    id: Uuid,
    project_id: Option<Uuid>,
    vulnerability: String,
    reason: String,
    owner: String,
    created_by: String,
    created_at: i64,
    expires_at: i64,
    revoked_at: Option<i64>,
    revoked_by: Option<String>,
}

fn count(n: i32) -> Result<u32, sqlx::Error> {
    u32::try_from(n).map_err(|e| sqlx::Error::Decode(e.into()))
}

fn signed(n: u32) -> Result<i32, sqlx::Error> {
    i32::try_from(n).map_err(|e| sqlx::Error::Encode(e.into()))
}

impl ScanRow {
    fn into_summary(self) -> Result<(String, ScanSummary), sqlx::Error> {
        let status = match self.status.as_str() {
            "ok" => ScanStatus::Ok,
            "unavailable" => ScanStatus::Unavailable,
            other => {
                return Err(sqlx::Error::Decode(
                    format!("unknown scan status {other:?}").into(),
                ));
            }
        };
        let findings = self
            .findings
            .iter()
            .filter_map(|f| {
                let (severity, id) = f.split_once(':')?;
                Some((Severity::parse(severity)?, id.to_owned()))
            })
            .collect();
        Ok((
            self.digest,
            ScanSummary {
                status,
                scanner: self.scanner,
                db_updated_at: self.db_updated_at,
                counts: Counts {
                    critical: count(self.critical)?,
                    high: count(self.high)?,
                    medium: count(self.medium)?,
                    low: count(self.low)?,
                    unknown: count(self.unknown)?,
                },
                findings,
                scanned_at: self.scanned_at,
            },
        ))
    }
}

impl From<ExceptionRow> for VulnException {
    fn from(r: ExceptionRow) -> Self {
        Self {
            id: r.id,
            project: r.project_id.map(ProjectId::from_uuid),
            vulnerability: r.vulnerability,
            reason: r.reason,
            owner: r.owner,
            created_by: r.created_by,
            created_at: r.created_at,
            expires_at: r.expires_at,
            revoked_at: r.revoked_at,
            revoked_by: r.revoked_by,
        }
    }
}

/// The distinct digests of a release's `artifacts` (process → digest).
fn digests_of(artifacts: &str) -> Result<Vec<String>, sqlx::Error> {
    let artifacts: BTreeMap<String, String> =
        serde_json::from_str(artifacts).map_err(|e| sqlx::Error::Decode(e.into()))?;
    Ok(artifacts
        .into_values()
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect())
}

impl Tenant {
    /// Record a scan.
    pub async fn record_scan(&mut self, scan: &NewScan) -> Result<Uuid, StoreError> {
        let id = Uuid::now_v7();
        let s = &scan.summary;
        let findings: Vec<String> = s
            .findings
            .iter()
            .take(kuben_core::scan::MAX_LISTED)
            .map(|(severity, id)| format!("{}:{id}", severity.as_str()))
            .collect();
        sqlx::query(INSERT_SCAN)
            .bind(id)
            .bind(self.org.to_string())
            .bind(&scan.repository)
            .bind(&scan.digest)
            .bind(s.status.as_str())
            .bind(&s.scanner)
            .bind(s.db_updated_at)
            .bind(signed(s.counts.critical)?)
            .bind(signed(s.counts.high)?)
            .bind(signed(s.counts.medium)?)
            .bind(signed(s.counts.low)?)
            .bind(signed(s.counts.unknown)?)
            .bind(findings)
            .bind(scan.detail.as_deref())
            .bind(scan.build_attempt.map(|a| *a.as_uuid()))
            .bind(s.scanned_at)
            .execute(&mut *self.tx)
            .await?;
        Ok(id)
    }

    /// The newest scan of each of `digests` that has one.
    pub async fn latest_scans(
        &mut self,
        digests: &[String],
    ) -> Result<BTreeMap<String, ScanSummary>, StoreError> {
        let rows: Vec<ScanRow> = sqlx::query_as(LATEST_SCANS)
            .bind(self.org.to_string())
            .bind(digests)
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(ScanRow::into_summary)
            .collect::<Result<_, _>>()?)
    }

    /// The newest `limit` scans of `digest`.
    pub async fn scan_history(&mut self, digest: &str, limit: i64) -> Result<Vec<ScanSummary>, StoreError> {
        let rows: Vec<ScanRow> = sqlx::query_as(SCAN_HISTORY)
            .bind(self.org.to_string())
            .bind(digest)
            .bind(limit)
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(|r| r.into_summary().map(|(_, s)| s))
            .collect::<Result<_, _>>()?)
    }

    /// Keep the gzip `content` of `digest`'s SBOM, unless one is kept already.
    pub async fn put_sbom(&mut self, digest: &str, content: &[u8]) -> Result<bool, StoreError> {
        let rows = sqlx::query(INSERT_SBOM)
            .bind(self.org.to_string())
            .bind(digest)
            .bind(content)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Which of `digests` have an SBOM.
    pub async fn sboms_kept(&mut self, digests: &[String]) -> Result<BTreeSet<String>, StoreError> {
        let kept: Vec<String> = sqlx::query_scalar(SBOMS_KEPT)
            .bind(self.org.to_string())
            .bind(digests)
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(kept.into_iter().collect())
    }

    /// The gzip SBOM of `digest`.
    pub async fn sbom(&mut self, digest: &str) -> Result<Option<Vec<u8>>, StoreError> {
        Ok(sqlx::query_scalar(SBOM)
            .bind(self.org.to_string())
            .bind(digest)
            .fetch_optional(&mut *self.tx)
            .await?)
    }

    /// Grant an exception.
    pub async fn create_exception(&mut self, e: &NewException) -> Result<Uuid, StoreError> {
        let id = Uuid::now_v7();
        sqlx::query(INSERT_EXCEPTION)
            .bind(id)
            .bind(self.org.to_string())
            .bind(e.project.map(|p| *p.as_uuid()))
            .bind(&e.vulnerability)
            .bind(&e.reason)
            .bind(&e.owner)
            .bind(&e.created_by)
            .bind(now_ms())
            .bind(e.expires_at)
            .execute(&mut *self.tx)
            .await?;
        Ok(id)
    }

    /// The exceptions in force, or every one when `all`.
    pub async fn exceptions(&mut self, all: bool) -> Result<Vec<VulnException>, StoreError> {
        let rows: Vec<ExceptionRow> = sqlx::query_as(EXCEPTIONS)
            .bind(self.org.to_string())
            .bind(all)
            .bind(now_ms())
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows.into_iter().map(VulnException::from).collect())
    }

    /// Revoke exception `id`. False when there is no such unrevoked one.
    pub async fn revoke_exception(&mut self, id: Uuid, by: &str) -> Result<bool, StoreError> {
        let rows = sqlx::query(REVOKE_EXCEPTION)
            .bind(id)
            .bind(self.org.to_string())
            .bind(now_ms())
            .bind(by)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Up to `limit` `(repository, digest)` pairs of running images whose
    /// newest scan is older than `before` (Unix milliseconds).
    pub async fn rescan_due(&mut self, before: i64, limit: i64) -> Result<Vec<(String, String)>, StoreError> {
        Ok(sqlx::query_as(RESCAN_DUE)
            .bind(self.org.to_string())
            .bind(before)
            .bind(limit)
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// The distinct digests of `release`, if the organization has it.
    pub async fn release_digests(&mut self, release: ReleaseId) -> Result<Option<Vec<String>>, StoreError> {
        let artifacts: Option<String> = sqlx::query_scalar(RELEASE_DIGESTS)
            .bind(*release.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        artifacts.map(|a| digests_of(&a)).transpose().map_err(Into::into)
    }

    /// What the scan gate of `target`'s environment says about `release` at
    /// `now`. `None` when the target or release is not the organization's.
    pub async fn scan_verdict(
        &mut self,
        target: TargetId,
        release: ReleaseId,
        now: i64,
    ) -> Result<Option<GateVerdict>, StoreError> {
        let org = self.org.to_string();
        let row: Option<(Uuid, String)> = sqlx::query_as(TARGET_RELEASE)
            .bind(*target.as_uuid())
            .bind(*release.as_uuid())
            .bind(&org)
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some((project, artifacts)) = row else {
            return Ok(None);
        };
        let Some(policy) = self.policy_of_target(target).await? else {
            return Ok(Some(GateVerdict::Pass));
        };
        let digests = digests_of(&artifacts)?;
        let mut latest = self.latest_scans(&digests).await?;
        let excepted: BTreeSet<String> = sqlx::query_scalar::<_, String>(EXCEPTED)
            .bind(&org)
            .bind(project)
            .bind(now)
            .fetch_all(&mut *self.tx)
            .await?
            .into_iter()
            .collect();
        let scans: Vec<(String, Option<ScanSummary>)> = digests
            .into_iter()
            .map(|d| {
                let scan = latest.remove(&d);
                (d, scan)
            })
            .collect();
        Ok(Some(evaluate(&policy.policy.scan, &scans, &excepted, now)))
    }
}

#[cfg(test)]
mod tests {
    use kuben_core::{
        ids::OrgId,
        ops::Generation,
        policy::EnvironmentPolicy,
        scan::{ScanGate, Severity},
    };
    use serde_json::json;

    use super::*;
    use crate::{
        Store,
        repo::{NewAudit, PortableRelease, RunReason, StartDeployment, Started},
        testing::{pg_store, skip},
    };

    const DIGEST: &str = "sha256:1111111111111111111111111111111111111111111111111111111111111111";
    const NOW: i64 = 1_800_000_000_000;

    struct Fixture {
        org: OrgId,
        project: ProjectId,
        target: TargetId,
        release: ReleaseId,
    }

    async fn fixture(store: &Store, gate: ScanGate) -> Fixture {
        let org = store.create_org("a", "A").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let environment = t
            .create_environment(project, "prod", "Prod", true)
            .await
            .expect("environment");
        let policy = EnvironmentPolicy {
            scan: gate,
            ..EnvironmentPolicy::open()
        };
        t.set_environment_policy(project, environment, &policy, "user:a")
            .await
            .expect("policy")
            .expect("environment");
        let cluster = t.create_cluster("eu-1").await.expect("cluster");
        let placement = t
            .create_placement(project, environment, cluster, "a-shop")
            .await
            .expect("placement");
        let application = t.create_application(project, "web", "Web").await.expect("app");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        let (release, _) = t
            .create_release(
                project,
                &PortableRelease {
                    application,
                    artifacts: [("web".to_owned(), DIGEST.parse().expect("digest"))].into(),
                    process_contract: json!({}),
                    portable_config: json!({}),
                    renderer_schema: 1,
                    source: Some(json!({ "image_repository": "registry.local/acme/web" })),
                    created_by: "user:a".into(),
                },
            )
            .await
            .expect("release");
        t.commit().await.expect("commit");
        Fixture {
            org,
            project,
            target,
            release,
        }
    }

    fn scan(critical: u32, ids: &[&str], at: i64) -> NewScan {
        NewScan {
            repository: "registry.local/acme/web".into(),
            digest: DIGEST.into(),
            summary: ScanSummary {
                status: ScanStatus::Ok,
                scanner: "trivy 0.74.0".into(),
                db_updated_at: Some(at - 3_600_000),
                counts: Counts {
                    critical,
                    ..Counts::default()
                },
                findings: ids
                    .iter()
                    .map(|id| (Severity::Critical, (*id).to_owned()))
                    .collect(),
                scanned_at: at,
            },
            detail: None,
            build_attempt: None,
        }
    }

    async fn deploy(store: &Store, f: &Fixture, reason: RunReason) -> Started {
        let mut t = store.tenant(f.org).await.expect("tenant");
        let (revision, _) = t
            .create_config_revision(f.project, f.target, &json!({}), "user:a")
            .await
            .expect("revision")
            .expect("target");
        let state = t.target_state(f.target).await.expect("read").expect("target");
        let req = StartDeployment {
            project: f.project,
            target: f.target,
            release: f.release,
            config_revision: revision,
            render_plan: None,
            expected_generation: state.desired_generation,
            lifecycle_uid: state.lifecycle_uid,
            reason,
            requested_by: "user:a".into(),
            input_hash: format!("{revision}").into_bytes(),
        };
        let started = t
            .start_deployment(&req, NewAudit::default(), None)
            .await
            .expect("start");
        t.commit().await.expect("commit");
        started
    }

    #[tokio::test]
    async fn scans_sboms_and_the_newest_scan_per_digest() {
        let Some(store) = pg_store().await else {
            return skip("scans_sboms_and_the_newest_scan_per_digest");
        };
        let f = fixture(&store, ScanGate::production()).await;
        let mut t = store.tenant(f.org).await.expect("tenant");
        t.record_scan(&scan(2, &["CVE-1", "CVE-2"], NOW - 1000))
            .await
            .expect("scan");
        t.record_scan(&scan(0, &[], NOW)).await.expect("scan");
        let latest = t
            .latest_scans(&[DIGEST.to_owned(), "sha256:22".into()])
            .await
            .expect("read");
        assert_eq!(latest.len(), 1);
        assert_eq!(
            (latest[DIGEST].counts.critical, latest[DIGEST].scanned_at),
            (0, NOW)
        );
        let history = t.scan_history(DIGEST, 10).await.expect("read");
        assert_eq!(history.len(), 2);
        assert_eq!(history[1].findings[0], (Severity::Critical, "CVE-1".to_owned()));
        let gz = [0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3, 1, 2, 3, 4, 5, 6, 7, 8];
        assert!(t.put_sbom(DIGEST, &gz).await.expect("sbom"));
        assert!(!t.put_sbom(DIGEST, &gz).await.expect("sbom"), "kept once");
        assert_eq!(t.sbom(DIGEST).await.expect("read"), Some(gz.to_vec()));
        assert_eq!(
            t.sboms_kept(&[DIGEST.to_owned(), "sha256:33".into()])
                .await
                .expect("read"),
            BTreeSet::from([DIGEST.to_owned()])
        );
        assert_eq!(
            t.release_digests(f.release).await.expect("read"),
            Some(vec![DIGEST.to_owned()])
        );
        t.commit().await.expect("commit");
    }

    #[tokio::test]
    async fn the_gate_refuses_open_findings_until_an_exception_covers_them() {
        let Some(store) = pg_store().await else {
            return skip("the_gate_refuses_open_findings_until_an_exception_covers_them");
        };
        let f = fixture(&store, ScanGate::production()).await;
        assert!(
            matches!(
                deploy(&store, &f, RunReason::Deploy).await,
                Started::Accepted { .. }
            ),
            "no scan yet"
        );
        let mut t = store.tenant(f.org).await.expect("tenant");
        t.record_scan(&scan(1, &["CVE-2026-1"], now_ms()))
            .await
            .expect("scan");
        t.commit().await.expect("commit");
        assert_eq!(
            deploy(&store, &f, RunReason::Deploy).await,
            Started::VulnerabilityBlocked
        );
        assert!(
            matches!(
                deploy(&store, &f, RunReason::Restart).await,
                Started::Accepted { .. }
            ),
            "a restart keeps the running release"
        );

        let mut t = store.tenant(f.org).await.expect("tenant");
        let exception = NewException {
            project: Some(f.project),
            vulnerability: "CVE-2026-1".into(),
            reason: "not reachable".into(),
            owner: "platform".into(),
            created_by: "user:a".into(),
            expires_at: now_ms() + 3_600_000,
        };
        let id = t.create_exception(&exception).await.expect("exception");
        assert_eq!(
            t.scan_verdict(f.target, f.release, now_ms())
                .await
                .expect("verdict"),
            Some(GateVerdict::Pass)
        );
        assert_eq!(t.exceptions(false).await.expect("read").len(), 1);
        assert!(t.revoke_exception(id, "user:a").await.expect("revoke"));
        assert!(!t.revoke_exception(id, "user:a").await.expect("revoke"), "once");
        assert!(t.exceptions(false).await.expect("read").is_empty());
        assert_eq!(
            t.exceptions(true).await.expect("read")[0].revoked_by.as_deref(),
            Some("user:a")
        );
        assert!(matches!(
            t.scan_verdict(f.target, f.release, now_ms())
                .await
                .expect("verdict"),
            Some(GateVerdict::Block(_))
        ));
        assert_eq!(
            t.scan_verdict(f.target, ReleaseId::new(), now_ms())
                .await
                .expect("verdict"),
            None
        );
        t.commit().await.expect("commit");
        let state = {
            let mut t = store.tenant(f.org).await.expect("tenant");
            t.target_state(f.target).await.expect("read").expect("target")
        };
        assert_eq!(
            state.desired_generation,
            Generation(2),
            "refused runs wrote nothing"
        );
    }

    #[tokio::test]
    async fn running_images_are_due_for_a_rescan() {
        let Some(store) = pg_store().await else {
            return skip("running_images_are_due_for_a_rescan");
        };
        let f = fixture(&store, ScanGate::off()).await;
        let Started::Accepted { operation, .. } = deploy(&store, &f, RunReason::Deploy).await else {
            panic!("not accepted");
        };
        let mut t = store.tenant(f.org).await.expect("tenant");
        assert!(
            t.rescan_due(NOW, 10).await.expect("read").is_empty(),
            "nothing succeeded yet"
        );
        sqlx::query("UPDATE deployment_runs SET phase = 'succeeded' WHERE operation_id = $1")
            .bind(*operation.as_uuid())
            .execute(&mut *t.tx)
            .await
            .expect("succeed");
        let due = t.rescan_due(now_ms(), 10).await.expect("read");
        assert_eq!(due, [("registry.local/acme/web".to_owned(), DIGEST.to_owned())]);
        t.record_scan(&scan(0, &[], now_ms())).await.expect("scan");
        assert!(
            t.rescan_due(now_ms() - 60_000, 10)
                .await
                .expect("read")
                .is_empty(),
            "scanned recently"
        );
        t.commit().await.expect("commit");
    }
}
