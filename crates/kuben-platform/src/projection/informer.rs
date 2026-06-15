//! Informers: one shared watch per kind, with backoff, label selectors and
//! `managedFields` stripped, feeding [`Projections`].

use std::{fmt::Debug, sync::Arc};

use futures::{StreamExt, TryStreamExt};
use k8s_openapi::api::core::v1::Pod;
use kube::{
    Api, Client, Resource, ResourceExt,
    runtime::{WatchStreamExt, watcher},
};
use kuben_crd::{App, Environment, Project, labels};
use serde::de::DeserializeOwned;
use tokio_util::sync::CancellationToken;

use super::{AppView, EnvironmentView, PodView, ProjectView, Projections};
use crate::registry::ClusterRegistry;

/// Page size for the initial LIST. Keeps CPU bursts bounded on large clusters.
const PAGE_SIZE: u32 = 500;

/// Run all informers for the primary cluster until cancelled.
pub async fn run(
    registry: ClusterRegistry,
    projections: Arc<Projections>,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let client = registry.primary();
    tokio::select! {
        r = watch_pods(client.clone(), projections.clone(), token.child_token()) => r,
        r = watch_projects(client.clone(), projections.clone(), token.child_token()) => r,
        r = watch_environments(client.clone(), projections.clone(), token.child_token()) => r,
        r = watch_apps(client, projections, token.child_token()) => r,
        () = token.cancelled() => Ok(()),
    }
}

/// A generic watch loop. `on_event` receives every event; `Init*` events are
/// staged and swapped in at `InitDone` so readers never see a half-empty set.
async fn watch_kind<K, F>(
    api: Api<K>,
    cfg: watcher::Config,
    token: CancellationToken,
    mut on_event: F,
) -> anyhow::Result<()>
