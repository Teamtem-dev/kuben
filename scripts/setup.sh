#!/usr/bin/env bash
# One-time setup: the Rust toolchain from rust-toolchain.toml, the JS
# dependencies, and cargo-nextest (the Rust test runner behind `bun run test`).
#
#   bun run setup
set -euo pipefail
cd "$(dirname "$0")/.."

need() { command -v "$1" >/dev/null || { echo "missing: $1 (install: $2)" >&2; exit 2; }; }
need rustup https://rustup.rs
need bun https://bun.com/docs/installation

rustup show active-toolchain
bun install --frozen-lockfile
if ! cargo nextest --version >/dev/null 2>&1; then
  echo "installing cargo-nextest"
  cargo install --locked cargo-nextest
fi
echo "ready: bun run dev (API on :8080, UI on :5173) · bun run ci (everything CI checks)"
