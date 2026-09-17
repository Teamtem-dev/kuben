//! `kuben` — the single binary: the server (`serve`, `migrate`, `doctor`,
//! `setup`, `backup`, …) and a client of one (`login`, `apps`, `deploy`,
//! `status`, `logs`, `rollback`).

#[global_allocator]
static GLOBAL: mimalloc::MiMalloc = mimalloc::MiMalloc;

mod bootstrap;
mod bundle;
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
        cli::Command::Setup(_)
            | cli::Command::CopySelf(_)
            | cli::Command::Status(_)
            | cli::Command::Login(_)
            | cli::Command::Apps(_)
            | cli::Command::Deploy(_)
            | cli::Command::Logs(_)
            | cli::Command::Rollback(_)
            | cli::Command::Uninstall(_)
            | cli::Command::SetupToken
            | cli::Command::AgentToken(_)
    );
    if !quiet {
        telemetry::init(&cfg)?;
    }
    let runtime = cfg.runtime.clone();

    match args.command {
        cli::Command::Setup(opts) => cli::setup::setup(&opts),
        cli::Command::CopySelf(opts) => cli::copy_self(&opts),
        cli::Command::Status(opts) => match opts.app.clone() {
            None => cli::setup::status(),
            Some(app) => serve::block_on(&runtime, cli::client::status(opts, &app)),
        },
        cli::Command::Login(opts) => serve::block_on(&runtime, cli::client::login(opts)),
        cli::Command::Apps(opts) => serve::block_on(&runtime, cli::client::apps(opts)),
        cli::Command::Deploy(opts) => serve::block_on(&runtime, cli::client::deploy(opts)),
        cli::Command::Logs(opts) => serve::block_on(&runtime, cli::client::logs(opts)),
        cli::Command::Rollback(opts) => serve::block_on(&runtime, cli::client::rollback(opts)),
        cli::Command::Uninstall(opts) => cli::setup::uninstall(&opts),
        cli::Command::Serve(_) => serve::run(cfg),
        cli::Command::Migrate => serve::block_on(&runtime, async move {
            let store = kuben_store::Store::connect_unmigrated(&cfg.database).await?;
            cli::upgrade::migrate(&cfg, &store).await?;
            tracing::info!(backend = store.backend(), "migrations applied");
            store.close().await?;
            Ok(())
        }),
        cli::Command::UpgradeCheck(opts) => serve::block_on(&runtime, cli::upgrade::check(cfg, opts)),
        cli::Command::Doctor(opts) => serve::block_on(&runtime, cli::doctor::run(cfg, opts)),
        cli::Command::ResetAdmin(opts) => serve::block_on(&runtime, cli::admin::reset(cfg, opts)),
        cli::Command::SetupToken => bootstrap::print_setup_token(&cfg),
        cli::Command::AgentToken(opts) => serve::block_on(&runtime, cli::agent::token(cfg, opts)),
        cli::Command::SupportBundle(opts) => {
            let path = args.config.clone();
            serve::block_on(&runtime, cli::support::run(cfg, opts, path.as_deref()))
        }
        cli::Command::Backup(opts) => serve::block_on(&runtime, cli::backup::run(cfg, opts)),
        cli::Command::Restore(opts) => serve::block_on(&runtime, cli::backup::restore(cfg, opts)),
        cli::Command::Version(opts) => {
            if opts.json {
                print!("{}", bundle::LOCK);
            } else {
                println!("{}", cli::version_string());
                if opts.bundle {
                    println!("{}", bundle::summary());
                }
            }
            Ok(())
        }
    }
}
