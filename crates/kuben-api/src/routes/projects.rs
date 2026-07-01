//! Projects: read from the in-memory projection; write through the
//! Kubernetes API (CRD is the source of truth).

use std::collections::BTreeMap;

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use kube::{
    Api,
    api::{DeleteParams, ObjectMeta, PostParams},
};
use kuben_core::{Error, authz::ScopeChain, perm::Perm};
use kuben_crd::{Project, ProjectSpec, labels};
use kuben_platform::projection::ProjectView;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{scope, validate};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

#[derive(Debug, Serialize, ToSchema)]
pub struct ProjectDto {
