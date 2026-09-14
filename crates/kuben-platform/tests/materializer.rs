//! The materializer against a real API server and PostgreSQL (ADR-032).
//!
//! Needs both: the tests are ignored by default and run with
//! `cargo test -- --ignored` in the kind job of CI, where `KUBEN_TEST_PG_URL`
//! names the job's PostgreSQL. Each test works in a project and namespace of
//! its own, and in a database schema of its own. No controller runs here: the
//! tests create the environment's namespace and play the App controller.

use std::{collections::BTreeMap, time::Duration};

use k8s_openapi::{
    api::core::v1::{
        Namespace, PersistentVolumeClaim, PersistentVolumeClaimSpec, VolumeResourceRequirements,
    },
    apimachinery::pkg::api::resource::Quantity,
};
use kube::{
    Api, Client, ResourceExt,
    api::{DeleteParams, ObjectMeta, Patch, PatchParams, PostParams},
};
use kuben_core::{
    artifact::Digest,
    ids::{
        ConfigRevisionId, DeploymentRunId, EnvironmentId, OperationId, OrgId, ProjectId, ReleaseId, TargetId,
    },
    ops::{Generation, RunPhase},
};
use kuben_crd::{App, AppSpec, Environment, PreviewPolicy, Project, ProjectSpec, labels};
use kuben_platform::{
    controller::{crd_apply, resources::RETAIN},
    materializer::{Worker, drift::Finding, render::annotations},
};
use kuben_store::{
    Store,
    repo::{
        ENVIRONMENT_APPLY, ENVIRONMENT_DELETE, NewAudit, PROJECT_DELETE, PortableRelease, RunReason,
        StartDeployment, Started, Subject, TARGET_DELETE,
    },
    testing::pg_store,
};
use serde_json::json;
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
const REPOSITORY: &str = "registry.example.com/acme/web";

/// One organization with project `m<random>`, environment `prod` and app
/// `web`, a release and a configuration revision, in SQL; the environment's
/// namespace in the cluster.
struct World {
    store: Store,
    client: Client,
    org: OrgId,
    project: ProjectId,
    environment: EnvironmentId,
    target: TargetId,
    lifecycle_uid: Uuid,
    release: ReleaseId,
    revision: ConfigRevisionId,
    slug: String,
    namespace: String,
}

impl World {
    async fn new() -> Self {
        let store = pg_store()
            .await
            .expect("set KUBEN_TEST_PG_URL: these tests need PostgreSQL");
        let client = Client::try_default().await.expect("kube client");
        crd_apply::ensure(client.clone()).await.expect("apply the CRDs");
        let id = Uuid::now_v7().simple().to_string();
        let slug = format!("m{}", &id[id.len() - 10..]);
        let namespace = format!("kb-{slug}-prod");

        let org = store.create_org(&slug, &slug).await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project(&slug, "Materialized").await.expect("project");
        let env = t
            .create_environment(project, "prod", "Prod", false)
            .await
            .expect("environment");
        let cluster = t.create_cluster("kind").await.expect("cluster");
        let placement = t
            .create_placement(project, env, cluster, &namespace)
            .await
            .expect("placement");
        let application = t.create_application(project, "web", "Web").await.expect("app");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        let digest: Digest = DIGEST.parse().expect("digest");
        let (release, _) = t
            .create_release(
                project,
                &PortableRelease {
                    application,
                    artifacts: BTreeMap::from([("web".to_owned(), digest)]),
                    process_contract: json!({}),
                    portable_config: json!({}),
                    renderer_schema: 1,
                    source: Some(json!({ "image_repository": REPOSITORY })),
                    created_by: "user:test".into(),
                },
            )
            .await
            .expect("release");
        let config = json!({ "runtime": { "processes": { "web": { "port": 8080 } } } });
        let (revision, _) = t
            .create_config_revision(project, target, &config, "user:test")
            .await
            .expect("revision")
            .expect("target");
        let lifecycle_uid = t
            .target_state(target)
            .await
            .expect("read")
            .expect("target")
            .lifecycle_uid;
        t.commit().await.expect("commit");

        let ns = Namespace {
            metadata: ObjectMeta {
                name: Some(namespace.clone()),
                ..ObjectMeta::default()
            },
            ..Namespace::default()
        };
        Api::<Namespace>::all(client.clone())
            .create(&PostParams::default(), &ns)
            .await
            .expect("namespace");
        Self {
            store,
            client,
            org,
            project,
            environment: env,
            target,
            lifecycle_uid,
            release,
            revision,
            slug,
            namespace,
        }
    }

    /// Accept a deployment that expects generation `expected`.
    async fn deploy(&self, expected: u64) -> (OperationId, DeploymentRunId) {
        let request = StartDeployment {
            project: self.project,
            target: self.target,
            release: self.release,
            config_revision: self.revision,
            render_plan: None,
            expected_generation: Generation(expected),
            lifecycle_uid: self.lifecycle_uid,
            reason: RunReason::Deploy,
            requested_by: "user:test".into(),
            input_hash: format!("deploy-{expected}").into_bytes(),
        };
        let audit = NewAudit {
            actor_kind: "user".into(),
            action: "startDeployment".into(),
            outcome: "accepted".into(),
            ..NewAudit::default()
        };
        let mut t = self.store.tenant(self.org).await.expect("tenant");
        let started = t.start_deployment(&request, audit, None).await.expect("start");
        t.commit().await.expect("commit");
        let Started::Accepted { operation, run, .. } = started else {
            panic!("not accepted: {started:?}");
        };
        (operation, run)
    }

    async fn phase(&self, run: DeploymentRunId) -> RunPhase {
        let mut t = self.store.tenant(self.org).await.expect("tenant");
        t.run_phase(run).await.expect("read").expect("run").0
    }

    fn apps(&self) -> Api<App> {
        Api::namespaced(self.client.clone(), &self.namespace)
    }

    fn worker(&self) -> Worker {
        Worker::new(self.store.clone(), self.client.clone(), "materializer-test")
    }

    fn managed_meta(name: &str, org: OrgId) -> ObjectMeta {
        ObjectMeta {
            name: Some(name.to_owned()),
            labels: Some(BTreeMap::from([
                (labels::MANAGED_BY.to_owned(), labels::MANAGER.to_owned()),
                (labels::ORG.to_owned(), org.to_string()),
            ])),
            ..ObjectMeta::default()
        }
    }

    /// Environments follow their project (owner reference).
    async fn cleanup(self) {
        let _ = Api::<Project>::all(self.client.clone())
            .delete(&self.slug, &DeleteParams::background())
            .await;
        let _ = Api::<Namespace>::all(self.client)
            .delete(&self.namespace, &DeleteParams::background())
            .await;
    }
}

fn generation_annotation(app: &App) -> Option<&str> {
    app.annotations().get(annotations::GENERATION).map(String::as_str)
}

async fn wait_for_app(api: &Api<App>) -> App {
    for _ in 0..100 {
        if let Some(app) = api.get_opt("web").await.expect("read") {
            return app;
        }
        tokio::time::sleep(Duration::from_millis(300)).await;
    }
    panic!("the App was never written");
}

/// Play the App controller: the written generation is observed and ready.
async fn report_ready(api: &Api<App>, app: &App) {
    let generation = app.metadata.generation;
    let status = json!({
        "apiVersion": "kuben.dev/v1alpha1",
        "kind": "App",
        "status": {
            "observedGeneration": generation,
            "conditions": [{
                "type": "Ready", "status": "True", "reason": "Available",
                "observedGeneration": generation,
            }],
        },
    });
    api.patch_status(
        "web",
        &PatchParams::apply("test-controller").force(),
        &Patch::Apply(&status),
    )
    .await
    .expect("status");
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster and PostgreSQL; run with --ignored"]
async fn a_run_is_written_verified_and_recorded() {
    let w = World::new().await;
    let (operation, run) = w.deploy(0).await;
    let worker = w.worker().with_verify_deadline(Duration::from_mins(2));
    let token = CancellationToken::new();
    let task = tokio::spawn(async move { worker.work_once(&token).await });

    let apps = w.apps();
    let app = wait_for_app(&apps).await;
    assert_eq!(generation_annotation(&app), Some("1"));
    assert_eq!(
        app.annotations().get(annotations::OPERATION),
        Some(&operation.to_string())
    );
    assert_eq!(
        app.spec.source.image.as_deref(),
        Some(format!("{REPOSITORY}@{DIGEST}").as_str())
    );
    report_ready(&apps, &app).await;
    assert_eq!(task.await.expect("join").expect("work"), Some(operation));
    assert_eq!(w.phase(run).await, RunPhase::Succeeded);

    let mut t = w.store.tenant(w.org).await.expect("tenant");
    let written = t.materialized(w.target).await.expect("read").expect("recorded");
    drop(t);
    assert_eq!(written.generation, Generation(1));
    assert_eq!(Some(written.resource_uid.as_str()), app.metadata.uid.as_deref());

    let env = Api::<Environment>::all(w.client.clone())
        .get(&format!("{}-prod", w.slug))
        .await
        .expect("environment");
    assert_eq!(env.labels()[labels::ORG], w.org.to_string());
    assert_eq!(
        env.metadata.owner_references.as_ref().expect("owner")[0].kind,
        "Project"
    );
    w.cleanup().await;
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster and PostgreSQL; run with --ignored"]
async fn a_superseded_run_never_writes_and_a_forged_generation_is_replaced() {
    let w = World::new().await;
    let (older, older_run) = w.deploy(0).await;
    let (newer, newer_run) = w.deploy(1).await;

    // Someone else writes the App and claims generation 99 on it.
    let mut meta = World::managed_meta("web", w.org);
    meta.annotations = Some(BTreeMap::from([(
        annotations::GENERATION.to_owned(),
        "99".to_owned(),
    )]));
    let spec: AppSpec = serde_json::from_value(json!({
        "source": { "image": "evil.example.com/web:latest" },
        "runtime": { "processes": { "web": { "port": 80 } } },
    }))
    .expect("spec");
    let forged = App {
        metadata: meta,
        spec,
        status: None,
    };
    w.apps()
        .create(&PostParams::default(), &forged)
        .await
        .expect("forged app");

    // No controller answers: verification ends at once.
    let worker = w.worker().with_verify_deadline(Duration::ZERO);
    let token = CancellationToken::new();
    assert_eq!(
        worker.work_once(&token).await.expect("work"),
        Some(older),
        "the older operation is due first"
    );
    assert_eq!(w.phase(older_run).await, RunPhase::Superseded);
    let live = w.apps().get("web").await.expect("app");
    assert_eq!(
        generation_annotation(&live),
        Some("99"),
        "a superseded run writes nothing"
    );

    assert_eq!(worker.work_once(&token).await.expect("work"), Some(newer));
    let live = w.apps().get("web").await.expect("app");
    assert_eq!(
        generation_annotation(&live),
        Some("2"),
        "the forged value is replaced"
    );
    assert_eq!(
        live.spec.source.image.as_deref(),
        Some(format!("{REPOSITORY}@{DIGEST}").as_str())
    );
    assert_eq!(
        w.phase(newer_run).await,
        RunPhase::Failed,
        "nobody verified it in time"
    );
    w.cleanup().await;
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster and PostgreSQL; run with --ignored"]
async fn a_name_another_organization_holds_is_never_taken() {
    let w = World::new().await;
    let theirs = Project {
        metadata: World::managed_meta(&w.slug, OrgId::new()),
        spec: ProjectSpec {
            display_name: "Theirs".into(),
            description: None,
            previews: PreviewPolicy::default(),
        },
        status: None,
    };
    let projects = Api::<Project>::all(w.client.clone());
    projects
        .create(&PostParams::default(), &theirs)
        .await
        .expect("their project");

    let (operation, run) = w.deploy(0).await;
    assert_eq!(
        w.worker()
            .work_once(&CancellationToken::new())
            .await
            .expect("work"),
        Some(operation)
    );
    assert_eq!(w.phase(run).await, RunPhase::Failed);
    assert!(
        w.apps().get_opt("web").await.expect("read").is_none(),
        "nothing was written"
    );
    assert_eq!(
        projects
            .get(&w.slug)
            .await
            .expect("their project")
            .spec
            .display_name,
        "Theirs"
    );
    w.cleanup().await;
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster and PostgreSQL; run with --ignored"]
async fn drift_is_recorded_and_replaced() {
    let w = World::new().await;
    let (operation, _) = w.deploy(0).await;
    let worker = w.worker().with_verify_deadline(Duration::from_mins(2));
    let task = {
        let worker = worker.clone();
        tokio::spawn(async move { worker.work_once(&CancellationToken::new()).await })
    };
    let apps = w.apps();
    let app = wait_for_app(&apps).await;
    report_ready(&apps, &app).await;
    assert_eq!(task.await.expect("join").expect("work"), Some(operation));
    assert_eq!(
        worker.check_drift(&app).await.expect("check"),
        Finding::Clean,
        "a status update is not drift"
    );

    // Someone edits the image with kubectl.
    let edit = PatchParams {
        field_manager: Some("kubectl-edit".into()),
        ..PatchParams::default()
    };
    let body = json!({ "spec": { "source": { "image": "evil.example.com/web:latest" } } });
    let edited = apps
        .patch("web", &edit, &Patch::Merge(&body))
        .await
        .expect("edit");
    let Finding::Drifted(drift) = worker.check_drift(&edited).await.expect("check") else {
        panic!("an edit is drift");
    };
    assert!(drift.spec_changed && !drift.deleted);
    assert_eq!(drift.managers, vec!["kubectl-edit".to_owned()]);
    let replaced = apps.get("web").await.expect("app");
    assert_eq!(
        replaced.spec.source.image.as_deref(),
        Some(format!("{REPOSITORY}@{DIGEST}").as_str()),
        "SQL's rendering is written again"
    );
    assert_eq!(
        worker.check_drift(&replaced).await.expect("check"),
        Finding::Clean,
        "the replacement is not drift"
    );

    // Someone deletes it: it is written again, as a new object.
    apps.delete("web", &DeleteParams::default())
        .await
        .expect("delete");
    for _ in 0..50 {
        if apps.get_opt("web").await.expect("read").is_none() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    let Finding::Drifted(drift) = worker.check_drift(&replaced).await.expect("check") else {
        panic!("a deletion is drift");
    };
    assert!(drift.deleted);
    let recreated = apps.get("web").await.expect("written again");
    assert_ne!(recreated.uid(), replaced.uid());

    let mut t = w.store.tenant(w.org).await.expect("tenant");
    let record = t.materialized(w.target).await.expect("read").expect("recorded");
    drop(t);
    assert_eq!(record.drift_count, 2);
    assert_eq!(
        Some(record.resource_uid.as_str()),
        recreated.metadata.uid.as_deref()
    );
    assert_eq!(Some(record.resource_generation), recreated.metadata.generation);
    assert_eq!(record.drift.expect("described")["deleted"], true);
    w.cleanup().await;
}

impl World {
    /// Record `kind` on `subject` the way the API does: a deletion marks its
    /// subject deleting in the same transaction.
    async fn request(&self, kind: &str, subject: Subject) -> OperationId {
        let mut t = self.store.tenant(self.org).await.expect("tenant");
        let marked = match kind {
            TARGET_DELETE => t.mark_target_deleting(self.target).await,
            ENVIRONMENT_DELETE => t.mark_environment_deleting(self.environment).await,
            PROJECT_DELETE => t.mark_project_deleting(self.project).await,
            _ => Ok(true),
        };
        assert!(marked.expect("mark"), "{kind}: marked deleting once");
        let audit = NewAudit {
            actor_kind: "user".into(),
            action: kind.into(),
            outcome: "accepted".into(),
            ..NewAudit::default()
        };
        let operation = t
            .request(kind, subject, "user:test", audit)
            .await
            .expect("request");
        t.commit().await.expect("commit");
        operation
    }

    /// A volume of the app `web` that its deletion keeps unless asked.
    fn retained_volume() -> PersistentVolumeClaim {
        PersistentVolumeClaim {
            metadata: ObjectMeta {
                name: Some("web-data".into()),
                labels: Some(BTreeMap::from([(labels::APP.to_owned(), "web".to_owned())])),
                annotations: Some(BTreeMap::from([(RETAIN.to_owned(), "true".to_owned())])),
                ..ObjectMeta::default()
            },
            spec: Some(PersistentVolumeClaimSpec {
                access_modes: Some(vec!["ReadWriteOnce".into()]),
                resources: Some(VolumeResourceRequirements {
                    requests: Some(BTreeMap::from([("storage".to_owned(), Quantity("1Mi".into()))])),
                    ..VolumeResourceRequirements::default()
                }),
                ..PersistentVolumeClaimSpec::default()
            }),
            ..PersistentVolumeClaim::default()
        }
    }
}

#[tokio::test]
#[ignore = "needs a Kubernetes cluster and PostgreSQL; run with --ignored"]
async fn lifecycle_operations_write_and_remove_resources() {
    let w = World::new().await;
    let worker = w
        .worker()
        .with_verify_deadline(Duration::from_mins(2))
        .with_deletion_check(Duration::ZERO);
    let token = CancellationToken::new();
    let environments = Api::<Environment>::all(w.client.clone());
    let env_name = format!("{}-prod", w.slug);

    // An environment is written as soon as it exists.
    let apply = w
        .request(ENVIRONMENT_APPLY, Subject::environment(w.project, w.environment))
        .await;
    assert_eq!(worker.work_once(&token).await.expect("work"), Some(apply));
    let env = environments.get(&env_name).await.expect("environment");
    assert_eq!(env.spec.project, w.slug);
    assert_eq!(
        env.annotations().get(annotations::OPERATION),
        Some(&apply.to_string())
    );
    Api::<Project>::all(w.client.clone())
        .get(&w.slug)
        .await
        .expect("project");

    // An app is deployed, then deleted with the volume it would keep.
    let (deployed, _) = w.deploy(0).await;
    let task = {
        let worker = worker.clone();
        tokio::spawn(async move { worker.work_once(&CancellationToken::new()).await })
    };
    let apps = w.apps();
    let app = wait_for_app(&apps).await;
    report_ready(&apps, &app).await;
    assert_eq!(task.await.expect("join").expect("work"), Some(deployed));
    let claims = Api::<PersistentVolumeClaim>::namespaced(w.client.clone(), &w.namespace);
    claims
        .create(&PostParams::default(), &World::retained_volume())
        .await
        .expect("volume");
    let delete = w
        .request(
            TARGET_DELETE,
            Subject::target(w.project, w.environment, w.target, true),
        )
        .await;
    assert_eq!(worker.work_once(&token).await.expect("work"), Some(delete));
    assert!(
        apps.get_opt("web").await.expect("read").is_none(),
        "the app is gone"
    );
    assert!(
        claims
            .get_opt("web-data")
            .await
            .expect("read")
            .is_none_or(|v| v.metadata.deletion_timestamp.is_some()),
        "the volume is deleted, as asked"
    );
    let mut t = w.store.tenant(w.org).await.expect("tenant");
    assert!(t.app(w.environment, "web").await.expect("read").is_none());
    drop(t);

    // The environment: its object goes first, then its rows.
    w.request(ENVIRONMENT_DELETE, Subject::environment(w.project, w.environment))
        .await;
    for round in 0.. {
        let mut t = w.store.tenant(w.org).await.expect("tenant");
        if t.environment(w.project, "prod").await.expect("read").is_none() {
            break;
        }
        drop(t);
        assert!(round < 20, "the environment deletion never finished");
        worker.work_once(&token).await.expect("work");
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    assert!(environments.get_opt(&env_name).await.expect("read").is_none());

    // The project, now empty.
    let delete = w.request(PROJECT_DELETE, Subject::project(w.project)).await;
    assert_eq!(worker.work_once(&token).await.expect("work"), Some(delete));
    let mut t = w.store.tenant(w.org).await.expect("tenant");
    assert!(t.project(&w.slug).await.expect("read").is_none());
    drop(t);
    assert!(
        Api::<Project>::all(w.client.clone())
            .get_opt(&w.slug)
            .await
            .expect("read")
            .is_none_or(|p| p.metadata.deletion_timestamp.is_some()),
        "the project object is deleted"
    );
    w.cleanup().await;
}
