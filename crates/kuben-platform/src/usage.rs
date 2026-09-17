//! Bounded resource usage (M5.5; plan §14.5): CPU and memory of Kuben's
//! apps from the Kubernetes Metrics API.
//!
//! - **Live window.** Each replica keeps the last hour in memory: at most
//!   [`SAMPLES_PER_SERIES`] samples for each of at most [`MAX_SERIES`] apps.
//!   The least recently updated series goes first when the budget is full.
//! - **Rollups.** Hourly averages and maxima go to SQL and are kept for a
//!   week (the retention pass removes older rows).
//! - **Missing data.** A window without samples is *unavailable*, never
//!   zero. That covers a cluster without the Metrics API, a fresh replica
//!   and an app that runs no pods.

use std::{
    collections::{HashMap, VecDeque},
    sync::{Arc, Mutex, PoisonError},
    time::Duration,
};

use kube::{
    Api, Client, ResourceExt,
    api::{ApiResource, DynamicObject, GroupVersionKind, ListParams},
};
use kuben_core::{
    capacity::{bytes, cpu_millis},
    time::now_ms,
};
use kuben_crd::labels;
use serde::Serialize;
use serde_json::Value;
use tokio_util::sync::CancellationToken;

use crate::health::Health;

/// Samples kept per app: an hour at the scrape interval.
pub const SAMPLES_PER_SERIES: usize = 120;
/// Apps tracked at most.
pub const MAX_SERIES: usize = 5_000;
/// How often the Metrics API is read.
pub const SCRAPE: Duration = Duration::from_secs(30);
const HOUR_MS: i64 = 3_600_000;
const HEALTH: &str = "usage";

/// One app's usage at one moment, summed over its pods.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Sample {
    /// Unix milliseconds.
    pub at: i64,
    pub cpu_millis: u64,
    pub memory_bytes: u64,
    pub pods: u32,
}

/// An app, as its pods are labelled.
#[derive(Clone, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct SeriesKey {
    pub namespace: String,
    pub app: String,
    /// The organization label of its pods.
    pub org: String,
}

#[derive(Debug, Default)]
struct Series {
    samples: VecDeque<Sample>,
    touched: i64,
}

/// The live usage of a replica.
#[derive(Debug, Default)]
pub struct UsageBuffer {
    series: Mutex<HashMap<SeriesKey, Series>>,
    /// Why the last scrape failed, if it did.
    unavailable: Mutex<Option<String>>,
}

impl UsageBuffer {
    #[must_use]
    pub fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }

    /// Add `sample` to `key`'s series, within the budgets.
    pub fn record(&self, key: SeriesKey, sample: Sample) {
        let mut series = self.series.lock().unwrap_or_else(PoisonError::into_inner);
        if !series.contains_key(&key)
            && series.len() >= MAX_SERIES
            && let Some(oldest) = series
                .iter()
                .min_by_key(|(_, s)| s.touched)
                .map(|(k, _)| k.clone())
        {
            series.remove(&oldest);
        }
        let entry = series.entry(key).or_default();
        entry.samples.push_back(sample);
        while entry.samples.len() > SAMPLES_PER_SERIES {
            entry.samples.pop_front();
        }
        entry.touched = sample.at;
    }

    /// Forget samples older than an hour, and series left empty.
    pub fn trim(&self, now: i64) {
        let mut series = self.series.lock().unwrap_or_else(PoisonError::into_inner);
        for s in series.values_mut() {
            while s.samples.front().is_some_and(|x| x.at < now - HOUR_MS) {
                s.samples.pop_front();
            }
        }
        series.retain(|_, s| !s.samples.is_empty());
    }

    /// `namespace/app`'s samples since `since`, oldest first.
    #[must_use]
    pub fn window(&self, namespace: &str, app: &str, since: i64) -> Vec<Sample> {
        let series = self.series.lock().unwrap_or_else(PoisonError::into_inner);
        series
            .iter()
            .find(|(k, _)| k.namespace == namespace && k.app == app)
            .map(|(_, s)| s.samples.iter().filter(|x| x.at >= since).copied().collect())
            .unwrap_or_default()
    }

    /// Every series' samples within `[from, to)`.
    #[must_use]
    pub fn between(&self, from: i64, to: i64) -> Vec<(SeriesKey, Vec<Sample>)> {
        let series = self.series.lock().unwrap_or_else(PoisonError::into_inner);
        series
            .iter()
            .map(|(k, s)| {
                let samples = s
                    .samples
                    .iter()
                    .filter(|x| x.at >= from && x.at < to)
                    .copied()
                    .collect();
                (k.clone(), samples)
            })
            .filter(|(_, s): &(SeriesKey, Vec<Sample>)| !s.is_empty())
            .collect()
    }

    /// The number of series kept.
    #[must_use]
    pub fn len(&self) -> usize {
        self.series.lock().unwrap_or_else(PoisonError::into_inner).len()
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Why usage is unavailable right now, if it is.
    #[must_use]
    pub fn unavailable(&self) -> Option<String> {
        self.unavailable
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .clone()
    }

    fn set_unavailable(&self, why: Option<String>) {
        *self.unavailable.lock().unwrap_or_else(PoisonError::into_inner) = why;
    }
}

/// An hour of one app's usage.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Rollup {
    /// The hour's start, Unix milliseconds.
    pub hour: i64,
    pub cpu_avg: u64,
    pub cpu_max: u64,
    pub memory_avg: u64,
    pub memory_max: u64,
    pub samples: u32,
}

/// The rollup of `samples` of the hour starting at `hour`; `None` without
/// any.
#[must_use]
pub fn rollup(hour: i64, samples: &[Sample]) -> Option<Rollup> {
    let n = u64::try_from(samples.len()).ok().filter(|n| *n > 0)?;
    let (cpu, memory): (u64, u64) = samples.iter().fold((0, 0), |(c, m), s| {
        (c.saturating_add(s.cpu_millis), m.saturating_add(s.memory_bytes))
    });
    Some(Rollup {
        hour,
        cpu_avg: cpu / n,
        cpu_max: samples.iter().map(|s| s.cpu_millis).max().unwrap_or(0),
        memory_avg: memory / n,
        memory_max: samples.iter().map(|s| s.memory_bytes).max().unwrap_or(0),
        samples: u32::try_from(n).unwrap_or(u32::MAX),
    })
}

/// The start of the hour `at` is in.
#[must_use]
pub const fn hour_of(at: i64) -> i64 {
    at - at.rem_euclid(HOUR_MS)
}

/// Sum the containers of each pod of a `PodMetricsList` into one sample per
/// app. Pods without Kuben's app label are skipped.
#[must_use]
pub fn summarize(items: &[Value], at: i64) -> Vec<(SeriesKey, Sample)> {
    let mut sums: HashMap<SeriesKey, Sample> = HashMap::new();
    for item in items {
        let meta = &item["metadata"];
        let (Some(namespace), Some(app), Some(org)) = (
            meta["namespace"].as_str(),
            meta["labels"][labels::APP].as_str(),
            meta["labels"][labels::ORG].as_str(),
        ) else {
            continue;
        };
        let key = SeriesKey {
            namespace: namespace.to_owned(),
            app: app.to_owned(),
            org: org.to_owned(),
        };
        let entry = sums.entry(key).or_insert(Sample {
            at,
            cpu_millis: 0,
            memory_bytes: 0,
            pods: 0,
        });
        entry.pods += 1;
        for c in item["containers"].as_array().into_iter().flatten() {
            let usage = &c["usage"];
            entry.cpu_millis += usage["cpu"].as_str().and_then(cpu_nanos_to_millis).unwrap_or(0);
            entry.memory_bytes += usage["memory"].as_str().and_then(bytes).unwrap_or(0);
        }
    }
    let mut out: Vec<_> = sums.into_iter().collect();
    out.sort_by(|a, b| a.0.cmp(&b.0));
    out
}

/// The Metrics API reports CPU in nanocores (`123456n`) or microcores
/// (`123u`) as often as in cores or millicores.
fn cpu_nanos_to_millis(q: &str) -> Option<u64> {
    if let Some(n) = q.strip_suffix('n') {
        return n.parse::<u64>().ok().map(|n| n / 1_000_000);
    }
    if let Some(u) = q.strip_suffix('u') {
        return u.parse::<u64>().ok().map(|u| u / 1_000);
    }
    cpu_millis(q)
}

/// Read every Kuben pod's usage once.
pub async fn scrape(client: &Client) -> kube::Result<Vec<(SeriesKey, Sample)>> {
    let gvk = GroupVersionKind::gvk("metrics.k8s.io", "v1beta1", "PodMetrics");
    let resource = ApiResource::from_gvk_with_plural(&gvk, "pods");
    let api = Api::<DynamicObject>::all_with(client.clone(), &resource);
    let list = api
        .list(&ListParams::default().labels(labels::MANAGED_SELECTOR))
        .await?;
    let items: Vec<Value> = list
        .items
        .iter()
        .filter_map(|o| {
            let mut v = serde_json::to_value(o).ok()?;
            v["metadata"]["namespace"] = Value::from(o.namespace());
            Some(v)
        })
        .collect();
    Ok(summarize(&items, now_ms()))
}

/// Where hourly rollups go (the store, in the server).
#[async_trait::async_trait]
pub trait RollupSink: Send + Sync {
    /// Keep the rollups of `key`; a repeated hour keeps the larger values.
    async fn keep(&self, key: &SeriesKey, rollup: Rollup) -> anyhow::Result<()>;
}

/// Scrape into `buffer` until `token` is cancelled; after each full hour,
/// hand that hour's rollups to `sink`.
pub async fn run(
    client: Client,
    buffer: Arc<UsageBuffer>,
    sink: Arc<dyn RollupSink>,
    health: Health,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let mut rolled = hour_of(now_ms());
    loop {
        match scrape(&client).await {
            Ok(samples) => {
                for (key, sample) in samples {
                    buffer.record(key, sample);
                }
                buffer.set_unavailable(None);
                health.ok(HEALTH);
            }
            Err(e) => {
                // A missing Metrics API is a fact of the cluster, not a fault.
                buffer.set_unavailable(Some(format!("the Metrics API is unavailable: {e}")));
                health.ok(HEALTH);
            }
        }
        let now = now_ms();
        buffer.trim(now);
        let hour = hour_of(now);
        if hour > rolled {
            let mut from = rolled;
            while from < hour {
                for (key, samples) in buffer.between(from, from + HOUR_MS) {
                    if let Some(r) = rollup(from, &samples)
                        && let Err(e) = sink.keep(&key, r).await
                    {
                        tracing::warn!(error = %e, app = %key.app, "a usage rollup was not kept");
                    }
                }
                from += HOUR_MS;
            }
            rolled = hour;
        }
        tokio::select! {
            () = token.cancelled() => return Ok(()),
            () = tokio::time::sleep(SCRAPE) => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    fn key(app: &str) -> SeriesKey {
        SeriesKey {
            namespace: "kb-shop-prod".into(),
            app: app.into(),
            org: "o".into(),
        }
    }

    fn sample(at: i64, cpu: u64) -> Sample {
        Sample {
            at,
            cpu_millis: cpu,
            memory_bytes: cpu * 1_000,
            pods: 1,
        }
    }

    #[test]
    fn series_are_bounded() {
        let buffer = UsageBuffer::default();
        for i in 0..200 {
            buffer.record(key("web"), sample(i, 1));
        }
        assert_eq!(buffer.window("kb-shop-prod", "web", 0).len(), SAMPLES_PER_SERIES);
        assert_eq!(
            buffer.window("kb-shop-prod", "web", 0)[0].at,
            80,
            "the oldest go first"
        );
        assert!(
            buffer.window("kb-shop-prod", "other", 0).is_empty(),
            "nothing is not zero"
        );
        buffer.trim(HOUR_MS + 150);
        assert_eq!(buffer.window("kb-shop-prod", "web", 0).len(), 50);
        buffer.trim(10 * HOUR_MS);
        assert!(buffer.is_empty());
    }

    #[test]
    fn the_least_recent_series_makes_room() {
        let buffer = UsageBuffer::default();
        for i in 0..MAX_SERIES {
            buffer.record(
                key(&format!("app-{i}")),
                sample(i64::try_from(i).expect("i") + 1, 1),
            );
        }
        buffer.record(key("newcomer"), sample(1_000_000, 1));
        assert_eq!(buffer.len(), MAX_SERIES);
        assert!(buffer.window("kb-shop-prod", "app-0", 0).is_empty());
        assert_eq!(buffer.window("kb-shop-prod", "newcomer", 0).len(), 1);
    }

    #[test]
    fn rollups_average_and_keep_peaks() {
        let samples = [sample(10, 100), sample(20, 300), sample(30, 200)];
        let r = rollup(0, &samples).expect("rollup");
        assert_eq!((r.cpu_avg, r.cpu_max, r.samples), (200, 300, 3));
        assert_eq!((r.memory_avg, r.memory_max), (200_000, 300_000));
        assert_eq!(rollup(0, &[]), None);
        assert_eq!(hour_of(HOUR_MS + 5), HOUR_MS);
        let buffer = UsageBuffer::default();
        buffer.record(key("web"), sample(HOUR_MS - 1, 1));
        buffer.record(key("web"), sample(HOUR_MS + 1, 1));
        let first = buffer.between(0, HOUR_MS);
        assert_eq!(first[0].1.len(), 1);
    }

    #[test]
    fn pod_metrics_are_summed_per_app() {
        let pod = |name: &str, app: &str, cpu: &str, mem: &str| {
            json!({
                "metadata": { "name": name, "namespace": "kb-shop-prod",
                              "labels": { labels::APP: app, labels::ORG: "o" } },
                "containers": [{ "name": "c", "usage": { "cpu": cpu, "memory": mem } }]
            })
        };
        let items = [
            pod("a", "web", "250000000n", "64Mi"),
            pod("b", "web", "150m", "36Mi"),
            pod("c", "worker", "2500u", "1Gi"),
            json!({ "metadata": { "name": "x", "namespace": "kube-system", "labels": {} }, "containers": [] }),
        ];
        let sums = summarize(&items, 7);
        assert_eq!(sums.len(), 2);
        assert_eq!(
            (sums[0].0.app.as_str(), sums[0].1.cpu_millis, sums[0].1.pods),
            ("web", 400, 2)
        );
        assert_eq!(sums[0].1.memory_bytes, 100 << 20);
        assert_eq!((sums[1].1.cpu_millis, sums[1].1.memory_bytes), (2, 1 << 30));
    }
}
