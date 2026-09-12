//! `kuben doctor` — preflight checks. Each check prints OK / WARN / FAIL and
//! the command exits non-zero on any FAIL. Every error message is passed
//! through [`redact_credentials`]: URLs in errors may carry passwords.

use std::path::PathBuf;

use kuben_core::config::{Config, KubeCfg, in_cluster};
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
                format!(
                    "{} reachable, migrations applied ({})",
                    store.backend(),
                    cfg.database.url
                ),
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
        check_cluster(&mut r, &cfg).await;
    }

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
            check_api_group(
                r,
                &client,
                "gateway.networking.k8s.io",
                "gateway-api",
                "install Gateway API CRDs",
            )
            .await;
            check_api_group(
                r,
                &client,
                "cert-manager.io",
                "cert-manager",
                "install cert-manager for TLS",
            )
            .await;
            check_api_group(
                r,
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
}

async fn check_api_group(r: &mut Report, client: &kube::Client, group: &str, name: &str, hint: &str) {
    match client.list_api_groups().await {
        Ok(groups) if groups.groups.iter().any(|g| g.name == group) => r.line(Level::Ok, name, "present"),
        Ok(_) => r.line(Level::Warn, name, format!("not found — {hint}")),
        Err(e) => r.line(Level::Fail, name, e),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
