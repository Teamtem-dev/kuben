//! Organization members and roles (scenario 4).
//!
//! Rules: nobody grants a role above their own; only owners touch owners;
//! the last owner can be neither demoted nor removed; removing a member
//! revokes their sessions and tokens at once.

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use kuben_core::{
    Error,
    authz::ScopeChain,
    ids::{OrgId, UserId},
    model::Member,
    perm::{Perm, Role},
};
use serde::{Deserialize, Serialize};
use tokio::task::spawn_blocking;
use utoipa::ToSchema;

use super::validate;
use crate::{
    authz::Authz,
    error::{ApiError, ApiResult},
    state::ApiState,
};

fn default_role() -> String {
    "developer".into()
}

#[derive(Debug, Serialize, ToSchema)]
pub struct MemberDto {
    pub id: String,
    pub email: String,
