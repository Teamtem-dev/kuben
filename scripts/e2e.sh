#!/usr/bin/env bash
# End-to-end test: a real cluster (kind), the real binary, the public API.
#
#   scripts/e2e.sh                     # uses ./target/debug/kuben and the current kube context
#   KUBEN_BIN=target/release/kuben scripts/e2e.sh
#
# Exercises: CRD self-apply, the controller Lease, login, project → environment → namespace with
# quota/limits/isolation, app deploy → Deployment/Service rollout, scale,
# logs, restart, and the day-2 scenarios of blueprint §5.9: releases and
# rollback, API tokens, audit, cron jobs with "run now", volumes that survive
# app deletion, templates, promotion, domain checks, team invitations and
# login throttling; then deletes and garbage collection.
set -euo pipefail

BIN=${KUBEN_BIN:-target/debug/kuben}
PORT=${KUBEN_E2E_PORT:-18080}
BASE="http://127.0.0.1:${PORT}/api/v1"
PASSWORD="e2e-$(date +%s)-password"
IMAGE=${KUBEN_E2E_IMAGE:-nginxinc/nginx-unprivileged:1.27-alpine}
JOB_IMAGE=${KUBEN_E2E_JOB_IMAGE:-busybox:1.36}
P=e2e
LEASE_NS=${KUBEN_E2E_LEASE_NAMESPACE:-default}
NS="kb-${P}-dev"
NS_LIVE="kb-${P}-live"
APP="/projects/${P}/environments/dev/apps"

need() { command -v "$1" >/dev/null || { echo "missing: $1" >&2; exit 2; }; }
for c in kubectl curl jq; do need "$c"; done
[[ -x $BIN ]] || { echo "build the binary first: cargo build -p kuben ($BIN)" >&2; exit 2; }

work=$(mktemp -d)
cleanup() {
  status=$?
  if [[ -n ${pid:-} ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  if ((status != 0)); then
    echo "---- kuben log (last 80 lines) ----"
    tail -n 80 "$work/kuben.log" || true
    kubectl get projects,environments,apps -A 2>/dev/null || true
    kubectl -n "$NS" get all,resourcequota,networkpolicy,pvc,cronjobs,jobs 2>/dev/null || true
  fi
  kubectl delete environment "${P}-dev" "${P}-live" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete project "$P" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$LEASE_NS" delete lease kuben-controller --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# eventually <seconds> <description> <command...>
eventually() {
  local timeout=$1 what=$2
  shift 2
  for ((i = 0; i < timeout; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  fail "timed out after ${timeout}s waiting for: $what"
}

# request <cookie jar | bearer:TOKEN | none> <method> <path> [json] → HTTP status
request() {
  local auth=$1 method=$2 path=$3 body=${4:-}
  local args=(-sS -o "$work/body" -w '%{http_code}' -X "$method" -H 'content-type: application/json')
  case "$auth" in
  bearer:*) args+=(-H "authorization: Bearer ${auth#bearer:}") ;;
  none) args+=(-H 'x-kuben-client: e2e') ;;
  *) args+=(-b "$work/$auth" -c "$work/$auth" -H 'x-kuben-client: e2e') ;;
  esac
  [[ -n $body ]] && args+=(--data "$body")
  curl "${args[@]}" "$BASE$path"
}

api() { request cookies "$@"; }

expect() { # <status> <method> <path> [json]
  local want=$1
  shift
  local got
  got=$(api "$@")
  [[ $got == "$want" ]] || fail "$1 $2 → HTTP $got (want $want): $(cat "$work/body")"
}

expect_as() { # <status> <auth> <method> <path> [json]
  local want=$1 auth=$2
  shift 2
  local got
  got=$(request "$auth" "$@")
  [[ $got == "$want" ]] || fail "[$auth] $1 $2 → HTTP $got (want $want): $(cat "$work/body")"
}

step "start kuben"
KUBEN_SERVER__BIND="127.0.0.1:${PORT}" \
  KUBEN_SERVER__METRICS_BIND="127.0.0.1:$((PORT + 1))" \
  KUBEN_DATABASE__URL="sqlite://${work}/kuben.db" \
  KUBEN_BOOTSTRAP__ADMIN_PASSWORD="$PASSWORD" \
  KUBEN_SECURITY__COOKIE_SECURE=false \
