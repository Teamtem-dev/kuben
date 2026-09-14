//! M0 spike (ADR-027): two writers against a real Kubernetes API.
//!
//! Questions this answers before the agent is built:
//! 1. Does a replace with a stale `resourceVersion` fail with 409?
//! 2. Does server-side apply honour a `resourceVersion` precondition carried in
//!    the applied object, or does it overwrite?
//! 3. Does "re-read, compare generation, conditional replace" keep the highest
//!    generation when two writers race with different generations?
//!
//! Needs a cluster: the tests are ignored by default and run with
//! `cargo test -- --ignored` against the current kube context (the kind job in
//! CI), each in its own throwaway namespace. An unreachable cluster fails the
//! run instead of skipping it.

use std::collections::BTreeMap;

use k8s_openapi::api::core::v1::{ConfigMap, Namespace};
use kube::{
    Api, Client,
    api::{DeleteParams, ObjectMeta, Patch, PatchParams, PostParams},
};

const GEN_KEY: &str = "generation";

fn cm(name: &str, generation: u64) -> ConfigMap {
    ConfigMap {
        metadata: ObjectMeta {
            name: Some(name.into()),
            ..ObjectMeta::default()
        },
        data: Some(BTreeMap::from([(GEN_KEY.to_owned(), generation.to_string())])),
        ..ConfigMap::default()
    }
}

fn generation_of(cm: &ConfigMap) -> u64 {
    cm.data
        .as_ref()
        .and_then(|d| d.get(GEN_KEY))
        .and_then(|g| g.parse().ok())
        .unwrap_or(0)
}

fn is_conflict(err: &kube::Error) -> bool {
    matches!(err, kube::Error::Api(status) if status.code == 409)
}

struct TestNs {
    client: Client,
    name: String,
}

impl TestNs {
    async fn new() -> Self {
        let client = Client::try_default().await.expect("kube client");
        let name = format!("kuben-m0-cas-{}", uuid::Uuid::now_v7().simple());
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

    fn api(&self) -> Api<ConfigMap> {
        Api::namespaced(self.client.clone(), &self.name)
    }

    async fn cleanup(self) {
        let _ = Api::<Namespace>::all(self.client)
            .delete(&self.name, &DeleteParams::background())
            .await;
    }
}

/// The agent's write rule: write `generation` only if the live object is
/// older, retrying on conflicts with a fresh read. Returns whether it wrote.
async fn write_if_newer(api: &Api<ConfigMap>, name: &str, generation: u64) -> bool {
    for _ in 0..20 {
        let live = api.get(name).await.expect("get");
        if generation_of(&live) >= generation {
            return false;
        }
        let mut next = live.clone();
        next.data = cm(name, generation).data;
        match api.replace(name, &PostParams::default(), &next).await {
            Ok(_) => return true,
            Err(e) if is_conflict(&e) => {}
            Err(e) => panic!("replace failed: {e}"),
        }
    }
    panic!("no progress after 20 conflicts");
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster; run with --ignored"]
async fn stale_resource_version_replace_is_rejected() {
    let ns = TestNs::new().await;
    let api = ns.api();
    api.create(&PostParams::default(), &cm("target", 1))
        .await
        .expect("create");

    let seen_by_a = api.get("target").await.expect("a reads");
    let seen_by_b = api.get("target").await.expect("b reads");

    let mut b = seen_by_b.clone();
    b.data = cm("target", 2).data;
    api.replace("target", &PostParams::default(), &b)
        .await
        .expect("b writes first");

    let mut a = seen_by_a.clone();
    a.data = cm("target", 1).data;
    let err = api
        .replace("target", &PostParams::default(), &a)
        .await
        .expect_err("a's stale write must fail");
    assert!(is_conflict(&err), "expected 409, got {err}");
    assert_eq!(generation_of(&api.get("target").await.expect("read")), 2);
    ns.cleanup().await;
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster; run with --ignored"]
async fn server_side_apply_with_a_stale_resource_version_is_rejected() {
    let ns = TestNs::new().await;
    let api = ns.api();
    let pp = PatchParams::apply("kuben-m0").force();

    let first = api
        .patch("target", &pp, &Patch::Apply(cm("target", 1)))
        .await
        .expect("apply 1");
    let stale_rv = first.metadata.resource_version.clone();
    api.patch("target", &pp, &Patch::Apply(cm("target", 2)))
        .await
        .expect("apply 2");

    let mut stale = cm("target", 1);
    stale.metadata.resource_version = stale_rv;
    let result = api.patch("target", &pp, &Patch::Apply(stale)).await;

    // Record the answer either way; the ADR picks the primitive from it.
    match &result {
        Err(e) if is_conflict(e) => eprintln!("SSA honours resourceVersion preconditions (409)"),
        Ok(_) => eprintln!("SSA ignored the stale resourceVersion"),
        Err(e) => panic!("unexpected error: {e}"),
    }
    assert!(
        matches!(&result, Err(e) if is_conflict(e)),
        "SSA with a stale resourceVersion must not overwrite; otherwise use conditional replace"
    );
    assert_eq!(generation_of(&api.get("target").await.expect("read")), 2);
    ns.cleanup().await;
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster; run with --ignored"]
async fn racing_writers_keep_the_highest_generation() {
    let ns = TestNs::new().await;
    let api = ns.api();
    api.create(&PostParams::default(), &cm("target", 1))
        .await
        .expect("create");

    for round in 0..10u64 {
        let base = 10 * (round + 1);
        let (a, b) = (api.clone(), api.clone());
        // An old writer (base+1) and a new writer (base+2) race.
        let old = tokio::spawn(async move { write_if_newer(&a, "target", base + 1).await });
        let new = tokio::spawn(async move { write_if_newer(&b, "target", base + 2).await });
        let (_, wrote_new) = (old.await.expect("join"), new.await.expect("join"));
        let live = generation_of(&api.get("target").await.expect("read"));
        assert_eq!(live, base + 2, "round {round}: a lower generation won");
        assert!(wrote_new || live == base + 2);
    }
    ns.cleanup().await;
}
