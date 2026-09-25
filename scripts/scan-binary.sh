#!/usr/bin/env bash
# Scans a release binary the way users' scanners will see it (Trivy).
#
#   scripts/scan-binary.sh <binary>
#
# 1. The binary must carry its module list. Go embeds it in every binary
#    (`go version -m <binary>` prints it); without it a scanner sees an opaque
#    file and silently reports nothing.
# 2. No HIGH or CRITICAL vulnerability with a fixed version may ship.
#    govulncheck gates the source in CI; this checks the artifact.
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

modules=$(trivy rootfs --quiet --format cyclonedx "$dir" |
  jq '[.components[]? | select((.purl // "") | startswith("pkg:golang/"))] | length')
((modules > 0)) || error "$(basename "$bin") carries no module list (see 'go version -m'); build it with 'go build', unpacked"
echo "dependency list: ${modules} Go modules"

trivy rootfs --quiet --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 "$dir"
echo "no HIGH or CRITICAL vulnerabilities with a fix available"
