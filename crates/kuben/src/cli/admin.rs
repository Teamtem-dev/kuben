//! `kuben reset-admin`.

use kuben_api::auth::password::Hasher;
use kuben_core::config::Config;

use crate::cli::ResetAdminOpts;

pub async fn reset(cfg: Config, opts: ResetAdminOpts) -> anyhow::Result<()> {
    let store = kuben_store::Store::connect(&cfg.database).await?;
