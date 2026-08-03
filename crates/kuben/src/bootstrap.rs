//! First-boot bootstrap: default org + admin user (Invariant I-2: nothing is
//! ever seeded with a fixed secret; passwords are configured or generated).

use std::collections::BTreeMap;

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

/// Deliver a generated admin password. In a pod it goes into the
/// [`INITIAL_ADMIN_SECRET`] Secret, because pod logs are shipped to log
/// stores and kept there; only a binary running outside the cluster prints
/// it, to its own terminal.
pub async fn hand_over_password(cfg: &Config, cluster: Option<&ClusterRegistry>, password: &str) {
    let email = &cfg.bootstrap.admin_email;
    let (Some(registry), Some(namespace)) = (cluster, own_namespace(cfg.kube.namespace.as_deref())) else {
        tracing::warn!(%email, %password, "generated initial admin password");
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
