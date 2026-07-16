//! One-click templates (scenario 8): a small, reviewed catalogue of
//! single-process services. Generated credentials are stored in a Secret named
//! `<app>-credentials`; the App only *references* it, so no secret value ever
//! becomes part of an App spec, a release snapshot or the audit log.

use std::collections::BTreeMap;

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use k8s_openapi::{ByteString, api::core::v1::Secret};
use kube::{
    Api,
    api::{DeleteParams, ObjectMeta, PostParams},
};
use kuben_core::{Error, perm::Perm};
use kuben_crd::{
    AppSpec, EnvVar, HealthCheck, KeyRef, Process, Protocol, Replicas, Runtime, Source, Volume, labels,
};
use rand::{Rng, distr::Alphanumeric};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{apps, scope, validate};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

/// Length of generated passwords and keys (alphanumeric, ~190 bits).
const SECRET_LEN: usize = 32;

enum Val {
    Lit(&'static str),
    /// A key of the credentials secret.
    Secret(&'static str),
}

struct Template {
    id: &'static str,
    name: &'static str,
    description: &'static str,
    category: &'static str,
    image: &'static str,
    port: u16,
    protocol: Protocol,
    command: &'static [&'static str],
    env: &'static [(&'static str, Val)],
    /// Secret keys generated as random strings.
    generated: &'static [&'static str],
    /// Secret keys derived from `{name}` and generated keys (`{password}`).
    derived: &'static [(&'static str, &'static str)],
    /// `(name, mount path, size)`.
    volumes: &'static [(&'static str, &'static str, &'static str)],
    health: Option<&'static str>,
    size: &'static str,
    /// For images that run as a non-root user and must write their volume.
    fs_group: Option<i64>,
}

use Val::{Lit, Secret as Sec};

const TEMPLATES: &[Template] = &[
    Template {
        id: "postgres",
        name: "PostgreSQL 17",
        description: "Relational database. Other apps connect with the `url` key of the credentials secret.",
        category: "database",
        image: "postgres:17-alpine",
        port: 5432,
        protocol: Protocol::Tcp,
        command: &[],
        env: &[
            ("POSTGRES_USER", Lit("app")),
            ("POSTGRES_DB", Lit("app")),
            ("POSTGRES_PASSWORD", Sec("password")),
            ("PGDATA", Lit("/var/lib/postgresql/data/pgdata")),
        ],
        generated: &["password"],
        derived: &[
            ("url", "postgres://app:{password}@{name}:5432/app"),
            ("host", "{name}"),
            ("port", "5432"),
            ("username", "app"),
            ("database", "app"),
        ],
        volumes: &[("data", "/var/lib/postgresql/data", "5Gi")],
        health: None,
        size: "small",
        fs_group: None,
    },
    Template {
        id: "redis",
        name: "Redis 7",
        description: "In-memory cache and queue with append-only persistence and a password.",
        category: "database",
        image: "redis:7-alpine",
        port: 6379,
        protocol: Protocol::Tcp,
        command: &[
            "sh",
            "-c",
            "exec redis-server --appendonly yes --requirepass \"$REDIS_PASSWORD\"",
        ],
        env: &[("REDIS_PASSWORD", Sec("password"))],
        generated: &["password"],
        derived: &[
            ("url", "redis://:{password}@{name}:6379/0"),
            ("host", "{name}"),
            ("port", "6379"),
        ],
        volumes: &[("data", "/data", "1Gi")],
        health: None,
        size: "nano",
        fs_group: None,
    },
    Template {
        id: "mariadb",
        name: "MariaDB 11",
        description: "MySQL-compatible database. Connect with the `url` key of the credentials secret.",
        category: "database",
        image: "mariadb:11",
        port: 3306,
        protocol: Protocol::Tcp,
        command: &[],
        env: &[
            ("MARIADB_DATABASE", Lit("app")),
            ("MARIADB_USER", Lit("app")),
            ("MARIADB_PASSWORD", Sec("password")),
            ("MARIADB_ROOT_PASSWORD", Sec("root-password")),
        ],
        generated: &["password", "root-password"],
        derived: &[
            ("url", "mysql://app:{password}@{name}:3306/app"),
            ("host", "{name}"),
            ("port", "3306"),
            ("username", "app"),
            ("database", "app"),
        ],
        volumes: &[("data", "/var/lib/mysql", "5Gi")],
        health: None,
        size: "medium",
        fs_group: None,
    },
    Template {
        id: "n8n",
        name: "n8n",
        description: "Workflow automation. Credentials are encrypted with a generated key.",
        category: "automation",
        image: "n8nio/n8n:stable",
        port: 5678,
        protocol: Protocol::Http,
        command: &[],
        env: &[
            ("N8N_ENCRYPTION_KEY", Sec("encryption-key")),
            ("N8N_PORT", Lit("5678")),
            ("GENERIC_TIMEZONE", Lit("UTC")),
        ],
        generated: &["encryption-key"],
        derived: &[],
        volumes: &[("data", "/home/node/.n8n", "1Gi")],
        health: Some("/healthz"),
        size: "small",
        fs_group: Some(1000),
    },
    Template {
        id: "uptime-kuma",
        name: "Uptime Kuma",
        description: "Self-hosted uptime monitoring with status pages.",
        category: "monitoring",
        image: "louislam/uptime-kuma:1",
        port: 3001,
        protocol: Protocol::Http,
        command: &[],
        env: &[],
        generated: &[],
        derived: &[],
        volumes: &[("data", "/app/data", "1Gi")],
        health: None,
        size: "small",
        fs_group: None,
    },
    Template {
        id: "vaultwarden",
        name: "Vaultwarden",
        description: "Bitwarden-compatible password manager. Sign-ups are closed; invite users from /admin with the generated admin token.",
        category: "security",
        image: "vaultwarden/server:latest",
        port: 80,
        protocol: Protocol::Http,
        command: &[],
        env: &[
            ("SIGNUPS_ALLOWED", Lit("false")),
            ("ADMIN_TOKEN", Sec("admin-token")),
        ],
        generated: &["admin-token"],
        derived: &[],
        volumes: &[("data", "/data", "1Gi")],
        health: Some("/alive"),
        size: "small",
        fs_group: None,
    },
    Template {
        id: "gitea",
        name: "Gitea",
        description: "Lightweight Git hosting (rootless image). Finish the installer on first visit.",
        category: "development",
        image: "gitea/gitea:1-rootless",
        port: 3000,
        protocol: Protocol::Http,
        command: &[],
        env: &[],
        generated: &[],
        derived: &[],
        volumes: &[
            ("data", "/var/lib/gitea", "5Gi"),
            ("config", "/etc/gitea", "100Mi"),
        ],
        health: Some("/api/healthz"),
        size: "small",
        fs_group: Some(1000),
    },
    Template {
        id: "whoami",
        name: "whoami",
        description: "Tiny HTTP echo service to test domains, TLS and routing.",
        category: "sample",
        image: "traefik/whoami:v1.10",
        port: 8080,
        protocol: Protocol::Http,
        command: &["/whoami", "--port", "8080"],
        env: &[],
        generated: &[],
        derived: &[],
        volumes: &[],
        health: None,
        size: "nano",
        fs_group: None,
    },
];

/// Name of the Secret holding a template app's generated credentials.
#[must_use]
pub fn credentials_secret(app: &str) -> String {
    format!("{app}-credentials")
}

fn random_secret() -> String {
    rand::rng()
        .sample_iter(&Alphanumeric)
        .take(SECRET_LEN)
        .map(char::from)
        .collect()
}

struct Rendered {
    spec: AppSpec,
    secret: BTreeMap<String, String>,
}

fn render(t: &Template, app: &str) -> Rendered {
    let mut secret: BTreeMap<String, String> = t
        .generated
        .iter()
        .map(|k| ((*k).to_owned(), random_secret()))
        .collect();
    for (key, pattern) in t.derived {
        let mut value = pattern.replace("{name}", app);
        for (k, v) in t
            .generated
            .iter()
            .filter_map(|k| secret.get(*k).map(|v| (*k, v.clone())))
        {
            value = value.replace(&format!("{{{k}}}"), &v);
        }
        secret.insert((*key).to_owned(), value);
    }
    let secret_name = credentials_secret(app);
    let env = t
        .env
        .iter()
        .map(|(name, value)| match value {
            Lit(v) => EnvVar {
                name: (*name).to_owned(),
                value: Some((*v).to_owned()),
                from_secret: None,
                from_service: None,
            },
            Sec(key) => EnvVar {
                name: (*name).to_owned(),
                value: None,
                from_secret: Some(KeyRef {
                    name: secret_name.clone(),
                    key: (*key).to_owned(),
                }),
                from_service: None,
            },
        })
        .collect();
    let spec = AppSpec {
        source: Source::from_image(t.image),
        runtime: Runtime {
            processes: BTreeMap::from([(
                "web".to_owned(),
                Process {
                    command: t.command.iter().map(ToString::to_string).collect(),
                    port: Some(t.port),
                    size: t.size.to_owned(),
                    replicas: Replicas::default(),
                    idle: None,
                    schedule: None,
                    time_zone: None,
                    protocol: t.protocol,
                },
            )]),
            health_check: t.health.map(|path| HealthCheck {
                path: path.to_owned(),
                port: None,
            }),
            fs_group: t.fs_group,
        },
        env,
        domains: Vec::new(),
        volumes: t
            .volumes
            .iter()
            .map(|(name, mount_path, size)| Volume {
                name: (*name).to_owned(),
                mount_path: (*mount_path).to_owned(),
                size: (*size).to_owned(),
                storage_class: None,
            })
            .collect(),
    };
    Rendered { spec, secret }
}

#[derive(Debug, Serialize, ToSchema)]
pub struct TemplateDto {
    pub id: String,
    pub name: String,
    pub description: String,
    pub category: String,
    pub image: String,
    pub port: u16,
    /// `http` (public route) or `tcp` (cluster-internal).
    pub protocol: String,
    /// `mountPath (size)` of each volume.
    pub volumes: Vec<String>,
    /// Keys of the `<app>-credentials` secret, e.g. `url`.
    pub connection_keys: Vec<String>,
}

fn template_dto(t: &Template) -> TemplateDto {
    let mut keys: Vec<String> = t.generated.iter().map(|k| (*k).to_owned()).collect();
    keys.extend(t.derived.iter().map(|(k, _)| (*k).to_owned()));
    keys.sort();
    TemplateDto {
        id: t.id.into(),
        name: t.name.into(),
        description: t.description.into(),
        category: t.category.into(),
        image: t.image.into(),
        port: t.port,
        protocol: if t.protocol.is_http() { "http" } else { "tcp" }.into(),
        volumes: t.volumes.iter().map(|(_, p, s)| format!("{p} ({s})")).collect(),
        connection_keys: keys,
    }
}

/// The template catalogue.
#[utoipa::path(
    get,
    path = "/templates", operation_id = "listTemplates",
    tag = "templates",
    responses((status = 200, body = Vec<TemplateDto>))
)]
pub async fn list(_authz: Authz) -> Json<Vec<TemplateDto>> {
    Json(TEMPLATES.iter().map(template_dto).collect())
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct DeployTemplate {
    /// App name (also the in-cluster hostname of TCP services).
    #[schema(example = "db")]
    pub name: String,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct DeployedTemplate {
    pub app: apps::AppDto,
    /// Secret with the generated credentials, e.g. `db-credentials`.
    pub credentials_secret: String,
    /// Reference it from other apps as `KEY=@<secret>/<key>`.
    pub connection_keys: Vec<String>,
}

/// Deploy a template into an environment.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/templates/{template}", operation_id = "deployTemplate",
    tag = "templates",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("template" = String, Path, description = "Template id"),
    ),
    request_body = DeployTemplate,
    responses(
        (status = 201, body = DeployedTemplate),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn deploy(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, template)): Path<(String, String, String)>,
    Json(body): Json<DeployTemplate>,
) -> ApiResult<(StatusCode, Json<DeployedTemplate>)> {
    let e = scope::environment(&state, &authz, &project, &environment)?;
    let _app = authz.require(&state, Perm::AppWrite, &e.chain())?;
    let _secret = authz.require(&state, Perm::SecretWrite, &e.chain())?;
    let t = TEMPLATES
        .iter()
        .find(|t| t.id == template)
        .ok_or_else(|| Error::NotFound(format!("template `{template}`")))?;
    validate::dns_label("name", &body.name, 40)?;
    let rendered = render(t, &body.name);
    apps::validate_spec(&rendered.spec)?;

    let secret_name = credentials_secret(&body.name);
    let secrets = Api::<Secret>::namespaced(scope::cluster(&state)?, &e.view.namespace);
    if secrets
        .get_opt(&secret_name)
        .await
        .map_err(|err| scope::kube_error(err, &secret_name))?
        .is_some()
    {
        return Err(Error::Conflict(format!("secret `{secret_name}` already exists")).into());
    }
    let object = Secret {
        metadata: ObjectMeta {
            name: Some(secret_name.clone()),
            namespace: Some(e.view.namespace.clone()),
            labels: Some(BTreeMap::from([
                (labels::MANAGED_BY.to_owned(), labels::MANAGER.to_owned()),
                (labels::ENVIRONMENT.to_owned(), e.view.name.clone()),
                (labels::APP.to_owned(), body.name.clone()),
            ])),
            ..ObjectMeta::default()
        },
        type_: Some("Opaque".into()),
        data: Some(
            rendered
                .secret
                .into_iter()
                .map(|(k, v)| (k, ByteString(v.into_bytes())))
                .collect(),
        ),
        ..Secret::default()
    };
    secrets
        .create(&PostParams::default(), &object)
        .await
        .map_err(|err| scope::kube_error(err, &secret_name))?;

    match apps::create_app(
        &state,
        &authz,
        &e,
        &body.name,
        rendered.spec,
        "template",
        Some(t.id.to_owned()),
    )
    .await
    {
        Ok(app) => Ok((
            StatusCode::CREATED,
            Json(DeployedTemplate {
                app,
                credentials_secret: secret_name,
