#!/usr/bin/env bash
# Everything CI gates on, run locally. shellcheck runs when it is installed
# (CI always runs it); golangci-lint runs in CI only. The kind suites and the
# hub's size budget stay separate: `bun run e2e`, `turbo run hub#size`.
# Database and envtest tests skip without KUBEN_TEST_PG_URL and
# KUBEBUILDER_ASSETS; CI runs them.
#
#   bun run ci
set -euo pipefail
cd "$(dirname "$0")/.."

# Go static checks (gofumpt, vet, exhaustive, NilAway: scripts/go-check.sh),
# Biome, go test -race, bun test. `check` is scoped to the TypeScript
# packages: go vet already type-checks every Go package.
bun turbo run biome:check lint test
bun turbo run check --filter='@kuben/*'
scripts/check-drift.sh
bun turbo run size --filter=@kuben/console
for m in go/hub go/agent go/kubenapi; do (cd "$m" && go tool govulncheck ./...); done

if command -v shellcheck >/dev/null; then shellcheck install.sh scripts/*.sh; else echo "shellcheck not installed: skipped (CI runs it)"; fi
scripts/ci-changes.test.sh
