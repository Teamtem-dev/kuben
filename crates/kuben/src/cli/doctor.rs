//! `kuben doctor` — preflight checks. Each check prints OK / WARN / FAIL and
//! the command exits non-zero on any FAIL. Every error message is passed
//! through [`redact_credentials`]: URLs in errors may carry passwords.

use std::path::PathBuf;

use kuben_core::config::{Config, KubeCfg, in_cluster};

use crate::cli::DoctorOpts;
use kuben_platform::{
    discovery::{self, ClusterFacts, Readiness},
    registry::{ClusterRegistry, redact_credentials},
};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Level {
    Ok,
    Warn,
    Fail,
}

struct Report {
    failed: bool,
}

impl Report {
    fn line(&mut self, level: Level, name: &str, detail: impl std::fmt::Display) {
        let tag = match level {
            Level::Ok => "OK  ",
            Level::Warn => "WARN",
            Level::Fail => {
                self.failed = true;
                "FAIL"
            }
        };
        println!("[{tag}] {name}: {}", redact_credentials(&detail.to_string()));
    }
}

pub async fn run(cfg: Config, opts: DoctorOpts) -> anyhow::Result<()> {
    let mut r = Report { failed: false };
    println!("{}", crate::cli::version_string());
    if opts.cluster {
        cluster_checks(&mut r, &cfg).await;
        if r.failed {
            anyhow::bail!("doctor found failures");
        }
        return Ok(());
    }

    // Database
    match kuben_store::Store::connect(&cfg.database).await {
        Ok(store) => {
            r.line(
                Level::Ok,
                "database",
                format!(
                    "{} reachable, migrations applied ({})",
                    store.backend(),
                    cfg.database.url
                ),
            );
            match store.role_bypassing_row_security().await {
                Ok(Some(role)) => r.line(
                    Level::Warn,
                    "database role",
                    format!(
                        "`{role}` is a superuser or has BYPASSRLS: row-level security, the second wall \
                         between organizations, does not apply. Connect as an ordinary role that owns \
                         the database (the Helm chart and `kuben setup` make one)"
                    ),
                ),
                Ok(None) => r.line(Level::Ok, "database role", "row-level security applies"),
                Err(e) => r.line(Level::Warn, "database role", format!("cannot read the role: {e}")),
            }
            let _ = store.close().await;
        }
        // The URL goes through `redact_credentials` like every line.
        Err(e) => r.line(
            Level::Fail,
            "database",
            format!(
                "{e} (url: {}; set KUBEN_DATABASE__URL to change it)",
                cfg.database.url
            ),
        ),
    }

    cluster_checks(&mut r, &cfg).await;

    if let Some(warning) = cfg.insecure_cookie_warning(in_cluster()) {
        r.line(Level::Warn, "cookies", warning);
    } else if cfg.cookie_secure() {
        r.line(Level::Ok, "cookies", "Secure + HttpOnly (__Host- prefix)");
    } else if cfg.security.cookie_secure == kuben_core::config::CookieSecure::AUTO {
        r.line(
            Level::Ok,
            "cookies",
            "HttpOnly, not Secure: the console is served over plain http (auto; becomes Secure once \
             KUBEN_SERVER__PUBLIC_URL is https)",
        );
    } else {
        r.line(Level::Warn, "cookies", "Secure flag disabled — development only");
    }

    if r.failed {
        anyhow::bail!("doctor found failures");
    }
    Ok(())
}

/// The cluster: reachable, and what each feature needs of it.
async fn cluster_checks(r: &mut Report, cfg: &Config) {
    if let Some((path, e)) = unreadable_kubeconfig(&cfg.kube) {
        let path = path.display();
        r.line(
            Level::Fail,
            "kubernetes",
            format!(
                "cannot read the kubeconfig {path}: {e}. k3s writes it for root only; give this user a copy: \
                 sudo install -D -m 600 -o \"$USER\" {path} ~/.kube/config && export KUBECONFIG=~/.kube/config"
            ),
        );
    } else {
        check_cluster(r, cfg).await;
    }
}

/// A kubeconfig that is configured and exists but cannot be read, like the
/// root-only `/etc/rancher/k3s/k3s.yaml` from a non-root shell. Without this
/// check it would read as "no cluster found".
fn unreadable_kubeconfig(kube: &KubeCfg) -> Option<(PathBuf, std::io::Error)> {
    let paths: Vec<PathBuf> = match &kube.kubeconfig {
        Some(path) => vec![PathBuf::from(path)],
        None => std::env::var_os("KUBECONFIG")
            .map(|paths| std::env::split_paths(&paths).collect())
            .unwrap_or_default(),
    };
    paths
        .into_iter()
        .filter(|path| path.is_file())
        .find_map(|path| std::fs::File::open(&path).err().map(|e| (path, e)))
}

async fn check_cluster(r: &mut Report, cfg: &Config) {
    match ClusterRegistry::connect(&cfg.kube).await {
        Ok(registry) => {
            let client = registry.primary();
            match client.apiserver_version().await {
                Ok(v) => r.line(Level::Ok, "kubernetes", format!("apiserver {}", v.git_version)),
                Err(e) => r.line(Level::Fail, "kubernetes", e),
            }
            let facts = discovery::discover(&client).await;
            check_capabilities(r, &facts);
            check_features(r, &facts);
            check_permissions(r, &client).await;
        }
        Err(e) if !cfg.kube.required => {
            r.line(
                Level::Warn,
                "kubernetes",
                format!(
                    "no cluster found (setup mode). Install one (e.g. k3s) and set KUBECONFIG to its kubeconfig; details: {e}"
                ),
            );
        }
        Err(e) => r.line(Level::Fail, "kubernetes", e),
    }
}

/// What the cluster can do (ADR-031), as the controllers see it: unknown
/// facts are warnings, never OK.
fn check_capabilities(r: &mut Report, facts: &ClusterFacts) {
    if !facts.unknown.is_empty() {
        r.line(
            Level::Warn,
            "capabilities",
            format!("unknown (probes failed: {})", facts.unknown.join(", ")),
        );
    }
    match &facts.gateway_api {
        Some(api) => r.line(
            Level::Ok,
            "gateway-api",
            format!(
                "{} ({} channel)",
                api.bundle_version.as_deref().unwrap_or("unknown version"),
                api.channel.as_deref().unwrap_or("unknown")
            ),
        ),
        None => r.line(
            Level::Warn,
            "gateway-api",
            "not installed — install the Gateway API CRDs (standard channel) to expose apps",
        ),
    }
    if facts.gateway_api.is_some() {
        readiness_line(
            r,
            "gateway-class",
            &facts.gateway_classes,
            "no GatewayClass is Accepted",
        );
    }
    if facts.cert_manager {
        r.line(Level::Ok, "cert-manager", "present");
        readiness_line(
            r,
            "cluster-issuer",
            &facts.cluster_issuers,
            "no ClusterIssuer is Ready",
        );
    } else {
        r.line(
            Level::Warn,
            "cert-manager",
            "not found — install cert-manager for automatic HTTPS",
        );
    }
    if facts.metrics_api {
        r.line(Level::Ok, "metrics-server", "present");
    } else {
        r.line(
            Level::Warn,
            "metrics-server",
            "not found — install metrics-server for autoscaling",
        );
    }
}

/// Envoy Gateway, the documented choice when a cluster has no Gateway
/// controller (ADR-031).
const ENVOY_GATEWAY: &str = "helm install eg oci://docker.io/envoyproxy/gateway-helm -n envoy-gateway-system \
     --create-namespace, then install Kuben with --set platform.gatewayClassName=eg";

/// What each feature needs, and whether this cluster has it: a missing
/// capability blocks only its feature.
fn check_features(r: &mut Report, facts: &ClusterFacts) {
    let accepted = facts.gateway_classes.iter().any(|c| c.ready);
    match (&facts.gateway_api, accepted) {
        (Some(_), true) => r.line(Level::Ok, "feature: public routes", "ready"),
        (Some(_), false) => r.line(
            Level::Warn,
            "feature: public routes",
            format!("no GatewayClass is accepted. Install a Gateway controller: {ENVOY_GATEWAY}"),
        ),
        (None, _) => r.line(
            Level::Warn,
            "feature: public routes",
            format!(
                "needs the Gateway API CRDs (kubectl apply --server-side -f \
                 https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml) \
                 and a Gateway controller: {ENVOY_GATEWAY}"
            ),
        ),
    }
    let issuer = facts.cluster_issuers.iter().any(|i| i.ready);
    match (facts.cert_manager, issuer) {
        (true, true) => r.line(Level::Ok, "feature: HTTPS", "ready (set platform.clusterIssuer to a Ready issuer)"),
        (true, false) => r.line(
            Level::Warn,
            "feature: HTTPS",
            "cert-manager has no Ready ClusterIssuer; apps are served over plain HTTP until one is",
        ),
        (false, _) => r.line(
            Level::Warn,
            "feature: HTTPS",
            "needs cert-manager with Gateway API support (--set config.enableGatewayAPI=true) and a ClusterIssuer",
        ),
    }
    match &facts.default_storage_class {
        Some(class) => r.line(
            Level::Ok,
            "feature: volumes",
            format!("default StorageClass {class}"),
        ),
        None => r.line(
            Level::Warn,
            "feature: volumes",
            "no default StorageClass: apps with volumes and the chart's PostgreSQL stay Pending",
        ),
    }
    match &facts.network_policy {
        Some(enforcer) => r.line(Level::Ok, "feature: isolation", format!("NetworkPolicies enforced by {enforcer}")),
        None => r.line(
            Level::Warn,
            "feature: isolation",
            "no known NetworkPolicy enforcer found (unknown, not proven absent): environments are not isolated \
             from each other on the network unless the CNI enforces policies",
        ),
    }
    if !facts.metrics_api {
        r.line(
            Level::Warn,
            "feature: autoscaling",
            "needs metrics-server; apps run with fixed replicas until then",
        );
    }
}

/// Kuben's own permissions: what the chart's ClusterRole grants, checked
/// for whoever runs this.
async fn check_permissions(r: &mut Report, client: &kube::Client) {
    use k8s_openapi::api::authorization::v1::{
        ResourceAttributes, SelfSubjectAccessReview, SelfSubjectAccessReviewSpec,
    };
    const NEEDED: [(&str, &str, &str); 8] = [
        ("create", "", "namespaces"),
        ("patch", "apps", "deployments"),
        ("create", "", "secrets"),
        ("create", "gateway.networking.k8s.io", "httproutes"),
        ("create", "gateway.networking.k8s.io", "gateways"),
        ("list", "cert-manager.io", "clusterissuers"),
        ("create", "apiextensions.k8s.io", "customresourcedefinitions"),
        ("update", "coordination.k8s.io", "leases"),
    ];
    let api = kube::Api::<SelfSubjectAccessReview>::all(client.clone());
    let mut denied = Vec::new();
    for (verb, group, resource) in NEEDED {
        let review = SelfSubjectAccessReview {
            spec: SelfSubjectAccessReviewSpec {
                resource_attributes: Some(ResourceAttributes {
                    verb: Some(verb.into()),
                    group: Some(group.into()),
                    resource: Some(resource.into()),
                    ..ResourceAttributes::default()
                }),
                ..SelfSubjectAccessReviewSpec::default()
            },
            ..SelfSubjectAccessReview::default()
        };
        match api.create(&kube::api::PostParams::default(), &review).await {
            Ok(answer) if answer.status.as_ref().is_some_and(|s| s.allowed) => {}
            Ok(_) => denied.push(format!("{verb} {resource}")),
            Err(e) => {
                r.line(Level::Warn, "permissions", format!("cannot check them: {e}"));
                return;
            }
        }
    }
    if denied.is_empty() {
        r.line(Level::Ok, "permissions", "everything Kuben needs");
    } else {
        r.line(
            Level::Fail,
            "permissions",
            format!(
                "denied: {} (the Helm chart's ClusterRole grants them)",
                denied.join(", ")
            ),
        );
    }
}

fn readiness_line(r: &mut Report, name: &str, list: &[Readiness], none: &str) {
    let ready: Vec<&str> = list.iter().filter(|x| x.ready).map(|x| x.name.as_str()).collect();
    if !ready.is_empty() {
        r.line(Level::Ok, name, ready.join(", "));
    }
    for x in list.iter().filter(|x| !x.ready) {
        r.line(
            Level::Warn,
            name,
            format!(
                "{} not ready: {}",
                x.name,
                x.message.as_deref().unwrap_or("no status yet")
            ),
        );
    }
    if list.is_empty() {
        r.line(Level::Warn, name, none);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_missing_feature_warns_and_never_fails_the_check() {
        let mut r = Report { failed: false };
        check_capabilities(&mut r, &ClusterFacts::default());
        check_features(&mut r, &ClusterFacts::default());
        assert!(!r.failed, "optional capabilities only warn");
        r.line(Level::Fail, "permissions", "denied: create namespaces");
        assert!(r.failed);
    }

    #[cfg(unix)]
    #[test]
    fn unreadable_kubeconfig_is_a_failure_not_a_missing_cluster() {
        use std::os::unix::fs::PermissionsExt as _;
        let file = std::env::temp_dir().join(format!("kuben-kubeconfig-{}", std::process::id()));
        std::fs::write(&file, "apiVersion: v1\n").expect("write");
        std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o000)).expect("chmod");
        let kube = KubeCfg {
            kubeconfig: Some(file.display().to_string()),
            ..KubeCfg::default()
        };
        // root reads files regardless of their mode
        let readable_anyway = std::fs::File::open(&file).is_ok();
        assert_eq!(unreadable_kubeconfig(&kube).is_some(), !readable_anyway);
        std::fs::remove_file(&file).ok();

        let missing = KubeCfg {
            kubeconfig: Some("/nonexistent/kubeconfig".into()),
            ..KubeCfg::default()
        };
        assert!(
            unreadable_kubeconfig(&missing).is_none(),
            "a missing file is the setup-mode case"
        );
    }
}
