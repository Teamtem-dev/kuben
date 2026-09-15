//! M1.9 (ADR-027): the agent's executor against a real apiserver.
//!
//! It writes the `ApplicationRuntime`, applies the plan's resources owned by
//! it, reports Ready, prunes what a newer plan dropped, and is refused by the
//! apiserver for a lower generation; an envelope whose digest does not match
//! or that holds a kind the agent does not apply is refused before anything
//! is written.
//!
//! Needs a cluster: the tests are ignored by default and run with
//! `cargo test -- --ignored` against the current kube context (the kind job in
//! CI), each in its own throwaway namespace. The Deployments have no
//! replicas, so nothing waits for an image.

use std::time::Duration;

use k8s_openapi::{
    api::{
        apps::v1::Deployment,
        core::v1::{Namespace, Service},
    },
    apiextensions_apiserver::pkg::apis::apiextensions::v1::CustomResourceDefinition,
};
use kube::{
    Api, Client, CustomResourceExt,
    api::{DeleteParams, ObjectMeta, Patch, PatchParams, PostParams},
};
use kuben_agent::{
    protocol::{Apply, Observation, RuntimePhase},
    runtime::KubeExecutor,
};
use kuben_crd::{ApplicationRuntime, ApplicationRuntimeSpec, FIELD_MANAGER, PlanEnvelope};
use serde_json::{Value, json};
use sha2::{Digest as _, Sha256};
use tokio::sync::mpsc;

fn sha256(text: &str) -> String {
    Sha256::digest(text.as_bytes())
        .iter()
        .fold(String::from("sha256:"), |mut s, b| {
            use std::fmt::Write as _;
            let _ = write!(s, "{b:02x}");
            s
        })
}

struct TestNs {
    client: Client,
    name: String,
}

impl TestNs {
    async fn new() -> Self {
        let client = Client::try_default().await.expect("kube client");
        let crds = Api::<CustomResourceDefinition>::all(client.clone());
        let crd = ApplicationRuntime::crd();
        let name = crd.metadata.name.clone().expect("CRD name");
        crds.patch(
            &name,
            &PatchParams::apply(FIELD_MANAGER).force(),
            &Patch::Apply(&crd),
        )
        .await
        .expect("install the ApplicationRuntime CRD");
        for _ in 0..60 {
            let live = crds.get(&name).await.expect("read CRD");
            if live
                .status
                .and_then(|s| s.conditions)
                .is_some_and(|cs| cs.iter().any(|c| c.type_ == "Established" && c.status == "True"))
            {
                break;
            }
            tokio::time::sleep(Duration::from_secs(1)).await;
        }
        let name = format!("kuben-m1-agent-{}", uuid::Uuid::now_v7().simple());
        let ns = Namespace {
            metadata: ObjectMeta {
                name: Some(name.clone()),
                ..ObjectMeta::default()
            },
            ..Namespace::default()
        };
        Api::<Namespace>::all(client.clone())
            .create(&PostParams::default(), &ns)
            .await
            .expect("create namespace");
        Self { client, name }
    }

    fn deployment(&self, name: &str) -> Value {
        json!({
            "apiVersion": "apps/v1",
            "kind": "Deployment",
            "metadata": { "name": name, "namespace": self.name, "labels": { "app.kubernetes.io/managed-by": "kuben", "kuben.dev/app": "web" } },
            "spec": {
                "replicas": 0,
                "selector": { "matchLabels": { "kuben.dev/app": "web" } },
                "template": {
                    "metadata": { "labels": { "kuben.dev/app": "web" } },
                    "spec": { "containers": [{ "name": "web", "image": "registry.invalid/web:none" }] }
                }
            }
        })
    }

    fn service(&self) -> Value {
        json!({
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": { "name": "web", "namespace": self.name, "labels": { "app.kubernetes.io/managed-by": "kuben", "kuben.dev/app": "web" } },
            "spec": { "selector": { "kuben.dev/app": "web" }, "ports": [{ "port": 80, "targetPort": 8080 }] }
        })
    }

    fn envelope(&self, generation: i64, resources: &Value, digest: Option<&str>) -> Apply {
        let resources = resources.to_string();
        let spec = ApplicationRuntimeSpec {
            target_id: "0199a0c0-0000-7000-8000-000000000001".into(),
            lifecycle_uid: "0199a0c0-0000-7000-8000-000000000002".into(),
            control_epoch: 0,
            generation,
            input_hash: sha256(&format!("input-{generation}")),
            release_id: "0199a0c0-0000-7000-8000-000000000003".into(),
            plan: PlanEnvelope {
                id: format!("plan-{generation}"),
                renderer_version: "kuben-renderer/1".into(),
                digest: digest.map_or_else(|| sha256(&resources), str::to_owned),
                resources,
            },
        };
        Apply {
            target: "0199a0c0-0000-7000-8000-000000000001".into(),
            namespace: self.name.clone(),
            name: "web".into(),
            spec: serde_json::to_string(&spec).expect("spec"),
        }
    }

    async fn cleanup(self) {
        let _ = Api::<Namespace>::all(self.client)
            .delete(&self.name, &DeleteParams::background())
            .await;
    }
}

/// Carry `apply` out and return the observations, in order.
async fn carry(executor: &KubeExecutor, apply: Apply) -> Vec<Observation> {
    let (tx, mut rx) = mpsc::channel(16);
    tokio::time::timeout(Duration::from_secs(90), executor.carry(apply, tx))
        .await
        .expect("carried out in time");
    let mut seen = Vec::new();
    while let Ok(observation) = rx.try_recv() {
        seen.push(observation);
    }
    seen
}

fn phases(seen: &[Observation]) -> Vec<RuntimePhase> {
    seen.iter().map(|o| o.phase).collect()
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster (cargo test -- --ignored)"]
async fn the_agent_carries_an_envelope_out_and_prunes_what_a_newer_plan_dropped() {
    let ns = TestNs::new().await;
    let executor = KubeExecutor::new(ns.client.clone())
        .with_verify_deadline(Duration::from_mins(1), Duration::from_secs(1));

    let first = carry(
        &executor,
        ns.envelope(1, &json!([ns.deployment("web-web"), ns.service()]), None),
    )
    .await;
    assert_eq!(
        phases(&first),
        [
            RuntimePhase::Accepted,
            RuntimePhase::Applying,
            RuntimePhase::Ready
        ],
        "{first:?}"
    );

    let runtimes: Api<ApplicationRuntime> = Api::namespaced(ns.client.clone(), &ns.name);
    let runtime = runtimes.get("web").await.expect("runtime");
    assert_eq!(runtime.spec.generation, 1);
    let status = runtime.status.expect("status");
    assert_eq!(
        (status.observed_generation, status.effective_generation),
        (Some(1), Some(1))
    );
    assert_eq!(status.inventory.len(), 2);

    let deployments: Api<Deployment> = Api::namespaced(ns.client.clone(), &ns.name);
    let web = deployments.get("web-web").await.expect("deployment");
    let owner = &web.metadata.owner_references.expect("owners")[0];
    assert_eq!(
        (owner.kind.as_str(), owner.controller),
        ("ApplicationRuntime", Some(true))
    );
    assert_eq!(Some(&owner.uid), runtime.metadata.uid.as_ref());

    // A newer plan without the Service: it is pruned, the Deployment stays.
    let second = carry(
        &executor,
        ns.envelope(2, &json!([ns.deployment("web-web")]), None),
    )
    .await;
    assert_eq!(phases(&second).last(), Some(&RuntimePhase::Ready), "{second:?}");
    let services: Api<Service> = Api::namespaced(ns.client.clone(), &ns.name);
    let mut gone = false;
    for _ in 0..30 {
        if services.get_opt("web").await.expect("read").is_none() {
            gone = true;
            break;
        }
        tokio::time::sleep(Duration::from_secs(1)).await;
    }
    assert!(gone, "the dropped Service is pruned");
    assert!(deployments.get_opt("web-web").await.expect("read").is_some());

    // A lower generation: the apiserver refuses the envelope.
    let stale = carry(
        &executor,
        ns.envelope(1, &json!([ns.deployment("web-web")]), None),
    )
    .await;
    assert_eq!(phases(&stale), [RuntimePhase::Rejected], "{stale:?}");
    assert_eq!(stale[0].reason.as_deref(), Some("EnvelopeRefused"));
    ns.cleanup().await;
}

/// A report that never reached the hub (plan §18.1 crash/ACK replay, I17):
/// the hub sends the same envelope again, and carrying it out again changes
/// nothing: the same objects, the same generations, Ready again.
#[tokio::test]
#[ignore = "needs a Kubernetes cluster (cargo test -- --ignored)"]
async fn the_same_envelope_carried_again_changes_nothing() {
    let ns = TestNs::new().await;
    let executor = KubeExecutor::new(ns.client.clone())
        .with_verify_deadline(Duration::from_mins(1), Duration::from_secs(1));
    let envelope = ns.envelope(1, &json!([ns.deployment("web-web"), ns.service()]), None);
    let first = carry(&executor, envelope.clone()).await;
    assert_eq!(phases(&first).last(), Some(&RuntimePhase::Ready), "{first:?}");

    let deployments: Api<Deployment> = Api::namespaced(ns.client.clone(), &ns.name);
    let runtimes: Api<ApplicationRuntime> = Api::namespaced(ns.client.clone(), &ns.name);
    let deployment = |d: Deployment| (d.metadata.uid, d.metadata.generation);
    let before = deployment(deployments.get("web-web").await.expect("deployment"));
    let runtime_before = runtimes.get("web").await.expect("runtime").metadata;

    let again = carry(&executor, envelope).await;
    assert_eq!(phases(&again).last(), Some(&RuntimePhase::Ready), "{again:?}");
    assert!(
        !phases(&again).contains(&RuntimePhase::Rejected),
        "the apiserver takes the same envelope again: {again:?}"
    );
    assert_eq!(
        deployment(deployments.get("web-web").await.expect("deployment")),
        before,
        "the same Deployment, not rolled again"
    );
    let runtime_after = runtimes.get("web").await.expect("runtime").metadata;
    assert_eq!(
        (runtime_after.uid, runtime_after.generation),
        (runtime_before.uid, runtime_before.generation)
    );
    ns.cleanup().await;
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster (cargo test -- --ignored)"]
async fn the_agent_refuses_a_wrong_digest_or_kind_before_writing_anything() {
    let ns = TestNs::new().await;
    let executor = KubeExecutor::new(ns.client.clone());
    let forged = carry(
        &executor,
        ns.envelope(
            1,
            &json!([ns.deployment("web-web")]),
            Some(&sha256("something else")),
        ),
    )
    .await;
    assert_eq!(forged[0].reason.as_deref(), Some("DigestMismatch"));
    let secret =
        json!([{ "apiVersion": "v1", "kind": "Secret", "metadata": { "name": "x", "namespace": ns.name } }]);
    let refused = carry(&executor, ns.envelope(1, &secret, None)).await;
    assert_eq!(refused[0].reason.as_deref(), Some("KindNotAllowed"));
    let runtimes: Api<ApplicationRuntime> = Api::namespaced(ns.client.clone(), &ns.name);
    assert!(
        runtimes.get_opt("web").await.expect("read").is_none(),
        "nothing written"
    );
    ns.cleanup().await;
}
