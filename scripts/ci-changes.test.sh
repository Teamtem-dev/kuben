#!/usr/bin/env bash
# Table tests for scripts/ci-changes.sh (run in CI's "Shell scripts" job and
# by `bun run ci`). Each case: changed paths → expected selected groups.
set -euo pipefail

cd "$(dirname "$0")/.."
failures=0

check() { # <name> <expected "web codegen scripts go"> <paths...>
  local name=$1 want=$2
  shift 2
  local got
  got=$(printf '%s\n' "$@" | scripts/ci-changes.sh | awk -F= '$2 == "true" { printf "%s ", $1 }')
  got=${got% }
  if [[ $got == "$want" ]]; then
    echo "ok   $name"
  else
    echo "FAIL $name: got [$got], want [$want]"
    failures=$((failures + 1))
  fi
}

check "docs only" "" docs/guide.md README.md
check "web page" "web" apps/console/src/routes/app.tsx
check "api client" "web codegen" packages/api-client/src/index.ts
check "frozen spec" "web codegen go" packages/api-client/openapi.json
check "frozen crds" "codegen scripts go" charts/kuben/crds/kuben.dev_all.yaml
check "helm template" "scripts" charts/kuben/templates/rbac.yaml
check "e2e script" "scripts" scripts/e2e.sh
check "workflow" "web codegen scripts go" .github/workflows/ci.yml
check "turbo config" "web codegen scripts go" turbo.json
check "root manifest" "web codegen scripts go" package.json
check "bun lockfile" "web codegen scripts go" bun.lock
check "bunfig" "web codegen scripts go" bunfig.toml
check "package turbo config" "web" apps/console/turbo.json
check "package manifest" "web" apps/console/package.json
check "mixed" "web go" go/hub/internal/api/apps.go apps/console/src/lib/api.ts
check "new script" "scripts" scripts/check-drift.sh
check "trivy exceptions" "scripts" .trivyignore.yaml
check "dockerfile" "scripts" Dockerfile
check "go source" "go" go/hub/internal/core/perm/perm.go
check "go module" "go" go/agent/go.mod
check "go workspace" "go" go.work.sum
check "go lint config" "go" .golangci.yml
check "go check script" "scripts go" scripts/go-check.sh
check "go build script" "scripts go" scripts/go-build.sh

all=$(printf '' | scripts/ci-changes.sh | grep -c '=true')
if [[ $all == 4 ]]; then echo "ok   empty list selects everything"; else echo "FAIL empty list"; failures=$((failures + 1)); fi
all=$(scripts/ci-changes.sh --all </dev/null | grep -c '=true')
if [[ $all == 4 ]]; then echo "ok   --all"; else echo "FAIL --all"; failures=$((failures + 1)); fi

if ((failures)); then
  echo "$failures case(s) failed" >&2
  exit 1
fi
echo "all change-detection cases passed"
