#!/usr/bin/env bash
# The API compatibility gate (M4.12): the OpenAPI spec of this change may add
# to the API, never break it, unless the change says so.
#
#   scripts/check-api-compat.sh <base spec> [head spec]
#
# The head spec defaults to packages/api-client/openapi.json. oasdiff comes
# from $OASDIFF or PATH (CI installs a pinned, checksummed release). With
# KUBEN_ALLOW_BREAKING=true (the pull request carries the `breaking` label)
# breaking changes are reported but pass. A base without a spec (the gate's
# first run) passes.
set -euo pipefail
cd "$(dirname "$0")/.."

base=${1:?usage: check-api-compat.sh <base spec> [head spec]}
head=${2:-packages/api-client/openapi.json}
oasdiff=${OASDIFF:-oasdiff}

summary() { if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then cat >>"$GITHUB_STEP_SUMMARY"; else cat >/dev/null; fi; }

if [[ ! -s $base ]]; then
  echo "no base spec: nothing to compare"
  exit 0
fi
command -v "$oasdiff" >/dev/null || { echo "oasdiff not found (set OASDIFF)" >&2; exit 2; }

{
  echo "### API changes"
  "$oasdiff" changelog "$base" "$head" --format markdown || true
} | summary

if "$oasdiff" breaking "$base" "$head" --fail-on ERR; then
  echo "the API stays compatible"
  exit 0
fi
if [[ ${KUBEN_ALLOW_BREAKING:-false} == true ]]; then
  echo "breaking API changes, allowed by the 'breaking' label"
  if [[ -n ${GITHUB_ACTIONS:-} ]]; then echo "::warning title=Breaking API change::allowed by the 'breaking' label"; fi
  exit 0
fi
echo "error: the change breaks the API; keep the old shape, or label the pull request 'breaking'" >&2
if [[ -n ${GITHUB_ACTIONS:-} ]]; then
  echo "::error title=Breaking API change::keep the old shape, or label the pull request 'breaking'"
fi
exit 1
