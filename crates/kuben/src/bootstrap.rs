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
