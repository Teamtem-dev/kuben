use kuben_core::{ids::UserId, model::Session, time::now_ms};

use crate::{
    Store, StoreError,
    db::{with_reader, with_writer},
};

/// Input for creating a session. `id_hash` is `sha256(session_id)`; the raw
/// id only ever lives in the browser cookie (Invariant I-3).
#[derive(Debug)]
