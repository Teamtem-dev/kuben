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

/// All `/api/v1` REST routes with their OpenAPI metadata.
pub fn api_router() -> OpenApiRouter<ApiState> {
    OpenApiRouter::new()
        .merge(auth::openapi_router())
        .routes(routes!(projects::list, projects::create))
        .routes(routes!(projects::get, projects::delete))
        .routes(routes!(environments::list, environments::create))
        .routes(routes!(environments::get, environments::delete))
        .routes(routes!(apps::crud::list, apps::crud::create))
        .routes(routes!(apps::crud::get, apps::crud::update, apps::crud::delete))
        .routes(routes!(apps::crud::restart))
        .routes(routes!(apps::crud::logs))
        .routes(routes!(apps::releases::releases))
        .routes(routes!(apps::releases::rollback))
        .routes(routes!(apps::jobs::run))
        .routes(routes!(apps::domains::domains))
        .routes(routes!(apps::promote::promote))
        .routes(routes!(secrets::list))
        .routes(routes!(secrets::put, secrets::delete))
        .routes(routes!(templates::list))
        .routes(routes!(templates::deploy))
        .routes(routes!(tokens::list, tokens::create))
        .routes(routes!(tokens::revoke))
        .routes(routes!(members::list, members::invite))
        .routes(routes!(members::update, members::remove))
        .routes(routes!(audit::list))
        .routes(routes!(health::details))
}

/// The complete spec (used by the `openapi` binary, the audit middleware and tests).
#[must_use]
pub fn spec() -> utoipa::openapi::OpenApi {
    let (_router, api) = OpenApiRouter::with_openapi(ApiDoc::openapi())
        .nest("/api/v1", api_router())
        .split_for_parts();
    api
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    #[test]
    fn spec_contains_core_paths() {
        let spec = super::spec();
        let json = serde_json::to_value(&spec).expect("json");
        let paths = json["paths"].as_object().expect("paths");
        let app = "/api/v1/projects/{project}/environments/{environment}/apps/{app}";
        for p in [
            "/api/v1/auth/login",
            "/api/v1/auth/logout",
            "/api/v1/me",
            "/api/v1/me/password",
            "/api/v1/projects",
            "/api/v1/projects/{project}",
            "/api/v1/projects/{project}/environments",
            "/api/v1/projects/{project}/environments/{environment}",
            "/api/v1/projects/{project}/environments/{environment}/apps",
            app,
            &format!("{app}/restart"),
            &format!("{app}/logs"),
            &format!("{app}/releases"),
            &format!("{app}/rollback"),
            &format!("{app}/run"),
            &format!("{app}/domains"),
            &format!("{app}/promote"),
            "/api/v1/projects/{project}/environments/{environment}/secrets",
            "/api/v1/projects/{project}/environments/{environment}/secrets/{secret}",
            "/api/v1/projects/{project}/environments/{environment}/templates/{template}",
            "/api/v1/templates",
            "/api/v1/tokens",
            "/api/v1/tokens/{token}",
            "/api/v1/members",
            "/api/v1/members/{member}",
            "/api/v1/audit",
            "/api/v1/healthz/details",
        ] {
            assert!(paths.contains_key(p), "missing path {p}");
        }
        assert_eq!(json["openapi"], "3.1.0");
    }

    #[test]
    fn operation_ids_are_unique() {
        let json = serde_json::to_value(super::spec()).expect("json");
        let mut seen = BTreeSet::new();
        for item in json["paths"].as_object().expect("paths").values() {
            for op in item.as_object().expect("item").values() {
                if let Some(id) = op["operationId"].as_str() {
                    assert!(seen.insert(id.to_owned()), "duplicate operationId {id}");
                }
            }
        }
    }
}
