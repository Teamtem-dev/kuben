#!/usr/bin/env bash
# Build the hub with the console embedded: copy apps/console/dist into
# internal/httpapi/web/dist, then `go build`; the agent is built next to it
# (kuben-agent in the same directory). Used by the turbo task //#go:build
# (which builds the console first).
#
#   scripts/go-build.sh [output] [version]      # default output: bin/kuben
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$root/bin/kuben}
version=${2:-dev}
dist=$root/internal/httpapi/web/dist
if [[ -f $root/apps/console/dist/index.html ]]; then
  find "$dist" -mindepth 1 ! -name .gitkeep -delete
  cp -R "$root/apps/console/dist/." "$dist/"
  find "$dist" -name '*.map' -delete
else
  echo "apps/console/dist is missing: the binary will say the UI is not embedded" >&2
fi
case $out in /*) ;; *) out=$PWD/$out ;; esac
agent=$(dirname "$out")/kuben-agent
cd "$root"
CGO_ENABLED=${CGO_ENABLED:-0} go build -trimpath \
  -ldflags "-s -w -X github.com/Teamtem-dev/kuben/internal/version.Version=$version" \
  -o "$out" ./cmd/kuben
CGO_ENABLED=${CGO_ENABLED:-0} go build -trimpath \
  -ldflags "-s -w -X main.version=$version" \
  -o "$agent" ./cmd/kuben-agent
echo "built $out and $agent ($version)"
