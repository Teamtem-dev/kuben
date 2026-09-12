#!/usr/bin/env bash
# Fail if a committed generated file is stale. Regenerates with `turbo run gen`
# and compares against the files as they were before, so no git is needed and
# a fresh `bun run gen` in the working tree passes.
#
#   bun run drift
set -euo pipefail
cd "$(dirname "$0")/.."

files=(packages/api-client/openapi.json packages/api-client/src/schema.d.ts charts/kuben/crds/kuben.dev_all.yaml)
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
