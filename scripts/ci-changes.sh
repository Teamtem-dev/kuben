#!/usr/bin/env bash
# Decide which CI job groups a change needs. Reads changed paths (one per
# line) on stdin and prints `group=true|false` lines for $GITHUB_OUTPUT.
#
#   git diff --name-only HEAD^1 HEAD | scripts/ci-changes.sh
#   scripts/ci-changes.sh --all        # everything (push to main, merge queue, …)
#
# Groups and the jobs they gate:
#   web      web app and TypeScript packages       → web, budgets
#   codegen  the frozen contracts and the TS client → drift, api-compat
#   scripts  shell scripts, installer, Helm chart  → scripts, e2e jobs
#   go       Go modules (go/, go.work), and the     → go, go-kind, oracle, e2e jobs,
#            frozen contracts the Go tests pin        budgets
#
# Fail open: a change to CI itself (.github/) or to the task runner every job
# goes through (turbo.json, the root package.json, bun.lock, bunfig.toml), or an
# empty or unreadable list, selects everything. Checks are never skipped by
# accident. Portable to bash 3.2 (macOS) so it can be run locally:
#   git diff --name-only origin/main...HEAD | scripts/ci-changes.sh
set -euo pipefail

web=false codegen=false scripts=false go=false
select_all() { web=true codegen=true scripts=true go=true; }

if [[ ${1:-} == --all ]]; then
  select_all
else
  count=0
  while IFS= read -r path || [[ -n $path ]]; do
    [[ -z $path ]] && continue
    count=$((count + 1))
    case "$path" in
    .github/* | turbo.json | package.json | bun.lock | bunfig.toml) select_all ;;
    go/* | go.work | go.work.sum | .golangci.yml) go=true ;;
    scripts/go-check.sh | scripts/go-build.sh)
      go=true
      scripts=true
      ;;
    # The frozen OpenAPI spec: the Go server is generated from it.
    packages/api-client/openapi.json)
      web=true
      codegen=true
      go=true
      ;;
    packages/api-client/*)
      web=true
      codegen=true
      ;;
    # The frozen CRD manifest: the Go tests pin the embedded copy to it.
    charts/kuben/crds/*)
      codegen=true
      scripts=true
      go=true
      ;;
    apps/* | packages/* | biome.json | tsconfig.base.json) web=true ;;
    scripts/* | install.sh | charts/* | deploy/* | Dockerfile | .trivyignore.yaml) scripts=true ;;
    *) ;; # docs, Markdown, license: no checks needed
    esac
  done
  if ((count == 0)); then select_all; fi
fi

printf 'web=%s\ncodegen=%s\nscripts=%s\ngo=%s\n' "$web" "$codegen" "$scripts" "$go"
