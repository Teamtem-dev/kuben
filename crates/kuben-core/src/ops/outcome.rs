//! What a build Job's observed state means for its attempt (ADR-028).
//!
//! The executor reads the Job and its pod and hands the facts to
//! [`classify`], a pure function, so every failure class (out of memory, a
//! full disk, the deadline, a lost node, a bad source, bad output) is decided
//! and tested in one place. A pod's termination message is only a hint: a
//! reported digest is checked against the registry before the attempt
//! succeeds.

use serde::{Deserialize, Serialize};

use crate::artifact::Digest;

/// Name of the init container that fetches the source.
pub const FETCH_CONTAINER: &str = "fetch";
/// Name of the container that builds and pushes.
pub const BUILD_CONTAINER: &str = "build";
/// Name of the container that scans the pushed image (M4.6). It runs after
/// the build and never decides the build's outcome: a scan that fails is
/// recorded as unavailable.
pub const SCAN_CONTAINER: &str = "scan";
/// How long a build pod may stay unschedulable before the attempt fails.
pub const UNSCHEDULABLE_LIMIT_SECS: u64 = 600;
const DETAIL_MAX: usize = 1024;

/// Why an attempt failed. The code is stored and shown; only a lost worker is
/// retried automatically, as a new attempt.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub enum BuildFailure {
    /// The build itself failed (a Dockerfile step, a compiler error).
    BuildError,
    /// The build exceeded its memory limit.
    OutOfMemory,
    /// The build exceeded its ephemeral-storage limit or the disk filled up.
    DiskFull,
    /// The build ran past its deadline.
    DeadlineExceeded,
    /// The node or pod disappeared under the build.
    LostWorker,
    /// The repository or commit could not be fetched.
    SourceUnavailable,
    /// The build finished without a valid, verifiable output.
    OutputRejected,
    /// No node could take the build within the limit.
    Unschedulable,
    /// The provider refused the credentials or the installation is suspended.
    CredentialsRefused,
    /// The Job could not be created with its budget.
    InvalidBudget,
}

impl BuildFailure {
    #[must_use]
    pub const fn code(self) -> &'static str {
        match self {
            Self::BuildError => "BuildError",
            Self::OutOfMemory => "OutOfMemory",
            Self::DiskFull => "DiskFull",
            Self::DeadlineExceeded => "DeadlineExceeded",
            Self::LostWorker => "LostWorker",
            Self::SourceUnavailable => "SourceUnavailable",
            Self::OutputRejected => "OutputRejected",
            Self::Unschedulable => "Unschedulable",
            Self::CredentialsRefused => "CredentialsRefused",
            Self::InvalidBudget => "InvalidBudget",
        }
    }

    /// Infrastructure failures a new attempt may not repeat.
    #[must_use]
    pub const fn retryable(self) -> bool {
        matches!(self, Self::LostWorker)
    }

    /// A short explanation with the next step for the user.
    #[must_use]
    pub const fn explain(self) -> &'static str {
        match self {
            Self::BuildError => "the build failed; see the build log",
            Self::OutOfMemory => {
                "the build ran out of memory; raise the build memory limit or use a build node"
            }
            Self::DiskFull => {
                "the build ran out of disk; raise the build storage limit or shrink the context"
            }
            Self::DeadlineExceeded => {
                "the build ran past its deadline; raise the deadline or speed the build up"
            }
            Self::LostWorker => "the build pod or its node was lost; the build is retried",
            Self::SourceUnavailable => {
                "the commit could not be fetched; check the repository and the app's access"
            }
            Self::OutputRejected => "the build reported no verifiable image",
            Self::Unschedulable => "no node had room for the build; add a build node or lower its requests",
            Self::CredentialsRefused => "the Git provider refused access; check the installation",
            Self::InvalidBudget => "the build budget is invalid",
        }
    }
}

/// A terminated container as the pod status reports it.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ContainerExit {
    pub name: String,
    pub exit_code: i32,
    pub reason: Option<String>,
    pub message: Option<String>,
}

/// The facts about the build pod.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct PodObservation {
    /// `Pending`, `Running`, `Succeeded`, `Failed` or `Unknown`.
    pub phase: String,
    pub reason: Option<String>,
    pub message: Option<String>,
    /// Containers (init or not) that have terminated.
    pub exits: Vec<ContainerExit>,
    /// Containers currently running.
    pub running: Vec<String>,
    /// How long the pod has been unschedulable, if it is.
    pub unschedulable_secs: Option<u64>,
}

/// The facts about the build Job.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct JobObservation {
    pub exists: bool,
    pub succeeded: bool,
    pub failed: bool,
    /// Reason of the Job's `Failed` condition (`DeadlineExceeded`,
    /// `BackoffLimitExceeded`).
    pub failed_reason: Option<String>,
    pub pod: Option<PodObservation>,
}

/// What the build container writes to its termination log.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct BuildReport {
    pub digest: Digest,
    /// The strategy the pod chose (`dockerfile` or `railpack`).
    #[serde(default)]
    pub strategy: String,
}

/// Where a build stands, from its Job.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum JobVerdict {
    /// Created, not started yet.
    Pending,
    /// Fetching the source.
    Fetching,
    /// Building (and pushing, which is the same command).
    Building,
    /// Finished with a report still to verify.
    Finished(BuildReport),
    Failed {
        failure: BuildFailure,
        detail: String,
    },
    /// The Job does not exist.
    Gone,
}

fn bounded(text: &str) -> String {
    let text = text.trim();
    if text.len() <= DETAIL_MAX {
        return text.to_owned();
    }
    let mut end = DETAIL_MAX;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    text[..end].to_owned()
}

fn failed(failure: BuildFailure, detail: impl AsRef<str>) -> JobVerdict {
    let detail = detail.as_ref();
    JobVerdict::Failed {
        failure,
        detail: if detail.trim().is_empty() {
            failure.explain().to_owned()
        } else {
            bounded(detail)
        },
    }
}

fn mentions_disk(text: Option<&str>) -> bool {
    text.is_some_and(|t| {
        let t = t.to_ascii_lowercase();
        t.contains("no space left on device")
            || t.contains("ephemeral-storage")
            || t.contains("ephemeral local storage")
    })
}

/// The meaning of `job` for the attempt, by the precedence documented on
/// each rule: resource kills first, then the deadline, the source, the
/// build, the output and the worker.
#[must_use]
pub fn classify(job: &JobObservation) -> JobVerdict {
    if !job.exists {
        return JobVerdict::Gone;
    }
    let pod = job.pod.as_ref();
    let all_exits = pod.map_or(&[][..], |p| p.exits.as_slice());
    let scan_exit = all_exits.iter().find(|e| e.name == SCAN_CONTAINER);
    let exits: Vec<&ContainerExit> = all_exits.iter().filter(|e| e.name != SCAN_CONTAINER).collect();
    let failed_exit = exits.iter().copied().find(|e| e.exit_code != 0);
    let report = || {
        exits
            .iter()
            .find(|e| e.name == BUILD_CONTAINER && e.exit_code == 0)
            .and_then(|e| e.message.as_deref())
            .and_then(|m| serde_json::from_str::<BuildReport>(m.trim()).ok())
    };

    // 1. The kernel's OOM killer is unambiguous.
    if exits.iter().any(|e| e.reason.as_deref() == Some("OOMKilled")) {
        return failed(BuildFailure::OutOfMemory, "");
    }
    // 2. An eviction for ephemeral storage, or a build that saw ENOSPC.
    let evicted = pod.is_some_and(|p| p.reason.as_deref() == Some("Evicted"));
    let disk = (evicted && mentions_disk(pod.and_then(|p| p.message.as_deref())))
        || exits
            .iter()
            .any(|e| e.exit_code != 0 && mentions_disk(e.message.as_deref()));
    if disk {
        return failed(
            BuildFailure::DiskFull,
            pod.and_then(|p| p.message.clone()).unwrap_or_default(),
        );
    }
    // 3. The Job's activeDeadlineSeconds.
    let deadline = job.failed_reason.as_deref() == Some("DeadlineExceeded")
        || pod.is_some_and(|p| p.reason.as_deref() == Some("DeadlineExceeded"));
    if deadline {
        return failed(BuildFailure::DeadlineExceeded, "");
    }
    // 4. Any other eviction or a vanished node is the worker, not the build.
    let lost =
        evicted || pod.is_some_and(|p| p.phase == "Unknown" || p.reason.as_deref() == Some("NodeLost"));
    if lost {
        return failed(
            BuildFailure::LostWorker,
            pod.and_then(|p| p.message.clone()).unwrap_or_default(),
        );
    }
    // 5. The fetch init container failed.
    if let Some(exit) = failed_exit.filter(|e| e.name == FETCH_CONTAINER) {
        return failed(
            BuildFailure::SourceUnavailable,
            exit.message.as_deref().unwrap_or_default(),
        );
    }
    // 6. The build container failed.
    if let Some(exit) = failed_exit {
        let detail = exit
            .message
            .clone()
            .unwrap_or_else(|| format!("{} exited with code {}", exit.name, exit.exit_code));
        return failed(BuildFailure::BuildError, detail);
    }
    // 7. Success needs a parsable report from the build container; a scan
    // that ended, however it ended, does not change that.
    if job.succeeded || (job.failed && scan_exit.is_some()) {
        return match report() {
            Some(report) => JobVerdict::Finished(report),
            None => failed(
                BuildFailure::OutputRejected,
                "the build container reported no image digest",
            ),
        };
    }
    // 8. A failed Job without an explanation lost its pod.
    if job.failed {
        return failed(
            BuildFailure::LostWorker,
            job.failed_reason
                .as_deref()
                .unwrap_or("the build pod disappeared"),
        );
    }
    let Some(pod) = pod else {
        return JobVerdict::Pending;
    };
    if pod
        .unschedulable_secs
        .is_some_and(|s| s >= UNSCHEDULABLE_LIMIT_SECS)
    {
        return failed(
            BuildFailure::Unschedulable,
            pod.message.as_deref().unwrap_or_default(),
        );
    }
    if pod
        .running
        .iter()
        .any(|c| c == BUILD_CONTAINER || c == SCAN_CONTAINER)
    {
        JobVerdict::Building
    } else if pod.running.iter().any(|c| c == FETCH_CONTAINER)
        || exits.iter().any(|e| e.name == FETCH_CONTAINER)
    {
        JobVerdict::Fetching
    } else {
        JobVerdict::Pending
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    fn exit(name: &str, code: i32, reason: Option<&str>, message: Option<&str>) -> ContainerExit {
        ContainerExit {
            name: name.into(),
            exit_code: code,
            reason: reason.map(Into::into),
            message: message.map(Into::into),
        }
    }

    fn job(pod: PodObservation) -> JobObservation {
        JobObservation {
            exists: true,
            pod: Some(pod),
            ..JobObservation::default()
        }
    }

    fn failure(verdict: &JobVerdict) -> Option<BuildFailure> {
        match verdict {
            JobVerdict::Failed { failure, .. } => Some(*failure),
            _ => None,
        }
    }

    #[test]
    fn a_missing_job_is_gone() {
        assert_eq!(classify(&JobObservation::default()), JobVerdict::Gone);
    }

    #[test]
    fn progress_follows_the_running_container() {
        let pending = job(PodObservation {
            phase: "Pending".into(),
            ..PodObservation::default()
        });
        assert_eq!(classify(&pending), JobVerdict::Pending);
        let fetching = job(PodObservation {
            phase: "Pending".into(),
            running: vec![FETCH_CONTAINER.into()],
            ..PodObservation::default()
        });
        assert_eq!(classify(&fetching), JobVerdict::Fetching);
        let building = job(PodObservation {
            phase: "Running".into(),
            exits: vec![exit(FETCH_CONTAINER, 0, Some("Completed"), None)],
            running: vec![BUILD_CONTAINER.into()],
            ..PodObservation::default()
        });
        assert_eq!(classify(&building), JobVerdict::Building);
        let no_pod_yet = JobObservation {
            exists: true,
            ..JobObservation::default()
        };
        assert_eq!(classify(&no_pod_yet), JobVerdict::Pending);
    }

    #[test]
    fn success_needs_a_valid_report() {
        let mut done = job(PodObservation {
            phase: "Succeeded".into(),
            exits: vec![
                exit(FETCH_CONTAINER, 0, None, None),
                exit(
                    BUILD_CONTAINER,
                    0,
                    Some("Completed"),
                    Some(&format!(r#"{{"digest":"{DIGEST}","strategy":"railpack"}}"#)),
                ),
            ],
            ..PodObservation::default()
        });
        done.succeeded = true;
        let JobVerdict::Finished(report) = classify(&done) else {
            panic!("not finished");
        };
        assert_eq!(report.digest.as_str(), DIGEST);
        assert_eq!(report.strategy, "railpack");

        let mut forged = done.clone();
        forged.pod.as_mut().expect("pod").exits[1].message = Some(r#"{"digest":"sha256:nope"}"#.into());
        assert_eq!(failure(&classify(&forged)), Some(BuildFailure::OutputRejected));
        let mut silent = done;
        silent.pod.as_mut().expect("pod").exits[1].message = None;
        assert_eq!(failure(&classify(&silent)), Some(BuildFailure::OutputRejected));
    }

    #[test]
    fn out_of_memory_wins_over_a_failed_job() {
        let mut oom = job(PodObservation {
            phase: "Failed".into(),
            exits: vec![exit(BUILD_CONTAINER, 137, Some("OOMKilled"), None)],
            ..PodObservation::default()
        });
        oom.failed = true;
        oom.failed_reason = Some("BackoffLimitExceeded".into());
        let verdict = classify(&oom);
        assert_eq!(failure(&verdict), Some(BuildFailure::OutOfMemory));
        assert!(matches!(verdict, JobVerdict::Failed { detail, .. } if detail.contains("memory")));
    }

    #[test]
    fn a_full_disk_is_told_apart_from_other_evictions() {
        let disk = job(PodObservation {
            phase: "Failed".into(),
            reason: Some("Evicted".into()),
            message: Some(
                "Pod ephemeral local storage usage exceeds the total limit of containers 2Gi.".into(),
            ),
            ..PodObservation::default()
        });
        assert_eq!(failure(&classify(&disk)), Some(BuildFailure::DiskFull));
        let enospc = job(PodObservation {
            phase: "Failed".into(),
            exits: vec![exit(
                BUILD_CONTAINER,
                1,
                Some("Error"),
                Some("write /tmp/x: no space left on device"),
            )],
            ..PodObservation::default()
        });
        assert_eq!(failure(&classify(&enospc)), Some(BuildFailure::DiskFull));
        let pressure = job(PodObservation {
            phase: "Failed".into(),
            reason: Some("Evicted".into()),
            message: Some("The node was low on resource: memory.".into()),
            ..PodObservation::default()
        });
        assert_eq!(failure(&classify(&pressure)), Some(BuildFailure::LostWorker));
        assert!(BuildFailure::LostWorker.retryable());
        assert!(!BuildFailure::DiskFull.retryable());
    }

    #[test]
    fn the_deadline_is_reported_as_such() {
        let late = JobObservation {
            exists: true,
            failed: true,
            failed_reason: Some("DeadlineExceeded".into()),
            pod: None,
            ..JobObservation::default()
        };
        assert_eq!(failure(&classify(&late)), Some(BuildFailure::DeadlineExceeded));
    }

    #[test]
    fn fetch_and_build_errors_keep_their_message() {
        let fetch = job(PodObservation {
            phase: "Failed".into(),
            exits: vec![exit(
                FETCH_CONTAINER,
                128,
                Some("Error"),
                Some("fatal: reference is not a tree"),
            )],
            ..PodObservation::default()
        });
        assert_eq!(
            classify(&fetch),
            JobVerdict::Failed {
                failure: BuildFailure::SourceUnavailable,
                detail: "fatal: reference is not a tree".into()
            }
        );
        let build = job(PodObservation {
            phase: "Failed".into(),
            exits: vec![
                exit(FETCH_CONTAINER, 0, None, None),
                exit(BUILD_CONTAINER, 1, Some("Error"), Some(&"e".repeat(5000))),
            ],
            ..PodObservation::default()
        });
        let verdict = classify(&build);
        assert!(
            matches!(&verdict, JobVerdict::Failed { failure: BuildFailure::BuildError, detail } if detail.len() == 1024)
        );
    }

    #[test]
    fn the_scan_never_decides_the_build() {
        let report = format!(r#"{{"digest":"{DIGEST}","strategy":"dockerfile"}}"#);
        let scanning = job(PodObservation {
            phase: "Running".into(),
            exits: vec![
                exit(FETCH_CONTAINER, 0, None, None),
                exit(BUILD_CONTAINER, 0, Some("Completed"), Some(&report)),
            ],
            running: vec![SCAN_CONTAINER.into()],
            ..PodObservation::default()
        });
        assert_eq!(classify(&scanning), JobVerdict::Building);
        for scan in [
            exit(SCAN_CONTAINER, 0, Some("Completed"), Some(r#"{"status":"ok"}"#)),
            exit(SCAN_CONTAINER, 137, Some("OOMKilled"), None),
            exit(SCAN_CONTAINER, 1, Some("Error"), Some("no space left on device")),
        ] {
            let failed = scan.exit_code != 0;
            let done = JobObservation {
                exists: true,
                succeeded: !failed,
                failed,
                failed_reason: failed.then(|| "BackoffLimitExceeded".to_owned()),
                pod: Some(PodObservation {
                    phase: if failed { "Failed" } else { "Succeeded" }.into(),
                    exits: vec![
                        exit(FETCH_CONTAINER, 0, None, None),
                        exit(BUILD_CONTAINER, 0, Some("Completed"), Some(&report)),
                        scan.clone(),
                    ],
                    ..PodObservation::default()
                }),
            };
            assert!(
                matches!(classify(&done), JobVerdict::Finished(r) if r.digest.as_str() == DIGEST),
                "{scan:?}"
            );
        }
    }

    #[test]
    fn lost_pods_and_unschedulable_builds() {
        let vanished = JobObservation {
            exists: true,
            failed: true,
            ..JobObservation::default()
        };
        assert_eq!(failure(&classify(&vanished)), Some(BuildFailure::LostWorker));
        let unknown = job(PodObservation {
            phase: "Unknown".into(),
            ..PodObservation::default()
        });
        assert_eq!(failure(&classify(&unknown)), Some(BuildFailure::LostWorker));
        let waiting = job(PodObservation {
            phase: "Pending".into(),
            unschedulable_secs: Some(UNSCHEDULABLE_LIMIT_SECS - 1),
            ..PodObservation::default()
        });
        assert_eq!(classify(&waiting), JobVerdict::Pending);
        let stuck = job(PodObservation {
            phase: "Pending".into(),
            unschedulable_secs: Some(UNSCHEDULABLE_LIMIT_SECS),
            message: Some("0/1 nodes are available: 1 Insufficient memory.".into()),
            ..PodObservation::default()
        });
        assert_eq!(failure(&classify(&stuck)), Some(BuildFailure::Unschedulable));
    }
}
