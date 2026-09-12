#!/usr/bin/env bash
# Smoke test of what users install, the way the README tells them to: the
# one-line installer from main, then the published Helm chart and image pulled
# from GHCR without logging in. Everything else in CI builds from source; this
# checks what was actually published.
#
#   scripts/smoke-test.sh                               # latest release, current kube context
#   KUBEN_SMOKE_VERSION=1.0.3-rc.1 scripts/smoke-test.sh
#   KUBEN_SMOKE_K3S=1 scripts/smoke-test.sh             # fresh server: installs k3s first
#
# Self-contained, so it also runs on a throwaway server as is:
#   curl -fsSL https://raw.githubusercontent.com/Teamtem-dev/kuben/main/scripts/smoke-test.sh | KUBEN_SMOKE_K3S=1 bash
#
# Environment:
#   KUBEN_SMOKE_VERSION       release under test, e.g. 1.0.2 (default: the latest stable, as users get it)
#   KUBEN_SMOKE_UPGRADE_FROM  chart version to install first, then upgrade from; `previous` is the
#                             newest stable chart older than the one under test (default: no upgrade)
#   KUBEN_SMOKE_K3S=1         install k3s when no cluster is reachable (needs root or sudo)
#   KUBEN_SMOKE_KEEP=1        leave the release installed when the test passes
#   KUBEN_SMOKE_BIN_DIR       install the binary here without sudo (default: /usr/local/bin, as documented)
#   KUBEN_SMOKE_APP_IMAGE     image of the test app (default: nginxinc/nginx-unprivileged:1.27-alpine)
#
# Exercises: anonymous pulls of the chart and the image; the installer (checksum,
# version); `kuben doctor` without a cluster; `helm install` from OCI; the
# published image rolling out under the chart's own RBAC; health endpoints; the
# generated admin password and login; project → environment → app through the
# in-cluster controller, serving traffic; `kuben doctor` in the pod; an upgrade
# that keeps the data; deletes and garbage collection; `helm uninstall`.
#
# It changes the machine it runs on (the kuben binary, k3s when asked, the Helm
# release `kuben` in kuben-system) and refuses to replace an existing release:
# run it on a throwaway server or a CI runner.
set -euo pipefail

REPO=Teamtem-dev/kuben
INSTALLER="https://raw.githubusercontent.com/${REPO}/main/install.sh"
REGISTRY=ghcr.io
CHART_REPO=teamtem-dev/charts/kuben
IMAGE_REPO=teamtem-dev/kuben
CHART="oci://${REGISTRY}/${CHART_REPO}"
RELEASE=kuben
NS=kuben-system
PORT=${KUBEN_SMOKE_PORT:-18080}
APP_PORT=$((PORT + 1))
BASE="http://127.0.0.1:${PORT}/api/v1"
APP_IMAGE=${KUBEN_SMOKE_APP_IMAGE:-nginxinc/nginx-unprivileged:1.27-alpine}
P=smoke
APP_NS="kb-${P}-dev"
APP="/projects/${P}/environments/dev/apps"

VERSION=${KUBEN_SMOKE_VERSION:-}
VERSION=${VERSION#v}
FROM=${KUBEN_SMOKE_UPGRADE_FROM:-}
FROM=${FROM#v}

need() { command -v "$1" >/dev/null || { echo "missing: $1" >&2; exit 2; }; }
for c in curl jq helm; do need "$c"; done
helm_version=$(helm version --short 2>/dev/null || echo unknown)

work=$(mktemp -d "${TMPDIR:-/tmp}/kuben-smoke.XXXXXX")
# A stranger has no registry credentials: Helm, and the Docker config it falls
# back to, start empty.
export HELM_CONFIG_HOME="$work/helm/config" HELM_CACHE_HOME="$work/helm/cache" \
  HELM_DATA_HOME="$work/helm/data" DOCKER_CONFIG="$work/docker"

# In GitHub Actions a failure also becomes an annotation on the run page: the
# public checks API shows it, while the job log needs a signed-in user.
annotate() {
  if [[ -n ${GITHUB_ACTIONS:-} ]]; then
    local msg=$* pct='%' pct_enc='%25' nl=$'\n' nl_enc='%0A'
    msg=${msg//$pct/$pct_enc}
    msg=${msg//$nl/$nl_enc}
    echo "::error title=smoke: ${current:-setup}::${msg}"
  fi
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

stop_forward() {
  if [[ -n ${fwd:-} ]]; then kill "$fwd" 2>/dev/null || true; wait "$fwd" 2>/dev/null || true; fi
  fwd=
}

cleanup() {
  status=$?
  stop_forward
  if [[ -n ${app_fwd:-} ]]; then kill "$app_fwd" 2>/dev/null || true; fi
  if ((status != 0)); then
    if [[ -z ${failed:-} ]]; then annotate "exit ${status}"; fi
    if [[ -n ${cluster:-} ]]; then
      annotate "helm ${helm_version}; pods and the latest events in ${NS}:
$(kubectl -n "$NS" get pods -o wide 2>&1 | tail -n 5)
$(kubectl -n "$NS" get events --sort-by=.lastTimestamp 2>&1 | tail -n 8)"
      echo "---- diagnostics ----"
      helm -n "$NS" status "$RELEASE" 2>/dev/null || true
      kubectl -n "$NS" get all,pvc 2>/dev/null || true
      kubectl -n "$NS" get events --sort-by=.lastTimestamp 2>/dev/null | tail -n 30 || true
      kubectl -n "$NS" logs "deploy/${RELEASE}" --tail=100 2>/dev/null || true
      kubectl -n "$NS" logs "deploy/${RELEASE}" --previous --tail=50 2>/dev/null || true
      kubectl get projects,environments,apps -A 2>/dev/null || true
      kubectl -n "$APP_NS" get all 2>/dev/null || true
    fi
    if [[ -n ${installed:-} ]]; then
      echo "left installed for inspection; remove with:"
      echo "  helm -n ${NS} uninstall ${RELEASE} && kubectl delete namespace ${NS} ${APP_NS} --ignore-not-found"
    fi
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

# run <description> <command...>: run a command and, when it fails, put the
# end of its output into the failure (and so into the annotation).
run() {
  local what=$1
  shift
  if "$@" >"$work/cmd.log" 2>&1; then
    cat "$work/cmd.log"
    return 0
  fi
  cat "$work/cmd.log"
  fail "${what} failed:
$(tail -n 12 "$work/cmd.log")"
}

# api <method> <path> [json] → HTTP status; the body lands in $work/body.
api() {
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -o "$work/body" -w '%{http_code}' -X "$method" -b "$work/cookies" -c "$work/cookies"
    -H 'content-type: application/json' -H 'x-kuben-client: smoke')
  if [[ -n $body ]]; then args+=(--data "$body"); fi
  curl "${args[@]}" "$BASE$path"
}

expect() { # <status> <method> <path> [json]
  local want=$1 got
  shift
  got=$(api "$@")
  [[ $got == "$want" ]] || fail "$1 $2 → HTTP $got (want $want): $(cat "$work/body")"
}

# anonymous_tags <repository> <file>: the tags, fetched the way an anonymous
# `helm pull` or image pull does (a token without credentials, then the list).
anonymous_tags() {
  local repo=$1 out=$2 code token
  code=$(curl -sS -o "$work/token.json" -w '%{http_code}' "https://${REGISTRY}/token?scope=repository:${repo}:pull") ||
    fail "cannot reach ${REGISTRY}"
  case "$code" in
  200) ;;
  401 | 403) fail "${REGISTRY}/${repo} is not public (HTTP ${code} without credentials), so every user's pull fails: GitHub → Packages → ${repo#*/} → Package settings → Change visibility → Public" ;;
  *) fail "${REGISTRY} returned HTTP ${code} for a pull token on ${repo}" ;;
  esac
  token=$(jq -r .token "$work/token.json")
  curl -fsS -H "Authorization: Bearer ${token}" "https://${REGISTRY}/v2/${repo}/tags/list?n=1000" |
    jq -r '.tags[]' >"$out" || fail "cannot list the tags of ${REGISTRY}/${repo}"
}

stable() { grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' "$1" | sort -V; }

start_forward() {
  stop_forward
  kubectl -n "$NS" port-forward "svc/${RELEASE}" "${PORT}:80" >"$work/forward.log" 2>&1 &
  fwd=$!
  eventually 30 "port-forward to svc/${RELEASE}" curl -fsS "http://127.0.0.1:${PORT}/livez"
}

# verify <version>: the release is deployed from the published chart and
# image, healthy, and usable with the generated admin password.
verify() {
  local v=$1 image restarts password got
  step "release ${v}: chart, image, rollout"
  helm -n "$NS" list --filter "^${RELEASE}\$" -o json >"$work/release.json"
  jq -e --arg v "$v" '.[0] | .status == "deployed" and .chart == "kuben-\($v)" and .app_version == $v' \
    "$work/release.json" >/dev/null || fail "helm release: $(cat "$work/release.json")"
  run "rollout of deploy/${RELEASE}" kubectl -n "$NS" rollout status "deploy/${RELEASE}" --timeout=300s
  image=$(kubectl -n "$NS" get "deploy/${RELEASE}" -o jsonpath='{.spec.template.spec.containers[0].image}')
  [[ $image == "${REGISTRY}/${IMAGE_REPO}:${v}" ]] || fail "image ${image}, want ${REGISTRY}/${IMAGE_REPO}:${v}"
  restarts=$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${RELEASE}" \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}')
  if grep -qv '^0$' <<<"$restarts"; then fail "the kuben container restarted: ${restarts//$'\n'/ }"; fi

  step "release ${v}: health, admin login"
  start_forward
  eventually 120 "readyz" curl -fsS "http://127.0.0.1:${PORT}/readyz"
  eventually 60 "Secret kuben-initial-admin" kubectl -n "$NS" get secret kuben-initial-admin
  password=$(kubectl -n "$NS" get secret kuben-initial-admin -o jsonpath='{.data.password}' | base64 -d)
  [[ -n $password ]] || fail "the generated admin password is empty"
  rm -f "$work/cookies"
  expect 200 POST /auth/login "$(jq -nc --arg p "$password" '{email: "admin@kuben.local", password: $p}')"
  got=$(api GET /me)
  [[ $got == 200 ]] || fail "login worked but GET /me → HTTP ${got}: the session cookie did not come back"
  [[ $(jq -r .email "$work/body") == admin@kuben.local ]] || fail "unexpected /me: $(cat "$work/body")"

  step "release ${v}: kuben doctor in the pod"
  kubectl -n "$NS" exec "deploy/${RELEASE}" -- /kuben doctor | tee "$work/doctor-pod.txt" ||
    fail "kuben doctor failed in the pod"
  grep -q '^\[OK  \] database' "$work/doctor-pod.txt" || fail "doctor in the pod: database not OK"
  grep -q '^\[OK  \] kubernetes' "$work/doctor-pod.txt" || fail "doctor in the pod: cluster not OK"
}

step "anonymous pulls from ${REGISTRY}"
anonymous_tags "$CHART_REPO" "$work/chart-tags"
anonymous_tags "$IMAGE_REPO" "$work/image-tags"
pinned=${VERSION:+1}
if [[ -z $VERSION ]]; then
  # What `helm install` without --version picks: the newest stable chart.
  VERSION=$(stable "$work/chart-tags" | tail -n 1)
  [[ -n $VERSION ]] || fail "no stable chart on ${CHART}"
fi
if [[ $FROM == previous ]]; then
  # The newest stable chart older than the one under test (for 1.1.0-rc.1: 1.0.x).
  core=${VERSION%%-*}
  FROM=$({ stable "$work/chart-tags"; echo "$core"; } | sort -V -u | awk -v c="$core" '$0 == c { print prev; exit } { prev = $0 }')
  if [[ -z $FROM ]]; then echo "no stable chart older than ${VERSION}: skipping the upgrade"; fi
fi
for v in "$VERSION" ${FROM:+"$FROM"}; do
  grep -qxF "$v" "$work/chart-tags" || fail "chart ${v} is not published on ${CHART}"
  grep -qxF "$v" "$work/image-tags" || fail "image ${REGISTRY}/${IMAGE_REPO}:${v} is not published"
done
echo "chart and image are public; testing ${VERSION}${FROM:+, upgraded from ${FROM}}"

step "one-line installer"
if [[ -n $pinned ]]; then export KUBEN_VERSION="$VERSION"; else unset KUBEN_VERSION; fi
if [[ -n ${KUBEN_SMOKE_BIN_DIR:-} ]]; then
  curl -fsSL "$INSTALLER" | bash -s -- --dir "$KUBEN_SMOKE_BIN_DIR" --no-sudo 2>&1 | tee "$work/install.txt"
  bin="${KUBEN_SMOKE_BIN_DIR}/kuben"
else
  curl -fsSL "$INSTALLER" | bash 2>&1 | tee "$work/install.txt"
  bin=$(command -v kuben) || fail "kuben is not on PATH after the install"
fi
# What the user sees must say so: checksum verified, then the installed version.
grep -q '^kuben: sha256 verified: [0-9a-f]\{64\}$' "$work/install.txt" || fail "the installer did not report a verified checksum"
grep -q "^kuben: installed kuben ${VERSION} to " "$work/install.txt" || fail "the installer did not report kuben ${VERSION} as installed"
got=$("$bin" --version)
if [[ $got != "kuben ${VERSION}" ]]; then
  [[ -n $pinned ]] || fail "the installer's latest release (${got}) and the newest chart (${VERSION}) differ"
  fail "the installer gave '${got}', want 'kuben ${VERSION}'"
fi

step "kuben doctor without a cluster"
# A clean environment: no kubeconfig, not in a pod, a scratch database.
if ! env -u KUBERNETES_SERVICE_HOST KUBECONFIG="$work/no-kubeconfig" \
  KUBEN_DATABASE__URL="sqlite://${work}/doctor.db" "$bin" doctor >"$work/doctor.txt" 2>&1; then
  cat "$work/doctor.txt"
  fail "kuben doctor failed without a cluster (want warnings only)"
fi
cat "$work/doctor.txt"
grep -q '^\[OK  \] database' "$work/doctor.txt" || fail "doctor: database not OK"
grep -q '^\[WARN\] kubernetes: no cluster found' "$work/doctor.txt" || fail "doctor: no setup-mode warning"

step "cluster"
if ! { command -v kubectl >/dev/null && kubectl version --request-timeout=5s >/dev/null 2>&1; }; then
  [[ ${KUBEN_SMOKE_K3S:-} == 1 ]] || fail "no cluster reachable: set KUBECONFIG, or KUBEN_SMOKE_K3S=1 to install k3s"
  step "install k3s"
  # As on a fresh server; the kubeconfig is made readable for kubectl and helm.
  curl -sfL https://get.k3s.io | K3S_KUBECONFIG_MODE=644 sh -
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  need kubectl
  eventually 120 "k3s node registered" bash -c 'kubectl get nodes -o name | grep -q .'
  kubectl wait --for=condition=Ready nodes --all --timeout=120s
fi
cluster=1
kubectl get nodes -o wide
if helm -n "$NS" status "$RELEASE" >/dev/null 2>&1; then
  fail "a Helm release ${RELEASE} already exists in ${NS}; this test would replace it: use a throwaway cluster"
fi

step "helm install from ${CHART}, no registry login"
args=(install "$RELEASE" "$CHART" --namespace "$NS" --create-namespace --wait --timeout 6m)
# The README's command has no --version: helm then picks the newest stable chart.
if [[ -n $pinned || -n $FROM ]]; then args+=(--version "${FROM:-$VERSION}"); fi
# Charts up to 1.0.3 render `KubenConfig.spec: null` with default values, which
# the API server rejects; an upgrade test starts them without the KubenConfig,
# and the upgrade then creates it.
if [[ -n $FROM && $(printf '%s\n' "$FROM" 1.0.3 | sort -V | head -n 1) == "$FROM" ]]; then
  args+=(--set platform.create=false)
fi
installed=1
run "helm install" helm "${args[@]}"
verify "${FROM:-$VERSION}"

step "project → environment → app through the in-cluster controller"
expect 201 POST /projects "{\"name\":\"${P}\",\"display_name\":\"Smoke\"}"
eventually 30 "project visible" curl -fsS -b "$work/cookies" "$BASE/projects/${P}"
eventually 60 "project ready" bash -c "kubectl get project ${P} -o jsonpath='{.status.conditions[0].status}' | grep -qx True"
expect 201 POST "/projects/${P}/environments" '{"name":"dev"}'
eventually 90 "namespace ${APP_NS}" kubectl get namespace "$APP_NS"
eventually 90 "environment ready" bash -c "kubectl get environment ${P}-dev -o jsonpath='{.status.phase}' | grep -qx Ready"
eventually 30 "environment visible" curl -fsS -b "$work/cookies" "$BASE/projects/${P}/environments/dev"
expect 201 POST "$APP" "{\"name\":\"web\",\"image\":\"${APP_IMAGE}\",\"port\":8080,\"health_check_path\":\"/\"}"
eventually 60 "deployment created" kubectl -n "$APP_NS" get deployment web-web
run "rollout of the test app" kubectl -n "$APP_NS" rollout status deployment/web-web --timeout=240s
eventually 90 "app ready via API" bash -c "curl -fsS -b '$work/cookies' $BASE$APP/web | jq -e .app.ready"
kubectl -n "$APP_NS" port-forward svc/web "${APP_PORT}:80" >"$work/app-forward.log" 2>&1 &
app_fwd=$!
eventually 30 "the app answers through its Service" curl -fsS "http://127.0.0.1:${APP_PORT}/"
kill "$app_fwd" 2>/dev/null || true
app_fwd=

if [[ -n $FROM ]]; then
  step "helm upgrade ${FROM} → ${VERSION}"
  stop_forward
  run "helm upgrade" helm upgrade "$RELEASE" "$CHART" --namespace "$NS" --version "$VERSION" --wait --timeout 6m
  # The login inside verify already proves the database survived: a lost
  # volume would bootstrap a different admin password.
  verify "$VERSION"
  step "the app survived the upgrade"
  eventually 120 "app ready after the upgrade" bash -c "curl -fsS -b '$work/cookies' $BASE$APP/web | jq -e .app.ready"
fi

step "delete app, environment, project → garbage collection"
expect 204 DELETE "$APP/web"
eventually 90 "deployment gone" bash -c "! kubectl -n $APP_NS get deployment web-web"
expect 202 DELETE "/projects/${P}/environments/dev"
eventually 180 "namespace ${APP_NS} gone" bash -c "! kubectl get namespace $APP_NS"
eventually 60 "project has no environments" bash -c "curl -fsS -b '$work/cookies' $BASE/projects/${P}/environments | jq -e 'length == 0'"
expect 204 DELETE "/projects/${P}"

if [[ ${KUBEN_SMOKE_KEEP:-} == 1 ]]; then
  echo "kept the release; remove with: helm -n ${NS} uninstall ${RELEASE} && kubectl delete namespace ${NS}"
else
  step "helm uninstall"
  stop_forward
  run "helm uninstall" helm uninstall "$RELEASE" --namespace "$NS" --wait --timeout 3m
  eventually 90 "kuben pods gone" bash -c "[[ -z \$(kubectl -n $NS get pods -l app.kubernetes.io/instance=${RELEASE} -o name) ]]"
  # The chart promises that uninstalling never deletes the user and audit database.
  kubectl -n "$NS" get pvc "${RELEASE}-data" >/dev/null || fail "helm uninstall deleted the database volume"
  # The volume and the generated admin Secret go with the namespace. The
  # KubenConfig is kept too (since 1.0.3); Helm never deletes the CRDs in
  # crds/ (every app would go with them).
  kubectl delete namespace "$NS" --timeout=120s
  kubectl delete kubenconfigs.kuben.dev "$RELEASE" --ignore-not-found
fi
installed=

summary="kuben ${VERSION}${FROM:+ (upgraded from ${FROM})} on $(kubectl version -o json | jq -r .serverVersion.gitVersion)"
if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then echo "Smoke test passed: ${summary}" >>"$GITHUB_STEP_SUMMARY"; fi
printf '\n\033[32mSMOKE PASSED\033[0m: %s\n' "$summary"
