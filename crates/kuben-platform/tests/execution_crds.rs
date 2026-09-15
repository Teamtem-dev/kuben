//! M1.9a (WS-E): the apiserver itself enforces the execution CRDs' rules
//! (ADR-027). The unit tests in `kuben-crd` run the same CEL rules on the
//! client; this proves the real apiserver accepts the CRDs (the rules fit its
//! cost budget) and refuses every forbidden change with 422:
//!
//! * `ApplicationRuntime`: a lower generation, the same generation with
//!   another input hash, a changed target, a lower observed generation;
//!   the same envelope again and a higher generation pass;
//! * `ExecutionTask`: a changed input, a withdrawn or changed cancel
//!   request, a rewritten receipt; adding a cancel request and writing the
//!   first receipt pass.
//!
//! Needs a cluster: the tests are ignored by default and run with
//! `cargo test -- --ignored` against the current kube context (the kind job in
//! CI), each in its own throwaway namespace.

use std::time::Duration;

use k8s_openapi::{
    api::core::v1::Namespace, apiextensions_apiserver::pkg::apis::apiextensions::v1::CustomResourceDefinition,
};
use kube::{
    Api, Client, CustomResourceExt,
    api::{DeleteParams, ObjectMeta, Patch, PatchParams, PostParams},
};
use kuben_crd::{
    ApplicationRuntime, ApplicationRuntimeSpec, ExecutionKind, ExecutionTask, ExecutionTaskSpec,
    FIELD_MANAGER, PlanEnvelope,
};
use serde_json::{Value, json};

fn hash(c: char) -> String {
    format!("sha256:{}", c.to_string().repeat(64))
}

fn is_invalid(err: &kube::Error) -> bool {
    matches!(err, kube::Error::Api(status) if status.code == 422)
}

struct TestNs {
    client: Client,
    name: String,
}

impl TestNs {
    /// A throwaway namespace, after installing both CRDs as `kuben serve`
    /// does (server-side apply) and waiting until they are established.
    async fn new() -> Self {
        let client = Client::try_default().await.expect("kube client");
        let crds = Api::<CustomResourceDefinition>::all(client.clone());
        for crd in [ApplicationRuntime::crd(), ExecutionTask::crd()] {
            let name = crd.metadata.name.clone().expect("CRD name");
            crds.patch(
                &name,
                &PatchParams::apply(FIELD_MANAGER).force(),
                &Patch::Apply(&crd),
            )
            .await
            .unwrap_or_else(|e| panic!("the apiserver refused CRD {name}: {e}"));
            let mut established = false;
            for _ in 0..60 {
                let live = crds.get(&name).await.expect("read CRD");
                established = live
                    .status
                    .and_then(|s| s.conditions)
                    .is_some_and(|cs| cs.iter().any(|c| c.type_ == "Established" && c.status == "True"));
                if established {
                    break;
                }
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            assert!(established, "CRD {name} established");
        }
        let name = format!("kuben-m1-exec-{}", uuid::Uuid::now_v7().simple());
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

    fn api<K>(&self) -> Api<K>
    where
        K: kube::Resource<Scope = kube::core::NamespaceResourceScope>,
        <K as kube::Resource>::DynamicType: Default,
    {
        Api::namespaced(self.client.clone(), &self.name)
    }

    async fn cleanup(self) {
        let _ = Api::<Namespace>::all(self.client)
            .delete(&self.name, &DeleteParams::background())
            .await;
    }
}

async fn merge<K>(api: &Api<K>, name: &str, patch: Value) -> Result<K, kube::Error>
where
    K: kube::Resource + Clone + serde::de::DeserializeOwned + std::fmt::Debug,
{
    api.patch(name, &PatchParams::default(), &Patch::Merge(&patch))
        .await
}

async fn merge_status<K>(api: &Api<K>, name: &str, patch: Value) -> Result<K, kube::Error>
where
    K: kube::Resource + Clone + serde::de::DeserializeOwned + std::fmt::Debug,
{
    api.patch_status(name, &PatchParams::default(), &Patch::Merge(&patch))
        .await
}

fn refused<K: std::fmt::Debug>(result: Result<K, kube::Error>, what: &str) {
    match result {
        Err(e) if is_invalid(&e) => {}
        other => panic!("{what}: expected 422, got {other:?}"),
    }
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster (cargo test -- --ignored)"]
async fn the_apiserver_fences_application_runtimes() {
    let ns = TestNs::new().await;
    let api = ns.api::<ApplicationRuntime>();
    let runtime = ApplicationRuntime::new(
        "web",
        ApplicationRuntimeSpec {
            target_id: "0199a0c0-0000-7000-8000-000000000001".into(),
            lifecycle_uid: "0199a0c0-0000-7000-8000-000000000002".into(),
            control_epoch: 1,
            generation: 5,
            input_hash: hash('a'),
            release_id: "0199a0c0-0000-7000-8000-000000000003".into(),
            plan: PlanEnvelope {
                id: "0199a0c0-0000-7000-8000-000000000004".into(),
                renderer_version: "kuben-renderer/1".into(),
                digest: hash('e'),
                resources: "[]".into(),
            },
        },
    );
    api.create(&PostParams::default(), &runtime)
        .await
        .expect("create");

    refused(
        merge(
            &api,
            "web",
            json!({ "spec": { "generation": 4, "inputHash": hash('b') } }),
        )
        .await,
        "a lower generation",
    );
    refused(
        merge(&api, "web", json!({ "spec": { "inputHash": hash('b') } })).await,
        "the same generation with another input hash",
    );
    refused(
        merge(
            &api,
            "web",
            json!({ "spec": { "targetId": "0199a0c0-0000-7000-8000-00000000000f" } }),
        )
        .await,
        "a changed target",
    );
    refused(
        merge(&api, "web", json!({ "spec": { "controlEpoch": 0 } })).await,
        "a lower control epoch",
    );
    merge(
        &api,
        "web",
        json!({ "spec": { "generation": 5, "inputHash": hash('a') } }),
    )
    .await
    .expect("the same envelope again");
    let moved = merge(
        &api,
        "web",
        json!({ "spec": { "generation": 6, "inputHash": hash('b') } }),
    )
    .await
    .expect("a higher generation");
    assert_eq!(moved.spec.generation, 6);

    merge_status(&api, "web", json!({ "status": { "observedGeneration": 6 } }))
        .await
        .expect("observed");
    refused(
        merge_status(&api, "web", json!({ "status": { "observedGeneration": 5 } })).await,
        "a lower observed generation",
    );
    ns.cleanup().await;
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster (cargo test -- --ignored)"]
async fn the_apiserver_keeps_task_inputs_cancels_and_receipts() {
    let ns = TestNs::new().await;
    let api = ns.api::<ExecutionTask>();
    let task = ExecutionTask::new(
        "build-1",
        ExecutionTaskSpec {
            kind: ExecutionKind::Build,
            operation_id: "0199a0c0-0000-7000-8000-000000000001".into(),
            attempt: 1,
            input: r#"{"source":"git"}"#.into(),
            input_hash: hash('a'),
            deadline: "2026-09-15T12:00:00Z".into(),
            cancel: None,
        },
    );
    api.create(&PostParams::default(), &task).await.expect("create");

    refused(
        merge(
            &api,
            "build-1",
            json!({ "spec": { "input": r#"{"source":"other"}"# } }),
        )
        .await,
        "a changed input",
    );
    refused(
        merge(&api, "build-1", json!({ "spec": { "kind": "Backup" } })).await,
        "a changed kind",
    );
    let cancel = json!({ "requestedAt": "2026-09-15T11:00:00Z", "reason": "user" });
    merge(&api, "build-1", json!({ "spec": { "cancel": cancel } }))
        .await
        .expect("a cancel request is added");
    refused(
        merge(
            &api,
            "build-1",
            json!({ "spec": { "cancel": { "reason": "someone else" } } }),
        )
        .await,
        "a changed cancel request",
    );
    refused(
        merge(&api, "build-1", json!({ "spec": { "cancel": null } })).await,
        "a withdrawn cancel request",
    );

    let receipt = json!({ "outcome": "Cancelled", "finishedAt": "2026-09-15T11:30:00Z" });
    merge_status(
        &api,
        "build-1",
        json!({ "status": { "phase": "Cancelled", "receipt": receipt } }),
    )
    .await
    .expect("the first receipt");
    refused(
        merge_status(
            &api,
            "build-1",
            json!({ "status": { "receipt": { "outcome": "Succeeded" } } }),
        )
        .await,
        "a rewritten receipt",
    );
    refused(
        merge_status(&api, "build-1", json!({ "status": { "receipt": null } })).await,
        "a removed receipt",
    );
    ns.cleanup().await;
}
