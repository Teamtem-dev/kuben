//! `kuben doctor` — preflight checks. Each check prints OK / WARN / FAIL and
//! the command exits non-zero on any FAIL. Every error message is passed
//! through [`redact_credentials`]: URLs in errors may carry passwords.

use kuben_core::config::Config;
use kuben_platform::registry::{ClusterRegistry, redact_credentials};

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

pub async fn run(cfg: Config) -> anyhow::Result<()> {
    let mut r = Report { failed: false };
    println!("{}", crate::cli::version_string());

    // Database
    match kuben_store::Store::connect(&cfg.database).await {
        Ok(store) => {
            r.line(
                Level::Ok,
                "database",
                format!("{} reachable, migrations applied", store.backend()),
            );
            let _ = store.checkpoint_and_close().await;
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

    // Cluster
    match ClusterRegistry::connect(&cfg.kube).await {
        Ok(registry) => {
            let client = registry.primary();
            match client.apiserver_version().await {
                Ok(v) => r.line(Level::Ok, "kubernetes", format!("apiserver {}", v.git_version)),
                Err(e) => r.line(Level::Fail, "kubernetes", e),
            }
            check_api_group(
                &mut r,
                &client,
                "gateway.networking.k8s.io",
                "gateway-api",
                "install Gateway API CRDs",
            )
            .await;
            check_api_group(
                &mut r,
                &client,
                "cert-manager.io",
                "cert-manager",
                "install cert-manager for TLS",
            )
            .await;
            check_api_group(
                &mut r,
                &client,
                "metrics.k8s.io",
                "metrics-server",
                "install metrics-server for autoscaling",
            )
            .await;
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

    if cfg.security.cookie_secure {
        r.line(Level::Ok, "cookies", "Secure + HttpOnly (__Host- prefix)");
    } else {
        r.line(Level::Warn, "cookies", "Secure flag disabled — development only");
    }

    if r.failed {
        anyhow::bail!("doctor found failures");
    }
    Ok(())
}

async fn check_api_group(r: &mut Report, client: &kube::Client, group: &str, name: &str, hint: &str) {
    match client.list_api_groups().await {
        Ok(groups) if groups.groups.iter().any(|g| g.name == group) => r.line(Level::Ok, name, "present"),
        Ok(_) => r.line(Level::Warn, name, format!("not found — {hint}")),
        Err(e) => r.line(Level::Fail, name, e),
    }
}
