//! Lifecycle operations (ADR-032): the resources of projects and
//! environments as soon as they exist in SQL, and the removal of what is
//! being deleted.
//!
//! An environment's namespace hosts secrets before any app is deployed, so
//! `environment.apply` writes the Project and Environment objects right away;
//! deployment runs write them again, identically. A deletion removes the
//! object and marks the rows deleted once it is gone. An Environment object
//! stays until its controller has removed the namespace (after the
//! protection grace period for production), so `environment.delete` checks
//! back until then; it never gives up.

use std::fmt::Debug;

use k8s_openapi::api::core::v1::PersistentVolumeClaim;
use kube::{
    Api, Resource, ResourceExt,
    api::{DeleteParams, ListParams, Patch, PatchParams},
};
use kuben_core::{
    ids::{EnvironmentId, OperationId, OrgId, ProjectId},
    ops::RunPhase,
};
use kuben_crd::{App, DeletionPolicy, Environment, Project, labels};
use kuben_store::repo::{
    AppRecord, Claim, ENVIRONMENT_APPLY, ENVIRONMENT_DELETE, EnvironmentRecord, PROJECT_APPLY,
    PROJECT_DELETE, Project as ProjectRecord, Subject, TARGET_DELETE,
};
use serde::de::DeserializeOwned;

use super::{
    render::{self, EnvironmentMaterial, ProjectMaterial, annotations},
    worker::{Stop, Worker, refused},
    write,
};
use crate::controller::resources::RETAIN;

impl Worker {
    /// Carry out the lifecycle operation `claim` holds.
    pub(super) async fn carry_lifecycle(&self, claim: &Claim) -> Stop {
        let subject = match self.store.lifecycle_subject(claim).await {
            Ok(Some(subject)) => subject,
            Ok(None) => return Stop::Settled(RunPhase::Failed, Some("BadPayload".into())),
            Err(e) => return e.into(),
        };
        let done = match claim.kind.as_str() {
            PROJECT_APPLY => self.apply_project(claim.org, claim.id, subject).await,
            ENVIRONMENT_APPLY => self.apply_environment(claim.org, claim.id, subject).await,
            TARGET_DELETE if subject.detach => self.detach_target(claim.org, subject).await,
            TARGET_DELETE => self.delete_target(claim.org, subject).await,
            ENVIRONMENT_DELETE => self.delete_environment(claim.org, subject).await,
            PROJECT_DELETE => self.delete_project(claim.org, subject).await,
            _ => Err(refused("UnknownKind")),
        };
        match done {
            Ok(()) => Stop::Settled(RunPhase::Succeeded, None),
            Err(Stop::Refused(code)) => Stop::Settled(RunPhase::Failed, Some(code)),
            // Nothing to do any more: the subject is gone or being deleted.
            Err(Stop::Superseded) => Stop::Settled(RunPhase::Cancelled, None),
            Err(stop) => stop,
        }
    }

    async fn project_record(&self, org: OrgId, project: ProjectId) -> Result<Option<ProjectRecord>, Stop> {
        let mut tenant = self.store.tenant(org).await?;
        Ok(tenant.projects().await?.into_iter().find(|p| p.id == project))
    }

    async fn environment_record(
        &self,
        org: OrgId,
        project: ProjectId,
        environment: Option<EnvironmentId>,
    ) -> Result<Option<EnvironmentRecord>, Stop> {
        let Some(environment) = environment else {
            return Err(refused("BadPayload"));
        };
        let mut tenant = self.store.tenant(org).await?;
        Ok(tenant
            .environments(project)
            .await?
            .into_iter()
            .find(|e| e.id == environment))
    }

    async fn apply_project(&self, org: OrgId, operation: OperationId, subject: Subject) -> Result<(), Stop> {
        let project = match self.project_record(org, subject.project).await? {
            Some(p) if !p.deleting => p,
            _ => return Err(Stop::Superseded),
        };
        let desired = render::project_object(&material(org, &project), operation);
        self.ensure(Api::<Project>::all(self.client.clone()), &desired, org)
            .await
            .map(drop)
    }

    async fn apply_environment(
        &self,
        org: OrgId,
        operation: OperationId,
        subject: Subject,
    ) -> Result<(), Stop> {
        let project = match self.project_record(org, subject.project).await? {
            Some(p) if !p.deleting => p,
            _ => return Err(Stop::Superseded),
        };
        let environment = match self
            .environment_record(org, subject.project, subject.environment)
            .await?
        {
            Some(e) if !e.deleting => e,
            _ => return Err(Stop::Superseded),
        };
        let p = material(org, &project);
        let mut desired = render::environment_object(&p, &environment_material(&environment), operation)
            .map_err(|e| refused(e.code()))?;
        let project_object = self
            .ensure(
                Api::<Project>::all(self.client.clone()),
                &render::project_object(&p, operation),
                org,
            )
            .await?;
        if !render::set_owner(&mut desired, &project_object) {
            return Err(refused("IncompleteObject"));
        }
        self.ensure(Api::<Environment>::all(self.client.clone()), &desired, org)
            .await
            .map(drop)
    }

    async fn delete_target(&self, org: OrgId, subject: Subject) -> Result<(), Stop> {
        let (Some(environment), Some(target)) = (subject.environment, subject.target) else {
            return Err(refused("BadPayload"));
        };
        let mut tenant = self.store.tenant(org).await?;
        let app: Option<AppRecord> = tenant
            .apps(environment)
            .await?
            .into_iter()
            .find(|a| a.target == target);
        let delivery = tenant.target_delivery(target).await?;
        drop(tenant);
        let Some(app) = app else {
            return Ok(()); // finished before
        };
        if delivery == Some(kuben_store::repo::Delivery::Agent) {
            // The runtime owns the target's objects, volumes excepted: they
            // go with it.
            let runtimes =
                Api::<kuben_crd::ApplicationRuntime>::namespaced(self.client.clone(), &app.namespace);
            remove(&runtimes, &app.slug).await?;
        }
        // The App object; for a target handed over to its agent, one a
        // handover that never finished left behind.
        let apps = Api::<App>::namespaced(self.client.clone(), &app.namespace);
        if let Some(live) = apps.get_opt(&app.slug).await? {
            let ours = live
                .annotations()
                .get(annotations::ID)
                .is_none_or(|id| *id == target.to_string());
            if write::belongs_to(live.meta(), org) && ours {
                remove(&apps, &app.slug).await?;
            }
        }
        if subject.delete_volumes {
            let claims = Api::<PersistentVolumeClaim>::namespaced(self.client.clone(), &app.namespace);
            let selector = format!("{}={}", labels::APP, app.slug);
            for pvc in claims.list(&ListParams::default().labels(&selector)).await?.items {
                if pvc.annotations().contains_key(RETAIN) {
                    remove(&claims, &pvc.name_any()).await?;
                }
            }
        }
        let mut tenant = self.store.tenant(org).await?;
        tenant.finish_target_deletion(subject.project, target).await?;
        tenant.commit().await?;
        Ok(())
    }

    async fn delete_environment(&self, org: OrgId, subject: Subject) -> Result<(), Stop> {
        let Some(project) = self.project_record(org, subject.project).await? else {
            return Ok(());
        };
        let Some(environment) = self
            .environment_record(org, subject.project, subject.environment)
            .await?
        else {
            return Ok(()); // finished before
        };
        let name = render::environment_name(&project.slug, &environment.slug);
        let environments = Api::<Environment>::all(self.client.clone());
        if let Some(live) = environments.get_opt(&name).await? {
            if !write::belongs_to(live.meta(), org) {
                return Err(refused("NameTaken"));
            }
            if live.metadata.deletion_timestamp.is_none() {
                self.keep_detached_namespace(org, environment.id, &live).await?;
                remove(&environments, &name).await?;
            }
            // Its controller removes the namespace first, then the object.
            return Err(Stop::Wait(self.deletion_check, "WaitingForNamespace"));
        }
        let mut tenant = self.store.tenant(org).await?;
        tenant.finish_environment_deletion(environment.id).await?;
        tenant.commit().await?;
        Ok(())
    }

    /// An environment with unreleased detached apps keeps its namespace
    /// (M4.11): its object is switched to `Retain` before it is deleted.
    async fn keep_detached_namespace(
        &self,
        org: OrgId,
        environment: EnvironmentId,
        live: &Environment,
    ) -> Result<(), Stop> {
        if live.spec.deletion_policy == DeletionPolicy::Retain {
            return Ok(());
        }
        let mut tenant = self.store.tenant(org).await?;
        let held = tenant.detached_held(environment).await?;
        drop(tenant);
        if held == 0 {
            return Ok(());
        }
        let retain = serde_json::json!({ "spec": { "deletionPolicy": DeletionPolicy::Retain } });
        Api::<Environment>::all(self.client.clone())
            .patch(&live.name_any(), &PatchParams::default(), &Patch::Merge(&retain))
            .await?;
        tracing::info!(%environment, held, "the namespace stays for detached apps");
        Ok(())
    }

    async fn delete_project(&self, org: OrgId, subject: Subject) -> Result<(), Stop> {
        let Some(project) = self.project_record(org, subject.project).await? else {
            return Ok(()); // finished before
        };
        let projects = Api::<Project>::all(self.client.clone());
        if let Some(live) = projects.get_opt(&project.slug).await?
            && write::belongs_to(live.meta(), org)
        {
            remove(&projects, &project.slug).await?;
        }
        let mut tenant = self.store.tenant(org).await?;
        tenant.finish_project_deletion(project.id).await?;
        tenant.commit().await?;
        Ok(())
    }
}

fn material(org: OrgId, project: &ProjectRecord) -> ProjectMaterial<'_> {
    ProjectMaterial {
        org,
        id: project.id,
        slug: &project.slug,
        name: &project.name,
        description: project.description.as_deref(),
    }
}

fn environment_material(e: &EnvironmentRecord) -> EnvironmentMaterial<'_> {
    EnvironmentMaterial {
        id: e.id,
        slug: &e.slug,
        name: &e.name,
        env_type: &e.env_type,
        quota: e.quota.as_ref(),
        namespace: e.namespace.as_deref(),
    }
}

/// Delete an object; one that is gone already is fine.
async fn remove<K>(api: &Api<K>, name: &str) -> Result<(), Stop>
where
    K: Resource + Clone + DeserializeOwned + Debug,
{
    match api.delete(name, &DeleteParams::background()).await {
        Ok(_) => Ok(()),
        Err(e) if matches!(&e, kube::Error::Api(s) if s.code == 404) => Ok(()),
        Err(e) => Err(e.into()),
    }
}
