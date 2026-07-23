//! `kuben backup` / `kuben restore`: export and re-apply the Kuben custom
//! resources (Projects, Environments, Apps). Secret *values* are never
//! exported; back up the database with your storage snapshots.

use std::{collections::BTreeMap, path::Path};

use k8s_openapi::{api::core::v1::Namespace, apimachinery::pkg::apis::meta::v1::OwnerReference};
use kube::{
    Api, Resource, ResourceExt,
    api::{ListParams, ObjectMeta, Patch, PatchParams},
};
use kuben_core::config::Config;
use kuben_crd::{App, Environment, FIELD_MANAGER, Project};
use kuben_platform::{
    controller::{crd_apply, resources},
    registry::ClusterRegistry,
};
use serde::{Serialize, de::DeserializeOwned};

use crate::cli::{BackupOpts, RestoreOpts};

const PROJECTS: &str = "projects.yaml";
const ENVIRONMENTS: &str = "environments.yaml";
const APPS: &str = "apps.yaml";

async fn cluster(cfg: &Config) -> anyhow::Result<kube::Client> {
    let Some(registry) = ClusterRegistry::from_config(&cfg.kube).await? else {
        anyhow::bail!("no kubernetes cluster configured");
    };
    Ok(registry.primary())
}

pub async fn run(cfg: Config, opts: BackupOpts) -> anyhow::Result<()> {
