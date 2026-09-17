//! Private registry logins of an environment (M4.4, ADR-030).
//!
//! A login is a managed secret of kind `registry`: encrypted revisions of a
//! username and password for one registry. Kuben resolves image tags from
//! that registry with it, and every run whose release comes from it pulls
//! with the revision it was accepted with (an immutable
//! `kubernetes.io/dockerconfigjson` Secret). The password is never returned.
//! Revisions and revocation are those of `/secrets/{name}`.

use std::collections::BTreeMap;

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use kuben_core::{Error, perm::Perm};
use kuben_platform::secrets::RegistryLogin;
use kuben_store::repo::{SecretDeleted, SecretKind, SecretSummary};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{
    request, scope,
    secrets::{NewRevision, store_revision},
    validate,
};
use crate::{authz::Authz, error::ApiResult, oci, state::ApiState};

/// Longest username or password accepted.
const MAX_FIELD: usize = 4096;

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct RegistryLoginDto {
    pub name: String,
    /// The registry as image references name it (`ghcr.io`, `docker.io`).
    pub registry: String,
    pub revision: u64,
    /// The current revision is revoked: images from the registry are not
    /// deployed until a new login is set.
    pub revoked: bool,
    pub updated_at: String,
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub struct PutRegistryLogin {
    /// `ghcr.io`, `docker.io`, `registry.example.com:5000`.
    pub registry: String,
    pub username: String,
    /// A password or access token; write-only.
    pub password: String,
}

impl RegistryLoginDto {
    fn of(s: SecretSummary) -> Option<Self> {
        let SecretKind::Registry(registry) = s.kind else {
            return None;
        };
        Some(Self {
            name: s.name,
            registry,
            revision: s.revision,
            revoked: s.revoked,
            updated_at: request::timestamp(s.updated_at),
        })
    }
}

/// The registry name of `given`, normalized as image references carry it.
fn registry_name(given: &str) -> Result<String, Error> {
    let given = given.trim().to_ascii_lowercase();
    let invalid = || Error::Validation(format!("`{given}` is not a registry name such as ghcr.io"));
    if given.is_empty() || given.contains('/') || given.contains('@') {
        return Err(invalid());
    }
    let parsed = oci::parse(&format!("{given}/kuben/probe")).map_err(|_| invalid())?;
    // A first part without a dot, port or `localhost` is a Docker Hub path.
    if parsed.registry != given {
        return Err(invalid());
    }
    Ok(parsed.registry)
}

fn check_login(body: &PutRegistryLogin) -> Result<(), Error> {
    for (field, value) in [("username", &body.username), ("password", &body.password)] {
        if value.is_empty() || value.len() > MAX_FIELD || value.chars().any(char::is_control) {
            return Err(Error::Validation(format!(
                "the {field} must be 1 to {MAX_FIELD} printable characters"
            )));
        }
    }
    if body.username.contains(':') {
        return Err(Error::Validation("the username cannot contain `:`".into()));
    }
    Ok(())
}

/// List the registry logins of an environment (never their passwords).
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/registries", operation_id = "listRegistryLogins",
    tag = "secrets",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    responses((status = 200, body = Vec<RegistryLoginDto>))
)]
pub async fn list(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
) -> ApiResult<Json<Vec<RegistryLoginDto>>> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::SecretRead, &e.chain())?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let logins = tenant.secrets(e.id()).await?;
    Ok(Json(
        logins.into_iter().filter_map(RegistryLoginDto::of).collect(),
    ))
}

/// Set the login of a registry: a new revision, used by the next runs.
#[utoipa::path(
    put,
    path = "/projects/{project}/environments/{environment}/registries/{name}", operation_id = "putRegistryLogin",
    tag = "secrets",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("name" = String, Path, description = "Login name"),
    ),
    request_body = PutRegistryLogin,
    responses(
        (status = 200, body = RegistryLoginDto),
        (status = 403, body = crate::error::Problem),
        (status = 409, description = "The name or registry is taken", body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
        (status = 503, body = crate::error::Problem),
    )
)]
pub async fn put(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, name)): Path<(String, String, String)>,
    Json(body): Json<PutRegistryLogin>,
) -> ApiResult<Json<RegistryLoginDto>> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::SecretWrite, &e.chain())?;
    validate::dns_label("login name", &name, 63)?;
    let registry = registry_name(&body.registry)?;
    check_login(&body)?;
    let values: BTreeMap<String, String> = RegistryLogin {
        username: body.username,
        password: body.password,
    }
    .values();
    let mut tenant = state.store.tenant(e.project.org).await?;
    let new = NewRevision {
        name: &name,
        kind: &SecretKind::Registry(registry),
        values: &values,
    };
    store_revision(&state, &mut tenant, &authz, &e, new).await?;
    let saved = tenant
        .secret(e.id(), &name)
        .await?
        .and_then(RegistryLoginDto::of)
        .ok_or_else(|| Error::Internal("the new registry login is missing".into()))?;
    tenant.commit().await?;
    Ok(Json(saved))
}

/// Delete a registry login. Runs accepted with it keep their revision.
#[utoipa::path(
    delete,
    path = "/projects/{project}/environments/{environment}/registries/{name}", operation_id = "deleteRegistryLogin",
    tag = "secrets",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("name" = String, Path, description = "Login name"),
    ),
    responses(
        (status = 204, description = "Deleted"),
        (status = 404, body = crate::error::Problem),
        (status = 409, description = "An app reads it as a secret", body = crate::error::Problem),
    )
)]
pub async fn delete(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, name)): Path<(String, String, String)>,
) -> ApiResult<StatusCode> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::SecretWrite, &e.chain())?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let not_found = || Error::NotFound(format!("registry login `{name}`"));
    let is_login = tenant
        .secret(e.id(), &name)
        .await?
        .is_some_and(|s| matches!(s.kind, SecretKind::Registry(_)));
    if !is_login {
        return Err(not_found().into());
    }
    let audit = request::audit(
        &authz,
        "secret.deleted",
        "secret",
        format!("{}/{}/{name}", e.project.slug(), e.short_name()),
    );
    match tenant.delete_secret(e.id(), &name, audit).await? {
        SecretDeleted::Done => {
            tenant.commit().await?;
            Ok(StatusCode::NO_CONTENT)
        }
        SecretDeleted::NotFound => Err(not_found().into()),
        SecretDeleted::InUse(apps) => Err(Error::Conflict(format!(
            "registry login `{name}` is read as a secret by {}",
            apps.join(", ")
        ))
        .into()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn registries_are_named_as_image_references_name_them() {
        for (given, expected) in [
            ("ghcr.io", "ghcr.io"),
            (" GHCR.io ", "ghcr.io"),
            ("registry.example.com:5000", "registry.example.com:5000"),
            ("localhost:5000", "localhost:5000"),
            ("docker.io", "docker.io"),
        ] {
            assert_eq!(registry_name(given).expect(given), expected);
        }
        for bad in ["", "ghcr.io/acme", "acme", "ghcr.io@x", "https://ghcr.io"] {
            assert!(registry_name(bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn logins_are_bounded() {
        let login = |username: &str, password: &str| PutRegistryLogin {
            registry: "ghcr.io".into(),
            username: username.into(),
            password: password.into(),
        };
        assert!(check_login(&login("bot", "token")).is_ok());
        assert!(check_login(&login("", "token")).is_err());
        assert!(check_login(&login("bot", "")).is_err());
        assert!(check_login(&login("a:b", "token")).is_err());
        assert!(check_login(&login("bot", "line\nbreak")).is_err());
        assert!(check_login(&login("bot", &"x".repeat(MAX_FIELD + 1))).is_err());
    }
}
