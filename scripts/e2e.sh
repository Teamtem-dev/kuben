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

# Runs from any directory (`turbo run e2e` starts it in crates/kuben).
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN=${KUBEN_BIN:-$ROOT/target/debug/kuben}
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

# In GitHub Actions a failure also becomes an annotation: the PR page and the
# public checks API show it without opening the sign-in-only job log.
annotate() {
  if [[ -n ${GITHUB_ACTIONS:-} ]]; then echo "::error title=e2e: ${current:-setup}::$*"; fi
}
step() {
  current=$*
  printf '\n\033[1m==> %s\033[0m\n' "$*"
}
fail() {
  echo "FAIL: $*" >&2
  annotate "$*"
  failed=1
  exit 1
}

cleanup() {
  status=$?
  if [[ -n ${pid:-} ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  if ((status != 0)); then
    if [[ -z ${failed:-} ]]; then
      # A command failed under `set -e` (e.g. a rollout timeout): name the step
      # and attach the end of the server log, newlines encoded for the annotation.
      annotate "exit ${status}; last kuben log lines:%0A$(tail -n 15 "$work/kuben.log" 2>/dev/null | sed 's/%/%25/g' | awk '{ printf "%s%%0A", $0 }')"
    fi
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
eventually 30 "service web on port 80" bash -c "kubectl -n $NS get service web -o jsonpath='{.spec.ports[0].port}' | grep -qx 80"
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

step "scenario 3: CI deploy with an API token"
expect 201 POST /tokens "{\"name\":\"ci\",\"role\":\"developer\",\"project\":\"${P}\"}"
token=$(jq -r .token "$work/body")
token_id=$(jq -r .info.id "$work/body")
expect_as 200 "bearer:$token" PATCH "$APP/web" '{"env":[{"name":"GREETING","value":"from-ci"}]}'
expect_as 403 "bearer:$token" POST /tokens '{"name":"escalate"}'
expect 204 DELETE "/tokens/${token_id}"
expect_as 401 "bearer:$token" GET /me

step "scenario 2: audit log"
expect 200 GET "/audit?limit=200"
jq -e '.events | map(.action) | (index("createApp") != null) and (index("rollbackApp") != null) and (index("revokeToken") != null)' \
  "$work/body" >/dev/null || fail "audit: $(jq -c '[.events[].action]' "$work/body")"

step "scenario 9: domain check"
expect 200 PATCH "$APP/web" '{"domains":["web.e2e.invalid"]}'
# The duplicate check reads the projection, which follows the cluster asynchronously.
eventually 30 "domain in projection" bash -c "curl -fsS -b '$work/cookies' $BASE$APP/web | jq -e '.app.domains | index(\"web.e2e.invalid\") != null'"
expect 409 POST "$APP" "{\"name\":\"clash\",\"image\":\"${IMAGE}\",\"port\":8080,\"domains\":[\"web.e2e.invalid\"]}"
expect 200 GET "$APP/web/domains"
jq -e '.[0].host == "web.e2e.invalid" and .[0].status == "unresolved"' "$work/body" >/dev/null || fail "domains: $(cat "$work/body")"

step "scenario 7: cron job and run now"
expect 201 POST "$APP" \
  "{\"name\":\"tick\",\"image\":\"${JOB_IMAGE}\",\"command\":[\"sh\",\"-c\",\"echo tick\"],\"schedule\":\"*/30 * * * *\",\"size\":\"nano\"}"
eventually 60 "cronjob created" kubectl -n "$NS" get cronjob tick-job
expect 202 POST "$APP/tick/run" '{}'
job=$(jq -r .job "$work/body")
kubectl -n "$NS" wait --for=condition=complete "job/${job}" --timeout=180s
eventually 60 "app reported as scheduled" bash -c "curl -fsS -b '$work/cookies' $BASE$APP/tick | jq -e '.app.reason == \"Scheduled\"'"

step "scenario 6: volume survives app deletion"
expect 201 POST "$APP" \
  "{\"name\":\"store\",\"image\":\"${IMAGE}\",\"port\":8080,\"volumes\":[{\"name\":\"data\",\"mount_path\":\"/data\",\"size\":\"100Mi\"}]}"
eventually 60 "pvc created" kubectl -n "$NS" get pvc store-data
eventually 60 "store deployment uses Recreate" bash -c "kubectl -n $NS get deployment store-web -o jsonpath='{.spec.strategy.type}' | grep -qx Recreate"
kubectl -n "$NS" rollout status deployment/store-web --timeout=180s
expect 422 PATCH "$APP/store" '{"replicas":3}'
expect 204 DELETE "$APP/store"
eventually 90 "store deployment gone" bash -c "! kubectl -n $NS get deployment store-web"
kubectl -n "$NS" get pvc store-data >/dev/null || fail "volume was deleted with the app"

step "scenario 8: redis from a template"
expect 201 POST "/projects/${P}/environments/dev/templates/redis" '{"name":"cache"}'
[[ $(jq -r .credentials_secret "$work/body") == cache-credentials ]] || fail "credentials secret name"
eventually 30 "credentials url key" bash -c "kubectl -n $NS get secret cache-credentials -o jsonpath='{.data.url}' | base64 -d | grep -q '^redis://:'"
eventually 60 "tcp service port 6379" bash -c "kubectl -n $NS get service cache -o jsonpath='{.spec.ports[0].port}' | grep -qx 6379"
eventually 60 "redis deployment" kubectl -n "$NS" get deployment cache-web
kubectl -n "$NS" rollout status deployment/cache-web --timeout=180s

step "scenario 10: promote dev → live"
expect 201 POST "/projects/${P}/environments" '{"name":"live"}'
eventually 60 "namespace ${NS_LIVE}" kubectl get namespace "$NS_LIVE"
eventually 60 "live ready" bash -c "kubectl get environment ${P}-live -o jsonpath='{.status.phase}' | grep -qx Ready"
eventually 30 "live visible" bash -c "curl -fsS -b '$work/cookies' $BASE/projects/${P}/environments/live"
expect 200 POST "$APP/web/promote" '{"to_environment":"live","dry_run":true}'
jq -e '.dry_run and .created and (.changes | length == 1)' "$work/body" >/dev/null || fail "dry run: $(cat "$work/body")"
kubectl -n "$NS_LIVE" get app web >/dev/null 2>&1 && fail "dry run must not write"
expect 200 POST "$APP/web/promote" '{"to_environment":"live"}'
eventually 60 "promoted deployment" kubectl -n "$NS_LIVE" get deployment web-web
[[ $(kubectl -n "$NS_LIVE" get app web -o jsonpath='{.spec.domains}') == "" ]] || fail "domains must not be promoted"

step "scenario 4: team invitation"
expect 201 POST /members '{"email":"dev@e2e.test","role":"developer"}'
temp=$(jq -r .temporary_password "$work/body")
expect_as 200 member POST /auth/login "{\"email\":\"dev@e2e.test\",\"password\":\"${temp}\"}"
expect_as 403 member GET /projects
expect_as 204 member POST /me/password "{\"current_password\":\"${temp}\",\"new_password\":\"a-much-longer-password\"}"
expect_as 200 member GET /projects

step "scenario 1: login throttling"
for _ in 1 2 3 4 5; do
  expect_as 401 none POST /auth/login '{"email":"nobody@e2e.test","password":"wrong"}'
done
expect_as 429 none POST /auth/login '{"email":"nobody@e2e.test","password":"wrong"}'

step "authorization and validation"
expect 422 POST "$APP" '{"name":"Bad_Name","image":"nginx"}'
expect 409 DELETE "/projects/${P}"

step "delete apps → children are garbage-collected"
for a in web tick cache; do expect 204 DELETE "$APP/${a}?delete_volumes=true"; done
eventually 90 "deployment gone" bash -c "! kubectl -n $NS get deployment web-web"
eventually 90 "cronjob gone" bash -c "! kubectl -n $NS get cronjob tick-job"
eventually 90 "template volume deleted on request" bash -c "! kubectl -n $NS get pvc cache-data"

step "delete environments → namespaces deleted"
expect 202 DELETE "/projects/${P}/environments/dev"
expect 202 DELETE "/projects/${P}/environments/live"
eventually 180 "namespace gone" bash -c "! kubectl get namespace $NS"
eventually 180 "live namespace gone" bash -c "! kubectl get namespace $NS_LIVE"
eventually 60 "environments gone" bash -c "! kubectl get environment ${P}-dev ${P}-live"

step "delete project"
eventually 30 "project has no environments" bash -c "curl -fsS -b '$work/cookies' $BASE/projects/${P}/environments | jq -e 'length == 0'"
expect 204 DELETE "/projects/${P}"

printf '\n\033[32mE2E PASSED\033[0m\n'
