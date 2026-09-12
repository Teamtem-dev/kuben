#!/usr/bin/env bash
# Everything CI gates on, run locally. cargo-deny and shellcheck run when they
# are installed (CI always runs them). The kind suite and the release-binary
# budget stay separate: `bun run e2e`, `turbo run kuben#size`.
#
#   bun run ci
set -euo pipefail
cd "$(dirname "$0")/.."

# clippy, Biome, nextest, doctests, bun test. `check` is scoped to the
# TypeScript packages: clippy already type-checks every crate.
bun turbo run biome:check lint lint:activator test test:doc
bun turbo run check --filter='@kuben/*'
bun turbo run format --filter=kuben-cargo -- --check
scripts/check-drift.sh
bun turbo run size --filter=@kuben/web

if command -v cargo-deny >/dev/null; then cargo deny check; else echo "cargo-deny not installed: skipped (CI runs it)"; fi
if command -v shellcheck >/dev/null; then shellcheck install.sh scripts/*.sh; else echo "shellcheck not installed: skipped (CI runs it)"; fi
scripts/ci-changes.test.sh
