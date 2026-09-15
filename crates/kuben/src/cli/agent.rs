//! `kuben agent-token`: a bootstrap token for a cluster's agent (ADR-027).
//! The token is printed once; only its hash is kept.

use std::time::Duration;

use kuben_core::config::Config;
use kuben_platform::agentlink::{CA_CERTIFICATE, cluster_ca, directory, new_token, token_hash};

use crate::cli::AgentTokenOpts;

pub async fn token(cfg: Config, opts: AgentTokenOpts) -> anyhow::Result<()> {
    let store = kuben_store::Store::connect(&cfg.database).await?;
    let slug = opts.org.clone().unwrap_or_else(|| cfg.bootstrap.org_slug.clone());
    let org = store
        .find_org_by_slug(&slug)
        .await?
        .ok_or_else(|| anyhow::anyhow!("no organization `{slug}`"))?;
    let minutes = opts.ttl_minutes.max(1);
    let token = new_token();
    let mut tenant = store.tenant(org.id).await?;
    let cluster = tenant.ensure_cluster(&opts.cluster).await?;
    let created = tenant
        .create_agent_token(
            cluster,
            &token_hash(&token),
            Duration::from_mins(minutes),
            "cli:agent-token",
        )
        .await?;
    anyhow::ensure!(
        created,
        "cluster `{}` is not in organization `{slug}`",
        opts.cluster
    );
    tenant.commit().await?;
    store.close().await?;

    // The agent pins this CA; the hub issues under it (made here if the hub
    // has not started yet, from the same state directory).
    let dir = directory(&cfg.state_dir());
    cluster_ca(&dir)?;
    println!("cluster:     {} ({cluster})", opts.cluster);
    println!("token:       {token}");
    println!("valid for:   {minutes} minutes, redeemed once");
    println!("hub CA:      {}", dir.join(CA_CERTIFICATE).display());
    println!();
    println!("Give the agent the token through a file or stdin, never as an argument:");
    println!("  kuben-agent --hub <host:port> --hub-ca ca.crt --cluster {cluster} --token-file <file>");
    Ok(())
}
