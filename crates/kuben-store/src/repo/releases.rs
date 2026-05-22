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
