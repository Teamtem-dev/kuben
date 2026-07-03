//! Secrets of an environment. Values are write-only through the API: they
//! can be set and replaced, never read back. Only Secrets Kuben created are
//! listed or touched (service-account tokens etc. are invisible).

use std::collections::BTreeMap;

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use k8s_openapi::{ByteString, api::core::v1::Secret};
use kube::{
    Api, ResourceExt,
    api::{DeleteParams, ListParams, ObjectMeta, Patch, PatchParams},
};
use kuben_core::{Error, perm::Perm};
use kuben_crd::{FIELD_MANAGER, labels};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{scope, validate};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

