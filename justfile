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

# Store tests against PostgreSQL as well (set KUBEN_TEST_PG_URL=postgres://...)
[group('check')]
test-postgres:
    if command -v cargo-nextest >/dev/null; then cargo nextest run -p kuben-store --locked --test matrix; else cargo test -p kuben-store --locked --test matrix; fi

# Fail if a generated file is stale (compares against a fresh `just gen`; no git needed)
[group('check')]
drift:
    #!/usr/bin/env bash
    set -euo pipefail
    files=(packages/api-client/openapi.json packages/api-client/src/schema.d.ts charts/kuben/crds/kuben.dev_all.yaml)
    snap=$(mktemp -d)
    trap 'rm -rf "$snap"' EXIT
    for f in "${files[@]}"; do mkdir -p "$snap/$(dirname "$f")"; cp "$f" "$snap/$f"; done
    {{ just_executable() }} gen
    stale=0
    for f in "${files[@]}"; do diff -u "$snap/$f" "$f" || stale=1; done
    if ((stale)); then echo "error: generated files were stale; run 'just gen' and commit the result" >&2; exit 1; fi
    echo "generated files are up to date"

# Supply-chain policy: licenses, advisories, bans, sources
[group('check')]
deny:
    cargo deny check

# Which CI job groups a pull request against `base` would run
[group('check')]
ci-changes base="origin/main":
    git diff --name-only {{ base }}...HEAD | scripts/ci-changes.sh

# Format Rust, TS, JSON and CSS
[group('dev')]
fmt:
    cargo fmt --all
    pnpm lint:fix

# Regenerate committed artifacts: OpenAPI spec, TS client types, CRD manifests
[group('dev')]
gen:
