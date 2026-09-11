#!/usr/bin/env bash
# Table tests for scripts/ci-changes.sh (run in CI's "Shell scripts" job and
# by `just ci`). Each case: changed paths → expected selected groups.
set -euo pipefail

cd "$(dirname "$0")/.."
failures=0

check() { # <name> <expected "rust deps web codegen scripts"> <paths...>
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
check "rust source" "rust codegen" crates/kuben-platform/src/controller/app.rs
check "crate manifest" "rust deps codegen" crates/kuben-api/Cargo.toml
check "lockfile" "rust deps codegen" Cargo.lock
check "web page" "web" apps/web/src/routes/app.tsx
check "api client" "web codegen" packages/api-client/src/index.ts
check "generated crds" "codegen scripts" charts/kuben/crds/kuben.dev_all.yaml
check "helm template" "scripts" charts/kuben/templates/rbac.yaml
check "e2e script" "scripts" scripts/e2e.sh
check "workflow" "rust deps web codegen scripts" .github/workflows/ci.yml
check "justfile" "rust deps web codegen scripts" justfile
check "mixed" "rust web codegen" crates/kuben-api/src/routes/apps.rs apps/web/src/lib/api.ts
check "nextest config" "rust codegen" .config/nextest.toml

all=$(printf '' | scripts/ci-changes.sh | grep -c '=true')
if [[ $all == 5 ]]; then echo "ok   empty list selects everything"; else echo "FAIL empty list"; failures=$((failures + 1)); fi
all=$(scripts/ci-changes.sh --all </dev/null | grep -c '=true')
if [[ $all == 5 ]]; then echo "ok   --all"; else echo "FAIL --all"; failures=$((failures + 1)); fi

if ((failures)); then
  echo "$failures case(s) failed" >&2
  exit 1
fi
echo "all change-detection cases passed"
