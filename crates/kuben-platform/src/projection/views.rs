//! View types. Small, `PartialEq` (so unchanged objects produce no delta) and
//! directly serializable for the UI. Every view carries its `org` so streams
//! can be filtered per tenant.

use k8s_openapi::api::core::v1::Pod;
use kube::ResourceExt;
use kuben_crd::{App, Condition, Environment, EnvironmentType, Project, condition::READY, labels};
use serde::Serialize;

use crate::controller::resources::namespace_name;

fn ready_condition(conditions: &[Condition]) -> Option<&Condition> {
    conditions.iter().find(|c| c.type_ == READY)
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum PodPhase {
    Pending,
    Running,
    Succeeded,
    Failed,
    Unknown,
}

impl From<Option<&str>> for PodPhase {
    fn from(s: Option<&str>) -> Self {
        match s {
            Some("Pending") => Self::Pending,
            Some("Running") => Self::Running,
            Some("Succeeded") => Self::Succeeded,
