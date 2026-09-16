//! Informers: one shared watch per kind, with backoff, label selectors and
//! `managedFields` stripped, feeding [`Projections`].

use std::{fmt::Debug, sync::Arc, time::Duration};

use futures::{StreamExt, TryStreamExt};
use k8s_openapi::api::core::v1::Pod;
use kube::{
    Api, Client, Resource, ResourceExt,
    api::{ApiResource, DynamicObject, GroupVersionKind},
    runtime::{WatchStreamExt, watcher},
};
use kuben_crd::{App, Environment, Project, labels};
use serde::de::DeserializeOwned;
use tokio_util::sync::CancellationToken;

use super::{AppView, CertificateView, EnvironmentView, PodView, ProjectView, Projections, RouteView};
use crate::{
    controller::gateway::TLS_SECRET_PREFIX,
    discovery::{CERT_MANAGER_GROUP, GATEWAY_GROUP},
    registry::ClusterRegistry,
};

/// Page size for the initial LIST. Keeps CPU bursts bounded on large clusters.
const PAGE_SIZE: u32 = 500;

/// How often an optional kind's API group is looked for until it appears.
const OPTIONAL_CHECK: Duration = Duration::from_mins(1);

/// How long the informers get for their first LIST before they are started
/// over. A request that hangs on an API server that has only just come up
/// would otherwise keep Kuben unready until the client's read timeout,
/// minutes later.
const FIRST_SYNC_DEADLINE: Duration = Duration::from_secs(45);

/// Run all informers for the primary cluster until cancelled.
pub async fn run(
    registry: ClusterRegistry,
    projections: Arc<Projections>,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let client = registry.primary();
    let deadline = first_sync_deadline(projections.clone());
    tokio::select! {
        () = watch_pods(client.clone(), projections.clone(), token.child_token()) => Ok(()),
        () = watch_projects(client.clone(), projections.clone(), token.child_token()) => Ok(()),
        () = watch_environments(client.clone(), projections.clone(), token.child_token()) => Ok(()),
        () = watch_apps(client.clone(), projections.clone(), token.child_token()) => Ok(()),
        () = watch_routes(client.clone(), projections.clone(), token.child_token()) => Ok(()),
        () = watch_certificates(client, projections, token.child_token()) => Ok(()),
        e = deadline => Err(e),
        () = token.cancelled() => Ok(()),
    }
}

/// Resolves, with the informers still waiting, when the first LIST has not
/// completed within [`FIRST_SYNC_DEADLINE`]; never once it has.
async fn first_sync_deadline(projections: Arc<Projections>) -> anyhow::Error {
    tokio::select! {
        () = projections.wait_synced() => std::future::pending().await,
        () = tokio::time::sleep(FIRST_SYNC_DEADLINE) => anyhow::anyhow!(
            "no complete LIST within {}s for: {}",
            FIRST_SYNC_DEADLINE.as_secs(),
            projections.pending_kinds().join(", ")
        ),
    }
}

/// A generic watch loop. `on_event` receives every event; `Init*` events are
/// staged and swapped in at `InitDone` so readers never see a half-empty set.
async fn watch_kind<K, F>(api: Api<K>, cfg: watcher::Config, token: CancellationToken, mut on_event: F)
where
    K: Resource + Clone + DeserializeOwned + Debug + Send + 'static,
    K::DynamicType: Default,
    F: FnMut(Event<K>) + Send,
{
    let kind = K::kind(&K::DynamicType::default()).into_owned();
    watch_named(api, cfg, &kind, token, &mut on_event).await;
}

/// [`watch_kind`] for any object, named `kind` in the logs.
async fn watch_named<K, F>(
    api: Api<K>,
    cfg: watcher::Config,
    kind: &str,
    token: CancellationToken,
    on_event: &mut F,
) where
    K: Resource + Clone + DeserializeOwned + Debug + Send + 'static,
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
    while let Some(ev) = stream.next().await {
        match ev {
            Ok(ev) => on_event(ev),
            // The watcher lists again by itself, paced by the backoff. Ending
            // here would restart every informer, with a delay that grows on
            // each error: on a cluster that has just started, where the
            // first requests fail (a CRD not there yet), that kept Kuben
            // unready for minutes.
            Err(e) => tracing::warn!(kind = %kind, error = %e, "watch failed; retrying"),
        }
    }
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

async fn watch_pods(client: Client, projections: Arc<Projections>, token: CancellationToken) {
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
    .await;
}

async fn watch_projects(client: Client, projections: Arc<Projections>, token: CancellationToken) {
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
    .await;
}

async fn watch_environments(client: Client, projections: Arc<Projections>, token: CancellationToken) {
    let api = Api::<Environment>::all(client);
    let cfg = watcher::Config::default().page_size(PAGE_SIZE);
    let mut staging: Vec<EnvironmentView> = Vec::new();
    watch_kind(api, cfg, token, move |ev| match ev {
        Event::Init => staging.clear(),
        Event::InitApply(e) => staging.push(EnvironmentView::from(&e)),
        Event::InitDone => {
            tracing::info!(environments = staging.len(), "environment informer synced");
            projections.replace_environments(std::mem::take(&mut staging));
        }
        Event::Apply(e) => projections.upsert_environment(EnvironmentView::from(&e)),
        Event::Delete(e) => projections.remove_environment(&e.name_any()),
    })
    .await;
}

async fn watch_apps(client: Client, projections: Arc<Projections>, token: CancellationToken) {
    let api = Api::<App>::all(client);
    let cfg = watcher::Config::default().page_size(PAGE_SIZE);
    let mut staging: Vec<AppView> = Vec::new();
    watch_kind(api, cfg, token, move |ev| match ev {
        Event::Init => staging.clear(),
        Event::InitApply(a) => staging.push(AppView::from(&a)),
        Event::InitDone => {
            tracing::info!(apps = staging.len(), "app informer synced");
            projections.replace_apps(std::mem::take(&mut staging));
        }
        Event::Apply(a) => projections.upsert_app(AppView::from(&a)),
        Event::Delete(a) => {
            projections.remove_app(&format!("{}/{}", a.namespace().unwrap_or_default(), a.name_any()));
        }
    })
    .await;
}

/// Watch a kind that may not be installed (Gateway API, cert-manager): look
/// for its API group every minute and watch once it is served. Optional kinds
/// never hold back readiness.
async fn watch_optional<F>(
    client: Client,
    resource: ApiResource,
    cfg: watcher::Config,
    token: CancellationToken,
    mut on_event: F,
) where
    F: FnMut(Event<DynamicObject>) + Send,
{
    loop {
        match client.list_api_groups().await {
            Ok(groups) if groups.groups.iter().any(|g| g.name == resource.group) => break,
            Ok(_) => {}
            Err(e) => tracing::debug!(error = %e, group = %resource.group, "cannot list API groups"),
        }
        tokio::select! {
            () = token.cancelled() => return,
            () = tokio::time::sleep(OPTIONAL_CHECK) => {}
        }
    }
    let kind = resource.kind.clone();
    let api = Api::<DynamicObject>::all_with(client, &resource);
    watch_named(api, cfg, &kind, token, &mut on_event).await;
}

async fn watch_routes(client: Client, projections: Arc<Projections>, token: CancellationToken) {
    let gvk = GroupVersionKind::gvk(GATEWAY_GROUP, "v1", "HTTPRoute");
    let resource = ApiResource::from_gvk_with_plural(&gvk, "httproutes");
    let cfg = watcher::Config::default()
        .labels(labels::MANAGED_SELECTOR)
        .page_size(PAGE_SIZE);
    let mut staging: Vec<RouteView> = Vec::new();
    watch_optional(client, resource, cfg, token, move |ev| match ev {
        Event::Init => staging.clear(),
        Event::InitApply(r) => staging.push(RouteView::from(&r)),
        Event::InitDone => {
            tracing::info!(routes = staging.len(), "route informer synced");
            projections.replace_routes(std::mem::take(&mut staging));
        }
        Event::Apply(r) => projections.upsert_route(RouteView::from(&r)),
        Event::Delete(r) => {
            projections.remove_route(&format!("{}/{}", r.namespace().unwrap_or_default(), r.name_any()));
        }
    })
    .await;
}

/// Only the certificates Kuben's gateway orders; a cluster may hold many
/// others.
async fn watch_certificates(client: Client, projections: Arc<Projections>, token: CancellationToken) {
    let gvk = GroupVersionKind::gvk(CERT_MANAGER_GROUP, "v1", "Certificate");
    let resource = ApiResource::from_gvk_with_plural(&gvk, "certificates");
    let cfg = watcher::Config::default().page_size(PAGE_SIZE);
    let ours = |c: &DynamicObject| c.name_any().starts_with(TLS_SECRET_PREFIX);
    let mut staging: Vec<CertificateView> = Vec::new();
    watch_optional(client, resource, cfg, token, move |ev| match ev {
        Event::Init => staging.clear(),
        Event::InitApply(c) if ours(&c) => staging.push(CertificateView::from(&c)),
        Event::InitDone => projections.replace_certificates(std::mem::take(&mut staging)),
        Event::Apply(c) if ours(&c) => projections.upsert_certificate(CertificateView::from(&c)),
        Event::Delete(c) if ours(&c) => {
            projections.remove_certificate(&format!(
                "{}/{}",
                c.namespace().unwrap_or_default(),
                c.name_any()
            ));
        }
        Event::InitApply(_) | Event::Apply(_) | Event::Delete(_) => {}
    })
    .await;
}
