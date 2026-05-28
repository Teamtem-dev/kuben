use kuben_core::{
    ids::{AuditId, OrgId},
    model::AuditEvent,
    time::now_ms,
};

use crate::{
    Store, StoreError,
    db::{with_reader, with_writer},
};

/// Input for an audit record. Append-only: there is no update or delete API.
#[derive(Debug, Default)]
pub struct NewAudit {
    pub org_id: Option<OrgId>,
    pub actor_kind: String,
    pub actor_id: Option<String>,
    pub action: String,
    pub target_kind: Option<String>,
    pub target_ref: Option<String>,
    pub outcome: String,
    pub ip: Option<String>,
    pub request_id: Option<String>,
    pub data: Option<serde_json::Value>,
}

#[derive(Debug, sqlx::FromRow)]
struct AuditRow {
    seq: i64,
    id: String,
    org_id: Option<String>,
    actor_kind: String,
    actor_id: Option<String>,
    action: String,
    target_kind: Option<String>,
    target_ref: Option<String>,
    outcome: String,
    ip: Option<String>,
    request_id: Option<String>,
    data: Option<String>,
    created_at: i64,
}

impl TryFrom<AuditRow> for AuditEvent {
    type Error = StoreError;
    fn try_from(r: AuditRow) -> Result<Self, Self::Error> {
        let decode = |e: uuid::Error| sqlx::Error::Decode(e.into());
        Ok(Self {
            seq: r.seq,
            id: r.id.parse().map_err(decode)?,
            org_id: r.org_id.map(|s| s.parse().map_err(decode)).transpose()?,
            actor_kind: r.actor_kind,
            actor_id: r.actor_id,
            action: r.action,
            target_kind: r.target_kind,
            target_ref: r.target_ref,
            outcome: r.outcome,
            ip: r.ip,
            request_id: r.request_id,
            data: r.data.and_then(|d| serde_json::from_str(&d).ok()),
            created_at: r.created_at,
        })
    }
}

const INSERT_AUDIT: &str = "INSERT INTO audit_events \
     (id, org_id, actor_kind, actor_id, action, target_kind, target_ref, outcome, ip, request_id, data, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)";
const SELECT_RECENT: &str = "SELECT seq, id, org_id, actor_kind, actor_id, action, target_kind, target_ref, outcome, ip, \
     request_id, data, created_at FROM audit_events ORDER BY seq DESC LIMIT $1";

const SELECT_ORG_PAGE: &str = "SELECT seq, id, org_id, actor_kind, actor_id, action, target_kind, target_ref, \
     outcome, ip, request_id, data, created_at FROM audit_events WHERE org_id = $1 AND seq < $2 \
     ORDER BY seq DESC LIMIT $3";

impl Store {
    pub async fn append_audit(&self, a: NewAudit) -> Result<AuditId, StoreError> {
        let id = AuditId::new();
        let data = a.data.as_ref().map(serde_json::Value::to_string);
        with_writer!(self, |pool| {
            sqlx::query(INSERT_AUDIT)
                .bind(id.to_string())
