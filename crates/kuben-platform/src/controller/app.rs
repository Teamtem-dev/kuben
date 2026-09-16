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

/// Everything an App's spec makes, before it is applied. The renderer
/// (`crate::render`) freezes the same objects into a RenderPlan.
pub(crate) struct Desired {
    pub(crate) deployments: Vec<Deployment>,
    pub(crate) autoscalers: Vec<HorizontalPodAutoscaler>,
    pub(crate) cron_jobs: Vec<CronJob>,
    pub(crate) volumes: Vec<PersistentVolumeClaim>,
    pub(crate) service: Option<Service>,
    pub(crate) route: Option<serde_json::Value>,
    /// The web process is HTTP and should be reachable through the gateway.
    pub(crate) exposes_http: bool,
}

pub(crate) fn build(app: &App, platform: &Platform, owner: &OwnerReference) -> Result<Desired, BuildError> {
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
/// An App this controller no longer writes: one being deleted (it has no
/// finalizer, so there is nothing to clean up), or one handed over to the
/// cluster's agent (M1.9). Writing its workloads now would give them back an
/// owner that is going away, and the garbage collector would take them along.
pub(crate) fn hands_off(app: &App) -> bool {
    app.metadata.deletion_timestamp.is_some()
        || app
            .metadata
            .annotations
            .as_ref()
            .is_some_and(|a| a.contains_key(crate::materializer::render::annotations::HANDOVER))
}

async fn reconcile(app: Arc<App>, ctx: Arc<Ctx>) -> Result<Action> {
    if hands_off(&app) {
        return Ok(Action::await_change());
    }
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
                "set spec.gatewayClassName (or spec.gateway) in KubenConfig to expose apps",
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
    let resource = ApiResource::from_gvk_with_plural(&gvk, "httproutes");
    let api: Api<DynamicObject> = Api::namespaced_with(client.clone(), ns, &resource);
    let Some(body) = route else {
        delete_if_exists(&api, name).await?;
        return Ok(false);
    };
    match api
        .patch(
            name,
            &PatchParams::apply(FIELD_MANAGER).force(),
            &Patch::Apply(body),
        )
        .await
    {
        Ok(_) => Ok(true),
        Err(e) if is_not_found(&e) => Ok(false),
        Err(e) => Err(e.into()),
    }
}

/// `(ready, reason, message)` from the live Deployments.
async fn rollout(api: &Api<Deployment>, desired: &[Deployment]) -> Result<(bool, &'static str, String)> {
    let mut waiting = Vec::new();
    for d in desired {
        let name = d.metadata.name.as_deref().unwrap_or_default();
        let Some(live) = api.get_opt(name).await? else {
            waiting.push(format!("{name}: not created yet"));
            continue;
        };
        let want = live.spec.as_ref().and_then(|s| s.replicas).unwrap_or(1);
        let Some(st) = live.status.as_ref() else {
            waiting.push(format!("{name}: pending"));
            continue;
        };
        let deadline_exceeded = st.conditions.as_ref().is_some_and(|cs| {
            cs.iter()
                .any(|c| c.type_ == "Progressing" && c.reason.as_deref() == Some("ProgressDeadlineExceeded"))
        });
        if deadline_exceeded {
            return Ok((
                false,
                "RolloutFailed",
                format!("{name}: rollout exceeded its progress deadline"),
            ));
        }
        let observed = st.observed_generation.unwrap_or(0) >= live.metadata.generation.unwrap_or(0);
        let updated = st.updated_replicas.unwrap_or(0);
        let available = st.available_replicas.unwrap_or(0);
        let total = st.replicas.unwrap_or(0);
        if !observed || updated < want || available < want || total > updated {
            waiting.push(format!("{name}: {available}/{want} available"));
        }
    }
    Ok(if waiting.is_empty() {
        (true, "Available", String::new())
    } else {
        (false, "Progressing", waiting.join("; "))
    })
}

async fn write_status(
    api: &Api<App>,
    app: &App,
    url: Option<String>,
    conditions: Vec<Condition>,
) -> Result<()> {
    let status = AppStatus {
        observed_generation: app.metadata.generation,
        current_release: None,
        url,
        conditions,
    };
    let patch = json!({ "apiVersion": "kuben.dev/v1alpha1", "kind": "App", "status": status });
    api.patch_status(
        &app.name_any(),
        &PatchParams::apply(FIELD_MANAGER).force(),
        &Patch::Apply(&patch),
    )
    .await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_deleting_or_handed_over_app_is_left_alone() {
        let app = |annotations: serde_json::Value, deleting: bool| -> App {
            let mut value = serde_json::json!({
                "apiVersion": "kuben.dev/v1alpha1",
                "kind": "App",
                "metadata": { "name": "web", "namespace": "kb-shop-prod", "annotations": annotations },
                "spec": {
                    "source": { "image": "nginx:1.27" },
                    "runtime": { "processes": { "web": { "port": 80 } } }
                }
            });
            if deleting {
                value["metadata"]["deletionTimestamp"] = "2026-09-15T00:00:00Z".into();
            }
            serde_json::from_value(value).expect("app")
        };
        assert!(!hands_off(&app(serde_json::json!({}), false)));
        assert!(hands_off(&app(serde_json::json!({}), true)), "being deleted");
        assert!(
            hands_off(&app(serde_json::json!({ "kuben.dev/handover": "t" }), false)),
            "handed over to the agent"
        );
    }
}
