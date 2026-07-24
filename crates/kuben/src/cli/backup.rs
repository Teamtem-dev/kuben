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
    std::fs::create_dir_all(&opts.out)?;
    let client = cluster(&cfg).await?;
    let projects = export::<Project>(&client, &opts.out, PROJECTS).await?;
    let environments = export::<Environment>(&client, &opts.out, ENVIRONMENTS).await?;
    let apps = export::<App>(&client, &opts.out, APPS).await?;
    let manifest = serde_json::json!({
        "kuben": crate::cli::VERSION,
        "created_at": kuben_core::time::now_ms(),
        "files": [PROJECTS, ENVIRONMENTS, APPS],
        "counts": { "projects": projects, "environments": environments, "apps": apps },
    });
    std::fs::write(
        opts.out.join("manifest.json"),
        serde_json::to_vec_pretty(&manifest)?,
    )?;
    println!(
        "backup written to {} ({projects} projects, {environments} environments, {apps} apps)",
        opts.out.display()
    );
    Ok(())
}

/// Write every object of kind `K` (all namespaces) as multi-document YAML.
async fn export<K>(client: &kube::Client, dir: &Path, file: &str) -> anyhow::Result<usize>
where
    K: Resource + Clone + std::fmt::Debug + Serialize + DeserializeOwned,
    K::DynamicType: Default,
{
    let list = Api::<K>::all(client.clone()).list(&ListParams::default()).await?;
    let mut out = String::new();
    let count = list.items.len();
    for mut item in list.items {
        strip_runtime_metadata(item.meta_mut());
        out.push_str("---\n");
