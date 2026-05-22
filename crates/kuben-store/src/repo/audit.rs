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
