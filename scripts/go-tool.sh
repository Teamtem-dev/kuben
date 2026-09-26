#!/usr/bin/env bash
# Run a Go tool pinned in tools/go.mod (gofumpt, ogen, controller-gen,
# nilaway, exhaustive, go-check-sumtype, govulncheck) from any directory:
#
#   scripts/go-tool.sh <tool> [args...]
#
# tools/ is a module of its own and not a member of go.work, so that its
# requirements never raise the versions the hub, the agent and the API
# module build with. `go tool -modfile` does not work in workspace mode, so
# the tool is built in its own module (GOWORK=off; go caches the binary) and
# then runs here, in the caller's directory and workspace: an analyzer loads
# packages exactly as `go build` does.
set -euo pipefail
tools=$(cd "$(dirname "${BASH_SOURCE[0]}")/../tools" && pwd)
name=${1:?usage: scripts/go-tool.sh <tool> [args...]}
shift
bin=$(cd "$tools" && GOWORK=off go tool -n "$name")
exec "$bin" "$@"
