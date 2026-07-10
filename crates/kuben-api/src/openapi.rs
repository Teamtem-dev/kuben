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
        description = "Kuben — Kubernetes-native PaaS control plane.",
        license(name = "Apache-2.0")
    ),
    tags(
        (name = "auth", description = "Sessions, identity and passwords"),
        (name = "projects", description = "Projects"),
        (name = "environments", description = "Environments (one namespace each)"),
        (name = "apps", description = "Apps, rollouts, releases, logs, scheduled runs and promotion"),
        (name = "secrets", description = "Write-only environment secrets"),
        (name = "templates", description = "One-click services and databases"),
        (name = "tokens", description = "Personal API tokens for CI/CD"),
        (name = "members", description = "Organization members and roles"),
        (name = "audit", description = "Audit log of every mutation"),
        (name = "system", description = "Health and diagnostics"),
    ),
    components(schemas(crate::error::Problem))
)]
pub struct ApiDoc;

