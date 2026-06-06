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
