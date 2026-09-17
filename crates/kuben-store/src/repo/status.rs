//! Public status pages (M5.3, migration 0032).

use kuben_core::{
    ids::{EnvironmentId, OrgId, ProjectId},
    time::now_ms,
};
use uuid::Uuid;

use super::Tenant;
use crate::{Store, StoreError};

const PAGE_OF: &str = "SELECT project_id, org_id, slug, title, enabled, environment_ids, updated_by, updated_at \
     FROM status_pages WHERE project_id = $1 AND org_id = $2";
const PAGE_BY_SLUG: &str = "SELECT project_id, org_id, slug, title, enabled, environment_ids, updated_by, updated_at \
     FROM status_pages WHERE slug = $1 AND enabled";
const UPSERT: &str = "INSERT INTO status_pages \
     (project_id, org_id, slug, title, enabled, environment_ids, updated_by, updated_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8) \
     ON CONFLICT (project_id) DO UPDATE SET slug = EXCLUDED.slug, title = EXCLUDED.title, \
       enabled = EXCLUDED.enabled, environment_ids = EXCLUDED.environment_ids, \
       updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at \
     WHERE status_pages.org_id = EXCLUDED.org_id";
const DELETE: &str = "DELETE FROM status_pages WHERE project_id = $1 AND org_id = $2";
/// Incidents of the page's apps: open ones, and those resolved lately.
const INCIDENTS: &str = "SELECT id, target_id, severity, opened_at, resolved_at FROM incidents \
     WHERE org_id = $1 AND target_id = ANY($2) AND (resolved_at IS NULL OR resolved_at >= $3) \
     ORDER BY opened_at DESC LIMIT 50";

/// A project's status page.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct StatusPage {
    pub project: ProjectId,
    pub org: OrgId,
    pub slug: String,
    pub title: String,
    pub enabled: bool,
    pub environments: Vec<EnvironmentId>,
    pub updated_by: String,
    pub updated_at: i64,
}

#[derive(sqlx::FromRow)]
struct PageRow {
    project_id: Uuid,
    org_id: String,
    slug: String,
    title: String,
    enabled: bool,
    environment_ids: Vec<Uuid>,
    updated_by: String,
    updated_at: i64,
}

impl TryFrom<PageRow> for StatusPage {
    type Error = sqlx::Error;

    fn try_from(r: PageRow) -> Result<Self, Self::Error> {
        Ok(Self {
            project: ProjectId::from_uuid(r.project_id),
            org: r
                .org_id
                .parse()
                .map_err(|e: uuid::Error| sqlx::Error::Decode(e.into()))?,
            slug: r.slug,
            title: r.title,
            enabled: r.enabled,
            environments: r
                .environment_ids
                .into_iter()
                .map(EnvironmentId::from_uuid)
                .collect(),
            updated_by: r.updated_by,
            updated_at: r.updated_at,
        })
    }
}

/// An incident as a status page may show it: when, how bad, which app.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct PublicIncident {
    pub id: Uuid,
    pub target_id: Option<Uuid>,
    pub severity: String,
    pub opened_at: i64,
    pub resolved_at: Option<i64>,
}

impl Tenant {
    /// `project`'s status page, if it has one.
    pub async fn status_page(&mut self, project: ProjectId) -> Result<Option<StatusPage>, StoreError> {
        let row: Option<PageRow> = sqlx::query_as(PAGE_OF)
            .bind(*project.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row.map(StatusPage::try_from).transpose()?)
    }

    /// Set `page` (its slug must be free across the installation).
    pub async fn set_status_page(&mut self, page: &StatusPage) -> Result<(), StoreError> {
        let environments: Vec<Uuid> = page.environments.iter().map(|e| *e.as_uuid()).collect();
        sqlx::query(UPSERT)
            .bind(*page.project.as_uuid())
            .bind(self.org.to_string())
            .bind(&page.slug)
            .bind(&page.title)
            .bind(page.enabled)
            .bind(&environments)
            .bind(&page.updated_by)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// Remove `project`'s status page. False when it has none.
    pub async fn delete_status_page(&mut self, project: ProjectId) -> Result<bool, StoreError> {
        let rows = sqlx::query(DELETE)
            .bind(*project.as_uuid())
            .bind(self.org.to_string())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Incidents of `targets` open now or resolved since `since`.
    pub async fn public_incidents(
        &mut self,
        targets: &[Uuid],
        since: i64,
    ) -> Result<Vec<PublicIncident>, StoreError> {
        Ok(sqlx::query_as(INCIDENTS)
            .bind(self.org.to_string())
            .bind(targets)
            .bind(since)
            .fetch_all(&mut *self.tx)
            .await?)
    }
}

impl Store {
    /// The enabled status page published as `slug`.
    pub async fn public_status_page(&self, slug: &str) -> Result<Option<StatusPage>, StoreError> {
        let row: Option<PageRow> = sqlx::query_as(PAGE_BY_SLUG)
            .bind(slug)
            .fetch_optional(self.pool())
            .await?;
        Ok(row.map(StatusPage::try_from).transpose()?)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        repo::NewIncident,
        testing::{pg_store, skip},
    };

    #[tokio::test]
    async fn status_pages_are_found_by_slug_only_when_enabled() {
        let Some(store) = pg_store().await else {
            return skip("status_pages_are_found_by_slug_only_when_enabled");
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let other = store.create_org("b", "B").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let env = t
            .create_environment(project, "prod", "Prod", true)
            .await
            .expect("env");
        let mut page = StatusPage {
            project,
            org,
            slug: "shop-status".into(),
            title: "Shop".into(),
            enabled: true,
            environments: vec![env],
            updated_by: "u".into(),
            updated_at: 0,
        };
        t.set_status_page(&page).await.expect("set");
        let target = Uuid::now_v7();
        let incident = NewIncident {
            project: Some(project),
            environment: Some(env),
            target: None,
            kind: "deployment.failed".into(),
            severity: "critical",
            dedupe_key: "k".into(),
            title: "internal words".into(),
            detail: None,
        };
        t.open_incident(&incident).await.expect("incident");
        assert!(
            t.public_incidents(&[target], 0).await.expect("read").is_empty(),
            "only the page's apps"
        );
        t.commit().await.expect("commit");
        let found = store
            .public_status_page("shop-status")
            .await
            .expect("read")
            .expect("page");
        assert_eq!((found.org, found.environments.clone()), (org, vec![env]));

        let mut t = store.tenant(other).await.expect("tenant");
        let theirs = t.create_project("web", "Web").await.expect("project");
        let env2 = t
            .create_environment(theirs, "prod", "Prod", true)
            .await
            .expect("env");
        let taken = StatusPage {
            project: theirs,
            org: other,
            environments: vec![env2],
            ..page.clone()
        };
        assert!(
            t.set_status_page(&taken)
                .await
                .is_err_and(|e| e.is_unique_violation()),
            "slugs are global"
        );
        drop(t);

        page.enabled = false;
        let mut t = store.tenant(org).await.expect("tenant");
        t.set_status_page(&page).await.expect("set");
        t.commit().await.expect("commit");
        assert!(
            store
                .public_status_page("shop-status")
                .await
                .expect("read")
                .is_none()
        );
        let mut t = store.tenant(org).await.expect("tenant");
        assert!(t.delete_status_page(project).await.expect("delete"));
        assert!(t.status_page(project).await.expect("read").is_none());
    }
}
