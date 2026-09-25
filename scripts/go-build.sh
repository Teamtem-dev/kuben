#!/usr/bin/env bash
# Build the Go hub with the console embedded: copy apps/console/dist into
# go/hub/internal/api/web/dist, then `go build`. Used by the turbo task
# hub#build (which builds the console first) and by goreleaser's hook.
#
#   scripts/go-build.sh [output] [version]
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$root/go/hub/bin/kuben}
version=${2:-dev}
dist=$root/go/hub/internal/api/web/dist
if [[ -f $root/apps/console/dist/index.html ]]; then
  find "$dist" -mindepth 1 ! -name .gitkeep -delete
  cp -R "$root/apps/console/dist/." "$dist/"
  find "$dist" -name '*.map' -delete
else
  echo "apps/console/dist is missing: the binary will say the UI is not embedded" >&2
fi
cd "$root/go/hub"
CGO_ENABLED=${CGO_ENABLED:-0} go build -trimpath \
  -ldflags "-s -w -X github.com/Teamtem-dev/kuben/go/hub/internal/version.Version=$version" \
  -o "$out" ./cmd/kuben
echo "built $out ($version)"
