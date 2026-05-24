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
