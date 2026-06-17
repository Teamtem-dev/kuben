//! App → Deployments (+ HPAs), a Service and a Gateway API HTTPRoute. Every
//! child carries an owner reference (garbage-collected with the App) and is
//! pruned when it disappears from the spec.

use std::{collections::BTreeSet, fmt::Debug, sync::Arc, time::Duration};

use futures::{Stream, StreamExt};
use k8s_openapi::{
    api::{
        apps::v1::Deployment,
        autoscaling::v2::HorizontalPodAutoscaler,
        batch::v1::CronJob,
        core::v1::{PersistentVolumeClaim, Service},
    },
    apimachinery::pkg::apis::meta::v1::OwnerReference,
};
use kube::{
    Api, Client, Resource, ResourceExt,
    api::{ApiResource, DeleteParams, DynamicObject, GroupVersionKind, ListParams, Patch, PatchParams},
    runtime::{Controller, controller::Action, watcher},
};
use kuben_crd::{App, AppStatus, Condition, FIELD_MANAGER, condition::READY, labels};
use serde::{Serialize, de::DeserializeOwned};
use serde_json::json;
use tokio_util::sync::CancellationToken;

use super::{
    Ctx, Error, Result, condition, error_policy, is_not_found,
    resources::{self, BuildError, Platform},
};

/// Condition type: is the app reachable through the gateway?
pub const EXPOSED: &str = "Exposed";

pub async fn run(
    ctx: Arc<Ctx>,
    config_changes: impl Stream<Item = ()> + Send + 'static,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let client = ctx.client.clone();
    let managed = watcher::Config::default().labels(labels::MANAGED_SELECTOR);
    Controller::new(Api::<App>::all(client.clone()), watcher::Config::default())
        .owns(Api::<Deployment>::all(client.clone()), managed.clone())
        .owns(Api::<Service>::all(client), managed)
        .reconcile_all_on(config_changes)
        .graceful_shutdown_on(token.cancelled_owned())
        .run(reconcile, error_policy, ctx)
        .for_each(|_| std::future::ready(()))
        .await;
    Ok(())
}

struct Desired {
    deployments: Vec<Deployment>,
    autoscalers: Vec<HorizontalPodAutoscaler>,
    cron_jobs: Vec<CronJob>,
    volumes: Vec<PersistentVolumeClaim>,
    service: Option<Service>,
    route: Option<serde_json::Value>,
    /// The web process is HTTP and should be reachable through the gateway.
    exposes_http: bool,
}

fn build(app: &App, platform: &Platform, owner: &OwnerReference) -> Result<Desired, BuildError> {
    resources::validate(app)?;
    Ok(Desired {
        deployments: resources::deployments(app, platform, owner)?,
        autoscalers: resources::autoscalers(app, owner),
        cron_jobs: resources::cron_jobs(app, platform, owner)?,
        volumes: resources::persistent_volume_claims(app),
        service: resources::service(app, owner)?,
        route: resources::http_route(app, platform, owner)?,
        exposes_http: matches!(resources::web_process(app)?, Some((_, p)) if p.protocol.is_http()),
    })
}

#[allow(clippy::needless_pass_by_value)] // signature required by `Controller::run`
async fn reconcile(app: Arc<App>, ctx: Arc<Ctx>) -> Result<Action> {
    let ns = app.namespace().ok_or(Error::Missing("metadata.namespace"))?;
    let owner = app
        .controller_owner_ref(&())
        .ok_or(Error::Missing("metadata.uid"))?;
    let platform = ctx.platform();
    let apps = Api::<App>::namespaced(ctx.client.clone(), &ns);
    let generation = app.metadata.generation;
    let previous = app.status.as_ref().map_or(&[][..], |s| s.conditions.as_slice());

    let desired = match build(&app, &platform, &owner) {
        Ok(d) => d,
        Err(e) => {
            // A spec problem: report it and wait for the user to change the App.
            let ready = condition(previous, READY, false, e.reason(), &e.to_string(), generation);
            write_status(&apps, &app, None, vec![ready]).await?;
            ctx.succeeded(app.as_ref());
            return Ok(Action::await_change());
        }
    };

    let client = &ctx.client;
    let selector = format!("{}={}", labels::APP, app.name_any());
    // Volumes first (pods wait for their claims). Never pruned: data outlives
    // spec edits and the App itself (scenario 6).
    let pvcs = Api::<PersistentVolumeClaim>::namespaced(client.clone(), &ns);
    apply_all(&pvcs, &desired.volumes).await?;

    let deployments = Api::<Deployment>::namespaced(client.clone(), &ns);
    apply_all(&deployments, &desired.deployments).await?;
    prune(&deployments, &selector, &owner.uid, &desired.deployments).await?;

    let hpas = Api::<HorizontalPodAutoscaler>::namespaced(client.clone(), &ns);
    apply_all(&hpas, &desired.autoscalers).await?;
    prune(&hpas, &selector, &owner.uid, &desired.autoscalers).await?;

    let crons = Api::<CronJob>::namespaced(client.clone(), &ns);
    apply_all(&crons, &desired.cron_jobs).await?;
    prune(&crons, &selector, &owner.uid, &desired.cron_jobs).await?;

    let services = Api::<Service>::namespaced(client.clone(), &ns);
    match &desired.service {
        Some(svc) => apply_all(&services, std::slice::from_ref(svc)).await?,
        None => delete_if_exists(&services, &app.name_any()).await?,
    }

    let routed = sync_route(client, &ns, &app.name_any(), desired.route.as_ref()).await?;
    let (ready, mut reason, message) = rollout(&deployments, &desired.deployments).await?;
    if ready && desired.deployments.is_empty() && !desired.cron_jobs.is_empty() {
        reason = "Scheduled";
    }

    let mut conditions = vec![condition(previous, READY, ready, reason, &message, generation)];
    if desired.exposes_http {
        let (ok, reason, msg) = match (&desired.route, routed, &platform.gateway) {
            (Some(_), true, _) => (true, "RouteApplied", ""),
            (Some(_), false, _) => (
                false,
                "GatewayAPIMissing",
                "the Gateway API CRDs are not installed",
            ),
            (None, _, None) => (
                false,
                "NoGateway",
                "set spec.gateway in KubenConfig to expose apps",
            ),
            (None, _, Some(_)) => (
                false,
                "NoHostname",
                "add a domain or set spec.baseDomain in KubenConfig",
            ),
        };
        conditions.push(condition(previous, EXPOSED, ok, reason, msg, generation));
    }
    let url = routed.then(|| resources::url(&app, &platform)).flatten();
    write_status(&apps, &app, url, conditions).await?;

    ctx.succeeded(app.as_ref());
    // Owned Deployments re-trigger us on rollout progress; the timer is a safety net.
    Ok(Action::requeue(if ready {
        Duration::from_mins(10)
    } else {
        Duration::from_secs(30)
    }))
}

async fn apply_all<K>(api: &Api<K>, objects: &[K]) -> Result<()>
where
    K: Resource + Clone + Serialize + DeserializeOwned + Debug,
{
    let pp = PatchParams::apply(FIELD_MANAGER).force();
    for obj in objects {
        let name = obj
            .meta()
            .name
            .as_deref()
            .ok_or(Error::Missing("metadata.name"))?;
        api.patch(name, &pp, &Patch::Apply(obj)).await?;
    }
    Ok(())
}

/// Delete children of this App (by owner uid) that are no longer desired.
async fn prune<K>(api: &Api<K>, selector: &str, owner_uid: &str, keep: &[K]) -> Result<()>
where
    K: Resource + Clone + DeserializeOwned + Debug,
{
    let keep: BTreeSet<&str> = keep.iter().filter_map(|o| o.meta().name.as_deref()).collect();
    for obj in api.list(&ListParams::default().labels(selector)).await?.items {
        let owned = obj.owner_references().iter().any(|r| r.uid == owner_uid);
        let name = obj.name_any();
        if owned && !keep.contains(name.as_str()) {
            delete_if_exists(api, &name).await?;
            tracing::info!(%name, "pruned object removed from the app spec");
        }
    }
    Ok(())
}

async fn delete_if_exists<K>(api: &Api<K>, name: &str) -> Result<()>
where
    K: Resource + Clone + DeserializeOwned + Debug,
{
    match api.delete(name, &DeleteParams::background()).await {
        Ok(_) => Ok(()),
        Err(e) if is_not_found(&e) => Ok(()),
        Err(e) => Err(e.into()),
    }
}

/// Apply or remove the HTTPRoute. `Ok(false)` when there is nothing routed
/// (no route desired, or the Gateway API CRDs are missing).
async fn sync_route(
    client: &Client,
    ns: &str,
    name: &str,
    route: Option<&serde_json::Value>,
) -> Result<bool> {
    let gvk = GroupVersionKind::gvk("gateway.networking.k8s.io", "v1", "HTTPRoute");
