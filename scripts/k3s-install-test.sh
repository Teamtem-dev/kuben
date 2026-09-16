#!/usr/bin/env bash
# M2.10: `kuben setup` on a fresh Linux host with nothing installed (the
# k3s-install job in CI): a pinned k3s, Traefik as the Gateway provider, the
# Gateway API CRDs, cert-manager, the cluster agent, Kuben's own Gateway and
# the console behind it; then the first admin over that HTTPS address (never
# over plain HTTP from another machine), an app that answers over HTTPS
# through the Gateway, a second run that changes nothing and leaves other
# people's objects alone, and `kuben uninstall --purge` taking k3s away.
# Needs root through sudo and changes the host: run it on a throwaway machine.
#
#   KUBEN_BIN=target/debug/kuben KUBEN_AGENT_IMAGE=docker.io/library/kuben-agent:e2e \
#     scripts/k3s-install-test.sh
#
# KUBEN_AGENT_IMAGE names the agent image when it is not the release's (CI
# puts that image into k3s's airgap directory before setup runs).
set -euo pipefail
cd "$(dirname "$0")/.."

BIN=${KUBEN_BIN:-target/debug/kuben}
AGENT_IMAGE=${KUBEN_AGENT_IMAGE:-}
IMAGE=${KUBEN_E2E_IMAGE:-nginxinc/nginx-unprivileged:1.27-alpine}
DOMAIN=k3s.test
CONSOLE=kuben.${DOMAIN}
BASE="https://${CONSOLE}/api/v1"
JOURNAL=/var/lib/kuben/install/journal.json
work=$(mktemp -d)
current=setup

step() {
  current=$*
  printf '\n\033[1m==> %s\033[0m\n' "$*"
}
fail() {
  echo "FAIL (${current}): $*" >&2
  if [[ -n ${GITHUB_ACTIONS:-} ]]; then echo "::error title=k3s install: ${current}::$*"; fi
  exit 1
}
diagnose() {
  local status=$?
  if ((status != 0)); then
    echo "---- kuben.service ----"
    sudo journalctl -u kuben --no-pager -n 80 -o cat 2>/dev/null || true
    echo "---- cluster ----"
    sudo k3s kubectl get pods -A -o wide 2>/dev/null || true
    sudo k3s kubectl get gatewayclass,gateway,httproute,certificate,clusterissuer -A 2>/dev/null || true
    sudo k3s kubectl -n kuben-system logs deploy/kuben-agent --tail=30 2>/dev/null || true
    sudo journalctl -u k3s --no-pager -n 30 -o cat 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap diagnose EXIT

kubectl() { sudo k3s kubectl "$@"; }
# curl to the console through the Gateway on this host's port 443, trusting
# the test CA.
kcurl() { curl --resolve "${CONSOLE}:443:127.0.0.1" --cacert "$work/ca.crt" "$@"; }
export -f kcurl
journal() { sudo jq -r "$1" "$JOURNAL"; }
owner_of() { journal "[.resources[] | select(.kind == \"$1\" and .name == \"$2\") | .owner][0] // \"none\""; }
eventually() { # <seconds> <what> <command...>
  local timeout=$1 what=$2
  shift 2
  for ((i = 0; i < timeout; i += 2)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  fail "timed out after ${timeout}s waiting for: $what"
}
# api <method> <path> [json] → HTTP status; body in $work/body
api() {
  local args=(-sS -o "$work/body" -w '%{http_code}' -X "$1" -H 'content-type: application/json'
    -H 'x-kuben-client: k3s-test' -b "$work/cookies" -c "$work/cookies")
  [[ -n ${3:-} ]] && args+=(--data "$3")
  kcurl "${args[@]}" "$BASE$2"
}
expect() { # <status> <method> <path> [json]
  local want=$1 got
  shift
  got=$(api "$@")
  [[ $got == "$want" ]] || fail "$1 $2 → HTTP $got (want $want): $(cat "$work/body")"
}
setup() {
  sudo env ${AGENT_IMAGE:+KUBEN_AGENT_IMAGE="$AGENT_IMAGE"} "$1" setup --yes --domain "$DOMAIN" \
    --acme-email ops@example.com --acme-staging 2>&1 | tee "$work/setup.txt"
}

step "kuben setup on a host with nothing installed"
[[ ! -e /etc/rancher/k3s/k3s.yaml ]] || fail "k3s is already installed; this test needs a fresh host"
setup "$BIN"
[[ $(journal '.runs[-1].succeeded') == true ]] || fail "setup did not succeed: $(journal '.runs[-1]')"
sudo k3s --version | grep -q 'v1.36.4+k3s1' || fail "k3s is not the pinned release: $(sudo k3s --version)"
for resource in "cluster k3s" "kubernetesObject crd/gateway-api" "kubernetesObject helmchartconfig/kube-system/traefik" \
  "kubernetesObject helmchart/kube-system/kuben-cert-manager" "kubernetesObject namespace/kuben-system" \
  "kubernetesObject kubenconfig/kuben" "kubernetesObject agent/kuben-system/kuben-agent" \
  "kubernetesObject clusterissuer/letsencrypt" "kubernetesObject console/kuben-console/kuben-console"; do
  # shellcheck disable=SC2086
  [[ $(owner_of $resource) == kuben ]] || fail "$resource is not recorded as created by setup"
done

step "the platform: Traefik's Gateway, cert-manager, Kuben's Gateway and agent"
eventually 300 "GatewayClass traefik accepted" bash -c \
  "sudo k3s kubectl get gatewayclass traefik -o jsonpath='{.status.conditions[?(@.type==\"Accepted\")].status}' | grep -qx True"
eventually 300 "cert-manager webhook available" \
  sudo k3s kubectl -n cert-manager wait deploy/cert-manager-webhook --for=condition=Available --timeout=5s
[[ $(kubectl get kubenconfig kuben -o jsonpath='{.spec.gatewayClassName}') == traefik ]] || fail "KubenConfig has no gateway class"
eventually 180 "Kuben's Gateway programmed" bash -c \
  "sudo k3s kubectl get kubenconfig kuben -o jsonpath='{.status.conditions[?(@.type==\"Gateway\")].status}' | grep -qx True"
eventually 300 "the cluster agent linked" bash -c "sudo journalctl -u kuben --no-pager -o cat | grep -q 'agent linked'"
sudo -u kuben /usr/local/bin/kuben doctor 2>&1 | tee "$work/doctor.txt" >/dev/null || fail "doctor: $(cat "$work/doctor.txt")"
grep -q '^\[OK  \] feature: public routes: ready' "$work/doctor.txt" || fail "doctor: $(cat "$work/doctor.txt")"

step "the setup link: HTTPS, with the token after #"
grep -q "https://${CONSOLE}/setup#token=" "$work/setup.txt" || fail "no HTTPS setup link: $(cat "$work/setup.txt")"
grep -q 'ssh -L 3000:127.0.0.1:3000' "$work/setup.txt" || fail "no SSH tunnel for the time before DNS"
sudo grep -qx 'public_url = "https://kuben.k3s.test"' /etc/kuben/config.toml || fail "public_url is not the console's HTTPS address"
sudo grep -qx 'trusted_proxies = \["10.42.0.0/16"\]' /etc/kuben/config.toml || fail "the Gateway's network is not the trusted proxy"

step "HTTPS: an issuer, the console through the Gateway, the first admin, an app"
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: { name: k3s-selfsigned }
spec: { selfSigned: {} }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: k3s-ca, namespace: cert-manager }
spec:
  isCA: true
  commonName: kuben k3s test CA
  secretName: k3s-ca
  privateKey: { algorithm: ECDSA, size: 256 }
  issuerRef: { name: k3s-selfsigned, kind: ClusterIssuer }
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: { name: k3s-ca }
spec: { ca: { secretName: k3s-ca } }
YAML
kubectl wait clusterissuer k3s-ca --for=condition=Ready --timeout=120s >/dev/null
kubectl -n cert-manager get secret k3s-ca -o jsonpath='{.data.ca\.crt}' | base64 -d >"$work/ca.crt"
kubectl patch kubenconfig kuben --type merge -p '{"spec":{"clusterIssuer":"k3s-ca"}}' >/dev/null

hub=$(sudo sed -n 's/^advertise = "\(.*\):7443"$/\1/p' /etc/kuben/config.toml)
[[ -n $hub ]] || fail "no server address in the configuration"
token=$(sudo cat /var/lib/kuben/setup-token)
first_admin="{\"org_name\":\"K3s\",\"email\":\"owner@k3s.test\",\"password\":\"a-long-first-password\",\"token\":\"${token}\"}"
# Plain HTTP from another address: refused, whatever the token.
refused=$(curl -sS -o "$work/body" -w '%{http_code}' -X POST -H 'content-type: application/json' \
  -H 'x-kuben-client: k3s-test' -H 'x-forwarded-proto: https' --data "$first_admin" "http://${hub}:3000/api/v1/setup")
[[ $refused == 403 && $(jq -r .code "$work/body") == insecure_transport ]] ||
  fail "plain HTTP setup from ${hub} → HTTP ${refused}: $(cat "$work/body")"
eventually 300 "https://${CONSOLE} through the Gateway" kcurl -fsS -o /dev/null --max-time 5 "https://${CONSOLE}/"
kcurl -fsS "https://${CONSOLE}/" | grep -qi '<html' || fail "the console page is not served over HTTPS"
kcurl -fsS "$BASE/setup" | jq -e '.needed and .secure' >/dev/null || fail "GET /setup over HTTPS: $(kcurl -sS "$BASE/setup")"
expect 200 POST /setup "$first_admin"
expect 201 POST /projects '{"name":"shop","display_name":"Shop"}'
expect 201 POST /projects/shop/environments '{"name":"prod"}'
eventually 90 "environment visible" bash -c "kcurl -fsS -b '$work/cookies' $BASE/projects/shop/environments/prod"
expect 201 POST /projects/shop/environments/prod/apps "{\"name\":\"web\",\"image\":\"${IMAGE}\",\"port\":8080}"
eventually 300 "web routed with a certificate" bash -c \
  "kcurl -fsS -b '$work/cookies' $BASE/projects/shop/environments/prod/apps/web | jq -e '.app.ready and .app.exposure.routed == true and .app.exposure.hosts[0].certificate_ready == true'"
kubectl -n kb-shop-prod get applicationruntime web >/dev/null || fail "the new app was not delivered by the cluster agent"
host=$(kcurl -fsS -b "$work/cookies" "$BASE/projects/shop/environments/prod/apps/web" | jq -r '.app.exposure.hosts[0].host')
[[ $host == "web-shop-prod.${DOMAIN}" ]] || fail "unexpected host ${host}"
https_ok() { curl -fsS -o /dev/null --max-time 5 --cacert "$work/ca.crt" --resolve "${host}:443:127.0.0.1" "https://${host}/"; }
eventually 120 "https://${host} through the Gateway" https_ok
redirect=$(curl -sS -o /dev/null --max-time 5 -w '%{http_code} %{redirect_url}' --resolve "${host}:80:127.0.0.1" "http://${host}/")
[[ $redirect =~ ^301\ https://${host}(:443)?/$ ]] || fail "plain HTTP is not redirected: ${redirect}"

step "setup again changes nothing and leaves others' objects alone"
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: { name: someone-elses }
spec: { selfSigned: {} }
---
apiVersion: v1
kind: Namespace
metadata: { name: edge }
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: { name: edge, namespace: edge }
spec:
  gatewayClassName: traefik
  listeners: [{ name: web, protocol: HTTP, port: 8000 }]
YAML
before=$(kubectl get clusterissuer someone-elses -o jsonpath='{.metadata.resourceVersion}'):$(kubectl -n edge get gateway edge -o jsonpath='{.metadata.generation}')
setup /usr/local/bin/kuben
grep -q 'Nothing changed' "$work/setup.txt" || fail "the second run did not say it changed nothing"
[[ $(journal '[.runs[-1].steps[] | select(.result != "unchanged")] | length') == 0 ]] ||
  fail "the second run changed: $(journal '[.runs[-1].steps[] | select(.result != "unchanged")]')"
after=$(kubectl get clusterissuer someone-elses -o jsonpath='{.metadata.resourceVersion}'):$(kubectl -n edge get gateway edge -o jsonpath='{.metadata.generation}')
[[ $before == "$after" ]] || fail "setup touched objects it did not create: ${before} → ${after}"
https_ok || fail "the app stopped answering after the second run"
kcurl -fsS -o /dev/null -b "$work/cookies" "$BASE/me" || fail "the console stopped answering after the second run"

step "kuben uninstall --purge removes what setup made, k3s included"
sudo /usr/local/bin/kuben uninstall --purge --yes
sudo test ! -e /etc/rancher/k3s/k3s.yaml || fail "k3s stayed although kuben setup installed it"
! systemctl is-active --quiet k3s || fail "k3s is still running"
sudo test ! -e "$JOURNAL" || fail "the journal stayed"
! id kuben >/dev/null 2>&1 || fail "the system user kuben stayed"

printf '\n\033[32mK3S INSTALL PASSED\033[0m\n'
