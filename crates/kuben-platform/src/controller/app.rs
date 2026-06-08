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
