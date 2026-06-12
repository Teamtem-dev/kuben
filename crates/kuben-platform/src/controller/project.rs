//! Project status: number of live environments + `Ready`.

use std::{sync::Arc, time::Duration};

use futures::StreamExt;
use kube::{
    Api, ResourceExt,
    api::{ListParams, Patch, PatchParams},
    runtime::{Controller, controller::Action, reflector::ObjectRef, watcher},
};
use kuben_crd::{Environment, FIELD_MANAGER, Project, ProjectStatus, condition::READY};
use serde_json::json;
use tokio_util::sync::CancellationToken;

use super::{Ctx, Result, condition, error_policy};

pub async fn run(ctx: Arc<Ctx>, token: CancellationToken) -> anyhow::Result<()> {
    let client = ctx.client.clone();
    Controller::new(Api::<Project>::all(client.clone()), watcher::Config::default())
        .watches(
            Api::<Environment>::all(client),
            watcher::Config::default(),
            |env| Some(ObjectRef::<Project>::new(&env.spec.project)),
        )
        .graceful_shutdown_on(token.cancelled_owned())
        .run(reconcile, error_policy, ctx)
        .for_each(|_| std::future::ready(()))
        .await;
    Ok(())
}

#[allow(clippy::needless_pass_by_value)] // signature required by `Controller::run`
async fn reconcile(project: Arc<Project>, ctx: Arc<Ctx>) -> Result<Action> {
    let name = project.name_any();
    // Projects are few and environments per project fewer; a LIST is cheaper
    // than keeping a second cache in sync.
    let envs = Api::<Environment>::all(ctx.client.clone())
        .list(&ListParams::default())
        .await?;
    let count = envs
        .items
        .iter()
        .filter(|e| e.spec.project == name && e.metadata.deletion_timestamp.is_none())
        .count();
    let previous = project
        .status
        .as_ref()
        .map_or(&[][..], |s| s.conditions.as_slice());
    let status = ProjectStatus {
        observed_generation: project.metadata.generation,
        environments: u32::try_from(count).unwrap_or(u32::MAX),
        conditions: vec![condition(
            previous,
            READY,
            true,
            "Reconciled",
            "",
            project.metadata.generation,
        )],
    };
    let patch = json!({ "apiVersion": "kuben.dev/v1alpha1", "kind": "Project", "status": status });
    Api::<Project>::all(ctx.client.clone())
        .patch_status(
            &name,
            &PatchParams::apply(FIELD_MANAGER).force(),
            &Patch::Apply(&patch),
        )
        .await?;
    ctx.succeeded(project.as_ref());
    Ok(Action::requeue(Duration::from_mins(15)))
}
