//! Preview environments (M5.1): pull request events, their lifecycle and
//! the janitor.
//!
//! A pull request of a repository the source environment of a project
//! builds from gets a preview:
//! - a new environment `pr<n>-<epoch>` with its own namespace;
//! - the source environment's policy without approvals;
//! - a copy of every app that builds from that repository, without secret
//!   references or custom domains, bound to the pull request's head.
//!
//! New commits sync those apps. Closing the pull request deletes the
//! environment. A reopened pull request gets a new epoch and so a new
//! environment. Events older than what a preview (or its closed
//! predecessor) saw are ignored, so a late delivery never brings a preview
//! back.
//!
//! A fork's preview is untrusted: only projects that allow forks get one,
//! and it never binds a secret or a registry login.
//!
//! The janitor deletes previews whose lifetime ran out. It also asks GitHub
//! whether the pull request of a preview not confirmed for a while is still
//! open, so a missed `closed` delivery is caught. An unclear answer never
//! deletes anything.

use std::{sync::Arc, time::Duration};

use kuben_core::{
    Error,
    ids::{EnvironmentId, OrgId, PlacementId, ProjectId},
    policy::EnvironmentPolicy,
    preview,
    source::{BranchName, PullAction, PullEvent},
    time::now_ms,
};
use kuben_platform::{build::ProviderError, controller::resources::namespace_name, health::Health};
use kuben_store::{
    Store, StoreError,
    repo::{
        CloseReason, ENVIRONMENT_APPLY, ENVIRONMENT_DELETE, EnvironmentKind, NewAudit, NewBinding,
        NewIncident, NewPreview, Preview, PreviewPolicy, PreviewSource, Subject, Tenant,
    },
};
use serde_json::json;
use tokio_util::sync::CancellationToken;

use crate::{error::ApiError, github::GithubApp, routes::scope, state::ApiState};

const HEALTH: &str = "previews";
const PRIMARY_CLUSTER: &str = "primary";
/// How often the janitor looks for expired previews.
const SWEEP: Duration = Duration::from_mins(1);
/// A preview's pull request is confirmed open at most this often.
const VERIFY_EVERY_MS: i64 = 30 * 60_000;
const BATCH: i64 = 20;

/// What a pull request event did to one project.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Outcome {
    pub project: ProjectId,
    pub result: &'static str,
}

fn audit(event: &PullEvent, action: &str, target: String) -> NewAudit {
    NewAudit {
        actor_kind: "webhook".into(),
        actor_id: Some(format!("github:{}", event.installation_id)),
        action: action.into(),
        target_kind: Some("environment".into()),
        target_ref: Some(target),
        outcome: "accepted".into(),
        data: Some(json!({
            "repository": event.repository,
            "pullRequest": event.number,
            "head": event.head,
            "headRepository": event.head_repository,
        })),
        ..NewAudit::default()
    }
}

/// Apply a pull request event to every project of `org` it concerns.
pub async fn on_pull(state: &ApiState, org: OrgId, event: &PullEvent) -> Result<Vec<Outcome>, ApiError> {
    let projects = {
        let mut tenant = state.store.tenant(org).await?;
        tenant
            .preview_projects(event.installation_id, &event.repository)
            .await?
    };
    let mut out = Vec::new();
    for project in projects {
        let mut tenant = state.store.tenant(org).await?;
        let result = apply(state, &mut tenant, project, event).await?;
        tenant.commit().await?;
        out.push(Outcome { project, result });
    }
    Ok(out)
}

async fn apply(
    state: &ApiState,
    tenant: &mut Tenant,
    project: ProjectId,
    event: &PullEvent,
) -> Result<&'static str, ApiError> {
    let Some(policy) = tenant.preview_policy(project).await?.filter(|p| p.enabled) else {
        return Ok("disabled");
    };
    let active = tenant
        .active_preview(project, &event.repository, event.number)
        .await?;
    if event.action == PullAction::Closed || !event.open {
        let Some(active) = active else {
            return Ok("noPreview");
        };
        let audit = audit(event, "closePreview", active.environment.clone());
        destroy(tenant, &active, CloseReason::Closed, event.updated_at, audit).await?;
        return Ok("closed");
    }
    if event.from_fork() && !policy.allow_forks {
        return Ok("forkRefused");
    }
    let expires = |current| preview::expiry(now_ms(), policy.ttl_hours, current);
    if let Some(active) = active {
        let env = active.environment_id();
        if !tenant
            .touch_preview(
                env,
                &event.head_repository,
                &event.head_branch,
                &event.head,
                event.updated_at,
                expires(Some(active.expires_at)),
            )
            .await?
        {
            return Ok("stale");
        }
        sync_apps(tenant, env, event).await?;
        return Ok("synced");
    }
    let (epoch, closed_at) = tenant
        .preview_epoch(project, &event.repository, event.number)
        .await?;
    if closed_at.is_some_and(|at| at >= event.updated_at) {
        return Ok("staleAfterClose");
    }
    if tenant.active_preview_count(project).await? >= u64::from(policy.max_active) {
        limit_incident(tenant, project, event, policy.max_active).await?;
        return Ok("limit");
    }
    create(state, tenant, &policy, event, epoch, expires(None)).await
}

/// A preview could not be made: the project has too many.
async fn limit_incident(
    tenant: &mut Tenant,
    project: ProjectId,
    event: &PullEvent,
    max: u32,
) -> Result<(), StoreError> {
    let incident = NewIncident {
        project: Some(project),
        environment: None,
        target: None,
        kind: "preview.limit".into(),
        severity: "warning",
        dedupe_key: format!("preview-limit:{project}"),
        title: format!(
            "No preview for {}#{}: the project has {max} already",
            event.repository, event.number
        ),
        detail: Some("Close or destroy a preview, or raise the project's preview limit.".into()),
    };
    tenant.open_incident(&incident).await.map(drop)
}

async fn create(
    state: &ApiState,
    tenant: &mut Tenant,
    policy: &PreviewPolicy,
    event: &PullEvent,
    epoch: u64,
    expires_at: i64,
) -> Result<&'static str, ApiError> {
    let project = policy.project;
    let Some(project_row) = tenant.projects().await?.into_iter().find(|p| p.id == project) else {
        return Ok("projectGone");
    };
    let Some(slug) = preview::slug(event.number, epoch) else {
        return Ok("nameTooLong");
    };
    let resource = scope::environment_resource_name(&project_row.slug, &slug);
    let namespace = namespace_name(&resource);
    if namespace.len() > 63 {
        return Ok("nameTooLong");
    }
    let environments = tenant.environments(project).await?;
    let Some(source) = environments
        .iter()
        .find(|e| e.id == policy.source_environment && !e.deleting)
    else {
        return Ok("sourceGone");
    };
    let sources = tenant
        .preview_sources(source.id, event.installation_id, &event.repository)
        .await?;
    if sources.is_empty() {
        return Ok("noApps");
    }
    crate::routes::apps::admission::admit_environment(state, tenant).await?;
    let source_policy = tenant
        .environment_policy(source.id)
        .await?
        .map_or_else(EnvironmentPolicy::open, |p| p.policy);
    let actor = format!("github:{}", event.installation_id);
    let env = tenant
        .create_environment_typed(
            project,
            &slug,
            &format!("PR #{}", event.number),
            EnvironmentKind::Preview,
            source.quota.as_ref(),
        )
        .await?;
    let cluster = tenant.ensure_cluster(PRIMARY_CLUSTER).await?;
    let placement = tenant.create_placement(project, env, cluster, &namespace).await?;
    tenant
        .set_environment_policy(
            project,
            env,
            &EnvironmentPolicy::for_preview(&source_policy),
            &actor,
        )
        .await?
        .ok_or_else(|| Error::Internal("the new preview environment is missing".into()))?;
    tenant
        .insert_preview(&NewPreview {
            environment: env,
            project,
            installation_id: event.installation_id,
            repository: &event.repository,
            number: event.number,
            epoch,
            head_repository: &event.head_repository,
            branch: &event.head_branch,
            commit: &event.head,
            trusted: !event.from_fork(),
            expires_at,
            event_at: event.updated_at,
            created_by: &actor,
        })
        .await?;
    tenant
        .request(
            ENVIRONMENT_APPLY,
            Subject::environment(project, env),
            &actor,
            audit(event, "openPreview", resource),
        )
        .await?;
    copy_apps(tenant, project, placement, sources, event, &actor).await?;
    sync_apps(tenant, env, event).await?;
    Ok("opened")
}

/// Copy the source apps into the preview's `placement`, each bound to the
/// pull request's head.
async fn copy_apps(
    tenant: &mut Tenant,
    project: ProjectId,
    placement: PlacementId,
    sources: Vec<PreviewSource>,
    event: &PullEvent,
    actor: &str,
) -> Result<(), ApiError> {
    let branch: BranchName = format!("pull/{}", event.number)
        .parse()
        .map_err(|e| Error::Internal(format!("preview branch: {e}")))?;
    for app in sources {
        let target = tenant.create_target(project, app.application, placement).await?;
        let (config, removed) = preview::preview_config(&app.config);
        if !removed.is_empty() {
            tracing::info!(app = %app.slug, ?removed, "left out of the preview");
        }
        tenant
            .create_config_revision(project, target, &config, actor)
            .await?
            .ok_or_else(|| Error::Internal("the preview app is missing".into()))?;
        let binding = NewBinding {
            installation_id: event.installation_id,
            repository: event.repository.clone(),
            branch: branch.clone(),
            recipe: app.recipe,
            image_repository: app.image_repository,
            pull_request: Some(event.number),
        };
        tenant.bind_source(project, target, &binding).await?;
    }
    Ok(())
}

/// Ask every app of preview `env` to read the pull request's head.
async fn sync_apps(tenant: &mut Tenant, env: EnvironmentId, event: &PullEvent) -> Result<(), StoreError> {
    for target in tenant.preview_targets(env).await? {
        if let Some(binding) = tenant.binding_of_target(target).await? {
            let audit = NewAudit {
                target_kind: Some("app".into()),
                target_ref: Some(target.to_string()),
                action: "syncSource".into(),
                ..audit(event, "syncSource", String::new())
            };
            tenant
                .request_sync(&binding, &format!("github:pull:{}", event.number), audit)
                .await?;
        }
    }
    Ok(())
}

/// Close `preview` for `reason` and delete its environment. False when it
/// was closed already.
pub async fn destroy(
    tenant: &mut Tenant,
    preview: &Preview,
    reason: CloseReason,
    event_at: i64,
    audit: NewAudit,
) -> Result<bool, StoreError> {
    let env = preview.environment_id();
    if !tenant.close_preview(env, reason, event_at).await? {
        return Ok(false);
    }
    if tenant.mark_environment_deleting(env).await? {
        let actor = audit.actor_id.clone().unwrap_or_else(|| "system".into());
        tenant
            .request(
                ENVIRONMENT_DELETE,
                Subject::environment(preview.project_id(), env),
                &actor,
                audit,
            )
            .await?;
    }
    Ok(true)
}

fn system_audit(preview: &Preview, action: &str, why: &str) -> NewAudit {
    NewAudit {
        actor_kind: "system".into(),
        actor_id: Some("system:previews".into()),
        action: action.into(),
        target_kind: Some("environment".into()),
        target_ref: Some(preview.environment.clone()),
        outcome: "accepted".into(),
        data: Some(
            json!({ "reason": why, "repository": preview.repository, "pullRequest": preview.pr_number }),
        ),
        ..NewAudit::default()
    }
}

/// The preview janitor of one process; every replica runs one.
#[derive(Clone)]
pub struct Janitor {
    store: Store,
    github: Option<Arc<GithubApp>>,
}

impl std::fmt::Debug for Janitor {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Janitor").finish_non_exhaustive()
    }
}

impl Janitor {
    #[must_use]
    pub const fn new(store: Store, github: Option<Arc<GithubApp>>) -> Self {
        Self { store, github }
    }

    /// Delete expired previews. The number deleted.
    pub async fn sweep(&self, now: i64) -> Result<usize, StoreError> {
        let mut done = 0;
        for org in self.store.preview_orgs().await? {
            let mut tenant = self.store.tenant(org).await?;
            for p in tenant.due_previews(now, BATCH).await? {
                let audit = system_audit(&p, "expirePreview", "expired");
                if destroy(&mut tenant, &p, CloseReason::Expired, 0, audit).await? {
                    done += 1;
                }
            }
            tenant.commit().await?;
        }
        Ok(done)
    }

    /// Delete previews whose pull request GitHub reports closed; confirm
    /// the open ones. The number deleted.
    pub async fn verify(&self, now: i64) -> Result<usize, StoreError> {
        let Some(github) = &self.github else {
            return Ok(0);
        };
        let mut done = 0;
        for org in self.store.preview_orgs().await? {
            let stale = {
                let mut tenant = self.store.tenant(org).await?;
                tenant.unverified_previews(now - VERIFY_EVERY_MS, BATCH).await?
            };
            for p in stale {
                let (Ok(installation), Ok(number), Ok(repository)) = (
                    u64::try_from(p.installation_id),
                    u64::try_from(p.pr_number),
                    p.repository.parse(),
                ) else {
                    continue;
                };
                let open = github.pull_request_open(installation, &repository, number).await;
                let mut tenant = self.store.tenant(org).await?;
                match open {
                    Ok(true) => tenant.preview_verified(p.environment_id(), now).await?,
                    Ok(false) | Err(ProviderError::NotFound(_)) => {
                        let audit = system_audit(&p, "closePreview", "the pull request is closed");
                        if destroy(&mut tenant, &p, CloseReason::Closed, now, audit).await? {
                            done += 1;
                        }
                    }
                    Err(e) => {
                        // Unclear: never a reason to delete. Try again later.
                        tracing::debug!(preview = %p.environment, error = %e, "the pull request state is unknown");
                        tenant
                            .preview_verified(p.environment_id(), now - VERIFY_EVERY_MS / 2)
                            .await?;
                    }
                }
                tenant.commit().await?;
            }
        }
        Ok(done)
    }
}

/// Run `janitor` until `token` is cancelled.
pub async fn run(janitor: Janitor, health: Health, token: CancellationToken) -> anyhow::Result<()> {
    health.ok(HEALTH);
    let mut rounds: u64 = 0;
    loop {
        tokio::select! {
            () = token.cancelled() => return Ok(()),
            () = tokio::time::sleep(SWEEP) => {}
        }
        let now = now_ms();
        let expired = janitor.sweep(now).await?;
        // Pull request states are read every tenth round.
        let closed = if rounds.is_multiple_of(10) {
            janitor.verify(now).await?
        } else {
            0
        };
        rounds += 1;
        if expired + closed > 0 {
            tracing::info!(expired, closed, "previews deleted");
        }
    }
}
