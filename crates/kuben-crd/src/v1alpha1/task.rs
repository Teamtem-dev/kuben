//! `ExecutionTask` — one attempt of a bounded execution: a build, backup,
//! restore, migration or platform change (ADR-027, plan §9.5). Internal: the
//! hub creates it, the cluster agent runs it and writes the receipt.
//!
//! The apiserver enforces with CEL:
//!
//! * the attempt's identity, kind, input, input hash and deadline never
//!   change;
//! * a cancel request is a separate channel: it can be added, never changed
//!   or withdrawn;
//! * a receipt, once written, is final.
//!
//! The input is canonical JSON in a string so that immutability covers all
//! of it (CEL cannot see fields a schema leaves unknown). The receipt stays
//! until the hub has durably acknowledged it: the agent removes
//! [`RECEIPT_FINALIZER`] only then.

use kube::{CustomResource, KubeSchema};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use super::common::Condition;

/// Keeps a task, and its receipt, until the hub has acknowledged the receipt.
pub const RECEIPT_FINALIZER: &str = "kuben.dev/receipt";

#[derive(CustomResource, KubeSchema, Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[kube(
    group = "kuben.dev",
    version = "v1alpha1",
    kind = "ExecutionTask",
    namespaced,
    cel,
    status = "ExecutionTaskStatus",
    shortname = "kbet",
    printcolumn = r#"{"name":"Kind","type":"string","jsonPath":".spec.kind"}"#,
    printcolumn = r#"{"name":"Attempt","type":"integer","jsonPath":".spec.attempt"}"#,
    printcolumn = r#"{"name":"Phase","type":"string","jsonPath":".status.phase"}"#,
    printcolumn = r#"{"name":"Age","type":"date","jsonPath":".metadata.creationTimestamp"}"#
)]
#[x_kube(validation = Rule::new("!has(oldSelf.cancel) || (has(self.cancel) && self.cancel == oldSelf.cancel)")
    .message("a cancel request can be added, never changed or withdrawn"))]
#[serde(rename_all = "camelCase")]
pub struct ExecutionTaskSpec {
    #[x_kube(validation = Rule::new("self == oldSelf").message("kind is immutable"))]
    pub kind: ExecutionKind,
    /// The SQL operation this attempt belongs to.
    #[schemars(length(max = 64))]
    #[x_kube(validation = Rule::new("self == oldSelf").message("operationId is immutable"))]
    pub operation_id: String,
    #[schemars(range(min = 1))]
    #[x_kube(validation = Rule::new("self == oldSelf").message("attempt is immutable"))]
    pub attempt: i64,
    /// The kind's input as canonical JSON; never a secret value.
    #[schemars(length(max = 131_072))]
    #[x_kube(validation = Rule::new("self == oldSelf").message("input is immutable"))]
    pub input: String,
    /// `sha256:` of `input`.
    #[schemars(length(max = 80), pattern(r"^sha256:[0-9a-f]{64}$"))]
    #[x_kube(validation = Rule::new("self == oldSelf").message("inputHash is immutable"))]
    pub input_hash: String,
    /// RFC 3339; the agent stops the attempt and reports `TimedOut` after it.
    #[schemars(length(max = 40))]
    #[x_kube(validation = Rule::new("self == oldSelf").message("deadline is immutable"))]
    pub deadline: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cancel: Option<CancelRequest>,
}

/// What an attempt runs; each kind has its own input schema and permissions.
#[derive(JsonSchema, Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub enum ExecutionKind {
    Build,
    Backup,
    Restore,
    Migration,
    PlatformChange,
}

#[derive(KubeSchema, Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CancelRequest {
    /// RFC 3339.
    #[schemars(length(max = 40))]
    pub requested_at: String,
    #[schemars(length(max = 256))]
    pub reason: String,
}

#[derive(KubeSchema, Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[x_kube(validation = Rule::new("!has(oldSelf.receipt) || (has(self.receipt) && self.receipt == oldSelf.receipt)")
    .message("a receipt is final"))]
#[serde(rename_all = "camelCase")]
pub struct ExecutionTaskStatus {
    /// `Pending | Running | Succeeded | Failed | Cancelled | TimedOut`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub phase: Option<String>,
    /// RFC 3339.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub started_at: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub receipt: Option<Receipt>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub conditions: Vec<Condition>,
}

/// The terminal result of an attempt.
#[derive(KubeSchema, Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Receipt {
    pub outcome: Outcome,
    /// RFC 3339.
    #[schemars(length(max = 40))]
    pub finished_at: String,
    /// The kind's result as canonical JSON (an artifact digest, a backup
    /// location, …), bounded like the input.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    #[schemars(length(max = 16_384))]
    pub result: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    #[schemars(length(max = 1024))]
    pub message: Option<String>,
}

#[derive(JsonSchema, Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub enum Outcome {
    Succeeded,
    Failed,
    Cancelled,
    TimedOut,
}

#[cfg(test)]
mod tests {
    use super::*;

    const HASH: &str = "sha256:1111111111111111111111111111111111111111111111111111111111111111";

    fn task() -> ExecutionTask {
        ExecutionTask::new(
            "build-1",
            ExecutionTaskSpec {
                kind: ExecutionKind::Build,
                operation_id: "0199a0c0-0000-7000-8000-000000000001".into(),
                attempt: 1,
                input: r#"{"source":"git"}"#.into(),
                input_hash: HASH.into(),
                deadline: "2026-09-15T12:00:00Z".into(),
                cancel: None,
            },
        )
    }

    fn cancel(reason: &str) -> CancelRequest {
        CancelRequest {
            requested_at: "2026-09-15T11:00:00Z".into(),
            reason: reason.into(),
        }
    }

    fn receipt(outcome: Outcome) -> Receipt {
        Receipt {
            outcome,
            finished_at: "2026-09-15T11:30:00Z".into(),
            result: None,
            message: None,
        }
    }

    #[test]
    fn a_new_task_is_valid() {
        task().validate_cel().expect("valid");
    }

    #[test]
    fn the_input_and_identity_never_change() {
        let old = task();
        let mut input = task();
        input.spec.input = r#"{"source":"other"}"#.into();
        assert!(input.validate_cel_update(&old).is_err(), "input changed");
        let mut attempt = task();
        attempt.spec.attempt = 2;
        assert!(attempt.validate_cel_update(&old).is_err(), "attempt changed");
        let mut kind = task();
        kind.spec.kind = ExecutionKind::Backup;
        assert!(kind.validate_cel_update(&old).is_err(), "kind changed");
        let mut deadline = task();
        deadline.spec.deadline = "2026-09-16T12:00:00Z".into();
        assert!(deadline.validate_cel_update(&old).is_err(), "deadline moved");
        task().validate_cel_update(&old).expect("the same task again");
    }

    #[test]
    fn a_cancel_request_is_added_once_and_kept() {
        let old = task();
        let mut cancelled = task();
        cancelled.spec.cancel = Some(cancel("user"));
        cancelled.validate_cel_update(&old).expect("cancel added");
        let mut changed = cancelled.clone();
        changed.spec.cancel = Some(cancel("someone else"));
        assert!(changed.validate_cel_update(&cancelled).is_err(), "cancel changed");
        assert!(old.validate_cel_update(&cancelled).is_err(), "cancel withdrawn");
    }

    #[test]
    fn a_receipt_is_final() {
        let mut running = task();
        running.status = Some(ExecutionTaskStatus {
            phase: Some("Running".into()),
            ..ExecutionTaskStatus::default()
        });
        let mut done = running.clone();
        done.status = Some(ExecutionTaskStatus {
            phase: Some("Succeeded".into()),
            receipt: Some(receipt(Outcome::Succeeded)),
            ..ExecutionTaskStatus::default()
        });
        done.validate_cel_update(&running).expect("receipt written");
        let mut rewritten = done.clone();
        rewritten.status = Some(ExecutionTaskStatus {
            phase: Some("Failed".into()),
            receipt: Some(receipt(Outcome::Failed)),
            ..ExecutionTaskStatus::default()
        });
        assert!(rewritten.validate_cel_update(&done).is_err(), "receipt rewritten");
        assert!(running.validate_cel_update(&done).is_err(), "receipt removed");
    }
}
