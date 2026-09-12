//! First-boot bootstrap: default org + admin user (Invariant I-2: nothing is
//! ever seeded with a fixed secret; passwords are configured or generated).

use std::{collections::BTreeMap, io::IsTerminal as _, path::PathBuf};

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use k8s_openapi::{ByteString, api::core::v1::Secret};
use kube::{
    Api,
    api::{ObjectMeta, Patch, PatchParams},
};
use kuben_api::{auth::password::Hasher, host::write_owner_only};
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

/// No admin yet and no password configured: the account is created from the
/// console. Print the setup link (with the token, on a public address) to
/// the terminal; without one, only say where the token is, since logs are
/// kept. `kuben setup-token` prints the link again.
pub fn announce_setup(cfg: &Config) {
    use kuben_api::setup::{current_or_new_token, setup_url, token_file, token_required};

    let token = if token_required(cfg) {
        match current_or_new_token(cfg) {
            Ok(token) => Some(token),
            Err(e) => {
                tracing::error!(error = %e, file = %token_file(cfg).display(), "cannot write the setup token");
                return;
            }
        }
    } else {
        None
    };
    if std::io::stderr().is_terminal() {
        eprintln!("{}", setup_banner(cfg, token.as_deref()));
    } else {
        tracing::warn!(
            url = %setup_url(cfg, None),
            "no admin account yet: finish the setup in the browser; `kuben setup-token` prints the link"
        );
    }
}

/// `kuben setup-token`: a fresh token and the link that carries it.
pub fn print_setup_token(cfg: &Config) -> anyhow::Result<()> {
    use kuben_api::setup::{issue_token, token_required};

    let token = if token_required(cfg) {
        Some(issue_token(cfg)?)
    } else {
        None
    };
    println!("{}", setup_banner(cfg, token.as_deref()));
    Ok(())
}

fn setup_banner(cfg: &Config, token: Option<&str>) -> String {
    let url = kuben_api::setup::setup_url(cfg, token);
    let validity = if token.is_some() {
        "\n\n  The link is valid for 30 minutes; print a new one with `kuben setup-token`."
    } else {
        ""
    };
    format!("\n  No admin account yet. Finish the setup in your browser:\n\n    {url}{validity}\n")
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

/// [`INITIAL_ADMIN_FILE`] in the installation's state directory.
fn password_file(cfg: &Config) -> PathBuf {
    cfg.state_dir().join(INITIAL_ADMIN_FILE)
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
    fn setup_banner_carries_the_token_only_when_there_is_one() {
        let mut cfg = Config::default();
        cfg.server.public_url = Some("http://203.0.113.7:3000".into());
        let with = setup_banner(&cfg, Some("abc"));
        assert!(with.contains("http://203.0.113.7:3000/setup?token=abc"), "{with}");
        assert!(with.contains("30 minutes"));
        let without = setup_banner(&cfg, None);
        assert!(without.contains("http://203.0.113.7:3000/setup\n"), "{without}");
        assert!(!without.contains("token"));
    }
}
