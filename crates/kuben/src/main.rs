//! `kuben` — the single binary. Sub-commands: `serve`, `migrate`, `doctor`,
//! `reset-admin`, `backup`, `restore`, `version`.

#[global_allocator]
static GLOBAL: mimalloc::MiMalloc = mimalloc::MiMalloc;

mod bootstrap;
mod cli;
mod serve;
mod telemetry;

use clap::Parser;

fn main() -> anyhow::Result<()> {
    let args = cli::Cli::parse();
    let cfg = args.load_config()?;
    telemetry::init(&cfg)?;
    let runtime = cfg.runtime.clone();

    match args.command {
        cli::Command::Serve(opts) => serve::run(cfg, &opts),
        cli::Command::Migrate => serve::block_on(&runtime, async move {
            let store = kuben_store::Store::connect(&cfg.database).await?;
            tracing::info!(backend = store.backend(), "migrations applied");
            store.checkpoint_and_close().await?;
            Ok(())
        }),
