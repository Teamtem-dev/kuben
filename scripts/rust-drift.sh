#!/usr/bin/env bash
# Rust files changed since the baseline of the Go port (go/PARITY.md), so a
# ported Go package does not silently fall behind the Rust it replaces.
# Exit 1 when anything drifted.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
base=$(grep -o 'Baseline: every Rust file as of `[0-9a-f]*`' "$root/go/PARITY.md" | grep -o '[0-9a-f]\{7,\}')
changed=$(git -C "$root" diff --name-only "$base" HEAD -- crates packages/api-client/openapi.json charts/kuben/crds)
if [[ -z $changed ]]; then
  echo "no drift since $base"
  exit 0
fi
echo "changed since $base (update the Go counterparts, then move the baseline):"
echo "$changed" | sed 's/^/  /'
exit 1
