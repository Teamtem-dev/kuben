//! Audit log reader (scenario 2): newest first, cursor-paginated, limited to
//! the orgs in which the caller holds `audit-read`.

use std::collections::HashMap;

use axum::{
    Json,
    extract::{Query, State},
};
use kuben_core::{Error, authz::ScopeChain, perm::Perm};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};

use crate::{authz::Authz, error::ApiResult, state::ApiState};

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct AuditQuery {
    /// Page size (1–200, default 50).
    pub limit: Option<i64>,
