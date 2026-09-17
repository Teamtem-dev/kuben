//! An environment's protection policy (M4.1): approvals, who deploys, who
//! approves and how long a change waits. Every change is a new revision;
//! weakening protection is an owner's decision.

use axum::{
    Json,
    extract::{Path, State},
};
use kuben_core::{
    Error,
    perm::{Perm, Role},
    policy::{DEFAULT_APPROVAL_TTL_SECS, EnvironmentPolicy},
};
use kuben_store::repo::PolicyRevision;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{request, scope};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

const fn default_ttl() -> u32 {
    DEFAULT_APPROVAL_TTL_SECS
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct PutPolicy {
    /// Distinct approvers a deployment needs (0–5).
    #[schema(example = 1)]
    pub required_approvals: u8,
    /// The weakest role that may deploy: `developer`, `admin` or `owner`.
    #[schema(example = "developer")]
    pub deploy_role: String,
    /// The weakest role that may approve: `admin` or `owner`.
    #[schema(example = "admin")]
    pub approve_role: String,
    /// How long a deployment waits for approvals (300 s – 30 days).
    #[serde(default = "default_ttl")]
    #[schema(example = 604_800)]
    pub approval_ttl_secs: u32,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct PolicyDto {
    /// 0 when the environment has no policy of its own yet.
    pub revision: u64,
    pub required_approvals: u8,
    pub deploy_role: String,
    pub approve_role: String,
    pub approval_ttl_secs: u32,
    pub updated_by: Option<String>,
    pub updated_at: Option<i64>,
}

impl PolicyDto {
    fn of(revision: Option<&PolicyRevision>) -> Self {
        let policy = revision.map_or_else(EnvironmentPolicy::open, |r| r.policy);
        Self {
            revision: revision.map_or(0, |r| r.revision),
            required_approvals: policy.required_approvals,
            deploy_role: policy.deploy_role.to_string(),
            approve_role: policy.approve_role.to_string(),
            approval_ttl_secs: policy.approval_ttl_secs,
            updated_by: revision.map(|r| r.created_by.clone()),
            updated_at: revision.map(|r| r.created_at),
        }
    }
}

fn policy_of(body: &PutPolicy) -> Result<EnvironmentPolicy, Error> {
    let policy = EnvironmentPolicy {
        required_approvals: body.required_approvals,
        deploy_role: body.deploy_role.parse::<Role>()?,
        approve_role: body.approve_role.parse::<Role>()?,
        approval_ttl_secs: body.approval_ttl_secs,
    };
    policy.validate().map_err(|e| Error::Validation(e.to_string()))?;
    Ok(policy)
}

/// The environment's protection policy.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/policy", operation_id = "getEnvironmentPolicy",
    tag = "environments",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    responses(
        (status = 200, body = PolicyDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
    )
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
) -> ApiResult<Json<PolicyDto>> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::EnvRead, &e.chain())?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let revision = tenant.environment_policy(e.id()).await?;
    Ok(Json(PolicyDto::of(revision.as_ref())))
}

/// Change the environment's protection policy. Stricter policies need
/// `env-write`; anything weaker needs an owner.
#[utoipa::path(
    put,
    path = "/projects/{project}/environments/{environment}/policy", operation_id = "putEnvironmentPolicy",
    tag = "environments",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    request_body = PutPolicy,
    responses(
        (status = 200, body = PolicyDto),
        (status = 403, body = crate::error::Problem, description = "Weakening needs an owner; tokens cannot change policies"),
        (status = 404, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn put(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
    Json(body): Json<PutPolicy>,
) -> ApiResult<Json<PolicyDto>> {
    authz.forbid_token()?;
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let chain = e.chain();
    let _proof = authz.require(&state, Perm::EnvWrite, &chain)?;
    let policy = policy_of(&body)?;
    let (_, actor) = request::actor(&authz);
    let mut tenant = state.store.tenant(e.project.org).await?;
    let current = tenant
        .environment_policy(e.id())
        .await?
        .map_or_else(EnvironmentPolicy::open, |r| r.policy);
    if policy == current {
        let revision = tenant.environment_policy(e.id()).await?;
        return Ok(Json(PolicyDto::of(revision.as_ref())));
    }
    if policy.weakens(&current) {
        let _owner = authz.require(&state, Perm::EnvProtect, &chain)?;
    }
    tenant
        .set_environment_policy(e.project.id(), e.id(), &policy, &actor)
        .await?
        .ok_or_else(|| Error::NotFound(format!("environment `{environment}`")))?;
    let revision = tenant.environment_policy(e.id()).await?;
    tenant.commit().await?;
    Ok(Json(PolicyDto::of(revision.as_ref())))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn body(approvals: u8, deploy: &str, approve: &str) -> PutPolicy {
        PutPolicy {
            required_approvals: approvals,
            deploy_role: deploy.into(),
            approve_role: approve.into(),
            approval_ttl_secs: DEFAULT_APPROVAL_TTL_SECS,
        }
    }

    #[test]
    fn bodies_become_valid_policies() {
        let p = policy_of(&body(2, "admin", "owner")).expect("valid");
        assert_eq!(p.required_approvals, 2);
        assert_eq!(p.deploy_role, Role::Admin);
        for bad in [
            body(6, "developer", "admin"),
            body(1, "viewer", "admin"),
            body(1, "developer", "developer"),
            body(1, "root", "admin"),
        ] {
            assert!(policy_of(&bad).is_err(), "{bad:?}");
        }
        let parsed: PutPolicy =
            serde_json::from_str(r#"{"requiredApprovals":1,"deployRole":"developer","approveRole":"admin"}"#)
                .expect("body");
        assert_eq!(parsed.approval_ttl_secs, DEFAULT_APPROVAL_TTL_SECS);
    }

    #[test]
    fn environments_without_a_policy_show_the_open_one() {
        let dto = PolicyDto::of(None);
        assert_eq!((dto.revision, dto.required_approvals), (0, 0));
        assert_eq!(dto.deploy_role, "developer");
        let revision = PolicyRevision {
            revision: 3,
            policy: EnvironmentPolicy::production(),
            created_by: "user:a".into(),
            created_at: 7,
        };
        let dto = PolicyDto::of(Some(&revision));
        assert_eq!((dto.revision, dto.required_approvals), (3, 1));
        assert_eq!(dto.updated_by.as_deref(), Some("user:a"));
    }
}
