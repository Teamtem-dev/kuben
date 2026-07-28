//! `kuben reset-admin`.

use kuben_api::auth::password::Hasher;
use kuben_core::config::Config;

use crate::cli::ResetAdminOpts;

pub async fn reset(cfg: Config, opts: ResetAdminOpts) -> anyhow::Result<()> {
    let store = kuben_store::Store::connect(&cfg.database).await?;
    let hasher = Hasher::from_config(&cfg.security);
    let password = opts
        .password
        .filter(|p| !p.is_empty())
        .unwrap_or_else(crate::bootstrap::random_password);
    let hash = hasher.hash(&password).map_err(|e| anyhow::anyhow!("hash: {e}"))?;

    let email = cfg.bootstrap.admin_email.clone();
    let user = if let Some(c) = store.find_user_by_email(&email).await? {
        store.set_password_hash(c.user.id, &hash).await?;
        c.user
    } else {
        let mut cfg = cfg.clone();
