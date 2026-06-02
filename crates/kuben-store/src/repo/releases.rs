//! Release history (scenario 5): every spec change of an App is recorded as a
//! numbered revision so it can be listed, compared and rolled back.

use kuben_core::{ids::OrgId, model::AppRelease, time::now_ms};

use crate::{
    Store, StoreError,
    db::{with_reader, with_writer},
};

/// Input for a release record. `spec` is the App spec, which only ever holds
/// Secret references, never values.
#[derive(Debug)]
pub struct NewRelease {
    pub org_id: Option<OrgId>,
    pub namespace: String,
    pub app: String,
    pub image: Option<String>,
    pub spec: serde_json::Value,
    pub reason: String,
    pub actor_id: Option<String>,
    pub note: Option<String>,
}

#[derive(Debug, sqlx::FromRow)]
struct ReleaseRow {
    id: String,
    revision: i64,
    namespace: String,
    app: String,
    image: Option<String>,
    spec: String,
    reason: String,
    actor_id: Option<String>,
    note: Option<String>,
    created_at: i64,
}

impl TryFrom<ReleaseRow> for AppRelease {
    type Error = StoreError;
    fn try_from(r: ReleaseRow) -> Result<Self, Self::Error> {
        Ok(Self {
            id: r.id,
            revision: r.revision,
            namespace: r.namespace,
            app: r.app,
            image: r.image,
            spec: serde_json::from_str(&r.spec).map_err(|e| sqlx::Error::Decode(e.into()))?,
            reason: r.reason,
            actor_id: r.actor_id,
            note: r.note,
            created_at: r.created_at,
        })
    }
}

const NEXT_REVISION: &str =
    "SELECT COALESCE(MAX(revision), 0) FROM app_releases WHERE namespace = $1 AND app = $2";
const INSERT_RELEASE: &str = "INSERT INTO app_releases \
     (id, org_id, namespace, app, revision, image, spec, reason, actor_id, note, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)";
const SELECT_RELEASES: &str = "SELECT id, revision, namespace, app, image, spec, reason, actor_id, note, created_at \
     FROM app_releases WHERE namespace = $1 AND app = $2 ORDER BY revision DESC LIMIT $3";
const SELECT_RELEASE: &str = "SELECT id, revision, namespace, app, image, spec, reason, actor_id, note, created_at \
     FROM app_releases WHERE namespace = $1 AND app = $2 AND revision = $3";

/// Concurrent writers (Postgres HA) may race for the same revision; the
/// unique index makes the loser retry with the next number.
const MAX_ATTEMPTS: u32 = 3;

impl Store {
    pub async fn record_release(&self, r: NewRelease) -> Result<AppRelease, StoreError> {
        let spec = r.spec.to_string();
        let mut attempt = 0;
        loop {
            attempt += 1;
            let (max,): (i64,) = with_writer!(self, |pool| sqlx::query_as(NEXT_REVISION)
                .bind(&r.namespace)
                .bind(&r.app)
                .fetch_one(pool)
                .await?);
            let release = AppRelease {
                id: uuid::Uuid::now_v7().to_string(),
                revision: max + 1,
                namespace: r.namespace.clone(),
                app: r.app.clone(),
                image: r.image.clone(),
                spec: r.spec.clone(),
                reason: r.reason.clone(),
                actor_id: r.actor_id.clone(),
                note: r.note.clone(),
                created_at: now_ms(),
            };
            let inserted = with_writer!(self, |pool| sqlx::query(INSERT_RELEASE)
                .bind(&release.id)
                .bind(r.org_id.map(|o| o.to_string()))
                .bind(&release.namespace)
                .bind(&release.app)
                .bind(release.revision)
                .bind(&release.image)
                .bind(&spec)
                .bind(&release.reason)
                .bind(&release.actor_id)
                .bind(&release.note)
                .bind(release.created_at)
                .execute(pool)
                .await
                .map(|_| ()));
            match inserted {
                Ok(()) => return Ok(release),
                Err(sqlx::Error::Database(e)) if e.is_unique_violation() && attempt < MAX_ATTEMPTS => {}
                Err(e) => return Err(e.into()),
            }
        }
    }

    /// Newest first.
    pub async fn list_releases(
        &self,
        namespace: &str,
        app: &str,
        limit: i64,
    ) -> Result<Vec<AppRelease>, StoreError> {
        let rows: Vec<ReleaseRow> = with_reader!(self, |pool| sqlx::query_as(SELECT_RELEASES)
            .bind(namespace)
            .bind(app)
            .bind(limit)
            .fetch_all(pool)
            .await?);
        rows.into_iter().map(AppRelease::try_from).collect()
    }

    pub async fn find_release(
        &self,
        namespace: &str,
        app: &str,
        revision: i64,
    ) -> Result<Option<AppRelease>, StoreError> {
        let row: Option<ReleaseRow> = with_reader!(self, |pool| sqlx::query_as(SELECT_RELEASE)
            .bind(namespace)
            .bind(app)
            .bind(revision)
            .fetch_optional(pool)
            .await?);
        row.map(AppRelease::try_from).transpose()
    }
}
