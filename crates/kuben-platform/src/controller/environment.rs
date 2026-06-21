//! Environment → Namespace (+ quota, limit defaults, isolation policy).
//!
//! Deletion is enforced by a finalizer managed here (not kube's helper, which
//! would drop the finalizer as soon as cleanup returns): `Retain` hands the
//! namespace back, `Delete` purges it once the protection grace period has
//! elapsed — a soft delete that can be cancelled by nobody but is visible
//! (`status.deletionScheduledAt`) the whole time (ADR-018).

use std::{sync::Arc, time::Duration};

use futures::StreamExt;
use k8s_openapi::{
    api::{
        core::v1::{LimitRange, Namespace, ResourceQuota},
        networking::v1::NetworkPolicy,
    },
    jiff::Timestamp,
};
use kube::{
    Api, ResourceExt,
    api::{DeleteParams, Patch, PatchParams},
    runtime::{Controller, controller::Action, reflector::ObjectRef, watcher},
};
use kuben_crd::{DeletionPolicy, Environment, EnvironmentStatus, FIELD_MANAGER, condition::READY, labels};
use serde_json::json;
use tokio_util::sync::CancellationToken;

use super::{Ctx, Result, condition, error_policy, is_not_found, resources};
use crate::duration;

pub async fn run(ctx: Arc<Ctx>, token: CancellationToken) -> anyhow::Result<()> {
    let client = ctx.client.clone();
    Controller::new(Api::<Environment>::all(client.clone()), watcher::Config::default())
        // Repair drift: a deleted or relabelled namespace re-triggers its environment.
        .watches(
            Api::<Namespace>::all(client),
            watcher::Config::default().labels(labels::MANAGED_SELECTOR),
            |ns| ns.labels().get(labels::ENVIRONMENT).map(|e| ObjectRef::<Environment>::new(e)),
        )
        .graceful_shutdown_on(token.cancelled_owned())
        .run(reconcile, error_policy, ctx)
        .for_each(|_| std::future::ready(()))
        .await;
    Ok(())
}

#[allow(clippy::needless_pass_by_value)] // signature required by `Controller::run`
async fn reconcile(env: Arc<Environment>, ctx: Arc<Ctx>) -> Result<Action> {
    let api = Api::<Environment>::all(ctx.client.clone());
    let has_finalizer = env.finalizers().iter().any(|f| f == resources::ENV_FINALIZER);
    let action = if env.metadata.deletion_timestamp.is_some() {
        if !has_finalizer {
            return Ok(Action::await_change());
        }
        cleanup(&env, &api, &ctx).await?
    } else {
        if !has_finalizer {
            set_finalizer(&env, &api, true).await?;
        }
        apply(&env, &api, &ctx).await?
    };
    ctx.succeeded(env.as_ref());
    Ok(action)
}

async fn apply(env: &Environment, api: &Api<Environment>, ctx: &Ctx) -> Result<Action> {
    let name = env.name_any();
    let ns = resources::namespace_name(&name);
    let namespaces = Api::<Namespace>::all(ctx.client.clone());

    if let Some(existing) = namespaces.get_opt(&ns).await? {
        let l = existing.labels();
        let ours = l.get(labels::MANAGED_BY).is_some_and(|v| v == labels::MANAGER)
            && l.get(labels::ENVIRONMENT).is_some_and(|v| *v == name);
        if !ours {
            // Never adopt a namespace someone else created.
            let msg = format!("namespace {ns} already exists and is not managed by this environment");
            write_status(api, env, "Degraded", None, "NamespaceConflict", &msg, false).await?;
            return Ok(Action::requeue(Duration::from_mins(5)));
        }
        if existing.metadata.deletion_timestamp.is_some() {
            let msg = format!("waiting for namespace {ns} to finish terminating");
            write_status(api, env, "Pending", None, "NamespaceTerminating", &msg, false).await?;
            return Ok(Action::requeue(Duration::from_secs(5)));
        }
    }

    let pp = PatchParams::apply(FIELD_MANAGER).force();
    let client = &ctx.client;
    namespaces
        .patch(&ns, &pp, &Patch::Apply(resources::namespace(env)))
        .await?;
    Api::<ResourceQuota>::namespaced(client.clone(), &ns)
        .patch(
            resources::QUOTA_NAME,
            &pp,
            &Patch::Apply(resources::resource_quota(env)),
        )
        .await?;
    Api::<LimitRange>::namespaced(client.clone(), &ns)
        .patch(
            resources::LIMITS_NAME,
            &pp,
            &Patch::Apply(resources::limit_range(env)),
        )
        .await?;
    Api::<NetworkPolicy>::namespaced(client.clone(), &ns)
        .patch(
            resources::NETPOL_NAME,
            &pp,
            &Patch::Apply(resources::network_policy(env)),
        )
        .await?;

    write_status(api, env, "Ready", None, "Provisioned", "", true).await?;
    Ok(Action::requeue(Duration::from_mins(10)))
}

async fn cleanup(env: &Environment, api: &Api<Environment>, ctx: &Ctx) -> Result<Action> {
    let ns = resources::namespace_name(&env.name_any());
    let namespaces = Api::<Namespace>::all(ctx.client.clone());
    match env.spec.deletion_policy {
        DeletionPolicy::Retain => {
            // Hand the namespace back: Kuben stops managing it, the data stays.
            let release =
                json!({ "metadata": { "labels": { labels::MANAGED_BY: null, labels::ENVIRONMENT: null } } });
            if let Err(e) = namespaces
                .patch(&ns, &PatchParams::default(), &Patch::Merge(&release))
                .await
                && !is_not_found(&e)
            {
                return Err(e.into());
            }
        }
        DeletionPolicy::Delete => {
            let grace = env
                .spec
                .protection
                .as_ref()
                .and_then(|p| duration::parse(&p.deletion_grace))
                .unwrap_or(Duration::ZERO);
            let now_ms = Timestamp::now().as_millisecond();
            let deleted_ms = env
                .metadata
                .deletion_timestamp
                .as_ref()
                .map_or(now_ms, |t| t.0.as_millisecond());
            let purge_ms = deleted_ms.saturating_add(i64::try_from(grace.as_millis()).unwrap_or(i64::MAX));
            if now_ms < purge_ms {
                let at = Timestamp::from_millisecond(purge_ms).ok().map(|t| t.to_string());
                let msg = "the namespace is deleted when the grace period ends";
                write_status(api, env, "Terminating", at, "DeletionScheduled", msg, false).await?;
                let remaining = Duration::from_millis(u64::try_from(purge_ms - now_ms).unwrap_or(0));
                return Ok(Action::requeue(remaining.min(Duration::from_hours(1))));
            }
            if let Some(existing) = namespaces.get_opt(&ns).await? {
                if existing.metadata.deletion_timestamp.is_none() {
                    namespaces.delete(&ns, &DeleteParams::background()).await?;
                    tracing::info!(namespace = %ns, "environment namespace deleted");
                }
                // Keep the finalizer until the namespace (and every app in it) is gone.
                return Ok(Action::requeue(Duration::from_secs(5)));
            }
        }
    }
    set_finalizer(env, api, false).await?;
    Ok(Action::await_change())
}

async fn set_finalizer(env: &Environment, api: &Api<Environment>, present: bool) -> Result<()> {
    let mut finalizers: Vec<String> = env
        .finalizers()
        .iter()
        .filter(|f| *f != resources::ENV_FINALIZER)
        .cloned()
        .collect();
    if present {
        finalizers.push(resources::ENV_FINALIZER.into());
    }
    // `resourceVersion` makes the write conditional: a concurrent update is a 409.
    let patch =
        json!({ "metadata": { "finalizers": finalizers, "resourceVersion": env.resource_version() } });
    api.patch(&env.name_any(), &PatchParams::default(), &Patch::Merge(&patch))
