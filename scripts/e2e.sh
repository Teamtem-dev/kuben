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
  KUBEN_KUBE__REQUIRED=true \
  KUBEN_KUBE__LEADER_ELECTION=true \
  KUBEN_KUBE__NAMESPACE="$LEASE_NS" \
  KUBEN_TELEMETRY__LOG_FORMAT=pretty \
  "$BIN" serve --roles=all >"$work/kuben.log" 2>&1 &
pid=$!
eventually 60 "CRDs applied" kubectl get crd apps.kuben.dev
eventually 30 "controller lease held" bash -c \
  "kubectl -n $LEASE_NS get lease kuben-controller -o jsonpath='{.spec.holderIdentity}' | grep -q ."
# Ready only once every informer has listed (projections complete).
eventually 90 "readyz" curl -fsS "http://127.0.0.1:${PORT}/readyz"

step "login"
expect 200 POST /auth/login "{\"email\":\"admin@kuben.local\",\"password\":\"${PASSWORD}\"}"
expect 200 GET /me
[[ $(jq -r .email "$work/body") == admin@kuben.local ]] || fail "unexpected /me"

step "project"
expect 201 POST /projects "{\"name\":\"${P}\",\"display_name\":\"E2E\"}"
eventually 30 "project visible" bash -c "curl -fsS -b '$work/cookies' $BASE/projects/${P}"
eventually 30 "project ready" bash -c "kubectl get project ${P} -o jsonpath='{.status.conditions[0].status}' | grep -qx True"

step "environment → namespace with guard rails"
expect 201 POST "/projects/${P}/environments" '{"name":"dev","quota":{"cpu":"4","memory":"4Gi","pods":30}}'
eventually 60 "namespace ${NS}" kubectl get namespace "$NS"
eventually 60 "environment ready" bash -c "kubectl get environment ${P}-dev -o jsonpath='{.status.phase}' | grep -qx Ready"
kubectl get namespace "$NS" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}' | grep -qx baseline || fail "PSA label"
kubectl -n "$NS" get resourcequota kuben-quota -o jsonpath='{.spec.hard.services\.loadbalancers}' | grep -qx 0 || fail "quota"
kubectl -n "$NS" get limitrange kuben-defaults >/dev/null || fail "limit range"
kubectl -n "$NS" get networkpolicy kuben-isolation >/dev/null || fail "network policy"
eventually 30 "environment visible" bash -c "curl -fsS -b '$work/cookies' $BASE/projects/${P}/environments/dev"

step "app deploy"
expect 201 POST "$APP" \
  "{\"name\":\"web\",\"image\":\"${IMAGE}\",\"port\":8080,\"env\":[{\"name\":\"GREETING\",\"value\":\"hello\"}],\"health_check_path\":\"/\"}"
eventually 30 "deployment created" kubectl -n "$NS" get deployment web-web
kubectl -n "$NS" rollout status deployment/web-web --timeout=180s
kubectl -n "$NS" get service web -o jsonpath='{.spec.ports[0].port}' | grep -qx 80 || fail "service"
kubectl -n "$NS" get deployment web-web -o jsonpath='{.spec.template.spec.containers[0].startupProbe.failureThreshold}' | grep -qx 60 || fail "startup probe"
eventually 60 "app ready via API" bash -c "curl -fsS -b '$work/cookies' $BASE$APP/web | jq -e '.app.ready and (.pods | length == 1)'"
expect 200 GET "$APP/web"
[[ $(jq -r '.app.env[] | select(.name == "GREETING") | .value' "$work/body") == hello ]] || fail "env value"

step "logs"
expect 200 GET "$APP/web/logs?tail=20"
jq -e 'length == 1 and .[0].error == null' "$work/body" >/dev/null || fail "logs: $(cat "$work/body")"

step "scale to 2"
expect 200 PATCH "$APP/web" '{"replicas":2}'
eventually 120 "2 ready replicas" bash -c "kubectl -n $NS get deployment web-web -o jsonpath='{.status.readyReplicas}' | grep -qx 2"

step "restart"
before=$(kubectl -n "$NS" get deployment web-web -o jsonpath='{.metadata.generation}')
expect 202 POST "$APP/web/restart"
eventually 60 "rollout triggered" bash -c "[[ \$(kubectl -n $NS get deployment web-web -o jsonpath='{.metadata.generation}') -gt $before ]]"
kubectl -n "$NS" rollout status deployment/web-web --timeout=180s

step "scenario 5: release history and rollback"
expect 200 GET "$APP/web/releases"
jq -e 'length >= 2 and .[0].current and (map(.reason) | index("create") != null)' "$work/body" >/dev/null ||
  fail "releases: $(cat "$work/body")"
expect 200 POST "$APP/web/rollback" '{"revision":1}'
eventually 120 "rolled back to 1 replica" bash -c "kubectl -n $NS get deployment web-web -o jsonpath='{.spec.replicas}' | grep -qx 1"
expect 200 GET "$APP/web/releases"
[[ $(jq -r '.[0].reason' "$work/body") == rollback ]] || fail "rollback not recorded"
