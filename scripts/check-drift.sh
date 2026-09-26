#!/usr/bin/env bash
# Fail if a committed generated file is stale. Regenerates with `turbo run gen`
# and compares against the files as they were before, so no git is needed and
# a fresh `bun run gen` in the working tree passes.
#
# Generated: the TS client types and the Go API server (ogen, with the API
# reference's copy of the spec) from the OpenAPI spec, and the DeepCopy of
# the CRD types (controller-gen). The OpenAPI spec
# (packages/api-client/openapi.json) and the CRD manifest
# (charts/kuben/crds/kuben.dev_all.yaml) are frozen contracts: the Rust code
# generated them up to 1.2, and the Go tests pin the server and the embedded
# CRDs to the committed files.
#
#   bun run drift
set -euo pipefail
cd "$(dirname "$0")/.."

files=(
  packages/api-client/src/schema.d.ts
  apps/kuben/internal/httpapi/apidocs/openapi.json
  packages/api/v1alpha1/zz_generated.deepcopy.go
)
while IFS= read -r f; do files+=("$f"); done < <(find apps/kuben/internal/httpapi/gen -type f | sort)
# An explicit template: BSD mktemp ignores $TMPDIR without one.
snap=$(mktemp -d "${TMPDIR:-/tmp}/kuben-drift.XXXXXX")
trap 'rm -rf "$snap"' EXIT
for f in "${files[@]}"; do
  mkdir -p "$snap/$(dirname "$f")"
  cp "$f" "$snap/$f"
done

bun turbo run gen --output-logs=errors-only

stale=0
for f in "${files[@]}"; do diff -u "$snap/$f" "$f" || stale=1; done
if ((stale)); then
  echo "error: generated files were stale; run 'bun run gen' and commit the result" >&2
  if [[ -n ${GITHUB_ACTIONS:-} ]]; then echo "::error title=Generated files are stale::run 'bun run gen' and commit the result"; fi
  exit 1
fi
echo "generated files are up to date"
