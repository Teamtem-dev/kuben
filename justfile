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
    # Build first: a failed build must not truncate the committed files below.
    cargo build -q --locked -p kuben-api -p kuben-crd --bin openapi --bin crdgen
    cargo run -q --locked -p kuben-api --bin openapi > packages/api-client/openapi.json
    pnpm gen
    mkdir -p charts/kuben/crds
    cargo run -q --locked -p kuben-crd --bin crdgen > charts/kuben/crds/kuben.dev_all.yaml

# API on :8080 and Vite on :5173 (Vite proxies /api)
[group('dev')]
dev:
    #!/usr/bin/env bash
    set -euo pipefail
    trap 'kill 0' EXIT
    cargo run -p kuben -- serve --roles=all --dev &
    pnpm dev

# Build the SPA and enforce its size budget
[group('build')]
web:
    pnpm build
    pnpm size

# Release binary with the embedded UI
[group('build')]
build: web
    cargo build -p kuben --release --locked --features embed-ui

# Static musl binary exactly like the release (needs zig + cargo-zigbuild)
[group('build')]
build-musl target="x86_64-unknown-linux-musl": web
    cargo zigbuild -p kuben --release --locked --features embed-ui --target {{ target }}

# Binary size budget on the release build
[group('build')]
budgets: build
    scripts/check-budgets.sh binary target/release/kuben

# Multi-arch image from source
[group('build')]
image tag="dev":
    docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/teamtem-dev/kuben:{{ tag }} .

# Apply the generated CRDs to the current kube context
[group('cluster')]
crds:
    kubectl apply --server-side -f charts/kuben/crds/

# End-to-end run against the current kube context (e.g. kind)
[group('cluster')]
e2e:
    cargo build -p kuben --locked
    scripts/e2e.sh
