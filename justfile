# Kuben task runner. `just` lists the recipes by group; CI runs the same commands.
set shell := ["bash", "-euo", "pipefail", "-c"]

export CARGO_TERM_COLOR := "always"

# List all recipes, grouped
default:
    @just --list --unsorted

# One-time setup: Rust toolchain (rust-toolchain.toml) and JS dependencies
[group('setup')]
setup:
    rustup show active-toolchain
    corepack enable
    pnpm install --frozen-lockfile
    @command -v cargo-nextest >/dev/null || echo "tip: cargo install cargo-nextest --locked (faster tests)"

# Everything CI gates on (cargo-deny / shellcheck run when installed locally)
[group('check')]
ci: lint test drift web
    if command -v cargo-deny >/dev/null; then cargo deny check; else echo "cargo-deny not installed: skipped (CI runs it)"; fi
    if command -v shellcheck >/dev/null; then shellcheck install.sh scripts/*.sh; else echo "shellcheck not installed: skipped (CI runs it)"; fi
    scripts/ci-changes.test.sh

# rustfmt, clippy (-D warnings), biome, tsc
[group('check')]
lint:
    cargo fmt --all --check
    cargo clippy --workspace --all-targets --locked -- -D warnings
    pnpm lint
    pnpm typecheck

# Rust tests (cargo-nextest when installed, plus doctests) and web tests
[group('check')]
test:
    if command -v cargo-nextest >/dev/null; then cargo nextest run --workspace --locked && cargo test --workspace --doc --locked; else cargo test --workspace --locked; fi
    pnpm test

