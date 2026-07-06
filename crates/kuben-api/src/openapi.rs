//! OpenAPI 3.1 document. The TypeScript client (`packages/api-client`) is
//! generated from this; CI fails on drift (ADR-007). Operation ids are also
//! the audit log's action names (scenario 2), so they must be unique.

use utoipa::OpenApi;
use utoipa_axum::{router::OpenApiRouter, routes};

use crate::{
    auth,
    routes::{apps, audit, environments, health, members, projects, secrets, templates, tokens},
    state::ApiState,
};

#[derive(Debug, OpenApi)]
#[openapi(
    info(
        title = "Kuben API",
        version = "0.1.0",
