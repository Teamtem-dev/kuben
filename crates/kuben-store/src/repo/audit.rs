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
