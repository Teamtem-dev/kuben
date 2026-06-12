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
