//! The agent's executor (ADR-027, M1.9): carries an execution envelope out in
//! its cluster.
//!
//! 1. The envelope is checked before anything is written: the plan's
//!    resources must match the plan's digest, and each must be of a kind the
//!    agent applies ([`KINDS`]) and in the envelope's namespace. The agent is
//!    not a general manifest runner.
//! 2. The `ApplicationRuntime` is written with server-side apply. The
//!    apiserver's CEL rules refuse a lower generation, or another envelope
//!    under the same generation: that is a rejection, not a retry.
//! 3. The plan's resources are applied as [`FIELD_MANAGER`], owned by the
//!    `ApplicationRuntime`. Resources an earlier plan had and this one does
//!    not are deleted. Volumes are neither owned nor deleted: they outlive
//!    their app.
//! 4. The agent follows the plan's Deployments until every one is available
//!    (Ready), one passes its progress deadline (Failed), or the verify
//!    deadline passes (Failed), and writes what it saw into the runtime's
//!    status.

use std::{collections::BTreeSet, fmt, pin::Pin, time::Duration};

use k8s_openapi::api::apps::v1::Deployment;
use kube::{
    Api, Client, Resource, ResourceExt,
    api::{ApiResource, DeleteParams, DynamicObject, GroupVersionKind, ListParams, Patch, PatchParams},
};
use kuben_crd::{
    ApplicationRuntime, ApplicationRuntimeSpec, ApplicationRuntimeStatus, Condition, InventoryItem,
    condition::READY, labels,
};
use serde_json::{Value, json};
use tokio::time::{Instant, sleep};

use crate::{
    enroll::hex_digest,
    link::{Executor, Reports},
    protocol::{Apply, Observation, RuntimePhase},
};

/// The field manager of everything the agent writes.
pub const FIELD_MANAGER: &str = "kuben-agent";

/// A kind the agent applies.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Kind {
    pub api_version: &'static str,
    pub kind: &'static str,
    pub plural: &'static str,
    /// Objects of this kind an earlier plan had and this one does not are
    /// deleted. Volumes are kept (they outlive their app, ADR scenario 6).
    pub prune: bool,
}

/// Every kind the agent applies; anything else in a plan is refused.
pub const KINDS: [Kind; 7] = [
    Kind {
        api_version: "v1",
        kind: "PersistentVolumeClaim",
        plural: "persistentvolumeclaims",
        prune: false,
    },
    Kind {
        api_version: "apps/v1",
        kind: "Deployment",
        plural: "deployments",
        prune: true,
    },
    Kind {
        api_version: "autoscaling/v2",
        kind: "HorizontalPodAutoscaler",
        plural: "horizontalpodautoscalers",
        prune: true,
    },
    Kind {
        api_version: "batch/v1",
        kind: "CronJob",
        plural: "cronjobs",
        prune: true,
    },
    Kind {
        api_version: "v1",
        kind: "Service",
        plural: "services",
        prune: true,
    },
    Kind {
        api_version: "gateway.networking.k8s.io/v1beta1",
        kind: "ReferenceGrant",
        plural: "referencegrants",
        prune: true,
    },
    Kind {
        api_version: "gateway.networking.k8s.io/v1",
        kind: "HTTPRoute",
        plural: "httproutes",
        prune: true,
    },
];

fn kind_of(api_version: &str, kind: &str) -> Option<&'static Kind> {
    KINDS
        .iter()
        .find(|k| k.api_version == api_version && k.kind == kind)
}

fn api_resource(kind: &Kind) -> ApiResource {
    let (group, version) = kind.api_version.split_once('/').unwrap_or(("", kind.api_version));
    ApiResource::from_gvk_with_plural(&GroupVersionKind::gvk(group, version, kind.kind), kind.plural)
}

/// Why an envelope was not carried out.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Refused {
    pub reason: &'static str,
    pub message: String,
}

impl Refused {
    fn new(reason: &'static str, message: impl fmt::Display) -> Self {
        Self {
            reason,
            message: message.to_string(),
        }
    }
}

/// An envelope that passed the checks.
#[derive(Clone, Debug)]
pub struct Checked {
    pub spec: ApplicationRuntimeSpec,
    /// The plan's resources, each of an allowed kind in the envelope's
    /// namespace.
    pub resources: Vec<Value>,
}

impl Checked {
    fn inventory(&self) -> Vec<InventoryItem> {
        self.resources
            .iter()
            .map(|r| InventoryItem {
                api_version: r["apiVersion"].as_str().unwrap_or_default().to_owned(),
                kind: r["kind"].as_str().unwrap_or_default().to_owned(),
                name: r["metadata"]["name"].as_str().unwrap_or_default().to_owned(),
                uid: None,
            })
            .collect()
    }
}

/// Check `apply` before anything is written.
pub fn check(apply: &Apply) -> Result<Checked, Refused> {
    let spec: ApplicationRuntimeSpec =
        serde_json::from_str(&apply.spec).map_err(|e| Refused::new("InvalidEnvelope", e))?;
    let digest = hex_digest(spec.plan.resources.as_bytes());
    if digest != spec.plan.digest {
        return Err(Refused::new(
            "DigestMismatch",
            format!("the plan's resources hash to {digest}, not {}", spec.plan.digest),
        ));
    }
    let resources: Vec<Value> =
        serde_json::from_str(&spec.plan.resources).map_err(|e| Refused::new("InvalidEnvelope", e))?;
    for resource in &resources {
        let (api_version, kind) = (
            resource["apiVersion"].as_str().unwrap_or_default(),
            resource["kind"].as_str().unwrap_or_default(),
        );
        if kind_of(api_version, kind).is_none() {
            return Err(Refused::new("KindNotAllowed", format!("{api_version} {kind}")));
        }
        if resource["metadata"]["name"].as_str().is_none_or(str::is_empty) {
            return Err(Refused::new(
                "InvalidEnvelope",
                format!("a {kind} without a name"),
            ));
        }
        if let Some(namespace) = resource["metadata"]["namespace"].as_str()
            && namespace != apply.namespace
        {
            return Err(Refused::new(
                "OtherNamespace",
                format!("{kind} in {namespace}, not {}", apply.namespace),
            ));
        }
    }
    Ok(Checked { spec, resources })
}

/// How the plan's Deployments stand.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Rollout {
    Ready,
    Waiting(String),
    Failed(String),
}

/// The rollout of the Deployments named in `live` (the object as read, or
/// `None` when it does not exist yet). The App controller's rules.
#[must_use]
pub fn rollout(live: &[(String, Option<Deployment>)]) -> Rollout {
    let mut waiting = Vec::new();
    for (name, deployment) in live {
        let Some(deployment) = deployment else {
            waiting.push(format!("{name}: not created yet"));
            continue;
        };
        let want = deployment.spec.as_ref().and_then(|s| s.replicas).unwrap_or(1);
        let Some(status) = &deployment.status else {
            waiting.push(format!("{name}: pending"));
            continue;
        };
        let past_deadline = status.conditions.as_ref().is_some_and(|cs| {
            cs.iter()
                .any(|c| c.type_ == "Progressing" && c.reason.as_deref() == Some("ProgressDeadlineExceeded"))
        });
        if past_deadline {
            return Rollout::Failed(format!("{name}: rollout exceeded its progress deadline"));
        }
        let observed = status.observed_generation.unwrap_or(0) >= deployment.metadata.generation.unwrap_or(0);
        let (updated, available, total) = (
            status.updated_replicas.unwrap_or(0),
            status.available_replicas.unwrap_or(0),
            status.replicas.unwrap_or(0),
        );
        if !observed || updated < want || available < want || total > updated {
            waiting.push(format!("{name}: {available}/{want} available"));
        }
    }
    if waiting.is_empty() {
        Rollout::Ready
    } else {
        Rollout::Waiting(waiting.join("; "))
    }
}

fn is_status(err: &kube::Error, code: u16) -> bool {
    matches!(err, kube::Error::Api(status) if status.code == code)
}

/// The executor of a cluster: talks to its apiserver with the agent's
/// credentials.
#[derive(Clone)]
pub struct KubeExecutor {
    client: Client,
    verify_deadline: Duration,
    poll: Duration,
}

impl fmt::Debug for KubeExecutor {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("KubeExecutor")
            .field("verify_deadline", &self.verify_deadline)
            .finish_non_exhaustive()
    }
}

impl KubeExecutor {
    /// Waits up to 15 minutes for a rollout, looking every 3 seconds.
    #[must_use]
    pub const fn new(client: Client) -> Self {
        Self {
            client,
            verify_deadline: Duration::from_mins(15),
            poll: Duration::from_secs(3),
        }
    }

    #[must_use]
    pub const fn with_verify_deadline(mut self, deadline: Duration, poll: Duration) -> Self {
        self.verify_deadline = deadline;
        self.poll = poll;
        self
    }

    async fn write_runtime(
        &self,
        apply: &Apply,
        spec: &ApplicationRuntimeSpec,
    ) -> Result<ApplicationRuntime, kube::Error> {
        let api: Api<ApplicationRuntime> = Api::namespaced(self.client.clone(), &apply.namespace);
        let body = json!({
            "apiVersion": "kuben.dev/v1alpha1",
            "kind": "ApplicationRuntime",
            "metadata": {
                "name": apply.name,
                "namespace": apply.namespace,
                "labels": { labels::MANAGED_BY: labels::MANAGER, labels::APP: apply.name },
            },
            "spec": spec,
        });
        api.patch(
            &apply.name,
            &PatchParams::apply(FIELD_MANAGER).force(),
            &Patch::Apply(&body),
        )
        .await
    }

    /// Apply the plan's resources, owned by `runtime`.
    async fn apply_resources(
        &self,
        apply: &Apply,
        checked: &Checked,
        runtime: &ApplicationRuntime,
    ) -> Result<(), Refused> {
        let owner = runtime
            .controller_owner_ref(&())
            .ok_or_else(|| Refused::new("KubernetesError", "the ApplicationRuntime has no uid"))?;
        let owner = serde_json::to_value(owner).map_err(|e| Refused::new("KubernetesError", e))?;
        for resource in &checked.resources {
            let (api_version, kind, name) = (
                resource["apiVersion"].as_str().unwrap_or_default(),
                resource["kind"].as_str().unwrap_or_default(),
                resource["metadata"]["name"].as_str().unwrap_or_default(),
            );
            let Some(allowed) = kind_of(api_version, kind) else {
                return Err(Refused::new("KindNotAllowed", format!("{api_version} {kind}")));
            };
            let mut object = resource.clone();
            object["metadata"]["namespace"] = json!(apply.namespace);
            // Volumes outlive their app (scenario 6): never owned by the
            // runtime, so never collected with it.
            if allowed.prune {
                object["metadata"]["ownerReferences"] = json!([owner]);
            }
            let api: Api<DynamicObject> =
                Api::namespaced_with(self.client.clone(), &apply.namespace, &api_resource(allowed));
            match api
                .patch(
                    name,
                    &PatchParams::apply(FIELD_MANAGER).force(),
                    &Patch::Apply(&object),
                )
                .await
            {
                Ok(_) => {}
                // The Gateway API is not installed: nothing is routed.
                Err(e)
                    if is_status(&e, 404)
                        && allowed.api_version.starts_with("gateway.networking.k8s.io/") => {}
                Err(e) => return Err(Refused::new("KubernetesError", format!("{kind}/{name}: {e}"))),
            }
        }
        Ok(())
    }

    /// Delete what `runtime` owns and this plan no longer has.
    async fn prune(&self, apply: &Apply, checked: &Checked, owner_uid: &str) -> Result<(), Refused> {
        let keep: BTreeSet<(String, String)> = checked
            .inventory()
            .into_iter()
            .map(|item| (item.kind, item.name))
            .collect();
        let selector = format!("{}={}", labels::APP, apply.name);
        for kind in KINDS.iter().filter(|k| k.prune) {
            let api: Api<DynamicObject> =
                Api::namespaced_with(self.client.clone(), &apply.namespace, &api_resource(kind));
            let listed = match api.list(&ListParams::default().labels(&selector)).await {
                Ok(listed) => listed,
                Err(e) if is_status(&e, 404) => continue,
                Err(e) => return Err(Refused::new("KubernetesError", format!("{}: {e}", kind.kind))),
            };
            for object in listed.items {
                let owned = object.owner_references().iter().any(|r| r.uid == owner_uid);
                let name = object.name_any();
                if owned && !keep.contains(&(kind.kind.to_owned(), name.clone())) {
                    match api.delete(&name, &DeleteParams::background()).await {
                        Ok(_) => {
                            tracing::info!(kind = kind.kind, %name, "pruned an object the plan no longer has");
                        }
                        Err(e) if is_status(&e, 404) => {}
                        Err(e) => {
                            return Err(Refused::new(
                                "KubernetesError",
                                format!("{}/{name}: {e}", kind.kind),
                            ));
                        }
                    }
                }
            }
        }
        Ok(())
    }

    /// Follow the plan's Deployments until they settle or the deadline passes.
    async fn verify(&self, apply: &Apply, checked: &Checked) -> Rollout {
        let names: Vec<String> = checked
            .resources
            .iter()
            .filter(|r| r["kind"] == "Deployment")
            .filter_map(|r| r["metadata"]["name"].as_str().map(str::to_owned))
            .collect();
        let api: Api<Deployment> = Api::namespaced(self.client.clone(), &apply.namespace);
        let deadline = Instant::now() + self.verify_deadline;
        loop {
            let mut live = Vec::new();
            for name in &names {
                match api.get_opt(name).await {
                    Ok(deployment) => live.push((name.clone(), deployment)),
                    Err(e) => return Rollout::Failed(format!("{name}: {e}")),
                }
            }
            match rollout(&live) {
                Rollout::Waiting(why) if Instant::now() >= deadline => {
                    return Rollout::Failed(format!("not ready within {:?}: {why}", self.verify_deadline));
                }
                Rollout::Waiting(_) => sleep(self.poll).await,
                settled => return settled,
            }
        }
    }

    /// Record what the agent saw of the runtime's generation.
    async fn write_status(&self, apply: &Apply, checked: &Checked, ready: bool, reason: &str, message: &str) {
        let generation = checked.spec.generation;
        let mut condition = Condition::new(READY, ready, reason);
        condition.message = (!message.is_empty()).then(|| message.to_owned());
        condition.observed_generation = Some(generation);
        let status = ApplicationRuntimeStatus {
            observed_generation: Some(generation),
            effective_generation: ready.then_some(generation),
            effective_release: ready.then(|| checked.spec.release_id.clone()),
            recovery_latch: None,
            inventory: checked.inventory(),
            inventory_hash: None,
            conditions: vec![condition],
        };
        let body =
            json!({ "apiVersion": "kuben.dev/v1alpha1", "kind": "ApplicationRuntime", "status": status });
        let api: Api<ApplicationRuntime> = Api::namespaced(self.client.clone(), &apply.namespace);
        if let Err(error) = api
            .patch_status(
                &apply.name,
                &PatchParams::apply(FIELD_MANAGER).force(),
                &Patch::Apply(&body),
            )
            .await
        {
            tracing::warn!(%error, runtime = %apply.name, "cannot write the runtime's status");
        }
    }

    /// Carry `apply` out, reporting each step.
    pub async fn carry(&self, apply: Apply, reports: Reports) {
        let observe = |generation: i64,
                       phase: RuntimePhase,
                       reason: Option<&str>,
                       message: Option<String>| Observation {
            target: apply.target.clone(),
            generation,
            phase,
            reason: reason.map(str::to_owned),
            message,
        };
        let checked = match check(&apply) {
            Ok(checked) => checked,
            Err(refused) => {
                tracing::warn!(target = %apply.target, reason = refused.reason, message = %refused.message, "envelope refused");
                let _ = reports
                    .send(observe(
                        0,
                        RuntimePhase::Rejected,
                        Some(refused.reason),
                        Some(refused.message),
                    ))
                    .await;
                return;
            }
        };
        let generation = checked.spec.generation;
        let runtime = match self.write_runtime(&apply, &checked.spec).await {
            Ok(runtime) => runtime,
            Err(e) => {
                let (phase, reason) = if is_status(&e, 422) {
                    (RuntimePhase::Rejected, "EnvelopeRefused")
                } else {
                    (RuntimePhase::Failed, "KubernetesError")
                };
                let _ = reports
                    .send(observe(generation, phase, Some(reason), Some(e.to_string())))
                    .await;
                return;
            }
        };
        let _ = reports
            .send(observe(generation, RuntimePhase::Accepted, None, None))
            .await;
        let owner_uid = runtime.metadata.uid.clone().unwrap_or_default();
        if let Err(refused) = self.apply_resources(&apply, &checked, &runtime).await {
            self.write_status(&apply, &checked, false, refused.reason, &refused.message)
                .await;
            let _ = reports
                .send(observe(
                    generation,
                    RuntimePhase::Failed,
                    Some(refused.reason),
                    Some(refused.message),
                ))
                .await;
            return;
        }
        if let Err(refused) = self.prune(&apply, &checked, &owner_uid).await {
            tracing::warn!(reason = refused.reason, message = %refused.message, "prune incomplete");
        }
        let _ = reports
            .send(observe(generation, RuntimePhase::Applying, None, None))
            .await;
        match self.verify(&apply, &checked).await {
            Rollout::Ready => {
                self.write_status(&apply, &checked, true, "Available", "").await;
                let _ = reports
                    .send(observe(generation, RuntimePhase::Ready, None, None))
                    .await;
            }
            Rollout::Failed(message) | Rollout::Waiting(message) => {
                self.write_status(&apply, &checked, false, "RolloutFailed", &message)
                    .await;
                let _ = reports
                    .send(observe(
                        generation,
                        RuntimePhase::Failed,
                        Some("RolloutFailed"),
                        Some(message),
                    ))
                    .await;
            }
        }
    }
}

impl Executor for KubeExecutor {
    fn apply(&self, apply: Apply, reports: Reports) -> Pin<Box<dyn Future<Output = ()> + Send>> {
        let this = self.clone();
        Box::pin(async move { this.carry(apply, reports).await })
    }
}

#[cfg(test)]
mod tests {
    use k8s_openapi::{
        api::apps::v1::{DeploymentCondition, DeploymentSpec, DeploymentStatus},
        apimachinery::pkg::apis::meta::v1::ObjectMeta,
    };
    use kuben_crd::PlanEnvelope;

    use super::*;

    fn apply_of(resources: &Value, digest: Option<String>) -> Apply {
        let resources = resources.to_string();
        let spec = ApplicationRuntimeSpec {
            target_id: "t".into(),
            lifecycle_uid: "l".into(),
            control_epoch: 0,
            generation: 3,
            input_hash: hex_digest(b"input"),
            release_id: "r".into(),
            plan: PlanEnvelope {
                id: "p".into(),
                renderer_version: "kuben-renderer/1".into(),
                digest: digest.unwrap_or_else(|| hex_digest(resources.as_bytes())),
                resources,
            },
        };
        Apply {
            target: "t".into(),
            namespace: "kb-shop-prod".into(),
            name: "web".into(),
            spec: serde_json::to_string(&spec).expect("spec"),
        }
    }

    fn deployment(name: &str) -> Value {
        json!({ "apiVersion": "apps/v1", "kind": "Deployment", "metadata": { "name": name, "namespace": "kb-shop-prod" } })
    }

    #[test]
    fn a_plan_of_allowed_kinds_in_its_namespace_passes() {
        let plan = json!([deployment("web-web"), { "apiVersion": "v1", "kind": "Service", "metadata": { "name": "web" } }]);
        let checked = check(&apply_of(&plan, None)).expect("checked");
        assert_eq!(checked.spec.generation, 3);
        assert_eq!(checked.inventory().len(), 2);
    }

    #[test]
    fn a_plan_that_does_not_match_its_digest_is_refused() {
        let plan = json!([deployment("web-web")]);
        let refused = check(&apply_of(&plan, Some(hex_digest(b"other")))).expect_err("refused");
        assert_eq!(refused.reason, "DigestMismatch");
    }

    #[test]
    fn only_allowed_kinds_in_the_envelopes_namespace_are_applied() {
        let secret = json!([{ "apiVersion": "v1", "kind": "Secret", "metadata": { "name": "x" } }]);
        assert_eq!(
            check(&apply_of(&secret, None)).expect_err("refused").reason,
            "KindNotAllowed"
        );
        let role = json!([{ "apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": { "name": "x" } }]);
        assert_eq!(
            check(&apply_of(&role, None)).expect_err("refused").reason,
            "KindNotAllowed"
        );
        let elsewhere = json!([{ "apiVersion": "apps/v1", "kind": "Deployment", "metadata": { "name": "x", "namespace": "kube-system" } }]);
        assert_eq!(
            check(&apply_of(&elsewhere, None)).expect_err("refused").reason,
            "OtherNamespace"
        );
        let nameless = json!([{ "apiVersion": "apps/v1", "kind": "Deployment", "metadata": {} }]);
        assert_eq!(
            check(&apply_of(&nameless, None)).expect_err("refused").reason,
            "InvalidEnvelope"
        );
    }

    fn live(replicas: i32, available: i32, deadline: bool) -> Deployment {
        Deployment {
            metadata: ObjectMeta {
                generation: Some(2),
                ..ObjectMeta::default()
            },
            spec: Some(DeploymentSpec {
                replicas: Some(replicas),
                ..DeploymentSpec::default()
            }),
            status: Some(DeploymentStatus {
                observed_generation: Some(2),
                replicas: Some(replicas),
                updated_replicas: Some(replicas),
                available_replicas: Some(available),
                conditions: deadline.then(|| {
                    vec![DeploymentCondition {
                        type_: "Progressing".into(),
                        status: "False".into(),
                        reason: Some("ProgressDeadlineExceeded".into()),
                        ..DeploymentCondition::default()
                    }]
                }),
                ..DeploymentStatus::default()
            }),
        }
    }

    #[test]
    fn a_rollout_is_ready_when_every_deployment_is_available() {
        assert_eq!(rollout(&[("a".into(), Some(live(2, 2, false)))]), Rollout::Ready);
        assert!(matches!(
            rollout(&[("a".into(), Some(live(2, 1, false)))]),
            Rollout::Waiting(_)
        ));
        assert!(matches!(rollout(&[("a".into(), None)]), Rollout::Waiting(_)));
        assert!(matches!(
            rollout(&[("a".into(), Some(live(2, 1, true)))]),
            Rollout::Failed(_)
        ));
        assert_eq!(rollout(&[]), Rollout::Ready, "nothing to wait for");
    }
}
