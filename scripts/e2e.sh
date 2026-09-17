#!/usr/bin/env bash
# End-to-end test: a real cluster (kind), the real binary, the public API.
#
#   KUBEN_E2E_DATABASE_URL=postgres://postgres:kuben@localhost:5432/postgres scripts/e2e.sh
#   KUBEN_BIN=target/release/kuben scripts/e2e.sh      # default: ./target/debug/kuben
#   KUBEN_AGENT_BIN=...                                # default: ./target/debug/kuben-agent (built if missing)
#
# Uses the current kube context and KUBEN_E2E_DATABASE_URL, an empty
# PostgreSQL database (ADR-025): the run creates the first admin, so a
# database left over from an earlier run fails at login. A local server:
#   docker run -d --rm --name kuben-e2e-pg -e POSTGRES_PASSWORD=kuben -p 5432:5432 postgres:17-alpine
#
# Exercises: CRD self-apply, the controller Lease, login, project → environment → namespace with
# quota/limits/isolation, app deploy → Deployment/Service rollout, scale,
# logs, restart, and the day-2 scenarios of blueprint §5.9: releases and
# rollback, API tokens, audit, cron jobs with "run now", volumes that survive
# app deletion, templates, promotion, domain checks, team invitations and
# login throttling; deploys by digest through `…/deployments` (idempotent
# replay, a stale expected generation refused, rollback) and a direct App
# edit replaced as drift (ADR-032); a cluster agent enrolled with a bootstrap
# token takes an existing app over from the App controller and carries a new
# app out through AgentLink (ADR-027, M1.9), reports its
# status and restarts it through a run; then deletes
# and garbage collection.
#
# With the public path of scripts/e2e-gateway.sh (KUBEN_E2E_GATEWAY_CLASS and
# friends, M2.4): Kuben creates its own Gateway; apps of both delivery paths
# answer over HTTPS from the runner, outside the cluster network, with a
# certificate the test CA signed, and plain HTTP is redirected. M2.5: with
# Kuben stopped (and its database, when KUBEN_E2E_PG_IMAGE names the
# container's image), a rescheduled app pod still serves over HTTPS.
# With KUBEN_E2E_AGENT_IMAGE (M2.8), the agent runs as a pod from the chart's
# template, enrolls from the Secret Kuben publishes and keeps its identity in
# a Secret of its own; KUBEN_E2E_HUB is the address pods reach Kuben at.
set -euo pipefail

# Runs from any directory (`turbo run e2e` starts it in crates/kuben).
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN=${KUBEN_BIN:-$ROOT/target/debug/kuben}
AGENT_BIN=${KUBEN_AGENT_BIN:-$ROOT/target/debug/kuben-agent}
DATABASE_URL=${KUBEN_E2E_DATABASE_URL:?set KUBEN_E2E_DATABASE_URL to an empty PostgreSQL database, e.g. postgres://postgres:kuben@localhost:5432/postgres}
PORT=${KUBEN_E2E_PORT:-18080}
BASE="http://127.0.0.1:${PORT}/api/v1"
AGENT_PORT=$((PORT + 2))
PASSWORD="e2e-$(date +%s)-password"
IMAGE=${KUBEN_E2E_IMAGE:-nginxinc/nginx-unprivileged:1.27-alpine}
JOB_IMAGE=${KUBEN_E2E_JOB_IMAGE:-busybox:1.36}
P=e2e
LEASE_NS=${KUBEN_E2E_LEASE_NAMESPACE:-default}
NS="kb-${P}-dev"
NS_LIVE="kb-${P}-live"
APP="/projects/${P}/environments/dev/apps"
# The public path (scripts/e2e-gateway.sh); empty: the HTTPS steps are skipped.
GATEWAY_CLASS=${KUBEN_E2E_GATEWAY_CLASS:-}
BASE_DOMAIN=${KUBEN_E2E_BASE_DOMAIN:-e2e.test}
NODE_IP=${KUBEN_E2E_NODE_IP:-}
HTTP_NODE_PORT=${KUBEN_E2E_HTTP_NODE_PORT:-30080}
HTTPS_NODE_PORT=${KUBEN_E2E_HTTPS_NODE_PORT:-30443}
AGENT_IMAGE=${KUBEN_E2E_AGENT_IMAGE:-}
AGENT_NS=kuben-system

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
  if [[ -n ${agent_pid:-} ]]; then kill "$agent_pid" 2>/dev/null || true; wait "$agent_pid" 2>/dev/null || true; fi
  if [[ -n ${pid:-} ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  if ((status != 0)); then
    if [[ -z ${failed:-} ]]; then
      # A command failed under `set -e` (e.g. a rollout timeout): name the step
      # and attach the end of the server log, newlines encoded for the annotation.
      annotate "exit ${status}; last kuben log lines:%0A$(tail -n 15 "$work/kuben.log" 2>/dev/null | sed 's/%/%25/g' | awk '{ printf "%s%%0A", $0 }')"
    fi
    echo "---- kuben log (last 80 lines) ----"
    tail -n 80 "$work/kuben.log" || true
    if [[ -s $work/agent.log ]]; then
      echo "---- kuben-agent log (last 40 lines) ----"
      tail -n 40 "$work/agent.log" || true
    fi
    kubectl get projects,environments,apps,applicationruntimes -A 2>/dev/null || true
    kubectl -n "$NS" get all,resourcequota,networkpolicy,pvc,cronjobs,jobs 2>/dev/null || true
  fi
  kubectl delete environment "${P}-dev" "${P}-live" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete project "$P" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$LEASE_NS" delete lease kuben-controller --ignore-not-found >/dev/null 2>&1 || true
  if [[ -n $AGENT_IMAGE ]]; then
    if ((status != 0)); then
      echo "---- agent pod log (last 40 lines) ----"
      kubectl -n "$AGENT_NS" logs deploy/kuben-agent --tail=40 2>/dev/null || true
    fi
    # Everything the chart's template made, the cluster-wide role included:
    # the chart is installed on this cluster next.
    if [[ -s $work/agent.yaml ]]; then
      kubectl -n "$AGENT_NS" delete -f "$work/agent.yaml" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
    kubectl -n "$AGENT_NS" delete secret/kuben-agent-identity secret/kuben-agent-enrollment \
      --ignore-not-found >/dev/null 2>&1 || true
  fi
  if [[ -n $GATEWAY_CLASS ]]; then
    kubectl delete kubenconfig kuben --ignore-not-found >/dev/null 2>&1 || true
    kubectl -n kuben-system delete gateway kuben --ignore-not-found >/dev/null 2>&1 || true
  fi
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

# deploy <app> <Idempotency-Key or ""> <json> → HTTP status; headers in $work/headers
deploy() {
  local app=$1 key=$2 body=$3
  local args=(-sS -o "$work/body" -D "$work/headers" -w '%{http_code}' -X POST
    -H 'content-type: application/json' -H 'x-kuben-client: e2e'
    -b "$work/cookies" -c "$work/cookies" --data "$body")
  [[ -n $key ]] && args+=(-H "idempotency-key: $key")
  curl "${args[@]}" "$BASE$APP/$app/deployments"
}

# run_succeeded <app> <run>: the deployment run has succeeded
run_succeeded() {
  [[ $(curl -fsS -b "$work/cookies" "$BASE$APP/$1/deployments/$2" | jq -r .phase) == succeeded ]]
}

# https_ok <host>: HTTP 200 over HTTPS through the Gateway's NodePort, from
# the runner, with a certificate chain the test CA signed.
https_ok() {
  curl -fsS -o /dev/null --max-time 5 --cacert "$KUBEN_E2E_GATEWAY_CA" \
    --resolve "$1:${HTTPS_NODE_PORT}:${NODE_IP}" "https://$1:${HTTPS_NODE_PORT}/"
}

# app_host <app>: the app's first hostname, as the API reports it.
app_host() {
  curl -fsS -b "$work/cookies" "$BASE$APP/$1" | jq -r '.app.exposure.hosts[0].host // empty'
}

# served_over_https <app>: the route is accepted, the certificate issued, the
# URL is https, and the Gateway answers from outside the cluster.
served_over_https() {
  local app=$1 host
  eventually 180 "${app}: route accepted and certificate issued" bash -c \
    "curl -fsS -b '$work/cookies' $BASE$APP/$app | jq -e '.app.exposure.routed == true and (.app.exposure.hosts[0].certificate_ready == true)'"
  host=$(app_host "$app")
  [[ -n $host ]] || fail "${app} has no hostname"
  expect 200 GET "$APP/$app"
  [[ $(jq -r .app.url "$work/body") == "https://${host}" ]] || fail "${app} URL: $(jq -r .app.url "$work/body")"
  eventually 120 "https://${host} answers through the Gateway" https_ok "$host"
  local got
  got=$(curl -sS -o /dev/null --max-time 5 -w '%{http_code} %{redirect_url}' \
    --resolve "${host}:${HTTP_NODE_PORT}:${NODE_IP}" "http://${host}:${HTTP_NODE_PORT}/")
  [[ $got =~ ^301\ https://${host}(:443)?/$ ]] || fail "plain HTTP for ${host} is not redirected to HTTPS: ${got}"
}

# doctor_status <app> <check id> [subject]: that check's status in the app's
# Doctor ("absent" when it has none); the report stays in $work/body.
doctor_status() {
  expect 200 GET "$APP/$1/doctor"
  jq -r --arg id "$2" --arg subject "${3:-}" \
    '[.checks[] | select(.id == $id and ($subject == "" or .subject == $subject)) | .status][0] // "absent"' "$work/body"
}

start_kuben() {
  local agent_env=()
  if [[ -n $AGENT_IMAGE ]]; then
    agent_env=(KUBEN_AGENT__LOCAL=true "KUBEN_AGENT__ADVERTISE=${KUBEN_E2E_HUB:?the address pods reach Kuben at}"
      "KUBEN_AGENT__NAMESPACE=${AGENT_NS}")
  fi
  env "${agent_env[@]}" \
    KUBEN_SERVER__BIND="127.0.0.1:${PORT}" \
    KUBEN_SERVER__METRICS_BIND="127.0.0.1:$((PORT + 1))" \
    KUBEN_DATABASE__URL="$DATABASE_URL" \
    KUBEN_BOOTSTRAP__ADMIN_PASSWORD="$PASSWORD" \
    KUBEN_SECURITY__COOKIE_SECURE=false \
    KUBEN_KUBE__REQUIRED=true \
    KUBEN_KUBE__LEADER_ELECTION=true \
    KUBEN_KUBE__NAMESPACE="$LEASE_NS" \
    KUBEN_TELEMETRY__LOG_FORMAT=pretty \
    KUBEN_SERVER__STATE_DIR="$work/state" \
    KUBEN_AGENT__BIND="${AGENT_BIND:-127.0.0.1}:${AGENT_PORT}" \
    "$BIN" serve --roles=all >>"$work/kuben.log" 2>&1 &
  pid=$!
}

step "start kuben"
# Agent pods reach the hub on the runner through the cluster network's gateway.
[[ -n $AGENT_IMAGE ]] && AGENT_BIND=0.0.0.0
start_kuben
eventually 60 "CRDs applied" kubectl get crd apps.kuben.dev
eventually 30 "controller lease held" bash -c \
  "kubectl -n $LEASE_NS get lease kuben-controller -o jsonpath='{.spec.holderIdentity}' | grep -q ."
# Ready only once every informer has listed (projections complete).
eventually 90 "readyz" curl -fsS "http://127.0.0.1:${PORT}/readyz"

if [[ -n $GATEWAY_CLASS ]]; then
  step "M2.4: Kuben creates and owns its Gateway"
  [[ -n $NODE_IP && -s ${KUBEN_E2E_GATEWAY_CA:-} ]] || fail "run scripts/e2e-gateway.sh first (KUBEN_E2E_NODE_IP, KUBEN_E2E_GATEWAY_CA)"
  kubectl apply -f - >/dev/null <<YAML
apiVersion: kuben.dev/v1alpha1
kind: KubenConfig
metadata: { name: kuben }
spec:
  baseDomain: ${BASE_DOMAIN}
  gatewayClassName: ${GATEWAY_CLASS}
  clusterIssuer: ${KUBEN_E2E_CLUSTER_ISSUER}
  # Traefik matches listeners to its entry points.
  gatewayPorts: { http: 8000, https: 8443 }
YAML
  eventually 120 "KubenConfig reports the Gateway programmed" bash -c \
    "kubectl get kubenconfig kuben -o jsonpath='{.status.conditions[?(@.type==\"Gateway\")].status}' | grep -qx True"
  kubectl -n kuben-system get gateway kuben -o jsonpath='{.metadata.labels.kuben\.dev/gateway-owner}' | grep -qx kuben ||
    fail "Kuben's Gateway is not labelled as Kuben's"
  grep -q "cluster capabilities" "$work/kuben.log" || fail "capability discovery did not run"

  # M2.9: what a BYOK operator runs before installing (no database needed).
  step "M2.9: kuben doctor --cluster reports every feature ready"
  "$BIN" doctor --cluster >"$work/doctor.txt" 2>&1 || fail "doctor --cluster failed: $(cat "$work/doctor.txt")"
  for feature in "public routes" "HTTPS" "volumes"; do
    grep -q "^\[OK  \] feature: ${feature}: " "$work/doctor.txt" || fail "doctor: ${feature} not ready: $(cat "$work/doctor.txt")"
  done
  grep -q '^\[OK  \] permissions: everything Kuben needs' "$work/doctor.txt" || fail "doctor: $(cat "$work/doctor.txt")"
fi

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

if [[ -n $GATEWAY_CLASS ]]; then
  step "M2.4: web answers over HTTPS from outside the cluster"
  served_over_https web

  step "M2.13: Doctor explains web's exposure"
  web_host=$(app_host web)
  for id in gateway-class gateway issuer route; do
    [[ $(doctor_status web "$id") == ok ]] || fail "doctor ${id}: $(cat "$work/body")"
  done
  [[ $(doctor_status web certificate "$web_host") == ok ]] || fail "doctor certificate: $(cat "$work/body")"
  # ${BASE_DOMAIN} has no DNS record: a failure with a hint, so the report
  # is not ok; nothing that could not be checked reads as ok.
  [[ $(doctor_status web dns "$web_host") == fail ]] || fail "doctor dns: $(cat "$work/body")"
  jq -e '.status == "fail" and all(.checks[]; .status == "ok" or .hint != null or .status == "unknown")' \
    "$work/body" >/dev/null || fail "doctor report: $(cat "$work/body")"
  [[ $(doctor_status web agent) == absent ]] || fail "an app the controller delivers has no agent check"
fi

step "logs"
expect 200 GET "$APP/web/logs?tail=20"
jq -e 'length == 1 and .[0].error == null' "$work/body" >/dev/null || fail "logs: $(cat "$work/body")"

step "M2.12: followed logs: capped per user, alive past the request timeout; events"
follow() { # <file> <seconds>: follow web's log in the background
  curl -sS -N --max-time "$2" -b "$work/cookies" -o "$1" -w '%{http_code}' \
    "$BASE$APP/web/logs?follow=true&tail=1" >"$1.status" &
}
followers=()
for i in 1 2 3 4; do
  follow "$work/follow-$i.txt" 60
  followers+=($!)
done
eventually 20 "four followed logs open" bash -c "grep -l '^event: line' $work/follow-[1-4].txt | wc -l | grep -qx 4"
fifth=$(curl -sS -o "$work/body" -w '%{http_code}' --max-time 10 -b "$work/cookies" "$BASE$APP/web/logs?follow=true")
[[ $fifth == 429 ]] || fail "a fifth followed log of one user → HTTP ${fifth} (want 429)"
kill "${followers[@]}" 2>/dev/null || true
wait "${followers[@]}" 2>/dev/null || true
# The server lets a place go at its next write to the closed connection
# (a keep-alive every 15 seconds at the latest).
sleep 20
follow "$work/follow.txt" 50
follower=$!
sleep 35 # longer than the request timeout of the REST routes
kubectl -n "$NS" exec deploy/web-web -- wget -qO- http://127.0.0.1:8080/e2e-follow-marker >/dev/null 2>&1 || true
eventually 30 "the new line in the followed log" grep -q 'e2e-follow-marker' "$work/follow.txt"
kill "$follower" 2>/dev/null || true
wait "$follower" 2>/dev/null || true
grep '^data:' "$work/follow.txt" | grep 'e2e-follow-marker' | sed 's/^data://' |
  jq -e '.pod | startswith("web-web-")' >/dev/null || fail "followed line: $(tail -n 5 "$work/follow.txt")"
expect 200 GET "$APP/web/events"
jq -e 'any(.[]; .kind == "Pod" and .reason == "Scheduled") and any(.[]; .kind == "Deployment" and .name == "web-web")' \
  "$work/body" >/dev/null || fail "events: $(cat "$work/body")"
# Web's own objects, and the certificates of its hosts beside the Gateway.
jq -e 'all(.[]; (.name | startswith("web")) or (.kind == "Certificate" and (.name | startswith("kuben-tls-"))))' \
  "$work/body" >/dev/null || fail "events of other objects: $(cat "$work/body")"

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

step "deploy by digest through /deployments (ADR-032)"
# The app's image as the materializer wrote it: the tag resolved to a digest.
pinned=$(kubectl -n "$NS" get app web -o jsonpath='{.spec.source.image}')
[[ $pinned == *@sha256:* ]] || fail "the App image is not pinned by digest: $pinned"
expect 200 GET "$APP/web/releases"
gen=$(jq -r '.[0].revision' "$work/body")
body="{\"image\":\"${pinned}\",\"expected_generation\":${gen}}"
key="e2e-deploy-$(date +%s)"
got=$(deploy web "$key" "$body")
[[ $got == 202 ]] || fail "deploy → HTTP $got: $(cat "$work/body")"
run=$(jq -r .run "$work/body")
[[ $(jq -r .generation "$work/body") == $((gen + 1)) ]] || fail "run generation: $(cat "$work/body")"
grep -qiE "^location: /api/v1${APP}/web/deployments/${run}"$'\r?$' "$work/headers" || fail "Location: $(cat "$work/headers")"
got=$(deploy web "$key" "$body")
[[ $got == 202 && $(jq -r .run "$work/body") == "$run" ]] || fail "a replayed key must return the first run → HTTP $got: $(cat "$work/body")"
got=$(deploy web "" "$body")
[[ $got == 409 ]] || fail "a stale expected generation must be refused → HTTP $got: $(cat "$work/body")"
eventually 180 "run ${run} succeeded" run_succeeded web "$run"
kubectl -n "$NS" get app web -o jsonpath='{.metadata.annotations.kuben\.dev/generation}' | grep -qx "$((gen + 1))" ||
  fail "App generation annotation"

step "rollback through /deployments"
got=$(deploy web "" "{\"image\":\"${pinned}\",\"reason\":\"rollback\",\"expected_generation\":$((gen + 1))}")
[[ $got == 202 ]] || fail "rollback deploy → HTTP $got: $(cat "$work/body")"
run=$(jq -r .run "$work/body")
eventually 180 "rollback run ${run} succeeded" run_succeeded web "$run"
expect 200 GET "$APP/web/releases"
[[ $(jq -r '.[0].reason' "$work/body") == rollback ]] || fail "rollback not in history: $(cat "$work/body")"

step "a direct App edit is drift: replaced from SQL"
kubectl -n "$NS" patch app web --type merge -p '{"spec":{"source":{"image":"evil.example.com/web:latest"}}}' >/dev/null
eventually 60 "edited image replaced" bash -c "[[ \$(kubectl -n $NS get app web -o jsonpath='{.spec.source.image}') == '$pinned' ]]"
kubectl -n "$NS" rollout status deployment/web-web --timeout=180s

step "M2.14: the kuben client: login, apps, status, logs, deploy, rollback"
expect 201 POST /tokens "{\"name\":\"cli\",\"role\":\"developer\",\"project\":\"${P}\"}"
cli_token=$(jq -r .token "$work/body")
cli_token_id=$(jq -r .info.id "$work/body")
cli() { KUBEN_CONTEXT_FILE="$work/contexts.json" "$BIN" "$@"; }
printf '%s\n' "$cli_token" | cli login "http://127.0.0.1:${PORT}" --name e2e --project "$P" --environment dev >"$work/cli.txt" ||
  fail "kuben login: $(cat "$work/cli.txt")"
[[ $(stat -c %a "$work/contexts.json" 2>/dev/null || stat -f %Lp "$work/contexts.json") == 600 ]] ||
  fail "the context file is readable by others"
grep -q "$cli_token" "$work/cli.txt" && fail "kuben login printed the token"
eventually 60 "app ready after drift fix" bash -c "curl -fsS -b '$work/cookies' $BASE$APP/web | jq -e '.app.ready'"
cli apps | grep -qE "^${P}/dev/web +ready" || fail "kuben apps: $(cli apps 2>&1)"
cli status web --json | jq -e '.app.ready and (.doctor.checks | length > 0)' >/dev/null ||
  fail "kuben status: $(cli status web --json 2>&1)"
cli logs web --tail 5 | grep -q . || fail "kuben logs printed nothing"
cli deploy web --image "$IMAGE" --timeout 240 >"$work/cli.txt" 2>&1 || fail "kuben deploy: $(cat "$work/cli.txt")"
grep -q "web: deployed" "$work/cli.txt" || fail "kuben deploy: $(cat "$work/cli.txt")"
before=$(cli status web --json | jq -r '[.releases[] | select(.current)][0].revision')
cli rollback web >"$work/cli.txt" || fail "kuben rollback: $(cat "$work/cli.txt")"
after=$(cli status web --json | jq -r '[.releases[] | select(.current)][0].revision')
((after > before)) || fail "kuben rollback made no new revision: ${before} → ${after}"
cli status web --json | jq -e '.releases[0].reason == "rollback"' >/dev/null ||
  fail "kuben rollback is not in the history: $(cli status web --json 2>&1)"
expect 204 DELETE "/tokens/${cli_token_id}"
cli apps >/dev/null 2>&1 && fail "a revoked token still works for the client"

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

# Last among the app scenarios: once the cluster's agent is linked, every new
# target of the cluster is delivered by it (the earlier apps stay with the
# App controller).
step "M1.9: an enrolled agent carries a new app out (ADR-027)"
KUBEN_SERVER__STATE_DIR="$work/state" KUBEN_DATABASE__URL="$DATABASE_URL" \
  "$BIN" agent-token --cluster primary >"$work/agent-token.out"
cluster=$(sed -n 's/^cluster: *[^ ]* (\([^)]*\))$/\1/p' "$work/agent-token.out")
(
  umask 077
  sed -n 's/^token: *//p' "$work/agent-token.out" >"$work/token"
)
[[ -n $cluster && -s $work/token ]] || fail "agent-token: $(cat "$work/agent-token.out")"
if [[ -n $AGENT_IMAGE ]]; then
  # M2.8: the agent as a pod from the chart's own template; no token copied.
  helm template kuben "$ROOT/charts/kuben" --namespace "$AGENT_NS" --show-only templates/agent.yaml \
    --set image.repository="${AGENT_IMAGE%:*}" --set image.tag="${AGENT_IMAGE##*:}" --set image.pullPolicy=Never \
    >"$work/agent.yaml"
  kubectl -n "$AGENT_NS" apply -f "$work/agent.yaml" >/dev/null
  eventually 120 "enrollment published with a token" bash -c \
    "kubectl -n $AGENT_NS get secret kuben-agent-enrollment -o jsonpath='{.data.token}' | grep -q ."
  [[ $(kubectl -n "$AGENT_NS" get secret kuben-agent-enrollment -o jsonpath='{.data.cluster}' | base64 -d) == "$cluster" ]] ||
    fail "the enrollment names another cluster"
  eventually 180 "agent linked" grep -q "agent linked" "$work/kuben.log"
  kubectl -n "$AGENT_NS" get secret kuben-agent-identity -o jsonpath='{.data.agent\.crt}' | grep -q . ||
    fail "the agent's certificate is not in its Secret"
  eventually 90 "the spent token is withdrawn" bash -c \
    "! kubectl -n $AGENT_NS get secret kuben-agent-enrollment -o jsonpath='{.data.token}' | grep -q ."
  # A new pod keeps the identity: it links again without a token.
  links=$(grep -c "agent linked" "$work/kuben.log")
  kubectl -n "$AGENT_NS" delete pod -l app.kubernetes.io/name=kuben-agent --wait=true --timeout=60s >/dev/null
  eventually 180 "a new agent pod links again with its stored identity" bash -c \
    "(( \$(grep -c 'agent linked' '$work/kuben.log') > $links ))"
  kubectl -n "$AGENT_NS" logs deploy/kuben-agent | grep -q "identity restored from its Secret" ||
    fail "the new pod did not restore its identity"
else
  if [[ ! -x $AGENT_BIN ]]; then
    cargo build --package kuben-agent --locked --quiet --manifest-path "$ROOT/Cargo.toml"
  fi
  [[ -x $AGENT_BIN ]] || fail "no agent binary at $AGENT_BIN"
  "$AGENT_BIN" --hub "127.0.0.1:${AGENT_PORT}" --hub-ca "$work/state/agentlink/ca.crt" --cluster "$cluster" \
    --state-dir "$work/agent" --token-file "$work/token" --log-format pretty >"$work/agent.log" 2>&1 &
  agent_pid=$!
  eventually 60 "agent linked" grep -q "agent linked" "$work/kuben.log"
fi
# An app the App controller delivers moves to the agent on request: its App
# object goes, and the agent adopts the same workloads (no new Deployment).
web_uid=$(kubectl -n "$NS" get deployment web-web -o jsonpath='{.metadata.uid}')
expect 202 POST "$APP/web/handover"
eventually 180 "web runtime ready" kubectl -n "$NS" wait --for=condition=Ready applicationruntime/web --timeout=5s
eventually 60 "web App object gone" bash -c "! kubectl -n $NS get app web"
[[ $(kubectl -n "$NS" get deployment web-web -o jsonpath='{.metadata.uid}') == "$web_uid" ]] ||
  fail "web-web was made again instead of adopted"
owner=$(kubectl -n "$NS" get deployment web-web -o jsonpath='{.metadata.ownerReferences[*].kind}')
[[ $owner == ApplicationRuntime ]] || fail "web-web is owned by '$owner' after the handover"
expect 409 POST "$APP/web/handover"
eventually 60 "web ready via API after the handover" bash -c "curl -fsS -b '$work/cookies' $BASE$APP/web | jq -e '.app.ready'"
expect 201 POST "$APP" "{\"name\":\"edge\",\"image\":\"${IMAGE}\",\"port\":8080}"
eventually 180 "edge runtime ready" kubectl -n "$NS" wait --for=condition=Ready applicationruntime/edge --timeout=5s
kubectl -n "$NS" get app edge >/dev/null 2>&1 && fail "an agent-delivered app has no App object"
kubectl -n "$NS" rollout status deployment/edge-web --timeout=180s
owner=$(kubectl -n "$NS" get deployment edge-web -o jsonpath='{.metadata.ownerReferences[0].kind}')
[[ $owner == ApplicationRuntime ]] || fail "edge-web is owned by '$owner', not its ApplicationRuntime"
# Its status comes from the agent's report (no App object), and a restart is a
# run of the same release that stamps the pod template.
eventually 60 "edge ready via API" bash -c "curl -fsS -b '$work/cookies' $BASE$APP/edge | jq -e '.app.ready'"
[[ $(doctor_status edge agent) == ok ]] || fail "doctor agent: $(cat "$work/body")"
before=$(kubectl -n "$NS" get deployment edge-web -o jsonpath='{.metadata.generation}')
expect 202 POST "$APP/edge/restart"
eventually 90 "edge restart rolled out" bash -c "[[ \$(kubectl -n $NS get deployment edge-web -o jsonpath='{.metadata.generation}') -gt $before ]]"
kubectl -n "$NS" get deployment edge-web -o jsonpath='{.spec.template.metadata.annotations.kuben\.dev/restarted-at}' |
  grep -q . || fail "edge-web has no restart stamp"
kubectl -n "$NS" rollout status deployment/edge-web --timeout=180s
eventually 90 "edge ready again via API" bash -c "curl -fsS -b '$work/cookies' $BASE$APP/edge | jq -e '.app.ready'"
expect 200 GET "$APP/edge/releases"
jq -e '.[0].reason == "restart"' "$work/body" >/dev/null || fail "restart missing from the history: $(cat "$work/body")"

if [[ -n $GATEWAY_CLASS ]]; then
  # The agent wrote this route: its hosts still get a listener and a certificate.
  step "M2.4: an agent-delivered app answers over HTTPS too"
  served_over_https edge

  step "M2.5: with Kuben down, a rescheduled pod keeps serving"
  edge_host=$(app_host edge)
  kill "$pid"
  wait "$pid" 2>/dev/null || true
  pid=""
  pg_container=""
  if [[ -n ${KUBEN_E2E_PG_IMAGE:-} ]]; then
    pg_container=$(docker ps -q --filter "ancestor=${KUBEN_E2E_PG_IMAGE}" | head -n1)
    [[ -n $pg_container ]] || fail "no running container of ${KUBEN_E2E_PG_IMAGE}"
    docker stop "$pg_container" >/dev/null
  fi
  # The only replica goes: the app is down until its successor is Ready,
  # which Kubernetes schedules without Kuben.
  old_pod=$(kubectl -n "$NS" get pods -l kuben.dev/app=edge -o jsonpath='{.items[0].metadata.name}')
  kubectl -n "$NS" delete pod "$old_pod" --timeout=120s >/dev/null
  kubectl -n "$NS" wait pod -l kuben.dev/app=edge --for=condition=Ready --timeout=180s >/dev/null
  eventually 60 "https://${edge_host} answers from the rescheduled pod" https_ok "$edge_host"
  for _ in 1 2 3; do
    https_ok "$edge_host" || fail "https://${edge_host} stopped answering while Kuben was down"
    sleep 1
  done
  if [[ -n $pg_container ]]; then
    docker start "$pg_container" >/dev/null
    eventually 60 "database back" docker exec "$pg_container" pg_isready -U postgres
  fi
  links=$(grep -c "agent linked" "$work/kuben.log")
  start_kuben
  eventually 90 "readyz after the restart" curl -fsS "http://127.0.0.1:${PORT}/readyz"
  # The agent's reconnect backoff doubles up to five minutes.
  eventually 330 "agent linked again" bash -c "(( \$(grep -c 'agent linked' '$work/kuben.log') > $links ))"
  expect 200 GET /me
fi
expect 204 DELETE "$APP/edge"
eventually 90 "edge runtime gone" bash -c "! kubectl -n $NS get applicationruntime edge"
eventually 90 "edge deployment gone" bash -c "! kubectl -n $NS get deployment edge-web"

step "M4.11: export, detach and release"
expect 201 POST "$APP" "{\"name\":\"keep\",\"image\":\"${IMAGE}\",\"port\":8080}"
eventually 180 "keep delivered (its export exists)" bash -c \
  "curl -fsS -b '$work/cookies' $BASE$APP/keep/export | jq -e '.format == \"kuben.dev/export/v1\"'"
eventually 180 "keep-web running" kubectl -n "$NS" rollout status deployment/keep-web --timeout=10s
expect 200 GET "$APP/keep/export"
jq -e '[.manifests.items[].kind] | index("Deployment") != null' "$work/body" >/dev/null ||
  fail "the export has no Deployment: $(cat "$work/body")"
expect 422 POST "$APP/keep/detach" '{"confirm":"other","reason":"e2e"}'
expect 202 POST "$APP/keep/detach" '{"confirm":"keep","reason":"e2e"}'
detached=$(jq -r .id "$work/body")
eventually 120 "detach complete" bash -c \
  "curl -fsS -b '$work/cookies' $BASE/projects/${P}/environments/dev/detached/${detached} | jq -e '.completedAt != null'"
kubectl -n "$NS" get app keep >/dev/null 2>&1 && fail "the App of a detached app is still there"
kubectl -n "$NS" get applicationruntime keep >/dev/null 2>&1 && fail "the runtime of a detached app is still there"
kubectl -n "$NS" get deployment keep-web -o json | jq -e '(.metadata.ownerReferences // []) == []' >/dev/null ||
  fail "the detached Deployment still has an owner"
sleep 5
kubectl -n "$NS" rollout status deployment/keep-web --timeout=30s || fail "the detached app stopped running"
expect 404 GET "$APP/keep"
expect 204 POST "/projects/${P}/environments/dev/detached/${detached}/release"
expect 409 POST "/projects/${P}/environments/dev/detached/${detached}/release"

step "M4.11: a local support bundle without secrets"
KUBEN_DATABASE__URL="$DATABASE_URL" KUBEN_KUBE__NAMESPACE="$LEASE_NS" KUBEN_SERVER__STATE_DIR="$work/state" \
  "$BIN" support-bundle --out "$work/support" >"$work/support.txt" 2>&1 || fail "support-bundle: $(cat "$work/support.txt")"
bundle=$(ls "$work"/support/kuben-support-*.json)
[[ $(stat -c %a "$bundle") == 600 ]] || fail "the support bundle is not private"
grep -qF "$PASSWORD" "$bundle" && fail "the support bundle holds the admin password"
jq -e '.about.support_envelope.profile == "supported-mvp" and (.database.summary.organizations >= 1)' "$bundle" >/dev/null ||
  fail "the support bundle lacks the envelope or the database summary"

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
