//! Read models of the SQL-backed routes (ADR-032, M1.8b step 2): the live
//! environments and apps of an organization, as the API lists them.
//!
//! Deleted rows are never listed; rows being deleted are, marked. An app is
//! a target: one application on the environment's placement. Its desired
//! state is its newest configuration revision and the release of its newest
//! deployment run.

use kuben_core::{
    ids::{ApplicationId, ConfigRevisionId, EnvironmentId, ProjectId, TargetId},
    ops::Generation,
};
use serde_json::Value;
use uuid::Uuid;

use super::{Tenant, product::counter};
use crate::StoreError;

const ENVIRONMENTS: &str = "SELECT e.id, e.project_id, e.slug, e.name, \
     COALESCE(e.env_type, CASE WHEN e.protected THEN 'production' ELSE 'standard' END) AS env_type, \
     e.quota::text AS quota, \
     e.protected, e.legacy_uid, e.deleting, e.created_at, pl.namespace \
     FROM environments e \
     LEFT JOIN environment_placements pl \
       ON pl.environment_id = e.id AND pl.org_id = e.org_id AND pl.state <> 'retired' \
     WHERE e.org_id = $1 AND e.project_id = $2 AND e.deleted_at IS NULL \
       AND ($3::text IS NULL OR e.slug = $3) \
     ORDER BY e.slug";
const APPS: &str = "SELECT t.id AS target_id, a.id AS application_id, a.slug, a.name, pl.namespace, \
     t.legacy_uid, t.deleting, t.lifecycle_uid, t.desired_generation, t.created_at, \
     c.id AS config_revision_id, c.config::text AS config, \
     COALESCE(r.source ->> 'image', (r.source ->> 'image_repository') || '@' || (r.artifacts ->> 'web')) AS image \
     FROM application_targets t \
     JOIN applications a ON a.id = t.application_id AND a.org_id = t.org_id \
     JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id \
     LEFT JOIN LATERAL (SELECT id, config FROM target_config_revisions \
                        WHERE target_id = t.id ORDER BY revision DESC LIMIT 1) c ON TRUE \
     LEFT JOIN LATERAL (SELECT rel.source, rel.artifacts FROM deployment_runs d \
                        JOIN releases rel ON rel.id = d.release_id \
                        WHERE d.target_id = t.id ORDER BY d.generation DESC LIMIT 1) r ON TRUE \
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
    /// The namespace of its placement, once it has one.
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
    /// The image of the newest run's release, as it was given (a tag the
    /// digest was resolved from), else `repository@digest`.
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
    image: Option<String>,
}

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
            image: self.image,
        })
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
    use std::{collections::BTreeMap, time::Duration};

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
