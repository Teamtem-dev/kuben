//! `kuben` — the single binary. Sub-commands: `serve`, `migrate`, `doctor`,
//! `reset-admin`, `backup`, `restore`, `version`.

#[global_allocator]
static GLOBAL: mimalloc::MiMalloc = mimalloc::MiMalloc;

mod bootstrap;
mod cli;
mod serve;
