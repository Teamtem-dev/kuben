//! Reconcilers: level-triggered and idempotent, writing with server-side
//! apply under the field manager `kuben`. A failing object backs off on its
//! own (per-object exponential backoff) without slowing the others.

pub mod app;
pub mod crd_apply;
pub mod environment;
pub mod gateway;
pub mod project;
pub mod resources;

use std::{sync::Arc, time::Duration};

use dashmap::DashMap;
use futures::StreamExt;
use kube::{
    Api, Client, Resource,
    runtime::{WatchStreamExt, controller::Action, reflector, watcher},
};
use kuben_crd::{Condition, KubenConfig};
pub use resources::Platform;
use tokio_util::sync::CancellationToken;

use crate::{health::Health, projection::Projections, registry::ClusterRegistry};

/// Name of the cluster-scoped `KubenConfig` singleton.
pub const KUBEN_CONFIG_NAME: &str = "kuben";

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("kubernetes API: {0}")]
    Kube(#[from] kube::Error),
    #[error("object has no {0}")]
    Missing(&'static str),
}

pub type Result<T, E = Error> = std::result::Result<T, E>;

/// State shared by all reconcilers.
pub struct Ctx {
    pub client: Client,
    config: reflector::Store<KubenConfig>,
    failures: DashMap<String, u32>,
}

impl std::fmt::Debug for Ctx {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Ctx")
            .field("failing_objects", &self.failures.len())
            .finish_non_exhaustive()
    }
}

impl Ctx {
    #[must_use]
    pub fn new(client: Client, config: reflector::Store<KubenConfig>) -> Self {
        Self {
            client,
            config,
            failures: DashMap::new(),
        }
    }

    /// Platform settings from the `KubenConfig` singleton, or defaults.
    #[must_use]
    pub fn platform(&self) -> Platform {
        let configs = self.config.state();
        let chosen = configs
            .iter()
            .find(|c| c.metadata.name.as_deref() == Some(KUBEN_CONFIG_NAME))
            .or_else(|| configs.first());
        Platform::from_spec(chosen.map(|c| &c.spec))
    }

    fn key<K: Resource<DynamicType = ()>>(obj: &K) -> String {
        let meta = obj.meta();
        format!(
            "{}/{}/{}",
            K::kind(&()),
            meta.namespace.as_deref().unwrap_or_default(),
            meta.name.as_deref().unwrap_or_default()
        )
    }

    /// Reset the object's backoff after a successful reconcile.
    pub fn succeeded<K: Resource<DynamicType = ()>>(&self, obj: &K) {
        self.failures.remove(&Self::key(obj));
    }

    fn failed<K: Resource<DynamicType = ()>>(&self, obj: &K) -> u32 {
        let mut n = self.failures.entry(Self::key(obj)).or_insert(0);
        *n = n.saturating_add(1);
        *n
    }
}

/// 2 s, 4 s, 8 s … capped at 5 minutes.
#[must_use]
pub fn backoff(failures: u32) -> Duration {
    Duration::from_secs(2_u64.saturating_pow(failures.clamp(1, 16))).min(Duration::from_mins(5))
}

/// Shared error policy: log, count, and requeue with per-object backoff.
#[allow(clippy::needless_pass_by_value)] // signature required by `Controller::run`
pub fn error_policy<K: Resource<DynamicType = ()>>(obj: Arc<K>, err: &Error, ctx: Arc<Ctx>) -> Action {
    let failures = ctx.failed(obj.as_ref());
    let delay = backoff(failures);
    let kind = K::kind(&()).to_string();
    let meta = obj.meta();
    tracing::warn!(
        %kind,
        namespace = meta.namespace.as_deref().unwrap_or_default(),
        name = meta.name.as_deref().unwrap_or_default(),
        error = %err,
        failures,
        retry_in_s = delay.as_secs(),
        "reconcile failed"
    );
    metrics::counter!("kuben_reconcile_errors_total", "kind" => kind).increment(1);
    Action::requeue(delay)
}

pub(crate) fn is_not_found(err: &kube::Error) -> bool {
    matches!(err, kube::Error::Api(status) if status.code == 404)
}

fn now_rfc3339() -> String {
    k8s_openapi::jiff::Timestamp::now()
        .strftime("%Y-%m-%dT%H:%M:%SZ")
        .to_string()
}

/// Build a condition, keeping `lastTransitionTime` while the status is unchanged
/// (so an unchanged reconcile writes an identical status: no watch churn).
#[must_use]
pub fn condition(
    previous: &[Condition],
    type_: &str,
    status: bool,
    reason: &str,
    message: &str,
    generation: Option<i64>,
) -> Condition {
    let status = if status { "True" } else { "False" };
    let last_transition_time = previous
        .iter()
        .find(|c| c.type_ == type_ && c.status == status)
        .and_then(|c| c.last_transition_time.clone())
        .or_else(|| Some(now_rfc3339()));
    Condition {
        type_: type_.into(),
        status: status.into(),
        reason: Some(reason.into()),
        message: (!message.is_empty()).then(|| message.to_owned()),
        last_transition_time,
        observed_generation: generation,
    }
}

/// Apply CRDs, then run every controller until cancelled. Supervised by the
/// caller: any controller failing restarts the set with backoff.
pub async fn run_all(
    registry: ClusterRegistry,
    projections: Arc<Projections>,
    health: Health,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let client = registry.primary();
    crd_apply::ensure(client.clone()).await?;

    // Platform config is mirrored in memory; a change re-reconciles every App.
    let (config, writer) = reflector::store::<KubenConfig>();
    let config_changes = reflector(
        writer,
        watcher(
            Api::<KubenConfig>::all(client.clone()),
            watcher::Config::default(),
        ),
    )
    .default_backoff()
    .applied_objects()
    .filter_map(|r| std::future::ready(r.ok().map(|_| ())));
    let ctx = Arc::new(Ctx::new(client, config));

    health.ok("controllers");
    tokio::try_join!(
        project::run(ctx.clone(), token.clone()),
        environment::run(ctx.clone(), token.clone()),
        gateway::run(ctx.clone(), projections, token.clone()),
        app::run(ctx, config_changes, token),
    )?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn backoff_grows_and_caps() {
        assert_eq!(backoff(0), Duration::from_secs(2));
        assert_eq!(backoff(1), Duration::from_secs(2));
        assert_eq!(backoff(3), Duration::from_secs(8));
        assert_eq!(backoff(30), Duration::from_mins(5));
    }

    #[test]
    fn condition_keeps_transition_time_while_unchanged() {
        let first = condition(&[], "Ready", true, "Available", "", Some(1));
        let again = condition(
            std::slice::from_ref(&first),
            "Ready",
            true,
            "Available",
            "",
            Some(2),
        );
        assert_eq!(first.last_transition_time, again.last_transition_time);
        assert_eq!(again.observed_generation, Some(2));
        let flipped = condition(&[first], "Ready", false, "Progressing", "0/1 available", Some(2));
        assert_eq!(flipped.status, "False");
        assert_eq!(flipped.message.as_deref(), Some("0/1 available"));
    }
}
