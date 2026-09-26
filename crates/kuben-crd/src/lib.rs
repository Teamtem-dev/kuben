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
    /// On a Gateway: Kuben may write its listeners. Kuben sets it on the
    /// Gateway it creates; an operator sets it to dedicate an existing one.
    pub const GATEWAY_OWNER: &str = "kuben.dev/gateway-owner";

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
        ApplicationRuntime::crd(),
        ExecutionTask::crd(),
    ]
}

/// A CRD as the chart's manifest holds it: YAML that YAML 1.1 readers
/// (kubectl, Helm) read the same way as YAML 1.2 ones.
///
/// # Panics
/// Never: a CRD always serializes.
#[must_use]
pub fn manifest_yaml(
    crd: &k8s_openapi::apiextensions_apiserver::pkg::apis::apiextensions::v1::CustomResourceDefinition,
) -> String {
    quote_yaml11_booleans(&serde_yaml_ng::to_string(crd).expect("CRD serializes to YAML"))
}

/// Words YAML 1.1 reads as booleans although YAML 1.2 (what serde_yaml_ng
/// writes) reads them as strings.
const YAML11_BOOLEANS: &[&str] = &[
    "y", "Y", "yes", "Yes", "YES", "n", "N", "no", "No", "NO", "on", "On", "ON", "off", "Off", "OFF",
];

/// Quote plain scalar values that YAML 1.1 would read as booleans.
/// kubectl and Helm parse manifests with YAML 1.1 rules, so an unquoted
/// enum value or default such as `off` would reach the API server as
/// `false`. Only whole values of `key: value` and `- value` lines change;
/// real booleans (`true`, `false`) and keys are left alone.
pub fn quote_yaml11_booleans(yaml: &str) -> String {
    let mut out = String::with_capacity(yaml.len());
    for line in yaml.split_inclusive('\n') {
        let body = line.trim_end_matches('\n');
        let split = body.rfind(": ").map(|i| i + 2).or_else(|| {
            body.trim_start()
                .starts_with("- ")
                .then(|| body.find("- ").map_or(0, |i| i + 2))
        });
        match split {
            Some(at) if YAML11_BOOLEANS.contains(&&body[at..]) => {
                out.push_str(&body[..at]);
                out.push('\'');
                out.push_str(&body[at..]);
                out.push('\'');
                out.push_str(&line[body.len()..]);
            }
            _ => out.push_str(line),
        }
    }
    out
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
        assert_eq!(KubenConfig::crd_name(), "kubenconfigs.kuben.dev");
        assert_eq!(ApplicationRuntime::crd_name(), "applicationruntimes.kuben.dev");
        assert_eq!(ExecutionTask::crd_name(), "executiontasks.kuben.dev");
    }

    #[test]
    fn all_crds_have_structural_schema() {
        for crd in all_crds() {
            let version = &crd.spec.versions[0];
            assert!(
                version.schema.is_some(),
                "{} must have a schema",
                crd.metadata.name.as_deref().unwrap_or("?")
            );
            assert!(
                version.subresources.as_ref().is_some_and(|s| s.status.is_some()),
                "status subresource"
            );
        }
    }

    #[test]
    fn app_crd_snapshot() {
        let crd = App::crd();
        insta::assert_snapshot!(manifest_yaml(&crd));
    }

    #[test]
    fn yaml11_booleans_are_quoted() {
        let yaml = "default: off\nenum:\n- off\n- zero\nnullable: true\noff: 1\nname: offline\n  - 'on'\n";
        assert_eq!(
            quote_yaml11_booleans(yaml),
            "default: 'off'\nenum:\n- 'off'\n- zero\nnullable: true\noff: 1\nname: offline\n  - 'on'\n"
        );
    }
}
