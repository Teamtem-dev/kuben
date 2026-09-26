#!/usr/bin/env bash
# Everything CI gates on, run locally. shellcheck runs when it is installed
# (CI always runs it), and so does golangci-lint (inside scripts/go-check.sh,
# the Go packages' `lint`). The kind suites and the hub's size budget stay
# separate: `bun run e2e`, `turbo run size --filter=kuben`.
# Database and envtest tests skip without KUBEN_TEST_PG_URL and
# KUBEBUILDER_ASSETS; CI runs them.
#
#   bun run ci
set -euo pipefail
cd "$(dirname "$0")/.."

# Biome; per package: the Go static checks (scripts/go-check.sh), go test
# -race and bun test. `check` is scoped to the TypeScript packages: go vet
# already runs in the Go packages' lint.
bun turbo run biome:check lint test
bun turbo run check --filter='@kuben/*'
scripts/check-drift.sh
bun turbo run size --filter=@kuben/console
# genspec, Kuben's own tool in the tools module (not a go.work member).
(cd tools && GOWORK=off go test ./genspec/)
for module in apps/kuben apps/kuben-agent packages/api; do
  (cd "$module" && ../../scripts/go-tool.sh govulncheck ./...)
done

if command -v shellcheck >/dev/null; then shellcheck install.sh scripts/*.sh; else echo "shellcheck not installed: skipped (CI runs it)"; fi
scripts/ci-changes.test.sh
