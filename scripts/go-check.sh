#!/usr/bin/env bash
# Static checks for the Go modules: what stands in for the guarantees the
# Rust compiler gave (apps/kuben/CONVENTIONS.md). gofumpt, go vet,
# exhaustive, go-check-sumtype and NilAway need nothing but `go` (the tools
# are pinned in tools/go.mod, see scripts/go-tool.sh); golangci-lint with
# .golangci.yml runs as well when it is installed (CI always runs it).
#
#   scripts/go-check.sh                  # every module of go.work
#   scripts/go-check.sh apps/kuben ...   # these modules (relative to the current directory)
#
# The turbo task `lint` of each Go package runs it on that package.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tool() { "$root/scripts/go-tool.sh" "$@"; }

modules=()
for m in "$@"; do modules+=("$(cd "$m" && pwd)"); done
if ((${#modules[@]} == 0)); then
  modules=("$root/apps/kuben" "$root/apps/kuben-agent" "$root/packages/api")
fi

for dir in "${modules[@]}"; do
  [[ -f $dir/go.mod ]] || { echo "not a Go module: $dir" >&2; exit 2; }
  echo "== ${dir#"$root"/}"
  cd "$dir"
  unformatted=$(tool gofumpt -l .)
  if [[ -n $unformatted ]]; then
    echo "not gofumpt-formatted:"; echo "$unformatted"; exit 1
  fi
  go vet ./...
  tool exhaustive -default-signifies-exhaustive=false -ignore-enum-types '^(reflect\.Kind|k8s\.io/api/core/v1\.ResourceName)$' ./...
  tool go-check-sumtype -default-signifies-exhaustive=false ./...
  # NilAway checks Kuben's own packages only: inferring through client-go and
  # the other dependencies takes more than 13 GiB (the analysis is killed on
  # a 16 GiB machine), and their nilness is not ours to fix. The generated
  # API server is excluded as generated code is.
  tool nilaway -test=true \
    -include-pkgs github.com/Teamtem-dev/kuben \
    -exclude-pkgs github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen \
    -exclude-file-docstrings "Code generated" ./...
  if command -v golangci-lint >/dev/null; then
    golangci-lint run ./...
  else
    echo "golangci-lint not installed: skipped (CI runs it)"
  fi
done
