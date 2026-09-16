//! Read models of the SQL-backed routes (ADR-032, M1.8b step 2): the live
//! environments and apps of an organization, as the API lists them.
//!
//! Deleted rows are never listed; rows being deleted are, marked. An app is
//! a target: one application on the environment's placement. Its desired
//! state is its newest configuration revision and the release of its newest
//! deployment run.

use std::collections::BTreeMap;

use kuben_core::{
    ids::{
        ApplicationId, ConfigRevisionId, DeploymentRunId, EnvironmentId, PlacementId, ProjectId, ReleaseId,
        TargetId,
    },
    ops::{Generation, RunPhase},
};
use serde_json::Value;
use uuid::Uuid;

use super::{Tenant, agents::Delivery, product::counter};
use crate::StoreError;

const ENVIRONMENTS: &str = "SELECT e.id, e.project_id, e.slug, e.name, \
     COALESCE(e.env_type, CASE WHEN e.protected THEN 'production' ELSE 'standard' END) AS env_type, \
     e.quota::text AS quota, \
     e.protected, e.legacy_uid, e.deleting, e.created_at, pl.id AS placement_id, pl.namespace \
     FROM environments e \
     LEFT JOIN environment_placements pl \
       ON pl.environment_id = e.id AND pl.org_id = e.org_id AND pl.state <> 'retired' \
     WHERE e.org_id = $1 AND e.project_id = $2 AND e.deleted_at IS NULL \
       AND ($3::text IS NULL OR e.slug = $3) \
     ORDER BY e.slug";
const APPS: &str = "SELECT t.id AS target_id, a.id AS application_id, a.slug, a.name, pl.namespace, \
     t.legacy_uid, t.deleting, t.lifecycle_uid, t.desired_generation, t.created_at, \
     c.id AS config_revision_id, c.config::text AS config, r.id AS release_id, \
     COALESCE(r.source ->> 'image', (r.source ->> 'image_repository') || '@' || (r.artifacts ->> 'web')) AS image, \
     t.delivery, o.generation AS observed_generation, o.phase AS observed_phase, o.reason AS observed_reason, \
     o.message AS observed_message, o.observed_at, rp.host AS observed_host, rp.tls AS observed_tls \
     FROM application_targets t \
     JOIN applications a ON a.id = t.application_id AND a.org_id = t.org_id \
     JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id \
     LEFT JOIN LATERAL (SELECT id, config FROM target_config_revisions \
                        WHERE target_id = t.id ORDER BY revision DESC LIMIT 1) c ON TRUE \
     LEFT JOIN LATERAL (SELECT rel.id, rel.source, rel.artifacts FROM deployment_runs d \
                        JOIN releases rel ON rel.id = d.release_id \
                        WHERE d.target_id = t.id ORDER BY d.generation DESC LIMIT 1) r ON TRUE \
     LEFT JOIN runtime_observations o ON o.target_id = t.id AND o.org_id = t.org_id \
     LEFT JOIN LATERAL (SELECT jsonb_path_query_first(p.resources, \
                                 '$[*] ? (@.kind == \"HTTPRoute\").spec.hostnames[0]') #>> '{}' AS host, \
                               p.capability_snapshot ->> 'clusterIssuer' IS NOT NULL AS tls \
                        FROM deployment_runs d \
                        JOIN render_plans p ON p.id = d.render_plan_id AND p.org_id = d.org_id \
                        WHERE d.target_id = t.id AND d.generation = o.generation) rp ON TRUE \
     WHERE t.org_id = $1 AND pl.environment_id = $2 AND t.deleted_at IS NULL AND a.deleted_at IS NULL \
       AND ($3::text IS NULL OR a.slug = $3) \
     ORDER BY a.slug";

/// A live environment of a project.
#[derive(Clone, Debug, PartialEq)]
pub struct EnvironmentRecord {
    pub id: EnvironmentId,
    pub project: ProjectId,
    pub slug: String,
    pub name: String,
    /// `standard`, `production` or `preview`.
    pub env_type: String,
    pub quota: Option<Value>,
    pub protected: bool,
    /// Its placement and the placement's namespace, once it has one.
    pub placement: Option<PlacementId>,
    pub namespace: Option<String>,
    pub legacy_uid: Option<Uuid>,
    pub deleting: bool,
    pub created_at: i64,
}

/// A live app: one application on an environment's placement.
#[derive(Clone, Debug, PartialEq)]
pub struct AppRecord {
    pub target: TargetId,
    pub application: ApplicationId,
    pub slug: String,
    pub name: String,
    pub namespace: String,
    pub legacy_uid: Option<Uuid>,
    pub deleting: bool,
    pub lifecycle_uid: Uuid,
    pub desired_generation: Generation,
    pub created_at: i64,
    /// The newest configuration revision: the App spec without its image.
    pub config_revision: Option<ConfigRevisionId>,
    pub config: Option<Value>,
    /// The release of the newest run, and its image as it was given (a tag
    /// the digest was resolved from), else `repository@digest`.
    pub release: Option<ReleaseId>,
    pub image: Option<String>,
    /// How the target's runs reach its cluster.
    pub delivery: Delivery,
    /// What the target's agent last reported, once it reported (M1.9).
    pub runtime: Option<RuntimeStatus>,
}

/// An agent-delivered target as its agent last saw it in the cluster
/// (ADR-027): the observation, and the public URL of the plan it observed.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RuntimeStatus {
    pub generation: Generation,
    /// `accepted`, `applying`, `ready`, `failed`, `rejected` or `unknown`.
    pub phase: String,
    pub reason: Option<String>,
    pub message: Option<String>,
    /// Unix milliseconds.
    pub observed_at: i64,
    /// The first hostname of the observed plan's route, over https when the
    /// plan's cluster issues certificates: what the App controller would
    /// report for the same plan.
    pub url: Option<String>,
}

impl RuntimeStatus {
    /// The observed generation is rolled out and ready.
    #[must_use]
    pub fn ready(&self) -> bool {
        self.phase == "ready"
    }
}

/// One deployment run of an app, as its release history shows it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RunRecord {
    pub run: DeploymentRunId,
    pub generation: Generation,
    /// `deploy`, `rollback` or `promotion`.
    pub reason: String,
    pub phase: RunPhase,
    /// How the run ended, written once: `succeeded`, `failed` or
    /// `cancelled`; `None` while it runs, or when a newer run superseded it
    /// first. A run superseded after it failed keeps `failed`.
    pub outcome: Option<String>,
    pub requested_by: String,
    pub created_at: i64,
    pub release: ReleaseId,
    pub config_revision: ConfigRevisionId,
    /// The release's image as it was given, else `repository@digest`.
    pub image: Option<String>,
}

#[derive(sqlx::FromRow)]
struct EnvironmentRow {
    id: Uuid,
    project_id: Uuid,
    slug: String,
    name: String,
    env_type: String,
    quota: Option<String>,
    protected: bool,
    legacy_uid: Option<Uuid>,
    deleting: bool,
    created_at: i64,
    placement_id: Option<Uuid>,
    namespace: Option<String>,
}

#[derive(sqlx::FromRow)]
struct AppRow {
    target_id: Uuid,
    application_id: Uuid,
    slug: String,
    name: String,
    namespace: String,
    legacy_uid: Option<Uuid>,
    deleting: bool,
    lifecycle_uid: Uuid,
    desired_generation: i64,
    created_at: i64,
    config_revision_id: Option<Uuid>,
    config: Option<String>,
    release_id: Option<Uuid>,
    image: Option<String>,
    delivery: String,
    observed_generation: Option<i64>,
    observed_phase: Option<String>,
    observed_reason: Option<String>,
    observed_message: Option<String>,
    observed_at: Option<i64>,
    observed_host: Option<String>,
    observed_tls: Option<bool>,
}

#[derive(sqlx::FromRow)]
struct RunRow {
    id: Uuid,
    generation: i64,
    reason: String,
    phase: String,
    outcome: Option<String>,
    requested_by: String,
    created_at: i64,
    release_id: Uuid,
    config_revision_id: Uuid,
    image: Option<String>,
}

const RUNS: &str = "SELECT d.id, d.generation, d.reason, d.phase, d.outcome, d.requested_by, d.created_at, \
     d.release_id, d.config_revision_id, \
     COALESCE(rel.source ->> 'image', (rel.source ->> 'image_repository') || '@' || (rel.artifacts ->> 'web')) AS image \
     FROM deployment_runs d JOIN releases rel ON rel.id = d.release_id AND rel.org_id = d.org_id \
     WHERE d.target_id = $1 AND d.org_id = $2 \
     ORDER BY d.generation DESC LIMIT $3";
const RUN_PHASES: &str = "SELECT p.run_id, p.phase, p.entered_at FROM deployment_run_phases p \
     JOIN deployment_runs d ON d.id = p.run_id AND d.org_id = p.org_id \
     WHERE d.target_id = $1 AND p.org_id = $2 AND p.run_id = ANY($3) \
     ORDER BY p.run_id, p.seq";
const CONFIG_REVISION: &str = "SELECT config::text FROM target_config_revisions \
     WHERE id = $1 AND target_id = $2 AND org_id = $3";
const DOMAINS: &str = "SELECT pl.namespace, a.slug, d.value ->> 'host' AS host \
     FROM application_targets t \
     JOIN applications a ON a.id = t.application_id AND a.org_id = t.org_id \
     JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id \
     JOIN LATERAL (SELECT config FROM target_config_revisions \
                   WHERE target_id = t.id ORDER BY revision DESC LIMIT 1) c ON TRUE \
     CROSS JOIN LATERAL jsonb_array_elements(COALESCE(c.config -> 'domains', '[]'::jsonb)) AS d(value) \
     WHERE t.org_id = $1 AND t.deleted_at IS NULL";
const APPLICATION: &str = "SELECT id FROM applications \
     WHERE org_id = $1 AND project_id = $2 AND slug = $3 AND deleted_at IS NULL";

fn json(text: Option<String>) -> Result<Option<Value>, sqlx::Error> {
    text.map(|t| serde_json::from_str(&t).map_err(|e| sqlx::Error::Decode(e.into())))
        .transpose()
}

impl EnvironmentRow {
    fn into_record(self) -> Result<EnvironmentRecord, sqlx::Error> {
        Ok(EnvironmentRecord {
            id: EnvironmentId::from_uuid(self.id),
            project: ProjectId::from_uuid(self.project_id),
            slug: self.slug,
            name: self.name,
            env_type: self.env_type,
            quota: json(self.quota)?,
            protected: self.protected,
            placement: self.placement_id.map(PlacementId::from_uuid),
            namespace: self.namespace,
            legacy_uid: self.legacy_uid,
            deleting: self.deleting,
            created_at: self.created_at,
        })
    }
}

impl AppRow {
    fn into_record(self) -> Result<AppRecord, sqlx::Error> {
        Ok(AppRecord {
            target: TargetId::from_uuid(self.target_id),
            application: ApplicationId::from_uuid(self.application_id),
            slug: self.slug,
            name: self.name,
            namespace: self.namespace,
            legacy_uid: self.legacy_uid,
            deleting: self.deleting,
            lifecycle_uid: self.lifecycle_uid,
            desired_generation: Generation(counter(self.desired_generation)?),
            created_at: self.created_at,
            config_revision: self.config_revision_id.map(ConfigRevisionId::from_uuid),
            config: json(self.config)?,
            release: self.release_id.map(ReleaseId::from_uuid),
            image: self.image,
            delivery: Delivery::parse(&self.delivery)
                .ok_or_else(|| sqlx::Error::Decode(format!("unknown delivery {:?}", self.delivery).into()))?,
            runtime: match (self.observed_generation, self.observed_phase, self.observed_at) {
                (Some(generation), Some(phase), Some(observed_at)) => Some(RuntimeStatus {
                    generation: Generation(counter(generation)?),
                    phase,
                    reason: self.observed_reason,
                    message: self.observed_message,
                    observed_at,
                    url: self.observed_host.map(|host| {
                        let scheme = if self.observed_tls == Some(true) {
                            "https"
                        } else {
                            "http"
                        };
                        format!("{scheme}://{host}")
                    }),
                }),
                _ => None,
            },
        })
    }
}

impl RunRow {
    fn into_record(self) -> Result<RunRecord, sqlx::Error> {
        Ok(RunRecord {
            run: DeploymentRunId::from_uuid(self.id),
            generation: Generation(counter(self.generation)?),
            phase: RunPhase::parse(&self.phase)
                .ok_or_else(|| sqlx::Error::Decode(format!("unknown run phase {:?}", self.phase).into()))?,
            reason: self.reason,
            outcome: self.outcome,
            requested_by: self.requested_by,
            created_at: self.created_at,
            release: ReleaseId::from_uuid(self.release_id),
            config_revision: ConfigRevisionId::from_uuid(self.config_revision_id),
            image: self.image,
        })
    }
}

impl Tenant {
    /// The newest `limit` deployment runs of `target`, newest first.
    pub async fn runs(&mut self, target: TargetId, limit: i64) -> Result<Vec<RunRecord>, StoreError> {
        let rows: Vec<RunRow> = sqlx::query_as(RUNS)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .bind(limit)
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(RunRow::into_record)
            .collect::<Result<_, _>>()?)
    }

    /// The phases each of `runs` of `target` entered, in order, with the
    /// time (Unix milliseconds) it entered them.
    pub async fn run_phases(
        &mut self,
        target: TargetId,
        runs: &[DeploymentRunId],
    ) -> Result<BTreeMap<DeploymentRunId, Vec<(String, i64)>>, StoreError> {
        let ids: Vec<Uuid> = runs.iter().map(|r| *r.as_uuid()).collect();
        let rows: Vec<(Uuid, String, i64)> = sqlx::query_as(RUN_PHASES)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .bind(&ids)
            .fetch_all(&mut *self.tx)
            .await?;
        let mut out: BTreeMap<DeploymentRunId, Vec<(String, i64)>> = BTreeMap::new();
        for (run, phase, at) in rows {
            out.entry(DeploymentRunId::from_uuid(run))
                .or_default()
                .push((phase, at));
        }
        Ok(out)
    }

    /// The content of configuration revision `revision` of `target`.
    pub async fn config_revision(
        &mut self,
        target: TargetId,
        revision: ConfigRevisionId,
    ) -> Result<Option<Value>, StoreError> {
        let text: Option<String> = sqlx::query_scalar(CONFIG_REVISION)
            .bind(*revision.as_uuid())
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(json(text)?)
    }

    /// The hostnames of every live app of the organization, from its newest
    /// configuration: `(namespace, app, host)`.
    pub async fn domains(&mut self) -> Result<Vec<(String, String, String)>, StoreError> {
        Ok(sqlx::query_as(DOMAINS)
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// The live application `slug` of `project`: every environment's app of
    /// that name belongs to it.
    pub async fn application(
        &mut self,
        project: ProjectId,
        slug: &str,
    ) -> Result<Option<ApplicationId>, StoreError> {
        let id: Option<Uuid> = sqlx::query_scalar(APPLICATION)
            .bind(self.org.to_string())
            .bind(*project.as_uuid())
            .bind(slug)
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(id.map(ApplicationId::from_uuid))
    }
}

impl Tenant {
    async fn environment_rows(
        &mut self,
        project: ProjectId,
        slug: Option<&str>,
    ) -> Result<Vec<EnvironmentRecord>, StoreError> {
        let rows: Vec<EnvironmentRow> = sqlx::query_as(ENVIRONMENTS)
            .bind(self.org.to_string())
            .bind(*project.as_uuid())
            .bind(slug)
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(EnvironmentRow::into_record)
            .collect::<Result<_, _>>()?)
    }

    /// The live environments of `project`, ordered by slug.
    pub async fn environments(&mut self, project: ProjectId) -> Result<Vec<EnvironmentRecord>, StoreError> {
        self.environment_rows(project, None).await
    }

    /// The live environment `slug` of `project`.
    pub async fn environment(
        &mut self,
        project: ProjectId,
        slug: &str,
    ) -> Result<Option<EnvironmentRecord>, StoreError> {
        Ok(self.environment_rows(project, Some(slug)).await?.pop())
    }

    async fn app_rows(
        &mut self,
        environment: EnvironmentId,
        slug: Option<&str>,
    ) -> Result<Vec<AppRecord>, StoreError> {
        let rows: Vec<AppRow> = sqlx::query_as(APPS)
            .bind(self.org.to_string())
            .bind(*environment.as_uuid())
            .bind(slug)
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(AppRow::into_record)
            .collect::<Result<_, _>>()?)
    }

    /// The live apps of `environment`, ordered by slug.
    pub async fn apps(&mut self, environment: EnvironmentId) -> Result<Vec<AppRecord>, StoreError> {
        self.app_rows(environment, None).await
    }

    /// The live app `slug` of `environment`.
    pub async fn app(
        &mut self,
        environment: EnvironmentId,
        slug: &str,
    ) -> Result<Option<AppRecord>, StoreError> {
        Ok(self.app_rows(environment, Some(slug)).await?.pop())
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use kuben_core::ids::OrgId;
    use serde_json::json;

    use super::*;
    use crate::{
        Store,
        repo::{
            EnvironmentKind, IdempotencyKey, NewAudit, PortableRelease, RunReason, StartDeployment, Started,
        },
        testing::{pg_store, skip},
    };

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    struct Shop {
        org: OrgId,
        project: ProjectId,
        environment: EnvironmentId,
        application: ApplicationId,
        target: TargetId,
    }

    async fn shop(store: &Store) -> Shop {
        let org = store.create_org("a", "A").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t
            .create_project_described("shop", "Shop", Some("The online shop"))
            .await
            .expect("project");
        let environment = t
            .create_environment_typed(
                project,
                "prod",
                "Prod",
                EnvironmentKind::Production,
                Some(&json!({ "cpu": "4" })),
            )
            .await
            .expect("environment");
        let cluster = t.ensure_cluster("primary").await.expect("cluster");
        assert_eq!(t.ensure_cluster("primary").await.expect("again"), cluster);
        let placement = t
            .create_placement(project, environment, cluster, "kb-shop-prod")
            .await
            .expect("placement");
        let application = t.create_application(project, "web", "Web").await.expect("app");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        t.commit().await.expect("commit");
        Shop {
            org,
            project,
            environment,
            application,
            target,
        }
    }

    /// A revision and a deployed release of `nginx:1.27`, resolved to [`DIGEST`].
    async fn deploy(store: &Store, s: &Shop) {
        let mut t = store.tenant(s.org).await.expect("tenant");
        let (release, _) = t
            .create_release(
                s.project,
                &PortableRelease {
                    application: s.application,
                    artifacts: BTreeMap::from([("web".to_owned(), DIGEST.parse().expect("digest"))]),
                    process_contract: json!({}),
                    portable_config: json!({}),
                    renderer_schema: 1,
                    source: Some(
                        json!({ "image_repository": "docker.io/library/nginx", "image": "nginx:1.27" }),
                    ),
                    created_by: "user:alice".into(),
                },
            )
            .await
            .expect("release");
        let config = json!({ "runtime": { "processes": { "web": { "port": 80 } } } });
        let (revision, _) = t
            .create_config_revision(s.project, s.target, &config, "user:alice")
            .await
            .expect("revision")
            .expect("target");
        let lifecycle_uid = t
            .target_state(s.target)
            .await
            .expect("read")
            .expect("target")
            .lifecycle_uid;
        let started = t
            .start_deployment(
                &StartDeployment {
                    project: s.project,
                    target: s.target,
                    release,
                    config_revision: revision,
                    render_plan: None,
                    expected_generation: Generation(0),
                    lifecycle_uid,
                    reason: RunReason::Deploy,
                    requested_by: "user:alice".into(),
                    input_hash: b"deploy".to_vec(),
                },
                NewAudit {
                    actor_kind: "user".into(),
                    action: "createApp".into(),
                    outcome: "accepted".into(),
                    ..NewAudit::default()
                },
                Some(&IdempotencyKey {
                    actor: "user:alice".into(),
                    key: "k".into(),
                    ttl: Duration::from_hours(1),
                }),
            )
            .await
            .expect("start");
        assert!(matches!(started, Started::Accepted { .. }));
        t.commit().await.expect("commit");
    }

    #[tokio::test]
    async fn lists_what_the_api_shows() {
        let Some(store) = pg_store().await else {
            skip("catalog");
            return;
        };
        let s = shop(&store).await;
        let mut t = store.tenant(s.org).await.expect("tenant");
        let projects = t.projects().await.expect("projects");
        assert_eq!(projects.len(), 1);
        assert_eq!(projects[0].description.as_deref(), Some("The online shop"));
        assert_eq!(projects[0].environments, 1);
        assert_eq!(
            t.project("shop").await.expect("read").map(|p| p.id),
            Some(s.project)
        );
        assert_eq!(t.project("nope").await.expect("read"), None);

        let envs = t.environments(s.project).await.expect("environments");
        assert_eq!(envs.len(), 1);
        assert_eq!(
            (envs[0].env_type.as_str(), envs[0].protected),
            ("production", true)
        );
        assert_eq!(envs[0].namespace.as_deref(), Some("kb-shop-prod"));
        assert_eq!(envs[0].quota, Some(json!({ "cpu": "4" })));

        let app = t.app(s.environment, "web").await.expect("read").expect("app");
        assert_eq!(
            (app.config.clone(), app.image.clone()),
            (None, None),
            "nothing deployed yet"
        );
        drop(t);

        deploy(&store, &s).await;
        let mut t = store.tenant(s.org).await.expect("tenant");
        let apps = t.apps(s.environment).await.expect("apps");
        assert_eq!(apps.len(), 1);
        assert_eq!(apps[0].target, s.target);
        assert_eq!(apps[0].namespace, "kb-shop-prod");
        assert_eq!(apps[0].desired_generation, Generation(1));
        assert_eq!(apps[0].image.as_deref(), Some("nginx:1.27"), "the image as given");
        assert_eq!(
            apps[0].config.as_ref().expect("config")["runtime"]["processes"]["web"]["port"],
            80
        );

        // M2.16: every phase a run enters is kept, in order.
        let run = t.runs(s.target, 1).await.expect("runs")[0].run;
        let first = t.run_phases(s.target, &[run]).await.expect("phases")[&run].clone();
        assert!(!first.is_empty(), "the phase the run started in");
        for phase in ["applying", "verifying", "succeeded"] {
            sqlx::query("UPDATE deployment_runs SET phase = $2, updated_at = updated_at + 1 WHERE id = $1")
                .bind(*run.as_uuid())
                .bind(phase)
                .execute(&mut *t.tx)
                .await
                .expect("advance");
        }
        let timeline = t.run_phases(s.target, &[run]).await.expect("phases")[&run].clone();
        let phases: Vec<&str> = timeline.iter().map(|(p, _)| p.as_str()).collect();
        assert_eq!(
            &phases[phases.len() - 3..],
            ["applying", "verifying", "succeeded"]
        );
        assert!(timeline.windows(2).all(|w| w[0].1 <= w[1].1), "{timeline:?}");
        assert!(
            sqlx::query("UPDATE deployment_run_phases SET phase = 'failed' WHERE run_id = $1")
                .bind(*run.as_uuid())
                .execute(&mut *t.tx)
                .await
                .is_err(),
            "the history is never rewritten"
        );
    }

    #[tokio::test]
    async fn deleted_rows_are_hidden_and_free_their_slug() {
        let Some(store) = pg_store().await else {
            skip("catalog deletion");
            return;
        };
        let s = shop(&store).await;
        let mut t = store.tenant(s.org).await.expect("tenant");
        for sql in [
            "UPDATE application_targets SET deleting = TRUE, deleted_at = 1 WHERE id = $1",
            "UPDATE applications SET deleted_at = 1 WHERE id = (SELECT application_id FROM application_targets WHERE id = $1)",
        ] {
            sqlx::query(sql)
                .bind(*s.target.as_uuid())
                .execute(&mut *t.tx)
                .await
                .expect("delete");
        }
        assert!(t.apps(s.environment).await.expect("apps").is_empty());
        let placement: Uuid =
            sqlx::query_scalar("SELECT placement_id FROM application_targets WHERE id = $1")
                .bind(*s.target.as_uuid())
                .fetch_one(&mut *t.tx)
                .await
                .expect("placement");
        let again = t
            .create_application(s.project, "web", "Web")
            .await
            .expect("the slug is free again");
        t.create_target(s.project, again, crate::repo::product::placement_id(placement))
            .await
            .expect("a new target on the same placement");
        assert_eq!(t.apps(s.environment).await.expect("apps").len(), 1);
        t.commit().await.expect("commit");

        let mut t = store.tenant(s.org).await.expect("tenant");
        assert!(
            sqlx::query("UPDATE application_targets SET deleted_at = NULL WHERE id = $1")
                .bind(*s.target.as_uuid())
                .execute(&mut *t.tx)
                .await
                .is_err(),
            "a deleted row stays deleted"
        );
    }
}
