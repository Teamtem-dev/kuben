#!/usr/bin/env bash
# The public path for the end-to-end test (M2.4, ADR-031): Gateway API CRDs
# (standard channel), Traefik as the Gateway provider on fixed NodePorts,
# cert-manager with Gateway API support, and a ClusterIssuer backed by a
# throwaway CA. Kuben then creates its own Gateway, and scripts/e2e.sh checks
# HTTPS from the runner, outside the cluster network (no port-forward).
#
#   scripts/e2e-gateway.sh        # current kube context; idempotent
#
# Prints the variables scripts/e2e.sh reads (also appended to $GITHUB_ENV
# when set). The CA certificate is written to $KUBEN_E2E_GATEWAY_CA.
set -euo pipefail

GATEWAY_API_VERSION=${GATEWAY_API_VERSION:-v1.5.1}
TRAEFIK_CHART_VERSION=${TRAEFIK_CHART_VERSION:-41.5.0}
CERT_MANAGER_VERSION=${CERT_MANAGER_VERSION:-v1.21.2}
HTTP_NODE_PORT=${KUBEN_E2E_HTTP_NODE_PORT:-30080}
HTTPS_NODE_PORT=${KUBEN_E2E_HTTPS_NODE_PORT:-30443}
ISSUER=e2e-ca
CA_FILE=${KUBEN_E2E_GATEWAY_CA:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/kuben-e2e-gateway-ca.crt}

need() { command -v "$1" >/dev/null || { echo "missing: $1" >&2; exit 2; }; }
for c in kubectl helm curl; do need "$c"; done

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
dump() { # <namespace>
  {
    kubectl -n "$1" get deploy,pods -o wide || true
    kubectl -n "$1" get events --sort-by=.lastTimestamp | tail -30 || true
    for d in $(kubectl -n "$1" get deploy -o name); do kubectl -n "$1" logs "$d" --all-containers --tail=30 || true; done
  } >&2
}
available() { # <namespace>
  kubectl -n "$1" wait deploy --all --for=condition=Available --timeout=5m >/dev/null ||
    { dump "$1"; echo "the $1 release is not Available" >&2; exit 1; }
}
# retry <tries> <command...>: webhooks answer a moment after they are Available.
retry() {
  local tries=$1
  shift
  for ((i = 1; i <= tries; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  "$@"
}

say "Gateway API ${GATEWAY_API_VERSION} (standard channel)"
kubectl apply --server-side -f \
  "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" >/dev/null

say "Traefik ${TRAEFIK_CHART_VERSION} as the Gateway provider, on NodePorts ${HTTP_NODE_PORT}/${HTTPS_NODE_PORT}"
# No Gateway from the chart: Kuben creates its own (KubenConfig.gatewayClassName).
helm upgrade --install traefik traefik --repo https://traefik.github.io/charts --version "$TRAEFIK_CHART_VERSION" \
  --namespace traefik --create-namespace \
  --set providers.kubernetesGateway.enabled=true \
  --set gateway.enabled=false --set gatewayClass.enabled=true \
  --set service.type=NodePort \
  --set ports.web.nodePort="$HTTP_NODE_PORT" --set ports.websecure.nodePort="$HTTPS_NODE_PORT" \
  --timeout 5m >/dev/null

say "cert-manager ${CERT_MANAGER_VERSION} with Gateway API support"
# startupapicheck is a post-install hook that helm blocks on; the retried
# apply below checks the same thing with a visible error.
helm upgrade --install cert-manager cert-manager --repo https://charts.jetstack.io --version "$CERT_MANAGER_VERSION" \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true --set startupapicheck.enabled=false \
  --set config.apiVersion=controller.config.cert-manager.io/v1alpha1 \
  --set config.kind=ControllerConfiguration --set config.enableGatewayAPI=true \
  --timeout 5m >/dev/null
available traefik
available cert-manager

say "ClusterIssuer ${ISSUER} (a throwaway CA)"
issuers=$(mktemp)
trap 'rm -f "$issuers"' EXIT
cat >"$issuers" <<YAML
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: { name: e2e-selfsigned }
spec: { selfSigned: {} }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: ${ISSUER}, namespace: cert-manager }
spec:
  isCA: true
  commonName: kuben e2e gateway CA
  secretName: ${ISSUER}
  privateKey: { algorithm: ECDSA, size: 256 }
  issuerRef: { name: e2e-selfsigned, kind: ClusterIssuer }
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: { name: ${ISSUER} }
spec: { ca: { secretName: ${ISSUER} } }
YAML
retry 30 kubectl apply -f "$issuers" || { dump cert-manager; exit 1; }
kubectl wait clusterissuer "$ISSUER" --for=condition=Ready --timeout=120s >/dev/null ||
  { dump cert-manager; echo "ClusterIssuer ${ISSUER} is not Ready" >&2; exit 1; }
kubectl -n cert-manager get secret "$ISSUER" -o jsonpath='{.data.ca\.crt}' | base64 -d >"$CA_FILE"

# Kuben's own Gateway lives here (KubenConfig default kuben-system/kuben).
kubectl create namespace kuben-system --dry-run=client -o yaml | kubectl apply -f - >/dev/null

node_ip=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
vars=(
  "KUBEN_E2E_GATEWAY_CLASS=traefik"
  "KUBEN_E2E_CLUSTER_ISSUER=${ISSUER}"
  "KUBEN_E2E_GATEWAY_CA=${CA_FILE}"
  "KUBEN_E2E_NODE_IP=${node_ip}"
  "KUBEN_E2E_HTTP_NODE_PORT=${HTTP_NODE_PORT}"
  "KUBEN_E2E_HTTPS_NODE_PORT=${HTTPS_NODE_PORT}"
)
printf '%s\n' "${vars[@]}"
if [[ -n ${GITHUB_ENV:-} ]]; then printf '%s\n' "${vars[@]}" >>"$GITHUB_ENV"; fi
printf '\n\033[32mGATEWAY STACK READY\033[0m\n'
