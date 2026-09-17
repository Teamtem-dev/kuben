//! Secrets of an environment (M4.4, ADR-030). Values are write-only: they
//! can be set and replaced, never read back.
//!
//! Every change is a new encrypted, immutable revision in SQL. Setting a
//! value rolls it out, by default, with a `rotation` run of every app of the
//! environment that references the secret, under the environment's policy
//! (a production rotation waits for its approval). A revision can be revoked
//! for good; runs bound to it fail instead of delivering it. Secrets Kuben
//! wrote into the cluster before M4.4 are still listed and can be deleted.

use std::collections::BTreeMap;

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use k8s_openapi::api::core::v1::Secret;
use kube::{
    Api, ResourceExt,
    api::{DeleteParams, ListParams},
};
use kuben_core::{Error, perm::Perm};
use kuben_crd::labels;
use kuben_platform::secrets::{Identity, Keyring, RegistryLogin, SECRET_ID};
use kuben_store::repo::{
    AppRecord, Reservation, Reserved, Revoked, RunReason, SecretDeleted, SecretKind, SecretRevisionInfo,
    SecretSummary, StartDeployment, Started, Tenant,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest as _, Sha256};
use utoipa::ToSchema;
use uuid::Uuid;

use super::{
    apps::approvals::ensure_may_deploy,
    request,
    scope::{self, EnvScope},
    validate,
};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

/// Upper bound for the sum of all values of one secret.
const MAX_SECRET_BYTES: usize = 256 * 1024;
/// Upper bound for the sealed JSON object of one revision.
const MAX_SEALED_BYTES: usize = 384 * 1024;
/// Most keys one secret holds.
const MAX_KEYS: usize = 256;

/// Where a secret's values are kept.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, ToSchema)]
#[serde(rename_all = "lowercase")]
pub enum SecretStorage {
    /// Encrypted revisions in Kuben's database.
    Encrypted,
    /// A Secret Kuben wrote into the cluster before encrypted revisions.
    Cluster,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct SecretDto {
    pub name: String,
    /// Key names only; values are never returned.
    pub keys: Vec<String>,
    pub created_at: Option<String>,
    pub storage: SecretStorage,
    /// The current revision of an encrypted secret.
    pub revision: Option<u64>,
    /// The current revision is revoked: set a new value before deploying.
    pub revoked: bool,
    pub updated_at: Option<String>,
    /// The runs that roll a new value out, after a change.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub rollouts: Vec<RolloutDto>,
}

/// One app's rollout of a new secret value.
#[derive(Debug, Serialize, ToSchema)]
pub struct RolloutDto {
    pub app: String,
    /// The rotation run; `None` when the app could not take one now.
    pub run: Option<Uuid>,
    pub approvals_required: u8,
    /// Why there is no run.
    pub skipped: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct SecretRevisionDto {
    pub revision: u64,
    pub keys: Vec<String>,
    pub current: bool,
    pub created_by: String,
    pub created_at: String,
    pub revoked_at: Option<String>,
    pub revoked_by: Option<String>,
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub struct PutSecret {
    /// Key → UTF-8 value. Replaces the whole secret with a new revision.
    pub data: BTreeMap<String, String>,
    /// Roll the new value out to the apps that reference the secret.
    #[serde(default = "rollout_by_default")]
    pub rollout: bool,
}

const fn rollout_by_default() -> bool {
    true
}

impl From<SecretSummary> for SecretDto {
    fn from(s: SecretSummary) -> Self {
        Self {
            name: s.name,
            keys: s.keys,
            created_at: Some(request::timestamp(s.created_at)),
            storage: SecretStorage::Encrypted,
            revision: Some(s.revision),
            revoked: s.revoked,
            updated_at: Some(request::timestamp(s.updated_at)),
            rollouts: Vec::new(),
        }
    }
}

impl From<&Secret> for SecretDto {
    fn from(s: &Secret) -> Self {
        Self {
            name: s.name_any(),
            keys: s
                .data
                .as_ref()
                .map(|d| d.keys().cloned().collect())
                .unwrap_or_default(),
            created_at: s.creation_timestamp().map(|t| t.0.to_string()),
            storage: SecretStorage::Cluster,
            revision: None,
            revoked: false,
            updated_at: None,
            rollouts: Vec::new(),
        }
    }
}

impl SecretRevisionDto {
    fn of(r: SecretRevisionInfo, current: u64) -> Self {
        Self {
            current: r.revision == current,
            revision: r.revision,
            keys: r.keys,
            created_by: r.created_by,
            created_at: request::timestamp(r.created_at),
            revoked_at: r.revoked_at.map(request::timestamp),
            revoked_by: r.revoked_by,
        }
    }
}

/// Secrets Kuben wrote into the cluster before M4.4 (not revision objects).
fn legacy_selector() -> String {
    format!("{},!{SECRET_ID}", labels::MANAGED_SELECTOR)
}

/// Check the keys and sizes of `data`.
fn check_values(data: &BTreeMap<String, String>) -> Result<(), Error> {
    if data.is_empty() {
        return Err(Error::Validation("a secret needs at least one key".into()));
    }
    if data.len() > MAX_KEYS {
        return Err(Error::Validation(format!(
            "a secret holds at most {MAX_KEYS} keys"
        )));
    }
    let mut total = 0usize;
    for (k, v) in data {
        validate::secret_key(k)?;
        total = total.saturating_add(v.len());
    }
    let sealed = serde_json::to_vec(data).map_or(usize::MAX, |j| j.len());
    if total > MAX_SECRET_BYTES || sealed > MAX_SEALED_BYTES {
        return Err(Error::Validation(format!(
            "secret values exceed {MAX_SECRET_BYTES} bytes"
        )));
    }
    Ok(())
}

fn keyring(state: &ApiState) -> Result<&Keyring, Error> {
    state
        .keyring
        .as_deref()
        .ok_or_else(|| Error::Unavailable("secret encryption is not configured".into()))
}

fn reference(e: &EnvScope, secret: &str) -> String {
    format!("{}/{}/{secret}", e.project.slug(), e.short_name())
}

/// List the secrets of an environment (names and keys).
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/secrets", operation_id = "listSecrets",
    tag = "secrets",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    responses((status = 200, body = Vec<SecretDto>))
)]
pub async fn list(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
) -> ApiResult<Json<Vec<SecretDto>>> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::SecretRead, &e.chain())?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let mut out: Vec<SecretDto> = tenant
        .secrets(e.id())
        .await?
        .into_iter()
        .filter(|s| s.kind == SecretKind::Opaque)
        .map(SecretDto::from)
        .collect();
    drop(tenant);
    if let Ok(client) = scope::cluster(&state) {
        let api = Api::<Secret>::namespaced(client, &e.namespace());
        match api.list(&ListParams::default().labels(&legacy_selector())).await {
            Ok(list) => {
                let legacy: Vec<SecretDto> = list
                    .items
                    .iter()
                    .filter(|s| !out.iter().any(|o| o.name == s.name_any()))
                    .map(SecretDto::from)
                    .collect();
                out.extend(legacy);
                out.sort_by(|a, b| a.name.cmp(&b.name));
            }
            Err(err) => tracing::warn!(error = %err, "cluster secrets are not listed"),
        }
    }
    Ok(Json(out))
}

/// Set a secret's values: a new revision, rolled out unless `rollout` is
/// false.
#[utoipa::path(
    put,
    path = "/projects/{project}/environments/{environment}/secrets/{secret}", operation_id = "putSecret",
    tag = "secrets",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("secret" = String, Path, description = "Secret name"),
    ),
    request_body = PutSecret,
    responses(
        (status = 200, body = SecretDto),
        (status = 403, body = crate::error::Problem),
        (status = 409, description = "The environment is being deleted, or the name is a registry login", body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
        (status = 503, body = crate::error::Problem),
    )
)]
pub async fn put(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, secret)): Path<(String, String, String)>,
    Json(body): Json<PutSecret>,
) -> ApiResult<Json<SecretDto>> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::SecretWrite, &e.chain())?;
    validate::dns_label("secret name", &secret, 63)?;
    check_values(&body.data)?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let new = NewRevision {
        name: &secret,
        kind: &SecretKind::Opaque,
        values: &body.data,
    };
    store_revision(&state, &mut tenant, &authz, &e, new).await?;
    let rollouts = if body.rollout {
        rotate(&state, &mut tenant, &authz, &e, &secret).await?
    } else {
        Vec::new()
    };
    let summary = tenant
        .secret(e.id(), &secret)
        .await?
        .ok_or_else(|| Error::Internal("the new secret revision is missing".into()))?;
    tenant.commit().await?;
    Ok(Json(SecretDto {
        rollouts,
        ..SecretDto::from(summary)
    }))
}

/// A new revision of a secret.
pub(crate) struct NewRevision<'a> {
    pub name: &'a str,
    pub kind: &'a SecretKind,
    pub values: &'a BTreeMap<String, String>,
}

/// Seal and record a new revision in `tenant`'s transaction.
pub(crate) async fn store_revision(
    state: &ApiState,
    tenant: &mut Tenant,
    authz: &Authz,
    e: &EnvScope,
    new: NewRevision<'_>,
) -> ApiResult<()> {
    let keyring = keyring(state)?;
    let deleting = || Error::Conflict(format!("environment `{}` is being deleted", e.short_name()));
    if e.deleting() {
        return Err(deleting().into());
    }
    // No secret ever reaches code from outside the repository (M5.1).
    if tenant.untrusted_environment(e.id()).await? {
        return Err(super::apps::untrusted().into());
    }
    let (_, actor) = request::actor(authz);
    let reserved = match tenant
        .reserve_secret_revision(e.project.id(), e.id(), new.name, new.kind, &actor)
        .await?
    {
        Reservation::Reserved(reserved) => reserved,
        Reservation::NoEnvironment => return Err(deleting().into()),
        Reservation::Taken(why) => return Err(Error::Conflict(why).into()),
    };
    let sealed = seal(keyring, e, reserved, new.values)?;
    let keys: Vec<String> = new.values.keys().cloned().collect();
    let audit = request::audit(authz, "secret.revision.created", "secret", reference(e, new.name));
    tenant
        .insert_secret_revision(reserved, &keys, &sealed, &actor, audit)
        .await?;
    Ok(())
}

/// The login of `registry` in `e`, opened.
pub(crate) async fn registry_login(
    state: &ApiState,
    tenant: &mut Tenant,
    e: &EnvScope,
    registry: &str,
) -> ApiResult<Option<RegistryLogin>> {
    if state.keyring.is_none() && tenant.registry_login(e.id(), registry).await?.is_none() {
        return Ok(None);
    }
    Ok(open_registry_login(keyring(state)?, tenant, e.project.org, e.id(), registry).await?)
}

/// The login of `environment` (of `org`) for `registry`, opened.
pub(crate) async fn open_registry_login(
    keyring: &Keyring,
    tenant: &mut Tenant,
    org: kuben_core::ids::OrgId,
    environment: kuben_core::ids::EnvironmentId,
    registry: &str,
) -> Result<Option<RegistryLogin>, Error> {
    let Some(current) = tenant.registry_login(environment, registry).await? else {
        return Ok(None);
    };
    let (org, id) = (org.to_string(), current.secret.to_string());
    let who = Identity {
        org: &org,
        secret: &id,
        revision: current.revision,
    };
    let values = keyring
        .open_values(who, &current.sealed)
        .map_err(|err| Error::Internal(format!("opening a registry login failed: {err}")))?;
    Ok(RegistryLogin::from_values(&values))
}

fn seal(
    keyring: &Keyring,
    e: &EnvScope,
    reserved: Reserved,
    data: &BTreeMap<String, String>,
) -> Result<kuben_store::repo::SealedBytes, Error> {
    let (org, id) = (e.project.org.to_string(), reserved.secret.to_string());
    let who = Identity {
        org: &org,
        secret: &id,
        revision: reserved.revision,
    };
    keyring
        .seal_values(who, data)
        .map_err(|err| Error::Internal(format!("sealing a secret failed: {err}")))
}

/// Start a rotation run for every app of `e` that references `secret`. The
/// caller must be allowed to deploy each of them; an app that cannot take a
/// run now (never deployed, pinned, being deleted) is reported, not fatal.
async fn rotate(
    state: &ApiState,
    tenant: &mut Tenant,
    authz: &Authz,
    e: &EnvScope,
    secret: &str,
) -> ApiResult<Vec<RolloutDto>> {
    let users = tenant.secret_users(e.id(), secret).await?;
    if users.is_empty() {
        return Ok(Vec::new());
    }
    let _deploy = authz.require(state, Perm::AppDeploy, &e.chain())?;
    let (_, actor) = request::actor(authz);
    let apps: Vec<AppRecord> = tenant.apps(e.id()).await?;
    let mut out = Vec::with_capacity(users.len());
    for (target, slug) in users {
        let Some(app) = apps.iter().find(|a| a.target == target) else {
            continue;
        };
        ensure_may_deploy(tenant, authz, target, &e.chain()).await?;
        let skipped = |why: &str| RolloutDto {
            app: slug.clone(),
            run: None,
            approvals_required: 0,
            skipped: Some(why.to_owned()),
        };
        let (Some(release), Some(config_revision)) = (app.release, app.config_revision) else {
            out.push(skipped("the app has no release yet"));
            continue;
        };
        let expected = app.desired_generation;
        let run = StartDeployment {
            project: e.project.id(),
            target,
            release,
            config_revision,
            render_plan: None,
            expected_generation: expected,
            lifecycle_uid: app.lifecycle_uid,
            reason: RunReason::Rotation,
            requested_by: actor.clone(),
            input_hash: Sha256::digest(format!(
                "rotation/{secret}/{release}/{config_revision}/{}",
                expected.0
            ))
            .to_vec(),
        };
        let audit = request::audit(
            authz,
            "deployment.accepted",
            "app",
            format!("{}/{}/{slug}", e.project.slug(), e.short_name()),
        );
        out.push(match tenant.start_deployment(&run, audit, None).await? {
            Started::Accepted {
                run,
                approvals_required,
                ..
            } => RolloutDto {
                app: slug.clone(),
                run: Some(*run.as_uuid()),
                approvals_required,
                skipped: None,
            },
            Started::Rejected(reject) => skipped(&reject.to_string()),
            Started::SecretRevoked => skipped("another secret it references is revoked"),
            Started::VulnerabilityBlocked => skipped("the vulnerability gate refuses its release"),
            Started::Frozen => skipped("the environment is frozen"),
            Started::Untrusted => skipped("an untrusted preview binds no secrets"),
            Started::NotFound | Started::Replayed(_) | Started::KeyReused(_) => {
                skipped("the app changed meanwhile")
            }
        });
    }
    Ok(out)
}

/// Delete a secret. Refused while an app of the environment references it.
#[utoipa::path(
    delete,
    path = "/projects/{project}/environments/{environment}/secrets/{secret}", operation_id = "deleteSecret",
    tag = "secrets",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("secret" = String, Path, description = "Secret name"),
    ),
    responses(
        (status = 204, description = "Deleted"),
        (status = 404, body = crate::error::Problem),
        (status = 409, description = "An app references the secret", body = crate::error::Problem),
    )
)]
pub async fn delete(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, secret)): Path<(String, String, String)>,
) -> ApiResult<StatusCode> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::SecretWrite, &e.chain())?;
    let in_use = |apps: &[String]| {
        Error::Conflict(format!(
            "secret `{secret}` is referenced by {}",
            apps.iter()
                .map(|a| format!("`{a}`"))
                .collect::<Vec<_>>()
                .join(", ")
        ))
    };
    let mut tenant = state.store.tenant(e.project.org).await?;
    let audit = request::audit(&authz, "secret.deleted", "secret", reference(&e, &secret));
    match tenant.delete_secret(e.id(), &secret, audit).await? {
        SecretDeleted::Done => {
            tenant.commit().await?;
            return Ok(StatusCode::NO_CONTENT);
        }
        SecretDeleted::InUse(apps) => return Err(in_use(&apps).into()),
        SecretDeleted::NotFound => {}
    }
    let users: Vec<String> = tenant
        .secret_users(e.id(), &secret)
        .await?
        .into_iter()
        .map(|(_, slug)| slug)
        .collect();
    drop(tenant);
    let api = Api::<Secret>::namespaced(scope::cluster(&state)?, &e.namespace());
    let existing = api
        .list(&ListParams::default().labels(&legacy_selector()))
        .await
        .map_err(|err| scope::kube_error(err, &secret))?
        .items
        .into_iter()
        .find(|s| s.name_any() == secret)
        .ok_or_else(|| Error::NotFound(format!("secret `{secret}`")))?;
    if !users.is_empty() {
        return Err(in_use(&users).into());
    }
    api.delete(&existing.name_any(), &DeleteParams::default())
        .await
        .map_err(|err| scope::kube_error(err, &secret))?;
    Ok(StatusCode::NO_CONTENT)
}

/// The revisions of an encrypted secret, newest first (no values).
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/secrets/{secret}/revisions", operation_id = "listSecretRevisions",
    tag = "secrets",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("secret" = String, Path, description = "Secret name"),
    ),
    responses((status = 200, body = Vec<SecretRevisionDto>), (status = 404, body = crate::error::Problem))
)]
pub async fn revisions(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, secret)): Path<(String, String, String)>,
) -> ApiResult<Json<Vec<SecretRevisionDto>>> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::SecretRead, &e.chain())?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let revisions = tenant
        .secret_revisions(e.id(), &secret)
        .await?
        .ok_or_else(|| Error::NotFound(format!("secret `{secret}`")))?;
    let current = revisions.first().map_or(0, |r| r.revision);
    Ok(Json(
        revisions
            .into_iter()
            .map(|r| SecretRevisionDto::of(r, current))
            .collect(),
    ))
}

/// Revoke a revision for good: runs bound to it fail instead of delivering
/// it. Revoking the current revision blocks deployments until a new value is
/// set.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/secrets/{secret}/revisions/{revision}/revoke", operation_id = "revokeSecretRevision",
    tag = "secrets",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("secret" = String, Path, description = "Secret name"),
        ("revision" = u64, Path, description = "Revision"),
    ),
    responses(
        (status = 204, description = "Revoked"),
        (status = 404, body = crate::error::Problem),
        (status = 409, description = "Revoked already", body = crate::error::Problem),
    )
)]
pub async fn revoke(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, secret, revision)): Path<(String, String, String, u64)>,
) -> ApiResult<StatusCode> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::SecretWrite, &e.chain())?;
    let (_, actor) = request::actor(&authz);
    let mut audit = request::audit(
        &authz,
        "secret.revision.revoked",
        "secret",
        reference(&e, &secret),
    );
    audit.data = Some(serde_json::json!({ "revision": revision }));
    let mut tenant = state.store.tenant(e.project.org).await?;
    match tenant
        .revoke_secret_revision(e.id(), &secret, revision, &actor, audit)
        .await?
    {
        Revoked::Done => {
            tenant.commit().await?;
            Ok(StatusCode::NO_CONTENT)
        }
        Revoked::Already => {
            Err(Error::Conflict(format!("revision {revision} of `{secret}` is revoked already")).into())
        }
        Revoked::NotFound => Err(Error::NotFound(format!("revision {revision} of secret `{secret}`")).into()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn values_are_bounded() {
        let one = |k: &str, v: &str| BTreeMap::from([(k.to_owned(), v.to_owned())]);
        assert!(check_values(&one("url", "postgres://db")).is_ok());
        assert!(check_values(&BTreeMap::new()).is_err());
        assert!(check_values(&one("bad key", "x")).is_err());
        assert!(check_values(&one("big", &"x".repeat(MAX_SECRET_BYTES + 1))).is_err());
        // Escaping grows the sealed object beyond the raw size.
        assert!(check_values(&one("escaped", &"\u{1}".repeat(MAX_SECRET_BYTES / 2))).is_err());
        let many: BTreeMap<String, String> = (0..=MAX_KEYS).map(|i| (format!("k{i}"), "v".into())).collect();
        assert!(check_values(&many).is_err());
    }

    #[test]
    fn put_rolls_out_unless_told_not_to() {
        let body: PutSecret = serde_json::from_str(r#"{"data":{"a":"b"}}"#).expect("body");
        assert!(body.rollout);
        let body: PutSecret = serde_json::from_str(r#"{"data":{"a":"b"},"rollout":false}"#).expect("body");
        assert!(!body.rollout);
        assert!(serde_json::from_str::<PutSecret>(r#"{"data":{},"value":1}"#).is_err());
        assert_eq!(
            legacy_selector(),
            "app.kubernetes.io/managed-by=kuben,!kuben.dev/secret-id"
        );
    }
}
