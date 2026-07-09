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
    pub name: String,
    pub uid: Option<String>,
    pub display_name: String,
    pub description: Option<String>,
    pub org: Option<String>,
    pub environments: u32,
    pub ready: bool,
    pub deleting: bool,
    pub created_at: Option<String>,
}

impl From<&ProjectView> for ProjectDto {
    fn from(p: &ProjectView) -> Self {
        Self {
            name: p.name.clone(),
            uid: p.uid.clone(),
            display_name: p.display_name.clone(),
            description: p.description.clone(),
            org: p.org.clone(),
            environments: p.environments,
            ready: p.ready,
            deleting: p.deleting,
            created_at: p.created_at.clone(),
        }
    }
}
