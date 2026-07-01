//! Environments of a project. Writes go to the `Environment` CRD (the
//! controller provisions the namespace); reads come from the projection.

use std::collections::BTreeMap;

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use k8s_openapi::apimachinery::pkg::apis::meta::v1::OwnerReference;
use kube::{
    Api,
    api::{DeleteParams, ObjectMeta, PostParams},
};
use kuben_core::{Error, perm::Perm};
use kuben_crd::{DeletionPolicy, Environment, EnvironmentSpec, EnvironmentType, Protection, Quota, labels};
use kuben_platform::{controller::resources::namespace_name, projection::EnvironmentView};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{scope, validate};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

/// Grace period before a deleted production environment is purged.
pub const PRODUCTION_DELETION_GRACE: &str = "168h";

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, ToSchema)]
#[serde(rename_all = "lowercase")]
pub enum EnvType {
    #[default]
    Standard,
    Production,
    Preview,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct EnvironmentDto {
    /// Short name used in URLs, e.g. `prod`.
    pub name: String,
    /// Kubernetes object name, e.g. `shop-prod`.
    pub resource_name: String,
    pub project: String,
    /// `standard`, `production` or `preview`.
    pub env_type: String,
    pub namespace: String,
    /// `Pending`, `Ready`, `Terminating` or `Degraded`.
    pub phase: Option<String>,
    pub ready: bool,
    pub message: Option<String>,
    pub deleting: bool,
    /// When a soft-deleted environment will be purged.
    pub deletion_scheduled_at: Option<String>,
    pub created_at: Option<String>,
}

impl EnvironmentDto {
    #[must_use]
    pub fn from_view(v: &EnvironmentView) -> Self {
        Self {
            name: scope::environment_short_name(&v.project, &v.name).to_owned(),
            resource_name: v.name.clone(),
            project: v.project.clone(),
            env_type: v.env_type.to_owned(),
            namespace: v.namespace.clone(),
            phase: v.phase.clone(),
            ready: v.ready,
            message: v.message.clone(),
