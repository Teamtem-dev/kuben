#!/usr/bin/env bash
# Scans a release binary the way users' scanners will see it (Trivy).
#
#   scripts/scan-binary.sh <binary>
#
# 1. The binary must carry its dependency list (cargo-auditable). Without it a
#    scanner sees an opaque file and silently reports nothing.
# 2. No HIGH or CRITICAL vulnerability with a fixed version may ship.
#    cargo-deny gates RustSec advisories on the lockfile; this checks the artifact.
set -euo pipefail

bin=${1:-}
[[ -f $bin ]] || { echo "usage: scripts/scan-binary.sh <binary>" >&2; exit 2; }

error() {
  echo "error: $*" >&2
  if [[ -n ${GITHUB_ACTIONS:-} ]]; then echo "::error title=Binary scan::$*"; fi
  exit 1
}

# Trivy only inspects executables in rootfs mode, and scans directories: give
# it one that holds just this binary.
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
cp "$bin" "$dir/"

crates=$(trivy rootfs --quiet --format cyclonedx "$dir" |
  jq '[.components[]? | select((.purl // "") | startswith("pkg:cargo/"))] | length')
((crates > 0)) || error "$(basename "$bin") carries no dependency list; build it with 'cargo auditable'"
echo "dependency list: ${crates} crates"

trivy rootfs --quiet --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 "$dir"
echo "no HIGH or CRITICAL vulnerabilities with a fix available"
