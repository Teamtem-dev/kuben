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
