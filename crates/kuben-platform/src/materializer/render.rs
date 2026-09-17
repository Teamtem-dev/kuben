//! Rendering (ADR-032): the `Project`, `Environment` and `App` objects of
//! SQL rows alone.
//!
//! The objects keep the names, labels and shape the API has always given
//! them, so the existing controllers reconcile them unchanged. Projects and
//! environments are rendered the same way for a deployment run and for the
//! lifecycle operations that write them as soon as they exist. The release
//! decides an App's image: `spec.source` becomes
//! `<image repository>@<digest>`, whatever the configuration revision says
//! about the source.

use std::collections::BTreeMap;

use k8s_openapi::apimachinery::pkg::apis::meta::v1::OwnerReference;
use kube::api::ObjectMeta;
use kuben_core::ids::{EnvironmentId, OperationId, OrgId, ProjectId};
use kuben_crd::{
    App, AppSpec, DeletionPolicy, Environment, EnvironmentSpec, EnvironmentType, PreviewPolicy, Project,
    ProjectSpec, Protection, Quota, labels,
};
use kuben_store::repo::Materialization;
use serde_json::{Value, json};

use crate::controller::resources::{self, BuildError, RESTARTED_AT};

/// Field manager of every write the materializer makes.
pub const FIELD_MANAGER: &str = "kuben-materializer";

/// Annotations on materialized objects.
pub mod annotations {
    /// The target generation an App object was written for: the fence.
    pub const GENERATION: &str = "kuben.dev/generation";
    /// The operation that wrote the object last.
    pub const OPERATION: &str = "kuben.dev/operation";
    /// The target's lifecycle UID: a recreated target is another App.
    pub const LIFECYCLE_UID: &str = "kuben.dev/lifecycle-uid";
    /// The SQL id the object was rendered from: project, environment or target.
    pub const ID: &str = "kuben.dev/id";
    /// On an App handed over to the cluster's agent (M1.9): the target id.
    /// The App controller leaves such an App alone, so it never writes its
    /// workloads again while the App goes.
    pub const HANDOVER: &str = "kuben.dev/handover";
}

/// Grace period before a deleted production environment is purged, as the
/// API sets it.
const PRODUCTION_DELETION_GRACE: &str = "168h";
/// Process whose digest the App runs (the config revision contract).
const WEB: &str = "web";
/// Longest object name that is also a DNS label.
const MAX_NAME: usize = 63;

/// The objects of one run, in the order they are applied.
#[derive(Clone, Debug)]
pub struct Rendered {
    pub project: Project,
    pub environment: Environment,
    pub app: App,
}

/// What a `Project` object is rendered from.
#[derive(Clone, Copy, Debug)]
pub struct ProjectMaterial<'a> {
    pub org: OrgId,
    pub id: ProjectId,
    pub slug: &'a str,
    pub name: &'a str,
    pub description: Option<&'a str>,
}

/// What an `Environment` object is rendered from.
#[derive(Clone, Copy, Debug)]
pub struct EnvironmentMaterial<'a> {
    pub id: EnvironmentId,
    pub slug: &'a str,
    pub name: &'a str,
    /// `standard`, `production` or `preview`.
    pub env_type: &'a str,
    pub quota: Option<&'a Value>,
    /// The namespace of its placement, when it has one.
    pub namespace: Option<&'a str>,
}

impl<'a> ProjectMaterial<'a> {
    #[must_use]
    pub fn of(m: &'a Materialization) -> Self {
        Self {
            org: m.org,
            id: m.project,
            slug: &m.project_slug,
            name: &m.project_name,
            description: m.project_description.as_deref(),
        }
    }
}

impl<'a> EnvironmentMaterial<'a> {
    #[must_use]
    pub fn of(m: &'a Materialization) -> Self {
        Self {
            id: m.environment,
            slug: &m.environment_slug,
            name: &m.environment_name,
            env_type: &m.env_type,
            quota: m.quota.as_ref(),
            namespace: Some(&m.namespace),
        }
    }
}

/// Why an object cannot be rendered. The operation fails with
/// [`RenderError::code`].
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum RenderError {
    #[error("release {0} has no image repository to pull its digest from")]
    NoImageRepository(String),
    #[error("release {0} has no `web` artifact")]
    NoArtifact(String),
    #[error("configuration revision {revision} is not an app spec: {reason}")]
    InvalidConfig { revision: u64, reason: String },
    #[error(transparent)]
    Invalid(#[from] BuildError),
    #[error("the placement namespace `{placement}` is not the environment's namespace `{expected}`")]
    Namespace { placement: String, expected: String },
    #[error("`{0}` is too long for a Kubernetes name")]
    NameTooLong(String),
    #[error("the environment quota is not valid: {0}")]
    InvalidQuota(String),
}

impl RenderError {
    /// Error code of the failed operation.
    #[must_use]
    pub fn code(&self) -> &'static str {
        match self {
            Self::NoImageRepository(_) => "NoImageRepository",
            Self::NoArtifact(_) => "NoArtifact",
            Self::InvalidConfig { .. } => "InvalidConfig",
            Self::Invalid(e) => e.reason(),
            Self::Namespace { .. } => "PlacementNamespace",
            Self::NameTooLong(_) => "NameTooLong",
            Self::InvalidQuota(_) => "InvalidQuota",
        }
    }
}

/// Kubernetes name of an environment: `<project>-<environment>`, as the API
/// names it.
#[must_use]
pub fn environment_name(project_slug: &str, environment_slug: &str) -> String {
    format!("{project_slug}-{environment_slug}")
}

/// Render the objects of the run `m`. Only one placement per environment
/// exists until M1.9, so the placement's namespace must be the one the
/// environment controller creates.
pub fn render(m: &Materialization) -> Result<Rendered, RenderError> {
    let project = ProjectMaterial::of(m);
    let environment = environment_object(&project, &EnvironmentMaterial::of(m), m.operation)?;
    if m.application_slug.len() > MAX_NAME {
        return Err(RenderError::NameTooLong(m.application_slug.clone()));
    }
    let environment_name = environment_name(&m.project_slug, &m.environment_slug);
    let mut app_labels = scope_labels(m.org, &m.project_slug);
    app_labels.insert(labels::ENVIRONMENT.to_owned(), environment_name);
    let mut app_annotations = BTreeMap::from([
        (annotations::GENERATION.to_owned(), m.generation.0.to_string()),
        (annotations::OPERATION.to_owned(), m.operation.to_string()),
        (annotations::LIFECYCLE_UID.to_owned(), m.lifecycle_uid.to_string()),
        (annotations::ID.to_owned(), m.target.to_string()),
    ]);
    if let Some(at) = m.restarted_at {
        // The builder copies it onto every pod template: a restart run
        // replaces the pods, a later run with the same stamp does not.
        app_annotations.insert(RESTARTED_AT.to_owned(), restart_stamp(at));
    }
    let app = App {
        metadata: ObjectMeta {
            name: Some(m.application_slug.clone()),
            namespace: Some(m.namespace.clone()),
            labels: Some(app_labels),
            annotations: Some(app_annotations),
            ..ObjectMeta::default()
        },
        spec: app_spec(m)?,
        status: None,
    };
    resources::validate(&app)?;
    Ok(Rendered {
        project: project_object(&project, m.operation),
        environment,
        app,
    })
}

/// A restart stamp as the API writes it on an App: RFC 3339, in seconds.
fn restart_stamp(ms: i64) -> String {
    k8s_openapi::jiff::Timestamp::from_millisecond(ms).map_or_else(
        |_| ms.to_string(),
        |t| t.strftime("%Y-%m-%dT%H:%M:%SZ").to_string(),
    )
}

/// The `Project` object of `p`, written by `operation`.
#[must_use]
pub fn project_object(p: &ProjectMaterial<'_>, operation: OperationId) -> Project {
    Project {
        metadata: ObjectMeta {
            name: Some(p.slug.to_owned()),
            labels: Some(managed_labels(p.org)),
            annotations: Some(written_by(operation, p.id.to_string())),
            ..ObjectMeta::default()
        },
        spec: ProjectSpec {
            display_name: p.name.to_owned(),
            description: p.description.map(str::to_owned),
            previews: PreviewPolicy::default(),
        },
        status: None,
    }
}

/// The `Environment` object of `e` in project `p`, written by `operation`.
pub fn environment_object(
    p: &ProjectMaterial<'_>,
    e: &EnvironmentMaterial<'_>,
    operation: OperationId,
) -> Result<Environment, RenderError> {
    let name = environment_name(p.slug, e.slug);
    let namespace = resources::namespace_name(&name);
    if namespace.len() > MAX_NAME {
        return Err(RenderError::NameTooLong(namespace));
    }
    if let Some(placement) = e.namespace
        && placement != namespace
    {
        return Err(RenderError::Namespace {
            placement: placement.to_owned(),
            expected: namespace,
        });
    }
    let (type_, protection) = match e.env_type {
        "production" => (
            EnvironmentType::Production,
            Some(Protection {
                require_approvals: 0,
                deletion_grace: PRODUCTION_DELETION_GRACE.into(),
            }),
        ),
        "preview" => (EnvironmentType::Preview, None),
        _ => (EnvironmentType::Standard, None),
    };
    let quota = e
        .quota
        .map(|q| serde_json::from_value::<Quota>(q.clone()))
        .transpose()
        .map_err(|err| RenderError::InvalidQuota(err.to_string()))?;
    Ok(Environment {
        metadata: ObjectMeta {
            name: Some(name),
            labels: Some(scope_labels(p.org, p.slug)),
            annotations: Some(written_by(operation, e.id.to_string())),
            ..ObjectMeta::default()
        },
        spec: EnvironmentSpec {
            project: p.slug.to_owned(),
            type_,
            deletion_policy: DeletionPolicy::Delete,
            protection,
            quota,
            ttl: None,
        },
        status: None,
    })
}

/// Make `environment` owned by `project`, as the API creates environments:
/// deleting the project deletes them. False while the project has no UID.
#[must_use]
pub fn set_owner(environment: &mut Environment, project: &Project) -> bool {
    let (Some(name), Some(uid)) = (&project.metadata.name, &project.metadata.uid) else {
        return false;
    };
    environment.metadata.owner_references = Some(vec![OwnerReference {
        api_version: "kuben.dev/v1alpha1".into(),
        kind: "Project".into(),
        name: name.clone(),
        uid: uid.clone(),
        ..OwnerReference::default()
    }]);
    true
}

fn managed_labels(org: OrgId) -> BTreeMap<String, String> {
    BTreeMap::from([
        (labels::MANAGED_BY.to_owned(), labels::MANAGER.to_owned()),
        (labels::ORG.to_owned(), org.to_string()),
    ])
}

fn scope_labels(org: OrgId, project_slug: &str) -> BTreeMap<String, String> {
    let mut l = managed_labels(org);
    l.insert(labels::PROJECT.to_owned(), project_slug.to_owned());
    l
}

fn written_by(operation: OperationId, id: String) -> BTreeMap<String, String> {
    BTreeMap::from([
        (annotations::OPERATION.to_owned(), operation.to_string()),
        (annotations::ID.to_owned(), id),
    ])
}

/// The configuration revision with the release's image as its source.
fn app_spec(m: &Materialization) -> Result<AppSpec, RenderError> {
    let digest = m
        .artifacts
        .get(WEB)
        .or_else(|| {
            let mut all = m.artifacts.values();
            match (all.next(), all.next()) {
                (Some(only), None) => Some(only),
                _ => None,
            }
        })
        .ok_or_else(|| RenderError::NoArtifact(m.release.to_string()))?;
    let repository = m
        .image_repository
        .as_deref()
        .filter(|r| !r.is_empty())
        .ok_or_else(|| RenderError::NoImageRepository(m.release.to_string()))?;
    let invalid = |reason: String| RenderError::InvalidConfig {
        revision: m.config_revision_number,
        reason,
    };
    let mut config = m.config.clone();
    let object = config
        .as_object_mut()
        .ok_or_else(|| invalid("not a JSON object".into()))?;
    object.insert(
        "source".into(),
        json!({ "image": format!("{repository}@{}", digest.as_str()) }),
    );
    let mut spec: AppSpec = serde_json::from_value(config).map_err(|e| invalid(e.to_string()))?;
    // A managed secret is read from the immutable object of the revision the
    // run is bound to; other names stay the cluster's own Secrets.
    for reference in spec.env.iter_mut().filter_map(|e| e.from_secret.as_mut()) {
        if let Some(bound) = m.secrets.iter().find(|b| b.name == reference.name) {
            reference.name = crate::secrets::object_name(&bound.name, bound.revision);
        }
    }
    Ok(spec)
}

#[cfg(test)]
mod tests {
    use kuben_core::{
        ids::{ApplicationId, ConfigRevisionId, DeploymentRunId, ReleaseId, TargetId},
        ops::{Generation, RunPhase},
    };
    use kuben_crd::Source;

    use super::*;

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    fn sample() -> Materialization {
        Materialization {
            render_plan: None,
            restarted_at: None,
            approval_expires_at: None,
            secrets: Vec::new(),
            cluster: kuben_core::ids::ClusterId::new(),
            delivery: kuben_store::repo::Delivery::Controller,
            org: OrgId::new(),
            run: DeploymentRunId::new(),
            operation: OperationId::new(),
            phase: RunPhase::Planned,
            generation: Generation(3),
            lifecycle_uid: uuid::Uuid::now_v7(),
            project: ProjectId::new(),
            project_slug: "shop".into(),
            project_name: "Shop".into(),
            project_description: Some("The online shop".into()),
            environment: EnvironmentId::new(),
            environment_slug: "production".into(),
            environment_name: "Production".into(),
            protected: true,
            env_type: "production".into(),
            quota: None,
            namespace: "kb-shop-production".into(),
            application: ApplicationId::new(),
            application_slug: "web".into(),
            application_name: "Web".into(),
            target: TargetId::new(),
            desired_generation: Generation(3),
            deleting: false,
            release: ReleaseId::new(),
            artifacts: BTreeMap::from([("web".to_owned(), DIGEST.parse().expect("digest"))]),
            image_repository: Some("ghcr.io/acme/web".into()),
            config_revision: ConfigRevisionId::new(),
            config_revision_number: 2,
            config: json!({
                "runtime": { "processes": { "web": { "port": 8080 } } },
                "env": [{ "name": "LOG_LEVEL", "value": "info" }],
            }),
        }
    }

    fn with_config(config: Value) -> Materialization {
        Materialization { config, ..sample() }
    }

    #[test]
    fn bound_secrets_are_read_from_their_revision_objects() {
        let m = Materialization {
            secrets: vec![kuben_store::repo::SecretBinding {
                name: "db".into(),
                secret: uuid::Uuid::now_v7(),
                revision: 4,
            }],
            ..with_config(json!({
                "runtime": { "processes": { "web": { "port": 8080 } } },
                "env": [
                    { "name": "DATABASE_URL", "fromSecret": { "name": "db", "key": "url" } },
                    { "name": "TOKEN", "fromSecret": { "name": "legacy", "key": "token" } },
                ],
            }))
        };
        let app = render(&m).expect("render").app;
        let names: Vec<_> = app
            .spec
            .env
            .iter()
            .filter_map(|e| e.from_secret.as_ref().map(|r| (r.name.as_str(), r.key.as_str())))
            .collect();
        assert_eq!(names, [("db.r4", "url"), ("legacy", "token")]);
    }

    #[test]
    fn a_restart_run_stamps_the_app() {
        let stamp = |m: &Materialization| {
            render(m)
                .expect("render")
                .app
                .metadata
                .annotations
                .and_then(|a| a.get(RESTARTED_AT).cloned())
        };
        assert_eq!(stamp(&sample()), None);
        let restarted = Materialization {
            restarted_at: Some(1_757_937_600_000),
            ..sample()
        };
        assert_eq!(stamp(&restarted).as_deref(), Some("2025-09-15T12:00:00Z"));
    }

    #[test]
    fn renders_the_objects_the_controllers_expect() {
        let m = sample();
        let r = render(&m).expect("render");

        assert_eq!(r.project.metadata.name.as_deref(), Some("shop"));
        assert_eq!(r.project.spec.display_name, "Shop");
        assert_eq!(r.project.spec.description.as_deref(), Some("The online shop"));
        assert_eq!(r.environment.metadata.name.as_deref(), Some("shop-production"));
        assert_eq!(r.environment.spec.project, "shop");
        assert_eq!(r.environment.spec.type_, EnvironmentType::Production);
        assert!(r.environment.spec.protection.is_some());
        assert_eq!(r.app.metadata.name.as_deref(), Some("web"));
        assert_eq!(r.app.metadata.namespace.as_deref(), Some("kb-shop-production"));

        let app_labels = r.app.metadata.labels.as_ref().expect("labels");
        assert_eq!(app_labels[labels::MANAGED_BY], labels::MANAGER);
        assert_eq!(app_labels[labels::ORG], m.org.to_string());
        assert_eq!(app_labels[labels::PROJECT], "shop");
        assert_eq!(app_labels[labels::ENVIRONMENT], "shop-production");
        let notes = r.app.metadata.annotations.as_ref().expect("annotations");
        assert_eq!(notes[annotations::GENERATION], "3");
        assert_eq!(notes[annotations::OPERATION], m.operation.to_string());
        assert_eq!(notes[annotations::LIFECYCLE_UID], m.lifecycle_uid.to_string());
        assert_eq!(notes[annotations::ID], m.target.to_string());
        for object in [&r.project.metadata, &r.environment.metadata] {
            let notes = object.annotations.as_ref().expect("annotations");
            assert_eq!(notes[annotations::OPERATION], m.operation.to_string());
            assert!(
                !notes.contains_key(annotations::GENERATION),
                "generations belong to targets"
            );
        }

        assert_eq!(
            r.app.spec.source,
            Source::from_image(format!("ghcr.io/acme/web@{DIGEST}"))
        );
        assert_eq!(r.app.spec.runtime.processes["web"].port, Some(8080));
        assert_eq!(r.app.spec.env[0].value.as_deref(), Some("info"));
    }

    #[test]
    fn the_release_decides_the_image() {
        let m = with_config(json!({
            "source": { "git": { "repo": "https://github.com/acme/web" } },
            "runtime": { "processes": { "worker": {} } },
        }));
        let r = render(&m).expect("render");
        assert_eq!(
            r.app.spec.source,
            Source::from_image(format!("ghcr.io/acme/web@{DIGEST}"))
        );

        let only = Materialization {
            artifacts: BTreeMap::from([("api".to_owned(), DIGEST.parse().expect("digest"))]),
            ..sample()
        };
        assert!(
            render(&only).is_ok(),
            "a release with one artifact names its image"
        );
    }

    #[test]
    fn environments_carry_their_type_and_quota() {
        let m = sample();
        let project = ProjectMaterial::of(&m);
        let operation = OperationId::new();
        let preview = EnvironmentMaterial {
            env_type: "preview",
            namespace: None,
            quota: Some(&json!({ "cpu": "4", "memory": "4Gi", "pods": 30 })),
            ..EnvironmentMaterial::of(&m)
        };
        let env = environment_object(&project, &preview, operation).expect("render");
        assert_eq!(env.spec.type_, EnvironmentType::Preview);
        assert!(env.spec.protection.is_none());
        let quota = env.spec.quota.expect("quota");
        assert_eq!(
            (quota.cpu.as_deref(), quota.memory.as_deref(), quota.pods),
            (Some("4"), Some("4Gi"), Some(30))
        );
        assert_eq!(
            env.metadata.annotations.expect("annotations")[annotations::OPERATION],
            operation.to_string()
        );

        let standard = EnvironmentMaterial {
            env_type: "standard",
            ..EnvironmentMaterial::of(&m)
        };
        let env = environment_object(&project, &standard, operation).expect("render");
        assert_eq!(env.spec.type_, EnvironmentType::Standard);

        let broken = EnvironmentMaterial {
            quota: Some(&json!({ "pods": "many" })),
            ..EnvironmentMaterial::of(&m)
        };
        assert_eq!(
            environment_object(&project, &broken, operation)
                .expect_err("quota")
                .code(),
            "InvalidQuota"
        );
    }

    #[test]
    fn an_object_that_cannot_be_rendered_says_why() {
        let codes = [
            (
                Materialization {
                    image_repository: None,
                    ..sample()
                },
                "NoImageRepository",
            ),
            (
                Materialization {
                    artifacts: BTreeMap::new(),
                    ..sample()
                },
                "NoArtifact",
            ),
            (with_config(json!([])), "InvalidConfig"),
            (with_config(json!({ "env": [] })), "InvalidConfig"),
            (
                with_config(json!({ "runtime": { "processes": {} } })),
                "NoProcesses",
            ),
            (
                Materialization {
                    namespace: "elsewhere".into(),
                    ..sample()
                },
                "PlacementNamespace",
            ),
            (
                Materialization {
                    environment_slug: "x".repeat(60),
                    ..sample()
                },
                "NameTooLong",
            ),
        ];
        for (m, code) in codes {
            let err = render(&m).expect_err(code);
            assert_eq!(err.code(), code, "{err}");
        }
    }

    #[test]
    fn environments_are_owned_by_their_project() {
        let r = render(&sample()).expect("render");
        let mut environment = r.environment.clone();
        assert!(
            !set_owner(&mut environment, &r.project),
            "no UID before the project is applied"
        );
        let mut project = r.project;
        project.metadata.uid = Some("uid-1".into());
        assert!(set_owner(&mut environment, &project));
        let owners = environment.metadata.owner_references.expect("owner");
        assert_eq!(
            (owners[0].kind.as_str(), owners[0].uid.as_str()),
            ("Project", "uid-1")
        );
    }
}
