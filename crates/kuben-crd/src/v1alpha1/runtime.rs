//! `ApplicationRuntime` — the execution envelope of one application target
//! (ADR-027, plan §9.5). Internal: the hub writes it, the cluster agent acts
//! on it. It holds the last accepted envelope — lifecycle UID, control epoch,
//! generation, input hash and the frozen RenderPlan — and, in status, what
//! the agent observed and made effective.
//!
//! The apiserver enforces the envelope's ordering with CEL:
//!
//! * the target and its lifecycle UID never change;
//! * `generation` and `controlEpoch` never decrease;
//! * the same generation carries the same envelope: a different
//!   `inputHash`, release or plan under an unchanged generation is refused
//!   as an integrity error, while re-applying the same envelope is a no-op.
//!
//! The plan's resources are canonical JSON in a string rather than a list of
//! objects: CEL cannot see fields a schema leaves unknown, so only a string
//! makes "the same generation, the same resources" enforceable. The agent
//! checks the resources against `plan.digest` before applying them.

use kube::{CustomResource, KubeSchema};
use serde::{Deserialize, Serialize};

use super::common::Condition;

/// Upper bound of an envelope's frozen resources, in bytes (ADR-027): a
/// larger model is refused or moved to an artifact referenced by digest.
pub const MAX_ENVELOPE_BYTES: usize = 128 * 1024;

#[derive(CustomResource, KubeSchema, Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[kube(
    group = "kuben.dev",
    version = "v1alpha1",
    kind = "ApplicationRuntime",
    namespaced,
    cel,
    status = "ApplicationRuntimeStatus",
    shortname = "kbrt",
    printcolumn = r#"{"name":"Generation","type":"integer","jsonPath":".spec.generation"}"#,
    printcolumn = r#"{"name":"Observed","type":"integer","jsonPath":".status.observedGeneration"}"#,
    printcolumn = r#"{"name":"Ready","type":"string","jsonPath":".status.conditions[?(@.type==\"Ready\")].status"}"#,
    printcolumn = r#"{"name":"Age","type":"date","jsonPath":".metadata.creationTimestamp"}"#
)]
#[x_kube(validation = Rule::new(
    "self.generation != oldSelf.generation || \
     (self.inputHash == oldSelf.inputHash && self.releaseId == oldSelf.releaseId && self.plan == oldSelf.plan)"
).message("the same generation must carry the same envelope (input hash, release and plan)"))]
#[serde(rename_all = "camelCase")]
pub struct ApplicationRuntimeSpec {
    /// The application target (SQL `application_targets.id`).
    #[schemars(length(max = 64))]
    #[x_kube(validation = Rule::new("self == oldSelf").message("targetId is immutable"))]
    pub target_id: String,
    /// The target's lifecycle UID: a recreated target is a new runtime.
    #[schemars(length(max = 64))]
    #[x_kube(validation = Rule::new("self == oldSelf").message("lifecycleUid is immutable"))]
    pub lifecycle_uid: String,
    /// Raised when control of the target moves (a new controller or a
    /// cutover); an older epoch never writes again.
    #[schemars(range(min = 0))]
    #[x_kube(validation = Rule::new("self >= oldSelf").message("controlEpoch never decreases"))]
    pub control_epoch: i64,
    /// The target's desired generation this envelope carries.
    #[schemars(range(min = 1))]
    #[x_kube(validation = Rule::new("self >= oldSelf").message("a lower generation is rejected"))]
    pub generation: i64,
    /// Digest of the run's inputs (release, configuration, generation).
    #[schemars(length(max = 80), pattern(r"^sha256:[0-9a-f]{64}$"))]
    pub input_hash: String,
    /// The release this generation makes effective.
    #[schemars(length(max = 64))]
    pub release_id: String,
    pub plan: PlanEnvelope,
}

/// The frozen RenderPlan the envelope carries (ADR-026): enough to apply
/// and resume without asking the hub.
#[derive(KubeSchema, Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PlanEnvelope {
    /// SQL `render_plans.id`.
    #[schemars(length(max = 64))]
    pub id: String,
    #[schemars(length(max = 64))]
    pub renderer_version: String,
    /// `sha256:` of `resources`.
    #[schemars(length(max = 80), pattern(r"^sha256:[0-9a-f]{64}$"))]
    pub digest: String,
    /// The normalized resources as canonical JSON (an array of objects).
    #[schemars(length(max = 131_072))]
    pub resources: String,
}

#[derive(KubeSchema, Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ApplicationRuntimeStatus {
    /// The newest generation the agent has acted on.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    #[x_kube(validation = Rule::new("self >= oldSelf").message("observedGeneration never decreases"))]
    pub observed_generation: Option<i64>,
    /// The generation whose resources are live and ready.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub effective_generation: Option<i64>,
    /// The release of `effectiveGeneration`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub effective_release: Option<String>,
    /// Set when the agent stopped moving forward and waits for a decision
    /// (a failed rollout with no pre-authorized compensation).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub recovery_latch: Option<RecoveryLatch>,
    /// What the agent applied for `observedGeneration`.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub inventory: Vec<InventoryItem>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub inventory_hash: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub conditions: Vec<Condition>,
}

#[derive(KubeSchema, Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RecoveryLatch {
    pub generation: i64,
    pub reason: String,
    /// RFC 3339.
    pub since: String,
}

/// One object of the applied inventory.
#[derive(KubeSchema, Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct InventoryItem {
    pub api_version: String,
    pub kind: String,
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub uid: Option<String>,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hash(c: char) -> String {
        format!("sha256:{}", c.to_string().repeat(64))
    }

    fn runtime(generation: i64, input: char, epoch: i64) -> ApplicationRuntime {
        ApplicationRuntime::new(
            "web",
            ApplicationRuntimeSpec {
                target_id: "0199a0c0-0000-7000-8000-000000000001".into(),
                lifecycle_uid: "0199a0c0-0000-7000-8000-000000000002".into(),
                control_epoch: epoch,
                generation,
                input_hash: hash(input),
                release_id: "0199a0c0-0000-7000-8000-000000000003".into(),
                plan: PlanEnvelope {
                    id: "0199a0c0-0000-7000-8000-000000000004".into(),
                    renderer_version: "kuben-renderer/1".into(),
                    digest: hash('e'),
                    resources: "[]".into(),
                },
            },
        )
    }

    #[test]
    fn a_new_envelope_is_valid() {
        runtime(1, 'a', 0).validate_cel().expect("valid");
    }

    #[test]
    fn a_lower_generation_is_rejected() {
        let old = runtime(5, 'a', 0);
        assert!(runtime(4, 'b', 0).validate_cel_update(&old).is_err());
        runtime(6, 'b', 0)
            .validate_cel_update(&old)
            .expect("a higher generation");
    }

    #[test]
    fn the_same_generation_carries_the_same_envelope() {
        let old = runtime(5, 'a', 0);
        runtime(5, 'a', 0).validate_cel_update(&old).expect("idempotent");
        assert!(
            runtime(5, 'b', 0).validate_cel_update(&old).is_err(),
            "another hash"
        );
        let mut plan = runtime(5, 'a', 0);
        plan.spec.plan.resources = r#"[{"kind":"Deployment"}]"#.into();
        assert!(plan.validate_cel_update(&old).is_err(), "another plan");
    }

    #[test]
    fn the_control_epoch_never_decreases() {
        let old = runtime(5, 'a', 3);
        assert!(runtime(6, 'b', 2).validate_cel_update(&old).is_err());
        runtime(5, 'a', 4)
            .validate_cel_update(&old)
            .expect("a new epoch, the same envelope");
    }

    #[test]
    fn the_identity_never_changes() {
        let old = runtime(5, 'a', 0);
        let mut other = runtime(6, 'b', 0);
        other.spec.target_id = "0199a0c0-0000-7000-8000-00000000000f".into();
        assert!(other.validate_cel_update(&old).is_err(), "target changed");
        let mut recreated = runtime(6, 'b', 0);
        recreated.spec.lifecycle_uid = "0199a0c0-0000-7000-8000-00000000000f".into();
        assert!(recreated.validate_cel_update(&old).is_err(), "lifecycle changed");
    }

    #[test]
    fn the_observed_generation_never_decreases() {
        let mut old = runtime(5, 'a', 0);
        old.status = Some(ApplicationRuntimeStatus {
            observed_generation: Some(5),
            ..ApplicationRuntimeStatus::default()
        });
        let mut new = old.clone();
        new.status = Some(ApplicationRuntimeStatus {
            observed_generation: Some(4),
            ..ApplicationRuntimeStatus::default()
        });
        assert!(new.validate_cel_update(&old).is_err());
    }
}
