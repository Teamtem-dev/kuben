#!/usr/bin/env bash
# One-time setup: checks for Go (the version in go.mod) and Bun,
# installs the JS dependencies and downloads the Go modules (the root module and tools/).
#
#   bun run setup
set -euo pipefail
cd "$(dirname "$0")/.."

need() { command -v "$1" >/dev/null || { echo "missing: $1 (install: $2)" >&2; exit 2; }; }
need go https://go.dev/doc/install
need bun https://bun.com/docs/installation

go version
bun install --frozen-lockfile
go mod download && (cd tools && go mod download)
echo "ready: bun run dev (API on :8080, UI on :5173) · bun run ci (everything CI checks)"
