#!/usr/bin/env bash
# Build one of the two Go binaries as it ships: static (CGO_ENABLED=0),
# trimmed, with its version linked in.
#
#   scripts/go-build.sh kuben [output] [version]         # default: apps/kuben/bin/kuben
#   scripts/go-build.sh kuben-agent [output] [version]   # default: apps/kuben-agent/bin/kuben-agent
#
# The hub embeds the console: apps/console/dist is copied into
# apps/kuben/internal/httpapi/web/dist before `go build`. The turbo tasks
# kuben#build and kuben-agent#build run this script (the first builds the
# console before); a relative output is relative to the caller's directory.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
name=${1:?usage: scripts/go-build.sh kuben|kuben-agent [output] [version]}
version=${3:-dev}
case $name in
kuben) ldflags="-s -w -X github.com/Teamtem-dev/kuben/apps/kuben/internal/version.Version=$version" ;;
kuben-agent) ldflags="-s -w -X main.version=$version" ;;
*)
  echo "unknown binary: $name (kuben or kuben-agent)" >&2
  exit 2
  ;;
esac
out=${2:-$root/apps/$name/bin/$name}
case $out in /*) ;; *) out=$PWD/$out ;; esac

if [[ $name == kuben ]]; then
  dist=$root/apps/kuben/internal/httpapi/web/dist
  if [[ -f $root/apps/console/dist/index.html ]]; then
    find "$dist" -mindepth 1 ! -name .gitkeep -delete
    cp -R "$root/apps/console/dist/." "$dist/"
    find "$dist" -name '*.map' -delete
  else
    echo "apps/console/dist is missing: the binary will say the UI is not embedded" >&2
  fi
fi

cd "$root/apps/$name"
CGO_ENABLED=${CGO_ENABLED:-0} go build -trimpath -ldflags "$ldflags" -o "$out" "./cmd/$name"
echo "built $out ($version)"
