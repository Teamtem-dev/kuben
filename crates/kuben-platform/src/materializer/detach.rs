//! Detach (M4.11): Kuben lets go of an app and leaves it running.
//!
//! A detach is a `target.delete` that orphans instead of removing:
//! 1. The app's ApplicationRuntime (agent delivery) and App are deleted with
//!    orphan propagation. Before the App goes, it is marked, so its
//!    controller leaves it alone. The garbage collector then drops them from
//!    their workloads' owners, so nothing Kuben runs prunes those workloads
//!    again.
//! 2. The Secrets of the app's last delivered run lose Kuben's
//!    `managed-by` label, so secret garbage collection never takes them.
//! 3. The target is marked deleted, and the detach is marked complete.
//!
//! Volumes were never owned and stay. Routes keep their labels: while Kuben
//! runs, its Gateway keeps serving the detached hostnames. An unreleased
//! detached app also keeps its namespace when the environment is deleted.

use std::time::Duration;

use k8s_openapi::api::core::v1::Secret;
use kube::{
    Api, Resource, ResourceExt,
    api::{DeleteParams, Patch, PatchParams, PropagationPolicy},
};
use kuben_core::ids::{OrgId, TargetId};
use kuben_crd::{App, ApplicationRuntime, labels};
use kuben_store::repo::{Delivery, Subject};
use serde_json::json;

use super::{
    render::annotations,
    worker::{Stop, Worker, refused},
    write,
};

/// How long a detach waits for the garbage collector before looking again.
pub(super) const ORPHAN_WAIT: Duration = Duration::from_secs(2);
/// The label a detached Secret gets instead of Kuben's `managed-by`.
pub const DETACHED: &str = "kuben.dev/detached";

fn orphan() -> DeleteParams {
    DeleteParams {
        propagation_policy: Some(PropagationPolicy::Orphan),
        ..DeleteParams::default()
    }
}

impl Worker {
    /// Take `slug`'s App in `namespace` from its controller and delete it,
    /// orphaning its workloads; done once it is gone. An App that is not
    /// `target`'s (of `org`) is refused.
    pub(super) async fn orphan_app_object(
        &self,
        namespace: &str,
        slug: &str,
        org: OrgId,
        target: TargetId,
    ) -> Result<(), Stop> {
        let apps = Api::<App>::namespaced(self.client.clone(), namespace);
        let Some(live) = apps.get_opt(slug).await? else {
            return Ok(());
        };
        let ours = live
            .annotations()
            .get(annotations::ID)
            .is_none_or(|id| *id == target.to_string());
        if !write::belongs_to(live.meta(), org) || !ours {
            return Err(refused("NameTaken"));
        }
        if !live.annotations().contains_key(annotations::HANDOVER) {
            // First take the App from its controller, which leaves a marked
            // App alone; delete it only after a pause, once a write of that
            // controller already under way has landed. A write after the
            // orphaning would give the workloads back an owner that is going,
            // and the garbage collector would take them with it (CI run
            // 34985518950).
            let mark = json!({ "metadata": { "annotations": {
                annotations::HANDOVER: target.to_string(),
            } } });
            apps.patch(slug, &PatchParams::default(), &Patch::Merge(&mark))
                .await?;
            return Err(Stop::Wait(ORPHAN_WAIT, "HandingOverApp"));
        }
        if live.metadata.deletion_timestamp.is_none() {
            match apps.delete(slug, &orphan()).await {
                Ok(_) => tracing::info!(%target, "the App goes, its workloads stay"),
                Err(kube::Error::Api(s)) if s.code == 404 => return Ok(()),
                Err(e) => return Err(e.into()),
            }
        }
        // The garbage collector drops the App from its workloads' owners,
        // then removes it.
        Err(Stop::Wait(ORPHAN_WAIT, "OrphaningApp"))
    }

    /// Delete `slug`'s ApplicationRuntime, orphaning its workloads; done
    /// once it is gone.
    async fn orphan_runtime(
        &self,
        namespace: &str,
        slug: &str,
        target: TargetId,
    ) -> Result<(), Stop> {
        let runtimes = Api::<ApplicationRuntime>::namespaced(self.client.clone(), namespace);
        let Some(live) = runtimes.get_opt(slug).await? else {
            return Ok(());
        };
        let managed = live
            .labels()
            .get(labels::MANAGED_BY)
            .is_some_and(|v| v == labels::MANAGER);
        let ours = live.spec.target_id == target.to_string();
        if !managed || !ours {
            return Err(refused("NameTaken"));
        }
        if live.metadata.deletion_timestamp.is_none() {
            match runtimes.delete(slug, &orphan()).await {
                Ok(_) => tracing::info!(runtime = slug, "the runtime goes, its workloads stay"),
                Err(kube::Error::Api(s)) if s.code == 404 => return Ok(()),
                Err(e) => return Err(e.into()),
            }
        }
        Err(Stop::Wait(ORPHAN_WAIT, "OrphaningRuntime"))
    }

    /// Carry out the detach of `subject`'s target.
    pub(super) async fn detach_target(&self, org: OrgId, subject: Subject) -> Result<(), Stop> {
        let Some(target) = subject.target else {
            return Err(refused("BadPayload"));
        };
        let mut tenant = self.store.tenant(org).await?;
        let Some(record) = tenant.detached_app(target).await? else {
            return Err(refused("NotDetached"));
        };
        if record.completed_at.is_some() {
            return Ok(()); // finished before
        }
        let delivery = tenant.target_delivery(target).await?;
        let secrets = tenant
            .export_material(target)
            .await?
            .map(|m| m.secrets)
            .unwrap_or_default();
        drop(tenant);
        let (namespace, slug) = (record.namespace.as_str(), record.app.as_str());
        if delivery == Some(Delivery::Agent) {
            self.orphan_runtime(namespace, slug, target).await?;
        }
        self.orphan_app_object(namespace, slug, org, target).await?;
        let api = Api::<Secret>::namespaced(self.client.clone(), namespace);
        let release = json!({ "metadata": { "labels": {
            labels::MANAGED_BY: null,
            DETACHED: target.to_string(),
        } } });
        for secret in &secrets {
            let name = secret.object();
            match api.get_opt(&name).await? {
                Some(live) if write::belongs_to(live.meta(), org) => {
                    api.patch(&name, &PatchParams::default(), &Patch::Merge(&release))
                        .await?;
                }
                _ => {}
            }
        }
        let mut tenant = self.store.tenant(org).await?;
        tenant.finish_target_deletion(subject.project, target).await?;
        tenant.complete_detach(target).await?;
        tenant.commit().await?;
        tracing::info!(%target, app = slug, namespace, secrets = secrets.len(), "app detached");
        Ok(())
    }
}
