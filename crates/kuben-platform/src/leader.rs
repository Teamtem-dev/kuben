//! Leader election for the controller role (ADR-023). Every replica serves
//! the API and keeps its own projections, but only the holder of the
//! `coordination.k8s.io/v1` Lease runs the reconcilers.
//!
//! Every write is a compare-and-swap on the Lease's `resourceVersion`, so two
//! candidates can never win the same term. Expiry is judged by how long
//! *this* replica has seen the record unchanged, never by comparing
//! `renewTime` with the local clock, so clock skew between nodes cannot
//! produce two leaders.

use std::{
    future::Future,
    time::{Duration, Instant},
};

use k8s_openapi::{
    api::coordination::v1::{Lease, LeaseSpec},
    apimachinery::pkg::apis::meta::v1::MicroTime,
    jiff::Timestamp,
};
use kube::{
    Api, Client,
    api::{ObjectMeta, PostParams},
};
use tokio_util::sync::CancellationToken;

use crate::health::Health;

/// Name of the Lease guarding the controllers.
pub const LEASE_NAME: &str = "kuben-controller";
/// How long a holder stays leader without renewing (seconds, as stored).
