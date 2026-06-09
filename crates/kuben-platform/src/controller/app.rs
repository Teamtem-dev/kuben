//! App → Deployments (+ HPAs), a Service and a Gateway API HTTPRoute. Every
//! child carries an owner reference (garbage-collected with the App) and is
//! pruned when it disappears from the spec.

use std::{collections::BTreeSet, fmt::Debug, sync::Arc, time::Duration};

use futures::{Stream, StreamExt};
use k8s_openapi::{
    api::{
        apps::v1::Deployment,
        autoscaling::v2::HorizontalPodAutoscaler,
        batch::v1::CronJob,
        core::v1::{PersistentVolumeClaim, Service},
    },
    apimachinery::pkg::apis::meta::v1::OwnerReference,
};
use kube::{
    Api, Client, Resource, ResourceExt,
    api::{ApiResource, DeleteParams, DynamicObject, GroupVersionKind, ListParams, Patch, PatchParams},
    runtime::{Controller, controller::Action, watcher},
};
use kuben_crd::{App, AppStatus, Condition, FIELD_MANAGER, condition::READY, labels};
use serde::{Serialize, de::DeserializeOwned};
use serde_json::json;
use tokio_util::sync::CancellationToken;

use super::{
