//! Kubernetes Job and Pod status → [`JobObservation`], without I/O.

use k8s_openapi::{
    api::{
        batch::v1::Job,
        core::v1::{ContainerStatus, Pod},
    },
    jiff::Timestamp,
};
use kuben_core::ops::outcome::{ContainerExit, JobObservation, PodObservation};

fn exits(statuses: Option<&Vec<ContainerStatus>>) -> (Vec<ContainerExit>, Vec<String>) {
    let mut exits = Vec::new();
    let mut running = Vec::new();
    for status in statuses.into_iter().flatten() {
        let state = status.state.as_ref();
        if let Some(t) = state.and_then(|s| s.terminated.as_ref()) {
            exits.push(ContainerExit {
                name: status.name.clone(),
                exit_code: t.exit_code,
                reason: t.reason.clone(),
                message: t.message.clone(),
            });
        } else if state.and_then(|s| s.running.as_ref()).is_some() {
            running.push(status.name.clone());
        }
    }
    (exits, running)
}

/// What `pod` shows at `now`.
#[must_use]
pub fn pod(pod: &Pod, now: Timestamp) -> PodObservation {
    let status = pod.status.as_ref();
    let (mut all_exits, mut running) = exits(status.and_then(|s| s.init_container_statuses.as_ref()));
    let (main_exits, main_running) = exits(status.and_then(|s| s.container_statuses.as_ref()));
    all_exits.extend(main_exits);
    running.extend(main_running);
    let unschedulable_secs = status
        .and_then(|s| s.conditions.as_ref())
        .into_iter()
        .flatten()
        .find(|c| {
            c.type_ == "PodScheduled" && c.status == "False" && c.reason.as_deref() == Some("Unschedulable")
        })
        .map(|c| {
            c.last_transition_time.as_ref().map_or(0, |t| {
                u64::try_from(now.as_second() - t.0.as_second()).unwrap_or(0)
            })
        });
    PodObservation {
        phase: status.and_then(|s| s.phase.clone()).unwrap_or_default(),
        reason: status.and_then(|s| s.reason.clone()),
        message: status.and_then(|s| s.message.clone()),
        exits: all_exits,
        running,
        unschedulable_secs,
    }
}

/// What `job` and its newest pod show at `now`; `None` for a missing Job.
#[must_use]
pub fn job(job: Option<&Job>, pods: &[Pod], now: Timestamp) -> JobObservation {
    let Some(job) = job else {
        return JobObservation::default();
    };
    let status = job.status.as_ref();
    let condition = |kind: &str| {
        status
            .and_then(|s| s.conditions.as_ref())
            .into_iter()
            .flatten()
            .find(|c| c.type_ == kind && c.status == "True")
    };
    let failed = condition("Failed");
    let newest = pods
        .iter()
        .max_by_key(|p| p.metadata.creation_timestamp.as_ref().map(|t| t.0));
    JobObservation {
        exists: true,
        succeeded: condition("Complete").is_some() || status.and_then(|s| s.succeeded).unwrap_or(0) > 0,
        failed: failed.is_some(),
        failed_reason: failed.and_then(|c| c.reason.clone()),
        pod: newest.map(|p| pod(p, now)),
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    fn parse<T: serde::de::DeserializeOwned>(value: serde_json::Value) -> T {
        serde_json::from_value(value).expect("object")
    }

    #[test]
    fn a_missing_job_does_not_exist() {
        assert!(!job(None, &[], Timestamp::now()).exists);
    }

    #[test]
    fn statuses_become_observations() {
        let j: Job = parse(json!({
            "metadata": { "name": "kbuild-1" },
            "status": { "conditions": [
                { "type": "Failed", "status": "True", "reason": "DeadlineExceeded" },
            ] },
        }));
        let old: Pod = parse(json!({
            "metadata": { "name": "old", "creationTimestamp": "2026-09-17T10:00:00Z" },
            "status": { "phase": "Failed" },
        }));
        let new: Pod = parse(json!({
            "metadata": { "name": "new", "creationTimestamp": "2026-09-17T10:05:00Z" },
            "status": {
                "phase": "Running",
                "initContainerStatuses": [
                    { "name": "fetch", "image": "i", "imageID": "", "ready": false, "restartCount": 0,
                      "state": { "terminated": { "exitCode": 0, "reason": "Completed" } } },
                ],
                "containerStatuses": [
                    { "name": "build", "image": "i", "imageID": "", "ready": true, "restartCount": 0,
                      "state": { "running": {} } },
                ],
            },
        }));
        let seen = job(Some(&j), &[old, new], Timestamp::now());
        assert!(seen.exists && seen.failed && !seen.succeeded);
        assert_eq!(seen.failed_reason.as_deref(), Some("DeadlineExceeded"));
        let pod = seen.pod.expect("pod");
        assert_eq!(pod.phase, "Running", "the newest pod");
        assert_eq!(pod.exits.len(), 1);
        assert_eq!(pod.exits[0].name, "fetch");
        assert_eq!(pod.running, vec!["build".to_owned()]);
    }

    #[test]
    fn unschedulable_time_is_measured() {
        let p: Pod = parse(json!({
            "metadata": { "name": "p" },
            "status": { "phase": "Pending", "conditions": [
                { "type": "PodScheduled", "status": "False", "reason": "Unschedulable",
                  "lastTransitionTime": "2026-09-17T10:00:00Z", "message": "0/1 nodes are available" },
            ] },
        }));
        let now: Timestamp = "2026-09-17T10:07:00Z".parse().expect("time");
        assert_eq!(pod(&p, now).unschedulable_secs, Some(420));
        let scheduled: Pod = parse(json!({ "metadata": { "name": "p" }, "status": { "phase": "Pending" } }));
        assert_eq!(pod(&scheduled, now).unschedulable_secs, None);
    }

    #[test]
    fn oom_kills_keep_their_reason() {
        let p: Pod = parse(json!({
            "metadata": { "name": "p" },
            "status": { "phase": "Failed", "containerStatuses": [
                { "name": "build", "image": "i", "imageID": "", "ready": false, "restartCount": 0,
                  "state": { "terminated": { "exitCode": 137, "reason": "OOMKilled" } } },
            ] },
        }));
        let seen = pod(&p, Timestamp::now());
        assert_eq!(seen.exits[0].reason.as_deref(), Some("OOMKilled"));
        assert_eq!(seen.exits[0].exit_code, 137);
    }
}
