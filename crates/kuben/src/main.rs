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
    // The operator commands print their own lines; no logger on top.
    let quiet = matches!(
        args.command,
        cli::Command::Setup(_) | cli::Command::Status | cli::Command::Uninstall(_) | cli::Command::SetupToken
    );
    if !quiet {
        telemetry::init(&cfg)?;
    }
    let runtime = cfg.runtime.clone();

    match args.command {
        cli::Command::Setup(opts) => cli::setup::setup(&opts),
        cli::Command::Status => cli::setup::status(),
        cli::Command::Uninstall(opts) => cli::setup::uninstall(&opts),
        cli::Command::Serve(opts) => serve::run(cfg, &opts),
        cli::Command::Migrate => serve::block_on(&runtime, async move {
            let store = kuben_store::Store::connect(&cfg.database).await?;
            tracing::info!(backend = store.backend(), "migrations applied");
            store.checkpoint_and_close().await?;
            Ok(())
        }),
        cli::Command::Doctor => serve::block_on(&runtime, cli::doctor::run(cfg)),
        cli::Command::ResetAdmin(opts) => serve::block_on(&runtime, cli::admin::reset(cfg, opts)),
        cli::Command::SetupToken => bootstrap::print_setup_token(&cfg),
        cli::Command::Backup(opts) => serve::block_on(&runtime, cli::backup::run(cfg, opts)),
        cli::Command::Restore(opts) => serve::block_on(&runtime, cli::backup::restore(cfg, opts)),
        cli::Command::Version => {
            println!("{}", cli::version_string());
            Ok(())
        }
    }
}
