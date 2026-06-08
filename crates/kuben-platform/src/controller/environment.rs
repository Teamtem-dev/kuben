//! Environment → Namespace (+ quota, limit defaults, isolation policy).
//!
//! Deletion is enforced by a finalizer managed here (not kube's helper, which
//! would drop the finalizer as soon as cleanup returns): `Retain` hands the
//! namespace back, `Delete` purges it once the protection grace period has
//! elapsed — a soft delete that can be cancelled by nobody but is visible
//! (`status.deletionScheduledAt`) the whole time (ADR-018).

use std::{sync::Arc, time::Duration};

use futures::StreamExt;
use k8s_openapi::{
    api::{
        core::v1::{LimitRange, Namespace, ResourceQuota},
        networking::v1::NetworkPolicy,
    },
    jiff::Timestamp,
};
use kube::{
    Api, ResourceExt,
    api::{DeleteParams, Patch, PatchParams},
    runtime::{Controller, controller::Action, reflector::ObjectRef, watcher},
};
use kuben_crd::{DeletionPolicy, Environment, EnvironmentStatus, FIELD_MANAGER, condition::READY, labels};
