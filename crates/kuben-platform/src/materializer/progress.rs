//! What the App controller's status says about a written generation.
//!
//! Until M1.9 the existing controllers carry a run out: the App controller
//! reconciles the App object into workloads and reports
//! `status.observedGeneration` and a `Ready` condition. The materializer maps
//! that onto the run's phases.

use kuben_crd::{App, condition::READY};

/// The App controller's `Ready` reasons that no wait will change: a rollout
/// past its progress deadline, or a spec it cannot build (`BuildError`).
const FAILED: &[&str] = &[
    "RolloutFailed",
    "NoProcesses",
    "MultiplePorts",
    "UnknownSize",
    "AwaitingBuild",
    "InvalidSource",
    "VolumeNeedsSingleReplica",
    "ScheduledWithPort",
    "InvalidSchedule",
];

/// How far the controller got with the written `metadata.generation`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Progress {
    /// The controller has not reconciled the written generation yet.
    Pending,
    /// It reconciled it and its workloads are rolling out.
    Applied,
    /// Every workload of the written generation is available.
    Ready,
    /// It will not become ready without a new generation.
    Failed(String),
}

/// The progress of the App object `app` for its `metadata.generation` `written`.
#[must_use]
pub fn progress(app: &App, written: i64) -> Progress {
    let Some(status) = &app.status else {
        return Progress::Pending;
    };
    if status.observed_generation.unwrap_or(0) < written {
        return Progress::Pending;
    }
    let Some(ready) = status.conditions.iter().find(|c| c.type_ == READY) else {
        return Progress::Applied;
    };
    if ready.observed_generation.is_some_and(|g| g < written) {
        return Progress::Applied;
    }
    match (ready.status.as_str(), ready.reason.as_deref()) {
        ("True", _) => Progress::Ready,
        ("False", Some(reason)) if FAILED.contains(&reason) => Progress::Failed(reason.to_owned()),
        _ => Progress::Applied,
    }
}

#[cfg(test)]
mod tests {
    use kuben_crd::{AppStatus, Condition};
    use serde_json::json;

    use super::*;

    fn app(status: Option<AppStatus>) -> App {
        let mut app: App = serde_json::from_value(json!({
            "apiVersion": "kuben.dev/v1alpha1",
            "kind": "App",
            "metadata": { "name": "web", "namespace": "kb-shop-production" },
            "spec": {
                "source": { "image": "ghcr.io/acme/web@sha256:00" },
                "runtime": { "processes": { "web": { "port": 8080 } } },
            },
        }))
        .expect("app");
        app.status = status;
        app
    }

    /// An App whose controller observed `generation`, with a `Ready`
    /// condition when given.
    fn observed(generation: i64, ready: Option<(bool, &str)>) -> App {
        app(Some(AppStatus {
            observed_generation: Some(generation),
            current_release: None,
            url: None,
            conditions: ready
                .map(|(ok, reason)| {
                    let mut c = Condition::new(READY, ok, reason);
                    c.observed_generation = Some(generation);
                    c
                })
                .into_iter()
                .collect(),
        }))
    }

    #[test]
    fn follows_the_written_generation() {
        assert_eq!(progress(&app(None), 4), Progress::Pending);
        assert_eq!(
            progress(&observed(3, Some((true, "Available"))), 4),
            Progress::Pending,
            "ready, but for an older generation"
        );
        assert_eq!(progress(&observed(4, None), 4), Progress::Applied);
        assert_eq!(
            progress(&observed(4, Some((false, "Progressing"))), 4),
            Progress::Applied
        );
        assert_eq!(
            progress(&observed(4, Some((true, "Available"))), 4),
            Progress::Ready
        );
        assert_eq!(
            progress(&observed(5, Some((true, "Scheduled"))), 4),
            Progress::Ready,
            "a later generation observed"
        );
    }

    #[test]
    fn permanent_reasons_fail_the_run() {
        assert_eq!(
            progress(&observed(4, Some((false, "RolloutFailed"))), 4),
            Progress::Failed("RolloutFailed".into())
        );
        assert_eq!(
            progress(&observed(4, Some((false, "UnknownSize"))), 4),
            Progress::Failed("UnknownSize".into())
        );
    }
}
