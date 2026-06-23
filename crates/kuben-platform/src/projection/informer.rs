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
where
    K: Resource + Clone + DeserializeOwned + Debug + Send + 'static,
    K::DynamicType: Default,
    F: FnMut(Event<K>) + Send,
{
    let stream = watcher(api, cfg)
        .default_backoff()
        .modify(|obj| obj.managed_fields_mut().clear())
        .map_ok(|ev| match ev {
            watcher::Event::Init => Event::Init,
            watcher::Event::InitApply(o) => Event::InitApply(o),
            watcher::Event::InitDone => Event::InitDone,
            watcher::Event::Apply(o) => Event::Apply(o),
            watcher::Event::Delete(o) => Event::Delete(o),
        });
    let mut stream = std::pin::pin!(stream.take_until(token.cancelled_owned()));
    while let Some(ev) = stream.try_next().await? {
        on_event(ev);
    }
    Ok(())
}

/// Backend-agnostic watch event (mirrors `kube::runtime::watcher::Event`).
#[derive(Debug)]
pub enum Event<K> {
    Init,
    InitApply(K),
    InitDone,
    Apply(K),
    Delete(K),
}

async fn watch_pods(
    client: Client,
    projections: Arc<Projections>,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let api = Api::<Pod>::all(client);
    let cfg = watcher::Config::default()
        .labels(labels::MANAGED_SELECTOR)
        .page_size(PAGE_SIZE);
    let mut staging: Vec<PodView> = Vec::new();
    watch_kind(api, cfg, token, move |ev| match ev {
        Event::Init => staging.clear(),
        Event::InitApply(p) => staging.push(PodView::from(&p)),
        Event::InitDone => {
            tracing::info!(pods = staging.len(), "pod informer synced");
            projections.replace_pods(std::mem::take(&mut staging));
        }
        Event::Apply(p) => projections.upsert_pod(PodView::from(&p)),
        Event::Delete(p) => {
            projections.remove_pod(&format!("{}/{}", p.namespace().unwrap_or_default(), p.name_any()));
        }
    })
    .await
}

async fn watch_projects(
    client: Client,
    projections: Arc<Projections>,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let api = Api::<Project>::all(client);
    let cfg = watcher::Config::default().page_size(PAGE_SIZE);
    let mut staging: Vec<ProjectView> = Vec::new();
    watch_kind(api, cfg, token, move |ev| match ev {
        Event::Init => staging.clear(),
        Event::InitApply(p) => staging.push(ProjectView::from(&p)),
        Event::InitDone => {
            tracing::info!(projects = staging.len(), "project informer synced");
            projections.replace_projects(std::mem::take(&mut staging));
        }
        Event::Apply(p) => projections.upsert_project(ProjectView::from(&p)),
        Event::Delete(p) => projections.remove_project(&p.name_any()),
    })
    .await
}

async fn watch_environments(
    client: Client,
    projections: Arc<Projections>,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let api = Api::<Environment>::all(client);
    let cfg = watcher::Config::default().page_size(PAGE_SIZE);
    let mut staging: Vec<EnvironmentView> = Vec::new();
    watch_kind(api, cfg, token, move |ev| match ev {
        Event::Init => staging.clear(),
        Event::InitApply(e) => staging.push(EnvironmentView::from(&e)),
        Event::InitDone => {
