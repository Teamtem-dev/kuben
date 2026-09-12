//! First-boot bootstrap: default org + admin user (Invariant I-2: nothing is
//! ever seeded with a fixed secret; passwords are configured or generated).

use std::{
    collections::BTreeMap,
    io::{IsTerminal as _, Write as _},
    path::{Path, PathBuf},
};

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use k8s_openapi::{ByteString, api::core::v1::Secret};
use kube::{
    Api,
    api::{ObjectMeta, Patch, PatchParams},
};
use kuben_api::auth::password::Hasher;
use kuben_core::{config::Config, perm::Role};
use kuben_platform::registry::{ClusterRegistry, own_namespace};
use kuben_store::Store;

/// Secret that receives a generated admin password when Kuben runs in a pod.
pub const INITIAL_ADMIN_SECRET: &str = "kuben-initial-admin";

/// File that receives a generated admin password when a binary runs without
/// a terminal (a systemd unit), next to the SQLite database.
pub const INITIAL_ADMIN_FILE: &str = "initial-admin-password";

/// Ensure the default org and an admin user exist. Returns the generated
/// password when one was created (so `serve` can hand it over exactly once).
///
/// Replicas that start together against an empty PostgreSQL race here; the
/// loser sees the unique constraint fail, finds the winner's rows and moves on.
pub async fn ensure_admin(cfg: &Config, store: &Store, hasher: &Hasher) -> anyhow::Result<Option<String>> {
    if store.count_users().await? > 0 {
        return Ok(None);
    }
    let slug = &cfg.bootstrap.org_slug;
    let org = match store.find_org_by_slug(slug).await? {
        Some(o) => o,
        None => match store.create_org(slug, &cfg.bootstrap.org_name).await {
            Ok(o) => o,
            Err(e) => store.find_org_by_slug(slug).await?.ok_or(e)?,
        },
    };
    let (password, generated) = match &cfg.bootstrap.admin_password {
        Some(p) if !p.is_empty() => (p.clone(), None),
        _ => {
            let p = random_password();
            (p.clone(), Some(p))
        }
    };
    let hash =
        tokio::task::block_in_place(|| hasher.hash(&password)).map_err(|e| anyhow::anyhow!("hash: {e}"))?;
    let user = match store
        .create_user(&cfg.bootstrap.admin_email, Some("Administrator"), Some(&hash))
        .await
    {
        Ok(user) => user,
        Err(e) => {
            if store.count_users().await? > 0 {
                tracing::info!("another replica bootstrapped the admin user");
                return Ok(None);
            }
            return Err(e.into());
        }
    };
    store.add_membership(org.id, user.id).await?;
    store.bind_org_role(org.id, user.id, Role::Owner).await?;
    tracing::info!(email = %user.email, org = %org.slug, "bootstrapped admin user");
    Ok(generated)
}

/// Deliver a generated admin password, never through the logger: logs are
/// shipped to log stores and kept there. In a pod it goes into the
/// [`INITIAL_ADMIN_SECRET`] Secret; a binary outside the cluster prints it to
/// its own terminal or, without one, writes [`INITIAL_ADMIN_FILE`].
pub async fn hand_over_password(cfg: &Config, cluster: Option<&ClusterRegistry>, password: &str) {
    let email = &cfg.bootstrap.admin_email;
    let (Some(registry), Some(namespace)) = (cluster, own_namespace(cfg.kube.namespace.as_deref())) else {
        hand_over_locally(cfg, email, password);
        return;
    };
    match store_password(registry, &namespace, email, password).await {
        Ok(()) => tracing::warn!(
            %email,
            read_with = %format!(
                "kubectl -n {namespace} get secret {INITIAL_ADMIN_SECRET} -o jsonpath='{{.data.password}}' | base64 -d"
            ),
            "generated initial admin password (stored in a Secret, not logged)"
        ),
        Err(e) => tracing::error!(
            error = %e,
            %email,
            "generated an initial admin password but could not store it in a Secret; set a new one with `kuben reset-admin`"
        ),
    }
}

fn hand_over_locally(cfg: &Config, email: &str, password: &str) {
    if std::io::stderr().is_terminal() {
        eprintln!(
            "\n  Initial admin: {email}\n  Password:      {password}\n\n  Shown only now. Sign in and change it under Account.\n"
        );
        tracing::warn!(%email, "generated initial admin password (printed to the terminal, not logged)");
        return;
    }
    let file = password_file(cfg);
    match write_owner_only(&file, &format!("{email}\n{password}\n")) {
        Ok(()) => tracing::warn!(
            %email,
            file = %file.display(),
            "generated initial admin password (written to the file, not logged); delete the file after signing in"
        ),
        Err(e) => tracing::error!(
            error = %e,
            %email,
            file = %file.display(),
            "generated an initial admin password but could not write it; set a new one with `kuben reset-admin`"
        ),
    }
}

/// [`INITIAL_ADMIN_FILE`] next to the SQLite database, else in systemd's
/// state directory, else in the working directory.
fn password_file(cfg: &Config) -> PathBuf {
    let dir = cfg
        .database
        .sqlite_file()
        .and_then(|db| db.parent().map(Path::to_path_buf))
        .filter(|dir| !dir.as_os_str().is_empty())
        .or_else(|| {
            std::env::var_os("STATE_DIRECTORY")
                .and_then(|dirs| dirs.to_string_lossy().split(':').next().map(PathBuf::from))
                .filter(|dir| !dir.as_os_str().is_empty())
        })
        .unwrap_or_else(|| PathBuf::from("."));
    dir.join(INITIAL_ADMIN_FILE)
}

fn write_owner_only(path: &Path, content: &str) -> std::io::Result<()> {
    // A leftover file would keep its old mode; always start from a new one.
    std::fs::remove_file(path).ok();
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    options.open(path)?.write_all(content.as_bytes())
}

async fn store_password(
    registry: &ClusterRegistry,
    namespace: &str,
    email: &str,
    password: &str,
) -> Result<(), kube::Error> {
    let secret = Secret {
        metadata: ObjectMeta {
            name: Some(INITIAL_ADMIN_SECRET.into()),
            namespace: Some(namespace.into()),
            labels: Some(BTreeMap::from([(
                "app.kubernetes.io/part-of".to_owned(),
                "kuben".to_owned(),
            )])),
            ..ObjectMeta::default()
        },
        type_: Some("Opaque".into()),
        data: Some(BTreeMap::from([
            ("email".to_owned(), ByteString(email.as_bytes().to_vec())),
            ("password".to_owned(), ByteString(password.as_bytes().to_vec())),
        ])),
        ..Secret::default()
    };
    Api::<Secret>::namespaced(registry.primary(), namespace)
        .patch(
            INITIAL_ADMIN_SECRET,
            &PatchParams::apply(kuben_crd::FIELD_MANAGER).force(),
            &Patch::Apply(&secret),
        )
        .await
        .map(|_| ())
}

/// 128 bits of entropy, URL-safe.
#[must_use]
pub fn random_password() -> String {
    let bytes: [u8; 16] = rand::random();
    URL_SAFE_NO_PAD.encode(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test(flavor = "multi_thread")]
    async fn bootstrap_runs_once_even_when_replicas_race() {
        let store = Store::memory().await.expect("store");
        let hasher = Hasher::insecure_for_tests();
        let cfg = Config::default();
        let (a, b) = tokio::join!(
            ensure_admin(&cfg, &store, &hasher),
            ensure_admin(&cfg, &store, &hasher)
        );
        let generated = [a.expect("first"), b.expect("second")];
        assert_eq!(
            generated.iter().filter(|p| p.is_some()).count(),
            1,
            "exactly one replica generates the password"
        );
        assert_eq!(store.count_users().await.expect("count"), 1);
        assert!(
            ensure_admin(&cfg, &store, &hasher)
                .await
                .expect("again")
                .is_none()
        );
    }

    #[test]
    fn password_file_sits_next_to_the_sqlite_database() {
        let mut cfg = Config::default();
        cfg.database.url = "sqlite:///var/lib/kuben/kuben.db".into();
        assert_eq!(
            password_file(&cfg),
            PathBuf::from("/var/lib/kuben").join(INITIAL_ADMIN_FILE)
        );
    }

    #[test]
    fn password_file_is_owner_only_and_replaced() {
        let dir = std::env::temp_dir().join(format!("kuben-password-{}", random_password()));
        std::fs::create_dir_all(&dir).expect("dir");
        let file = dir.join(INITIAL_ADMIN_FILE);
        write_owner_only(&file, "old\n").expect("first write");
        write_owner_only(&file, "admin@kuben.local\nsecret\n").expect("second write");
        assert_eq!(
            std::fs::read_to_string(&file).expect("read"),
            "admin@kuben.local\nsecret\n"
        );
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = std::fs::metadata(&file).expect("metadata").permissions().mode();
            assert_eq!(mode & 0o777, 0o600);
        }
        std::fs::remove_dir_all(&dir).ok();
    }
}
