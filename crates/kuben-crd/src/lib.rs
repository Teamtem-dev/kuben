//! Kuben CustomResourceDefinitions (`kuben.dev/v1alpha1`).
//!
//! This crate intentionally has no async runtime dependency so the CLI, the
//! `crdgen` binary and third-party controllers can depend on it cheaply.
//! Kubernetes (etcd) is the source of truth for everything defined here
//! (ADR-001 / ADR-015).

pub mod v1alpha1;

pub use v1alpha1::*;

/// API group of all Kuben CRDs.
pub const GROUP: &str = "kuben.dev";
/// API version of the current CRDs.
pub const VERSION: &str = "v1alpha1";
/// Field manager used for server-side apply.
pub const FIELD_MANAGER: &str = "kuben";

/// Well-known labels.
pub mod labels {
    pub const MANAGED_BY: &str = "app.kubernetes.io/managed-by";
    pub const MANAGER: &str = "kuben";
    pub const ORG: &str = "kuben.dev/org";
    pub const PROJECT: &str = "kuben.dev/project";
    pub const ENVIRONMENT: &str = "kuben.dev/environment";
    pub const APP: &str = "kuben.dev/app";
    pub const PROCESS: &str = "kuben.dev/process";

    /// Label selector matching everything Kuben manages.
    pub const MANAGED_SELECTOR: &str = "app.kubernetes.io/managed-by=kuben";
}

/// All CRDs, in install order.
#[must_use]
pub fn all_crds()
-> Vec<k8s_openapi::apiextensions_apiserver::pkg::apis::apiextensions::v1::CustomResourceDefinition> {
    use kube::CustomResourceExt;
    vec![
        KubenConfig::crd(),
        Project::crd(),
        Environment::crd(),
        App::crd(),
        Release::crd(),
        BuildRun::crd(),
    ]
}

#[cfg(test)]
mod tests {
    use kube::CustomResourceExt;

    use super::*;

    #[test]
    fn crd_names_follow_group() {
        assert_eq!(App::crd_name(), "apps.kuben.dev");
        assert_eq!(Project::crd_name(), "projects.kuben.dev");
        assert_eq!(Environment::crd_name(), "environments.kuben.dev");
        assert_eq!(Release::crd_name(), "releases.kuben.dev");
        assert_eq!(BuildRun::crd_name(), "buildruns.kuben.dev");
