//! Deployment approvals (M4.1): who may start a deployment under an
//! environment's policy, and the approve/reject decisions on a run waiting
//! for them.
//!
//! Approving is a person's act: API tokens cannot decide, the requester can
//! never decide on their own run, and the decision names the plan hash the
//! approver was shown, so a changed or superseded run is not approved by
//! mistake.

use axum::{
    Json,
    extract::{Path, State},
};
use kuben_core::{
    Error,
    authz::ScopeChain,
    ids::{DeploymentRunId, TargetId},
    perm::Perm,
    policy::{ApprovalError, Decision, MAX_COMMENT_CHARS, hex, principal, unhex},
    traits::effective_role,
};
use kuben_store::repo::{Decided, RunApproval, Tenant};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use super::releases::Actors;
use crate::{
    authz::Authz,
    error::{ApiError, ApiResult},
    routes::scope,
    state::ApiState,
};

/// Refuse a deployment `authz` may not start on `target` under its
/// environment's policy. Environments without a policy follow the role
/// check the route made already.
pub(crate) async fn ensure_may_deploy(
    tenant: &mut Tenant,
    authz: &Authz,
    target: TargetId,
    chain: &ScopeChain,
) -> ApiResult<()> {
    let Some(revision) = tenant.policy_of_target(target).await? else {
        return Ok(());
    };
    if revision.policy.may_deploy(effective_role(&authz.subject, chain)) {
        Ok(())
    } else {
        Err(Error::Forbidden.into())
    }
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct DecideRequest {
    /// The plan hash shown with the deployment (hex).
    pub plan_hash: String,
    /// Why, for the record; at most 1024 characters.
    pub comment: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct DecisionDto {
    /// Email of whoever decided.
    pub approver: String,
    /// `approved` or `rejected`.
    pub decision: String,
    pub comment: Option<String>,
    pub decided_at: i64,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct ApprovalDto {
    pub run: Uuid,
    pub phase: String,
    /// Email of whoever asked for the deployment.
    pub requested_by: String,
    /// Distinct approvals needed; 0 when the run needs none.
    pub required: u8,
    pub approved: u8,
    /// When the run is cancelled unless approved (Unix milliseconds).
    pub expires_at: Option<i64>,
    /// What an approver confirms: send it back with the decision.
    pub plan_hash: Option<String>,
    /// Whether the caller may decide on this run now.
    pub can_decide: bool,
    pub decisions: Vec<DecisionDto>,
}

fn dto(run: Uuid, a: &RunApproval, actors: &Actors, can_decide: bool) -> ApprovalDto {
    ApprovalDto {
        run,
        phase: a.phase.as_str().to_owned(),
        requested_by: actors.name(&a.requested_by),
        required: a.required,
        approved: a.approved(),
        expires_at: a.expires_at,
        plan_hash: a.plan_hash.as_deref().map(hex),
        can_decide,
        decisions: a
            .decisions
            .iter()
            .map(|d| DecisionDto {
                approver: actors.name(&format!("user:{}", d.approver)),
                decision: d.decision.as_str().to_owned(),
                comment: d.comment.clone(),
                decided_at: d.decided_at,
            })
            .collect(),
    }
}

/// Whether `authz` could decide on `a` now, as far as roles and the
/// requester rule go.
fn eligible(authz: &Authz, a: &RunApproval, may_approve: bool) -> bool {
    let me = authz.current.user.id.to_string();
    authz.current.token.is_none()
        && may_approve
        && a.phase == kuben_core::ops::RunPhase::AwaitingApproval
        && principal(&a.requested_by) != me
        && !a.decisions.iter().any(|d| d.approver == me)
}

fn refusal(e: ApprovalError) -> ApiError {
    ApiError(match e {
        ApprovalError::SelfApproval => Error::Forbidden,
        other => Error::Conflict(other.to_string()),
    })
}

/// The approval state of a deployment run.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/deployments/{run}/approval",
    operation_id = "getDeploymentApproval",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        ("run" = Uuid, Path, description = "Deployment run id"),
    ),
    responses(
        (status = 200, body = ApprovalDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
    )
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app, run)): Path<(String, String, String, Uuid)>,
) -> ApiResult<Json<ApprovalDto>> {
    let t = scope::sql_target(&state, &authz, &project, &environment, &app).await?;
    let chain = t.chain();
    let _proof = authz.require(&state, Perm::AppRead, &chain)?;
    let mut tenant = state.store.tenant(t.org).await?;
    let approval = tenant
        .run_approval(t.target, DeploymentRunId::from_uuid(run))
        .await?
        .ok_or_else(|| Error::NotFound(format!("deployment `{run}`")))?;
    let may_approve = tenant.policy_of_target(t.target).await?.is_some_and(|p| {
        authz.require(&state, Perm::ReleaseApprove, &chain).is_ok()
            && p.policy.may_approve(effective_role(&authz.subject, &chain))
    });
    drop(tenant);
    let actors = Actors::load(&state).await?;
    let can_decide = eligible(&authz, &approval, may_approve);
    Ok(Json(dto(run, &approval, &actors, can_decide)))
}

async fn decide(
    state: &ApiState,
    authz: &Authz,
    (project, environment, app, run): (String, String, String, Uuid),
    body: DecideRequest,
    decision: Decision,
) -> ApiResult<Json<ApprovalDto>> {
    authz.forbid_token()?;
    let t = scope::sql_target(state, authz, &project, &environment, &app).await?;
    let chain = t.chain();
    let _proof = authz.require(state, Perm::ReleaseApprove, &chain)?;
    let comment = body.comment.as_deref().map(str::trim).filter(|c| !c.is_empty());
    if comment.is_some_and(|c| c.chars().count() > MAX_COMMENT_CHARS) {
        return Err(
            Error::Validation(format!("a comment has at most {MAX_COMMENT_CHARS} characters")).into(),
        );
    }
    let plan_hash = unhex(body.plan_hash.trim())
        .ok_or_else(|| Error::Validation("`planHash` is not a hex plan hash".into()))?;
    let mut tenant = state.store.tenant(t.org).await?;
    let policy = tenant
        .policy_of_target(t.target)
        .await?
        .ok_or_else(|| Error::Conflict("the environment requires no approvals".into()))?;
    // The role is checked again at decision time: a demoted approver cannot
    // use a page opened earlier.
    if !policy.policy.may_approve(effective_role(&authz.subject, &chain)) {
        return Err(Error::Forbidden.into());
    }
    let run_id = DeploymentRunId::from_uuid(run);
    let approver = authz.current.user.id.to_string();
    match tenant
        .decide_run(t.target, run_id, &approver, decision, &plan_hash, comment)
        .await?
    {
        Decided::Recorded(tally) => {
            tracing::info!(%run, %approver, decision = decision.as_str(), ?tally, "deployment decision recorded");
        }
        Decided::Refused(e) => return Err(refusal(e)),
        Decided::NotFound => return Err(Error::NotFound(format!("deployment `{run}`")).into()),
    }
    let approval = tenant
        .run_approval(t.target, run_id)
        .await?
        .ok_or_else(|| Error::Internal("the decided run is missing".into()))?;
    tenant.commit().await?;
    let actors = Actors::load(state).await?;
    Ok(Json(dto(run, &approval, &actors, false)))
}

/// Approve a deployment waiting for approval. Enough approvals release it to
/// the cluster at once.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/deployments/{run}/approve",
    operation_id = "approveDeployment",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        ("run" = Uuid, Path, description = "Deployment run id"),
    ),
    request_body = DecideRequest,
    responses(
        (status = 200, body = ApprovalDto),
        (status = 403, body = crate::error::Problem, description = "Not an approver here, an API token, or the requester"),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem, description = "Not waiting, expired, already decided, or a changed plan"),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn approve(
    State(state): State<ApiState>,
    authz: Authz,
    Path(path): Path<(String, String, String, Uuid)>,
    Json(body): Json<DecideRequest>,
) -> ApiResult<Json<ApprovalDto>> {
    decide(&state, &authz, path, body, Decision::Approve).await
}

/// Reject a deployment waiting for approval; it is cancelled.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/deployments/{run}/reject",
    operation_id = "rejectDeployment",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        ("run" = Uuid, Path, description = "Deployment run id"),
    ),
    request_body = DecideRequest,
    responses(
        (status = 200, body = ApprovalDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn reject(
    State(state): State<ApiState>,
    authz: Authz,
    Path(path): Path<(String, String, String, Uuid)>,
    Json(body): Json<DecideRequest>,
) -> ApiResult<Json<ApprovalDto>> {
    decide(&state, &authz, path, body, Decision::Reject).await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn refusals_map_to_http() {
        assert!(matches!(refusal(ApprovalError::SelfApproval).0, Error::Forbidden));
        for e in [
            ApprovalError::NotAwaiting,
            ApprovalError::Expired,
            ApprovalError::AlreadyDecided,
            ApprovalError::StalePlan,
        ] {
            assert!(matches!(refusal(e).0, Error::Conflict(_)), "{e:?}");
        }
    }

    #[test]
    fn decisions_reject_unknown_fields() {
        let body: DecideRequest = serde_json::from_str(r#"{"planHash":"00ff"}"#).expect("body");
        assert_eq!(body.plan_hash, "00ff");
        assert!(serde_json::from_str::<DecideRequest>(r#"{"planHash":"00","approve":true}"#).is_err());
    }
}
