#!/usr/bin/env bash
# Decide which CI job groups a change needs. Reads changed paths (one per
# line) on stdin and prints `group=true|false` lines for $GITHUB_OUTPUT.
#
#   git diff --name-only HEAD^1 HEAD | scripts/ci-changes.sh
#   scripts/ci-changes.sh --all        # everything (push to main, merge queue, …)
#
# Groups and the jobs they gate:
#   rust     Rust sources, manifests, toolchain   → fmt, clippy, tests, MSRV, e2e, budgets
#   deps     dependency manifests                  → cargo-deny
#   web      web app and TypeScript packages       → web, budgets
#   codegen  generated files or their inputs       → drift (always with rust: Rust
#            types produce the OpenAPI spec and the CRDs)
#   scripts  shell scripts, installer, Helm chart  → scripts, e2e
#
# Fail open: a change to CI itself (.github/, justfile), or an empty or
# unreadable list, selects everything. Checks are never skipped by accident.
# Portable to bash 3.2 (macOS) so it can be run locally: `just ci-changes`.
set -euo pipefail

rust=false deps=false web=false codegen=false scripts=false
select_all() { rust=true deps=true web=true codegen=true scripts=true; }

if [[ ${1:-} == --all ]]; then
  select_all
else
  count=0
  while IFS= read -r path || [[ -n $path ]]; do
    [[ -z $path ]] && continue
    count=$((count + 1))
    case "$path" in
    .github/* | justfile) select_all ;;
    Cargo.toml | Cargo.lock | crates/*/Cargo.toml | deny.toml)
      rust=true
      deps=true
      ;;
    crates/* | rust-toolchain.toml | clippy.toml | rustfmt.toml | .cargo/* | .config/nextest.toml) rust=true ;;
    packages/api-client/*)
      web=true
      codegen=true
      ;;
    charts/kuben/crds/*)
      codegen=true
      scripts=true
      ;;
    apps/* | packages/* | package.json | pnpm-lock.yaml | pnpm-workspace.yaml | biome.json | tsconfig.base.json | .nvmrc | .npmrc) web=true ;;
    scripts/* | install.sh | charts/* | deploy/* | Dockerfile) scripts=true ;;
    *) ;; # docs, Markdown, license: no checks needed
    esac
  done
  if ((count == 0)); then select_all; fi
fi

if [[ $rust == true ]]; then codegen=true; fi

printf 'rust=%s\ndeps=%s\nweb=%s\ncodegen=%s\nscripts=%s\n' "$rust" "$deps" "$web" "$codegen" "$scripts"
