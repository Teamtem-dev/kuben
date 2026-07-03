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
