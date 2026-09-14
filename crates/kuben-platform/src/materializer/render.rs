//! Rendering (ADR-032): the `Project`, `Environment` and `App` objects of an
//! accepted deployment run, from its SQL rows alone.
//!
//! The objects keep the names, labels and shape the API has always given
//! them, so the existing controllers reconcile them unchanged. The release
//! decides the image: `spec.source` becomes `<image repository>@<digest>`,
//! whatever the configuration revision says about the source.

use std::collections::BTreeMap;

use k8s_openapi::apimachinery::pkg::apis::meta::v1::OwnerReference;
use kube::api::ObjectMeta;
use kuben_crd::{
    App, AppSpec, DeletionPolicy, Environment, EnvironmentSpec, EnvironmentType, PreviewPolicy, Project,
    ProjectSpec, Protection, labels,
};
use kuben_store::repo::Materialization;
use serde_json::json;

use crate::controller::resources::{self, BuildError};

/// Field manager of every write the materializer makes.
pub const FIELD_MANAGER: &str = "kuben-materializer";

/// Annotations on materialized objects.
pub mod annotations {
    /// The target generation an App object was written for: the fence.
    pub const GENERATION: &str = "kuben.dev/generation";
    /// The operation whose run wrote the object last.
    pub const OPERATION: &str = "kuben.dev/operation";
    /// The target's lifecycle UID: a recreated target is another App.
    pub const LIFECYCLE_UID: &str = "kuben.dev/lifecycle-uid";
    /// The SQL id the object was rendered from: project, environment or target.
    pub const ID: &str = "kuben.dev/id";
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

/// Why a run cannot be rendered. The run fails with [`RenderError::code`].
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
        }
    }
}

/// Kubernetes name of the run's environment: `<project>-<environment>`, as
/// the API names it.
#[must_use]
pub fn environment_name(m: &Materialization) -> String {
    format!("{}-{}", m.project_slug, m.environment_slug)
}

/// Render the objects of `m`. Only one placement per environment exists until
/// M1.9, so the placement's namespace must be the one the environment
/// controller creates.
pub fn render(m: &Materialization) -> Result<Rendered, RenderError> {
    let environment = environment_name(m);
    let namespace = resources::namespace_name(&environment);
    for name in [&namespace, &m.application_slug] {
        if name.len() > MAX_NAME {
            return Err(RenderError::NameTooLong(name.clone()));
        }
    }
    if m.namespace != namespace {
        return Err(RenderError::Namespace {
            placement: m.namespace.clone(),
            expected: namespace,
        });
    }
    let mut app_labels = scope_labels(m);
    app_labels.insert(labels::ENVIRONMENT.to_owned(), environment.clone());
    let app = App {
        metadata: ObjectMeta {
            name: Some(m.application_slug.clone()),
            namespace: Some(namespace),
            labels: Some(app_labels),
            annotations: Some(BTreeMap::from([
                (annotations::GENERATION.to_owned(), m.generation.0.to_string()),
                (annotations::OPERATION.to_owned(), m.operation.to_string()),
                (annotations::LIFECYCLE_UID.to_owned(), m.lifecycle_uid.to_string()),
                (annotations::ID.to_owned(), m.target.to_string()),
            ])),
            ..ObjectMeta::default()
        },
        spec: app_spec(m)?,
        status: None,
    };
    resources::validate(&app)?;
    Ok(Rendered {
        project: project(m),
        environment: environment_object(m, environment),
        app,
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

fn managed_labels(m: &Materialization) -> BTreeMap<String, String> {
    BTreeMap::from([
        (labels::MANAGED_BY.to_owned(), labels::MANAGER.to_owned()),
        (labels::ORG.to_owned(), m.org.to_string()),
    ])
}

fn scope_labels(m: &Materialization) -> BTreeMap<String, String> {
    let mut l = managed_labels(m);
    l.insert(labels::PROJECT.to_owned(), m.project_slug.clone());
    l
}

fn written_by(m: &Materialization, id: String) -> BTreeMap<String, String> {
    BTreeMap::from([
        (annotations::OPERATION.to_owned(), m.operation.to_string()),
        (annotations::ID.to_owned(), id),
    ])
}

fn project(m: &Materialization) -> Project {
    Project {
        metadata: ObjectMeta {
            name: Some(m.project_slug.clone()),
            labels: Some(managed_labels(m)),
            annotations: Some(written_by(m, m.project.to_string())),
            ..ObjectMeta::default()
        },
        spec: ProjectSpec {
            display_name: m.project_name.clone(),
            description: None,
            previews: PreviewPolicy::default(),
        },
        status: None,
    }
}

fn environment_object(m: &Materialization, name: String) -> Environment {
    // Protection is policy in SQL; production is the environment type that
    // carries it on the resource.
    let (type_, protection) = if m.protected {
        (
            EnvironmentType::Production,
            Some(Protection {
                require_approvals: 0,
                deletion_grace: PRODUCTION_DELETION_GRACE.into(),
            }),
        )
    } else {
        (EnvironmentType::Standard, None)
    };
    Environment {
        metadata: ObjectMeta {
            name: Some(name),
            labels: Some(scope_labels(m)),
            annotations: Some(written_by(m, m.environment.to_string())),
            ..ObjectMeta::default()
        },
        spec: EnvironmentSpec {
            project: m.project_slug.clone(),
            type_,
            deletion_policy: DeletionPolicy::Delete,
            protection,
            quota: None,
            ttl: None,
        },
        status: None,
    }
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
    serde_json::from_value(config).map_err(|e| invalid(e.to_string()))
}

#[cfg(test)]
mod tests {
    use kuben_core::{
        ids::{
            ApplicationId, ConfigRevisionId, DeploymentRunId, EnvironmentId, OperationId, OrgId, ProjectId,
            ReleaseId, TargetId,
        },
        ops::{Generation, RunPhase},
    };
    use kuben_crd::Source;
    use serde_json::Value;

    use super::*;

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    fn sample() -> Materialization {
        Materialization {
            org: OrgId::new(),
            run: DeploymentRunId::new(),
            operation: OperationId::new(),
            phase: RunPhase::Planned,
            generation: Generation(3),
            lifecycle_uid: uuid::Uuid::now_v7(),
            project: ProjectId::new(),
            project_slug: "shop".into(),
            project_name: "Shop".into(),
            environment: EnvironmentId::new(),
            environment_slug: "production".into(),
            environment_name: "Production".into(),
            protected: true,
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
    fn renders_the_objects_the_controllers_expect() {
        let m = sample();
        let r = render(&m).expect("render");

        assert_eq!(r.project.metadata.name.as_deref(), Some("shop"));
        assert_eq!(r.project.spec.display_name, "Shop");
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

        let unprotected = Materialization {
            protected: false,
            ..sample()
        };
        let r = render(&unprotected).expect("render");
        assert_eq!(r.environment.spec.type_, EnvironmentType::Standard);
        assert!(r.environment.spec.protection.is_none());
    }

    #[test]
    fn a_run_that_cannot_be_rendered_says_why() {
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
