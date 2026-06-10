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
