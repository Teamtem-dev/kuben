//! Drift (ADR-032): changes someone else made to a materialized App object.
//!
//! A watch on the App objects Kuben manages compares each with what SQL
//! renders for it. An object whose spec or generation annotation differs from
//! the rendering of its last materialized run, or that was deleted, was
//! changed by someone else. The drift is recorded on the target with the
//! field managers that own fields of the object, and the object is written
//! again under the same fence as a delivery. While a newer run of the target
//! is in flight, the object is left to that run. The watch runs once per
//! installation, next to the controllers under the leader lease.

use std::collections::BTreeSet;

use futures::StreamExt;
use kube::{
    Api, ResourceExt,
    runtime::{WatchStreamExt, watcher},
};
use kuben_crd::{App, labels};
use kuben_store::repo::Materialized;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use super::{
    fence::live_generation,
    render::{FIELD_MANAGER, annotations, render},
    worker::{Error, Stop, Worker},
};

/// What a drift check found.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Finding {
    /// The object is as SQL renders it.
    Clean,
    /// Someone else changed or deleted it.
    Drifted(Drift),
}

/// A change someone else made.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Drift {
    pub deleted: bool,
    pub spec_changed: bool,
    /// The generation annotation is not the written one.
    pub annotation_changed: bool,
    /// Field managers other than the materializer that own fields of the
    /// object (not of its status).
    pub managers: Vec<String>,
}

impl Drift {
    /// The description recorded on the target.
    #[must_use]
    pub fn describe(&self) -> Value {
        json!({
            "deleted": self.deleted,
            "specChanged": self.spec_changed,
            "annotationChanged": self.annotation_changed,
            "managers": self.managers,
        })
    }
}

/// The object is exactly as the last write left it: its `metadata.generation`
/// and generation annotation are the recorded ones.
#[must_use]
pub fn untouched(live: &App, written: &Materialized) -> bool {
    live.metadata.generation == Some(written.resource_generation)
        && live_generation(&live.metadata) == Some(written.generation.0)
}

/// Compare `live` (`None`: deleted) with `desired`, the rendering of the last
/// materialized run. Specs compare after the same typed round trip, so
/// defaults the API server filled in are no difference.
#[must_use]
pub fn compare(live: Option<&App>, desired: &App) -> Finding {
    let Some(app) = live else {
        return Finding::Drifted(Drift {
            deleted: true,
            spec_changed: false,
            annotation_changed: false,
            managers: Vec::new(),
        });
    };
    let spec_changed = serde_json::to_value(&app.spec).ok() != serde_json::to_value(&desired.spec).ok();
    let annotation_changed =
        app.annotations().get(annotations::GENERATION) != desired.annotations().get(annotations::GENERATION);
    if !spec_changed && !annotation_changed {
        return Finding::Clean;
    }
    Finding::Drifted(Drift {
        deleted: false,
        spec_changed,
        annotation_changed,
        managers: managers(app),
    })
}

fn managers(app: &App) -> Vec<String> {
    app.metadata
        .managed_fields
        .iter()
        .flatten()
        .filter(|e| e.subresource.as_deref().unwrap_or_default().is_empty())
        .filter_map(|e| e.manager.clone())
        .filter(|m| m != FIELD_MANAGER)
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect()
}

/// Watch the App objects Kuben manages until `token` is cancelled. A
/// restarted watch lists every object again, so each is checked on start.
pub async fn watch(worker: Worker, token: CancellationToken) -> anyhow::Result<()> {
    let api = Api::<App>::all(worker.client.clone());
    let config = watcher::Config::default().labels(labels::MANAGED_SELECTOR);
    let mut objects = watcher(api, config).default_backoff().touched_objects().boxed();
    loop {
        let next = tokio::select! {
            () = token.cancelled() => return Ok(()),
            next = objects.next() => next,
        };
        match next {
            Some(Ok(app)) => {
                if let Err(e) = worker.check_drift(&app).await {
                    tracing::warn!(app = %app.name_any(), error = %e, "drift check failed");
                }
            }
            Some(Err(e)) => tracing::warn!(error = %e, "drift watch interrupted; resuming"),
            None => return Ok(()),
        }
    }
}

impl Worker {
    /// Check the App object `touched`, as the watch last saw it, for drift,
    /// and replace it with its rendering when it drifted.
    pub async fn check_drift(&self, touched: &App) -> Result<Finding, Error> {
        let (Some(uid), Some(namespace)) = (touched.uid(), touched.namespace()) else {
            return Ok(Finding::Clean);
        };
        // Not materialized: an object of the resource model, left alone.
        let Some(written) = self.store.materialized_resource(&uid).await? else {
            return Ok(Finding::Clean);
        };
        let api = Api::<App>::namespaced(self.client.clone(), &namespace);
        let live = api
            .get_opt(&touched.name_any())
            .await?
            .filter(|a| a.uid().as_deref() == Some(uid.as_str()));
        if live.as_ref().is_some_and(|a| untouched(a, &written)) {
            return Ok(Finding::Clean);
        }

        let mut tenant = self.store.tenant(written.org).await?;
        let current = tenant.target_state(written.target).await?;
        let m = tenant.materialization(written.operation).await?;
        drop(tenant);
        let (Some(m), Some(current)) = (m, current) else {
            return Ok(Finding::Clean);
        };
        if current.desired_generation.0 != written.generation.0 {
            // A newer run is in flight; its delivery writes the object.
            return Ok(Finding::Clean);
        }
        let desired = match render(&m) {
            Ok(rendered) => rendered.app,
            Err(e) => {
                tracing::warn!(target = %m.target, error = %e, "cannot render the written run again");
                return Ok(Finding::Clean);
            }
        };
        let finding = compare(live.as_ref(), &desired);
        let Finding::Drifted(drift) = &finding else {
            return Ok(finding);
        };

        let replaced = match self.write_app(&m, &desired).await {
            Ok(app) => app.metadata.uid.zip(app.metadata.generation),
            Err(Stop::Retry(e)) => return Err(e),
            Err(stop) => {
                tracing::info!(target = %m.target, ?stop, "drift left in place");
                None
            }
        };
        let replaced_ref = replaced.as_ref().map(|(uid, g)| (uid.as_str(), *g));
        self.store
            .record_drift(&written, replaced_ref, &drift.describe())
            .await?;
        tracing::warn!(
            target = %m.target, managers = ?drift.managers, deleted = drift.deleted,
            replaced = replaced.is_some(), "drift on a materialized App"
        );
        Ok(finding)
    }
}

#[cfg(test)]
mod tests {
    use k8s_openapi::apimachinery::pkg::apis::meta::v1::ManagedFieldsEntry;
    use kuben_core::{
        ids::{OperationId, OrgId, ProjectId, TargetId},
        ops::Generation,
    };

    use super::*;

    fn app(image: &str, generation: i64, annotation: Option<&str>) -> App {
        let mut app: App = serde_json::from_value(json!({
            "apiVersion": "kuben.dev/v1alpha1",
            "kind": "App",
            "metadata": { "name": "web", "namespace": "kb-shop-production", "generation": generation },
            "spec": {
                "source": { "image": image },
                "runtime": { "processes": { "web": { "port": 8080 } } },
            },
        }))
        .expect("app");
        if let Some(value) = annotation {
            app.annotations_mut()
                .insert(annotations::GENERATION.to_owned(), value.to_owned());
        }
        app
    }

    fn written(resource_generation: i64) -> Materialized {
        Materialized {
            org: OrgId::new(),
            project: ProjectId::new(),
            target: TargetId::new(),
            generation: Generation(3),
            operation: OperationId::new(),
            resource_uid: "uid-1".into(),
            resource_generation,
            drift_count: 0,
            drift_detected_at: None,
            drift: None,
        }
    }

    fn entry(manager: &str, subresource: Option<&str>) -> ManagedFieldsEntry {
        ManagedFieldsEntry {
            manager: Some(manager.into()),
            subresource: subresource.map(str::to_owned),
            ..ManagedFieldsEntry::default()
        }
    }

    #[test]
    fn an_object_as_written_needs_no_render() {
        assert!(untouched(&app("a@sha256:1", 4, Some("3")), &written(4)));
        assert!(
            !untouched(&app("a@sha256:1", 5, Some("3")), &written(4)),
            "spec edited"
        );
        assert!(
            !untouched(&app("a@sha256:1", 4, Some("9")), &written(4)),
            "annotation edited"
        );
        assert!(
            !untouched(&app("a@sha256:1", 4, None), &written(4)),
            "annotation removed"
        );
    }

    #[test]
    fn drift_is_what_differs_from_the_rendering() {
        let desired = app("a@sha256:1", 1, Some("3"));
        assert_eq!(
            compare(Some(&app("a@sha256:1", 7, Some("3"))), &desired),
            Finding::Clean,
            "a newer metadata.generation with the same content"
        );

        let mut edited = app("evil:latest", 8, Some("3"));
        edited.metadata.managed_fields = Some(vec![
            entry(FIELD_MANAGER, None),
            entry("kubectl-edit", None),
            entry("kuben", Some("status")),
            entry("kubectl-edit", None),
        ]);
        assert_eq!(
            compare(Some(&edited), &desired),
            Finding::Drifted(Drift {
                deleted: false,
                spec_changed: true,
                annotation_changed: false,
                managers: vec!["kubectl-edit".into()],
            })
        );

        let Finding::Drifted(relabelled) = compare(Some(&app("a@sha256:1", 1, Some("99"))), &desired) else {
            panic!("a forged annotation is drift");
        };
        assert!(relabelled.annotation_changed && !relabelled.spec_changed);

        let Finding::Drifted(deleted) = compare(None, &desired) else {
            panic!("a deleted object is drift");
        };
        assert!(deleted.deleted);
        assert_eq!(deleted.describe()["deleted"], true);
    }
}
