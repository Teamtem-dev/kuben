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
        out.push_str(&serde_yaml_ng::to_string(&item)?);
    }
    std::fs::write(dir.join(file), out)?;
    Ok(count)
}

fn strip_runtime_metadata(meta: &mut ObjectMeta) {
    meta.managed_fields = None;
    meta.resource_version = None;
    meta.uid = None;
    meta.creation_timestamp = None;
    meta.generation = None;
    meta.deletion_timestamp = None;
    meta.deletion_grace_period_seconds = None;
}

fn read_docs<K: DeserializeOwned>(path: &Path) -> anyhow::Result<Vec<K>> {
    if !path.exists() {
        return Ok(Vec::new());
    }
    let text = std::fs::read_to_string(path)?;
    serde_yaml_ng::Deserializer::from_str(&text)
        .map(|doc| K::deserialize(doc).map_err(|e| anyhow::anyhow!("{}: {e}", path.display())))
        .collect()
}

pub async fn restore(cfg: Config, opts: RestoreOpts) -> anyhow::Result<()> {
    let manifest = opts.from.join("manifest.json");
    anyhow::ensure!(
        manifest.exists(),
        "not a kuben backup: {} missing",
        manifest.display()
    );
    let client = cluster(&cfg).await?;
    crd_apply::ensure(client.clone()).await?;
    let pp = PatchParams::apply(FIELD_MANAGER).force();

    // Projects first; remember their *new* uids to re-link environments.
    let mut project_uids = BTreeMap::new();
    let projects = Api::<Project>::all(client.clone());
    for mut p in read_docs::<Project>(&opts.from.join(PROJECTS))? {
        strip_runtime_metadata(p.meta_mut());
        p.metadata.owner_references = None;
        p.status = None;
        let applied = projects.patch(&p.name_any(), &pp, &Patch::Apply(&p)).await?;
        if let Some(uid) = applied.uid() {
            project_uids.insert(applied.name_any(), uid);
        }
    }

    // Environments: owner references point at the restored projects (a stale
    // uid would make the garbage collector delete them), and the namespace is
    // created right away with the controller's own builder so apps can land.
    let environments = Api::<Environment>::all(client.clone());
    let namespaces = Api::<Namespace>::all(client.clone());
    let mut restored_envs = 0usize;
    for mut e in read_docs::<Environment>(&opts.from.join(ENVIRONMENTS))? {
        strip_runtime_metadata(e.meta_mut());
        e.status = None;
        e.metadata.owner_references = project_uids.get(&e.spec.project).map(|uid| {
            vec![OwnerReference {
                api_version: "kuben.dev/v1alpha1".into(),
                kind: "Project".into(),
                name: e.spec.project.clone(),
                uid: uid.clone(),
                ..OwnerReference::default()
            }]
        });
        let name = e.name_any();
        environments.patch(&name, &pp, &Patch::Apply(&e)).await?;
        namespaces
            .patch(
                &resources::namespace_name(&name),
                &pp,
                &Patch::Apply(resources::namespace(&e)),
            )
            .await?;
        restored_envs += 1;
    }

    let mut restored_apps = 0usize;
    for mut a in read_docs::<App>(&opts.from.join(APPS))? {
        strip_runtime_metadata(a.meta_mut());
        a.metadata.owner_references = None;
        a.status = None;
        let ns = a
            .namespace()
            .ok_or_else(|| anyhow::anyhow!("app `{}` has no namespace", a.name_any()))?;
        Api::<App>::namespaced(client.clone(), &ns)
            .patch(&a.name_any(), &pp, &Patch::Apply(&a))
            .await?;
        restored_apps += 1;
    }

    println!(
        "restored {} projects, {restored_envs} environments, {restored_apps} apps; secrets are not part of backups",
        project_uids.len()
    );
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn multi_document_yaml_roundtrip() {
        let dir = std::env::temp_dir().join(format!("kuben-backup-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("dir");
        let spec: kuben_crd::ProjectSpec =
            serde_json::from_value(serde_json::json!({ "displayName": "Shop" })).expect("spec");
        let mut a = Project::new("shop", spec.clone());
        a.metadata.uid = Some("old-uid".into());
        let b = Project::new("blog", spec);
        let mut out = String::new();
        for mut p in [a, b] {
            strip_runtime_metadata(p.meta_mut());
            out.push_str("---\n");
            out.push_str(&serde_yaml_ng::to_string(&p).expect("yaml"));
        }
        let path = dir.join(PROJECTS);
        std::fs::write(&path, out).expect("write");
        let docs: Vec<Project> = read_docs(&path).expect("read");
        assert_eq!(
            docs.iter().map(ResourceExt::name_any).collect::<Vec<_>>(),
            vec!["shop", "blog"]
        );
        assert!(docs[0].metadata.uid.is_none(), "runtime metadata is stripped");
        assert!(
            read_docs::<Project>(&dir.join("missing.yaml"))
                .expect("missing ok")
                .is_empty()
        );
        std::fs::remove_dir_all(&dir).ok();
    }
}
