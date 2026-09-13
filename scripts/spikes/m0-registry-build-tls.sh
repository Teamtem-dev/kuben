#!/usr/bin/env bash
# M0 spikes (ADR-028, ADR-029, ADR-031) against the current kube context.
# Built for a disposable kind cluster; the Kuben API is never started, so
# every check below proves a path that does not depend on it.
#
#   1. A registry with its own auth (Zot: htpasswd + per-repository policy),
#      reachable and trusted by the node's container runtime.
#   2. A rootless BuildKit Job builds and pushes with a push-only credential.
#   3. A Pod pulls the image by digest using only a namespace pull secret.
#   4. A pull credential for one repository cannot read another.
#   5. A build over its memory limit fails cleanly; the node stays Ready.
#   6. Gateway API + Traefik + cert-manager serve the app over TLS.
#
# Requires: kubectl, helm, docker (to reach the kind node), openssl,
# htpasswd (apache2-utils), curl.
#
#   KIND_NODE=kuben-spikes-control-plane scripts/spikes/m0-registry-build-tls.sh

set -euo pipefail

NS=kuben-m0
KIND_NODE=${KIND_NODE:-kuben-spikes-control-plane}
REG_HOST=registry.kuben-m0.test
REG_PORT=30500
REG="${REG_HOST}:${REG_PORT}"
APP_HOST=app.kuben-m0.test
ZOT_IMAGE=${ZOT_IMAGE:-ghcr.io/project-zot/zot-linux-amd64:v2.1.19}
BUILDKIT_IMAGE=${BUILDKIT_IMAGE:-moby/buildkit:v0.33.0-rootless}
GATEWAY_API_VERSION=${GATEWAY_API_VERSION:-v1.5.1}
CERT_MANAGER_VERSION=${CERT_MANAGER_VERSION:-v1.21.0}

work=$(mktemp -d)
pf_pid=""
cleanup() {
  if [[ -n $pf_pid ]]; then
    kill "$pf_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

say() { printf '\n== %s\n' "$*"; }
pass() { printf '✔ %s\n' "$*"; }
fail() {
  printf '✘ %s\n' "$*" >&2
  kubectl -n "$NS" get pods -o wide >&2 || true
  kubectl -n "$NS" get events --sort-by=.lastTimestamp >&2 | tail -30 || true
  exit 1
}
k() { kubectl -n "$NS" "$@"; }

wait_job() { # <job> <condition: Complete|Failed> <timeout>
  kubectl -n "$NS" wait "job/$1" --for="condition=$2" --timeout="$3" >/dev/null
}

# A brand-new Deployment has no conditions yet; wait for Available itself,
# not for `rollout status`, which can return before the pods serve.
wait_available() { # <deployment> <timeout>
  kubectl -n "$NS" wait "deploy/$1" --for=condition=Available --timeout="$2" >/dev/null
}

# Retry a command until it succeeds: NodePort rules and EndpointSlices lag
# a ready Pod by a moment.
retry() { # <attempts> <command...>
  local attempts=$1
  shift
  for _ in $(seq 1 "$attempts"); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

say "Namespace and node access"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
node_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$KIND_NODE")
[[ -n $node_ip ]] || fail "cannot find the IP of kind node $KIND_NODE"
pass "kind node $KIND_NODE at $node_ip"

say "1. Registry with its own TLS and auth"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=kuben m0 spike CA" \
  -keyout "$work/ca.key" -out "$work/ca.crt" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=$REG_HOST" \
  -keyout "$work/reg.key" -out "$work/reg.csr" 2>/dev/null
printf 'subjectAltName=DNS:%s\nextendedKeyUsage=serverAuth\n' "$REG_HOST" >"$work/reg.ext"
openssl x509 -req -in "$work/reg.csr" -CA "$work/ca.crt" -CAkey "$work/ca.key" -CAcreateserial \
  -days 2 -extfile "$work/reg.ext" -out "$work/reg.crt" 2>/dev/null

pw() { openssl rand -hex 16; }
push_a=$(pw)
pull_a=$(pw)
pull_b=$(pw)
{
  htpasswd -Bbn push-a "$push_a"
  htpasswd -Bbn pull-a "$pull_a"
  htpasswd -Bbn pull-b "$pull_b"
} >"$work/htpasswd"

cat >"$work/zot.json" <<'JSON'
{
  "storage": { "rootDirectory": "/var/lib/registry" },
  "http": {
    "address": "0.0.0.0",
    "port": "5000",
    "tls": { "cert": "/tls/tls.crt", "key": "/tls/tls.key" },
    "auth": { "htpasswd": { "path": "/auth/htpasswd" } },
    "accessControl": {
      "repositories": {
        "tenant-a/**": {
          "policies": [
            { "users": ["push-a"], "actions": ["read", "create", "update"] },
            { "users": ["pull-a"], "actions": ["read"] }
          ]
        },
        "tenant-b/**": {
          "policies": [{ "users": ["pull-b"], "actions": ["read"] }]
        }
      }
    }
  },
  "log": { "level": "info" }
}
JSON

k create secret tls zot-tls --cert="$work/reg.crt" --key="$work/reg.key" --dry-run=client -o yaml | k apply -f - >/dev/null
k create secret generic zot-auth --from-file=htpasswd="$work/htpasswd" --dry-run=client -o yaml | k apply -f - >/dev/null
k create configmap zot-config --from-file=config.json="$work/zot.json" --dry-run=client -o yaml | k apply -f - >/dev/null

k apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: { name: zot }
spec:
  replicas: 1
  selector: { matchLabels: { app: zot } }
  template:
    metadata: { labels: { app: zot } }
    spec:
      automountServiceAccountToken: false
      containers:
        - name: zot
          image: $ZOT_IMAGE
          args: ["serve", "/etc/zot/config.json"]
          ports: [{ containerPort: 5000 }]
          readinessProbe:
            tcpSocket: { port: 5000 }
          volumeMounts:
            - { name: config, mountPath: /etc/zot }
            - { name: tls, mountPath: /tls }
            - { name: auth, mountPath: /auth }
            - { name: data, mountPath: /var/lib/registry }
      volumes:
        - { name: config, configMap: { name: zot-config } }
        - { name: tls, secret: { secretName: zot-tls } }
        - { name: auth, secret: { secretName: zot-auth } }
        - { name: data, emptyDir: {} }
---
apiVersion: v1
kind: Service
metadata: { name: zot }
spec:
  type: NodePort
  selector: { app: zot }
  ports: [{ port: 5000, targetPort: 5000, nodePort: $REG_PORT }]
YAML
wait_available zot 180s || fail "registry did not become ready"

# The node's container runtime (not the Pod network) must resolve and trust
# the registry: node-level DNS plus a containerd hosts.toml with the CA.
docker exec "$KIND_NODE" sh -c "grep -q ' $REG_HOST\$' /etc/hosts || echo '$node_ip $REG_HOST' >>/etc/hosts"
docker exec "$KIND_NODE" mkdir -p "/etc/containerd/certs.d/$REG"
docker cp "$work/ca.crt" "$KIND_NODE:/etc/containerd/certs.d/$REG/ca.crt"
printf 'server = "https://%s"\n\n[host."https://%s"]\n  capabilities = ["pull", "resolve"]\n  ca = "/etc/containerd/certs.d/%s/ca.crt"\n' \
  "$REG" "$REG" "$REG" >"$work/hosts.toml"
docker cp "$work/hosts.toml" "$KIND_NODE:/etc/containerd/certs.d/$REG/hosts.toml"

reg_curl() { # <user:pass> <path> [extra curl args...]
  local cred=$1 path=$2
  shift 2
  curl -sS --cacert "$work/ca.crt" --resolve "$REG:$node_ip" -u "$cred" "$@" "https://$REG$path"
}
retry 30 reg_curl "pull-a:$pull_a" /v2/ --fail -o /dev/null ||
  fail "registry NodePort $REG_PORT on $node_ip never answered /v2/ with valid credentials"
code=$(reg_curl "pull-a:$pull_a" /v2/ -o /dev/null -w '%{http_code}')
[[ $code == 200 ]] || fail "registry /v2/ with valid credentials answered $code"
code=$(curl -sS --cacert "$work/ca.crt" --resolve "$REG:$node_ip" -o /dev/null -w '%{http_code}' "https://$REG/v2/")
[[ $code == 401 ]] || fail "anonymous /v2/ answered $code, expected 401"
pass "registry serves TLS from a node-resolvable name and requires auth"

say "2. Rootless BuildKit Job builds and pushes with a push-only credential"
docker_config() { # <user> <pass>
  printf '{"auths":{"%s":{"auth":"%s"}}}' "$REG" "$(printf '%s:%s' "$1" "$2" | base64 | tr -d '\n')"
}
docker_config push-a "$push_a" >"$work/push.json"
k create secret generic push-a --from-file=config.json="$work/push.json" --dry-run=client -o yaml | k apply -f - >/dev/null
k create configmap registry-ca --from-file=ca.crt="$work/ca.crt" --dry-run=client -o yaml | k apply -f - >/dev/null
printf '[registry."%s"]\n  ca = ["/etc/registry-ca/ca.crt"]\n' "$REG" >"$work/buildkitd.toml"
k create configmap buildkitd --from-file=buildkitd.toml="$work/buildkitd.toml" --dry-run=client -o yaml | k apply -f - >/dev/null
cat >"$work/Dockerfile" <<'DOCKERFILE'
FROM busybox:1.37
RUN mkdir -p /www && echo "kuben m0 ok" >/www/index.html
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
DOCKERFILE
k create configmap build-context --from-file=Dockerfile="$work/Dockerfile" --dry-run=client -o yaml | k apply -f - >/dev/null

build_job() { # <name> <memory limit>
  k delete job "$1" --ignore-not-found >/dev/null
  k apply -f - >/dev/null <<YAML
apiVersion: batch/v1
kind: Job
metadata: { name: $1 }
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 600
  ttlSecondsAfterFinished: 600
  template:
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      hostAliases: [{ ip: "$node_ip", hostnames: ["$REG_HOST"] }]
      containers:
        - name: buildkit
          image: $BUILDKIT_IMAGE
          command: ["sh", "-c"]
          args:
            - >-
              buildctl-daemonless.sh build
              --frontend dockerfile.v0
              --local context=/workspace --local dockerfile=/workspace
              --output type=image,name=$REG/tenant-a/app:m0,push=true
              --metadata-file /tmp/metadata.json
              && cat /tmp/metadata.json
          env:
            - { name: BUILDKITD_FLAGS, value: "--oci-worker-no-process-sandbox --config /etc/buildkit/buildkitd.toml" }
            - { name: DOCKER_CONFIG, value: /docker }
          securityContext:
            runAsUser: 1000
            runAsGroup: 1000
            seccompProfile: { type: Unconfined }
            appArmorProfile: { type: Unconfined }
          resources:
            requests: { cpu: 250m, memory: $2 }
            limits: { memory: $2, ephemeral-storage: 2Gi }
          volumeMounts:
            - { name: context, mountPath: /workspace }
            - { name: buildkitd, mountPath: /etc/buildkit }
            - { name: docker, mountPath: /docker }
            - { name: ca, mountPath: /etc/registry-ca }
            - { name: state, mountPath: /home/user/.local/share/buildkit }
      volumes:
        - { name: context, configMap: { name: build-context } }
        - { name: buildkitd, configMap: { name: buildkitd } }
        - { name: docker, secret: { secretName: push-a } }
        - { name: ca, configMap: { name: registry-ca } }
        - { name: state, emptyDir: {} }
YAML
}

build_job build-ok 1Gi
wait_job build-ok Complete 600s || fail "rootless build did not complete"
digest=$(k logs job/build-ok | grep -o '"containerimage.digest": *"sha256:[0-9a-f]*"' | grep -o 'sha256:[0-9a-f]*' | tail -1)
[[ -n $digest ]] || fail "no image digest in the build metadata"
pass "rootless BuildKit pushed $REG/tenant-a/app@$digest"

accept='Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
head_digest=$(reg_curl "pull-a:$pull_a" /v2/tenant-a/app/manifests/m0 -I -H "$accept" |
  tr -d '\r' | awk -F': ' 'tolower($1)=="docker-content-digest" {print $2}')
[[ $head_digest == "$digest" ]] || fail "registry digest $head_digest differs from build metadata $digest"
pass "registry reports the same digest the build recorded"

say "3. Pull by digest with only a namespace pull secret (Kuben is not running)"
docker_config pull-a "$pull_a" >"$work/pull.json"
k create secret generic pull-a --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson="$work/pull.json" --dry-run=client -o yaml | k apply -f - >/dev/null
k apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: { name: app }
spec:
  replicas: 1
  selector: { matchLabels: { app: m0-app } }
  template:
    metadata: { labels: { app: m0-app } }
    spec:
      automountServiceAccountToken: false
      imagePullSecrets: [{ name: pull-a }]
      containers:
        - name: app
          image: $REG/tenant-a/app@$digest
          ports: [{ containerPort: 8080 }]
          readinessProbe:
            httpGet: { path: /, port: 8080 }
---
apiVersion: v1
kind: Service
metadata: { name: app }
spec:
  selector: { app: m0-app }
  ports: [{ port: 80, targetPort: 8080 }]
YAML
wait_available app 180s || fail "the node could not pull by digest"
pass "the node pulled $REG/tenant-a/app@$digest with a namespace pull secret"

say "4. A pull credential for one repository cannot read another"
code=$(reg_curl "pull-b:$pull_b" /v2/tenant-a/app/manifests/m0 -o /dev/null -w '%{http_code}' -H "$accept")
[[ $code == 401 || $code == 403 ]] || fail "tenant-b's credential read tenant-a's image (HTTP $code)"
code=$(reg_curl "pull-a:$pull_a" /v2/tenant-b/app/manifests/m0 -o /dev/null -w '%{http_code}' -H "$accept")
[[ $code == 401 || $code == 403 || $code == 404 ]] || fail "tenant-a's credential reached tenant-b (HTTP $code)"
pass "per-repository scopes hold (cross-tenant reads refused)"

say "5. A build over its memory limit fails cleanly"
build_job build-oom 48Mi
if wait_job build-oom Failed 300s; then
  reason=$(k get pods -l job-name=build-oom -o jsonpath='{.items[0].status.containerStatuses[0].state.terminated.reason}')
  pass "the over-limit build failed (${reason:-terminated}) instead of hanging"
else
  fail "the over-limit build neither failed nor finished"
fi
kubectl wait node --all --for=condition=Ready --timeout=60s >/dev/null || fail "a node is not Ready after the OOM build"
wait_available zot 30s || fail "the registry did not survive the OOM build"
pass "node and registry stayed healthy"

say "6. Gateway API + Traefik + cert-manager serve the app over TLS"
kubectl apply --server-side -f \
  "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" >/dev/null
helm repo add traefik https://traefik.github.io/charts >/dev/null
helm repo add jetstack https://charts.jetstack.io >/dev/null
helm repo update >/dev/null
helm upgrade --install traefik traefik/traefik --namespace traefik --create-namespace \
  --set providers.kubernetesGateway.enabled=true --set gateway.enabled=false \
  --set service.type=ClusterIP --wait --timeout 5m >/dev/null
helm upgrade --install cert-manager jetstack/cert-manager --namespace cert-manager --create-namespace \
  --version "$CERT_MANAGER_VERSION" --set crds.enabled=true --wait --timeout 5m >/dev/null

k apply -f - >/dev/null <<YAML
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: selfsigned }
spec: { selfSigned: {} }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: spike-ca }
spec:
  isCA: true
  commonName: kuben m0 gateway CA
  secretName: spike-ca
  issuerRef: { name: selfsigned, kind: Issuer }
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: spike-ca }
spec: { ca: { secretName: spike-ca } }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: app-tls }
spec:
  secretName: app-tls
  dnsNames: ["$APP_HOST"]
  issuerRef: { name: spike-ca, kind: Issuer }
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: { name: kuben }
spec:
  gatewayClassName: traefik
  listeners:
    - name: https
      protocol: HTTPS
      port: 8443
      hostname: "$APP_HOST"
      tls: { mode: Terminate, certificateRefs: [{ name: app-tls }] }
      allowedRoutes: { namespaces: { from: Same } }
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: { name: app }
spec:
  parentRefs: [{ name: kuben, sectionName: https }]
  hostnames: ["$APP_HOST"]
  rules: [{ backendRefs: [{ name: app, port: 80 }] }]
YAML
k wait certificate/app-tls --for=condition=Ready --timeout=120s >/dev/null || fail "cert-manager did not issue app-tls"
k wait gateway/kuben --for=condition=Programmed --timeout=120s >/dev/null || fail "the Gateway was not programmed"
k wait httproute/app --timeout=120s \
  --for=jsonpath='{.status.parents[0].conditions[?(@.type=="Accepted")].status}'=True >/dev/null ||
  fail "the HTTPRoute was not accepted"
k get secret app-tls -o jsonpath='{.data.ca\.crt}' | base64 -d >"$work/gateway-ca.crt"

kubectl -n traefik port-forward svc/traefik 18443:443 >/dev/null 2>&1 &
pf_pid=$!
body=""
for _ in $(seq 1 30); do
  body=$(curl -sS --cacert "$work/gateway-ca.crt" --resolve "$APP_HOST:18443:127.0.0.1" \
    "https://$APP_HOST:18443/" 2>/dev/null || true)
  [[ $body == "kuben m0 ok" ]] && break
  sleep 2
done
[[ $body == "kuben m0 ok" ]] || fail "no verified TLS answer through the Gateway (got: ${body:-nothing})"
pass "https://$APP_HOST answered through Traefik's Gateway with a cert-manager certificate"

say "M0 registry, build and Gateway spikes passed"
