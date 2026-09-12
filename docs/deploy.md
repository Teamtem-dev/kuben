# Installing and running Kuben in production

## 1. Prerequisites

| Item | Requirement | Notes |
|---|---|---|
| Kubernetes | ≥ 1.29 | k3s, kind, EKS, GKE or AKS |
| StorageClass with `ReadWriteOnce` | required (with SQLite) | users, sessions, releases and the audit log live on a 1 GiB PVC; apps with volumes also use it |
| Gateway API CRDs + a Gateway controller | for public app access | Traefik v3, Envoy Gateway or Cilium |
| cert-manager | for HTTPS | run with Gateway API support: `--set config.enableGatewayAPI=true` |
| metrics-server | for autoscaling | the HPA does not work without it; k3s ships it by default |
| A CNI that enforces NetworkPolicy | for tenant isolation | Calico and Cilium do; plain flannel does not |

`kuben doctor` checks all of the above.

## 2. Install with Helm

```bash
helm install kuben oci://ghcr.io/teamtem-dev/charts/kuben \
  --namespace kuben-system --create-namespace \
  --set publicUrl=https://kuben.example.com \
  --set platform.baseDomain=apps.example.com \
  --set platform.gateway=kuben-system/kuben \
  --set platform.clusterIssuer=letsencrypt
```

Important values (`charts/kuben/values.yaml`):

| Value | Default | Purpose |
|---|---|---|
| `admin.email` | `admin@kuben.local` | initial admin email |
| `admin.existingSecret` | empty | Secret with a `password` key. If empty, a password is generated on first start and stored in the Secret `kuben-initial-admin`; it is **never** written to the log |
| `database.url` / `database.existingSecret` | SQLite on a PVC | PostgreSQL. Required for `replicaCount > 1` (the chart **fails** without it); switches to rolling updates and drops the PVC |
| `persistence.size` | `1Gi` | the PVC carries `helm.sh/resource-policy: keep` and survives `helm uninstall` |
| `security.cookieSecure` | `true` | `__Host-` cookie with `Secure`. Set to false only for local development over HTTP |
| `platform.*` | empty | the `KubenConfig` singleton: `baseDomain`, `gateway`, `clusterIssuer` |
| `route.enabled` | `false` | publishes Kuben's own UI through an HTTPRoute |

The Kuben pod runs:
- as a non-root user (UID 65532)
- with a `readOnlyRootFilesystem`
- with all capabilities dropped
- with seccomp set to `RuntimeDefault`
- with the `Recreate` strategy on SQLite (its volume is RWO), and rolling updates on PostgreSQL

The chart sets `KUBEN_SECURITY__TRUST_FORWARDED_FOR=true`, because Kuben sits behind the Gateway, which appends the client address.

A pod reports ready only after its informers have listed every Project, Environment, App and Pod once, so it never answers `404` for objects that exist.

### Several replicas

With PostgreSQL, `replicaCount` can be raised:

- Every replica serves the API and the UI and keeps its own read models.
- The controllers run only on the replica holding the `kuben-controller` Lease in Kuben's namespace ([ADR-023](adr/0023-controller-leader-election.md)). A replica that shuts down releases the Lease and another takes over within seconds; after a crash, within 15 seconds.
- Upgrades roll one pod at a time (`maxUnavailable: 0`). A PodDisruptionBudget stops node drains from evicting more than one pod.
- Login throttling is stored in the database, so all replicas share one budget.
- A revoked session can stay valid on another replica for up to `KUBEN_SECURITY__SESSION_CACHE_TTL_SECS` (5 s by default).

### Running the binary on a server

The one-line installer puts a single `kuben` binary on a machine. It manages a cluster through a kubeconfig and keeps its own data in SQLite.

1. Get a cluster to manage. On a single server, k3s is the quickest. It writes its kubeconfig for root only, so give your user a copy:
   ```bash
   curl -sfL https://get.k3s.io | sh -
   ```
   ```bash
   sudo install -D -m 600 -o "$USER" /etc/rancher/k3s/k3s.yaml ~/.kube/config && export KUBECONFIG=~/.kube/config
   ```
2. Check the prerequisites:
   ```bash
   kuben doctor
   ```
   The `database` line shows where the data lives: `~/.local/state/kuben/kuben.db` by default (systemd's state directory under `StateDirectory=`), or `/data/kuben.db` on a server that already has `/data`. To keep it elsewhere, set `KUBEN_DATABASE__URL`. Running Kuben as a systemd service is described in the [binary guide](https://kuben.teamtem.com/docs/getting-started/binary/#keep-it-running).
3. Start Kuben. The first start prints the generated admin password once, in this terminal (never in the log):
   ```bash
   kuben serve
   ```
4. The session cookie is `Secure`, so open the UI over HTTPS, or through an SSH tunnel to `localhost`:
   ```bash
   ssh -L 8080:localhost:8080 root@your-server
   ```
   Then browse to `http://localhost:8080`. Only for a quick test over plain HTTP, set `KUBEN_SECURITY__COOKIE_SECURE=false`.

For production, prefer the Helm install above: it runs Kuben inside the cluster with probes, RBAC and a persistent volume.

## 3. First sign-in

```bash
kubectl -n kuben-system get secret kuben-initial-admin -o jsonpath='{.data.password}' | base64 -d
```

Delete the `kuben-initial-admin` Secret once you have signed in and changed the password. A binary running outside Kubernetes prints the generated password to its terminal instead, or, without a terminal, writes it to `initial-admin-password` next to its database.

```bash
kubectl -n kuben-system port-forward svc/kuben 8080:80
```

Then open `http://localhost:8080`. Browsers treat `localhost` as a secure context, so the secure cookie works.

Invite your team from *Team* (see [the user guide](guide.md#4-work-as-a-team)). If the admin password is lost:

```bash
kubectl -n kuben-system exec deploy/kuben -- /kuben reset-admin
```

## 4. Configuration

Precedence, lowest to highest:
1. built-in defaults
2. `/etc/kuben/config.toml`
3. `./kuben.toml`
4. `KUBEN_*` environment variables — nested keys are separated by `__`

| Variable | Default |
|---|---|
| `KUBEN_SERVER__BIND` | `0.0.0.0:8080` |
| `KUBEN_SERVER__METRICS_BIND` | `0.0.0.0:9090` (Prometheus) |
| `KUBEN_DATABASE__URL` | `sqlite:///data/kuben.db` in the image and the chart; for the binary `/data/kuben.db` if `/data` exists, else `$STATE_DIRECTORY`, else `~/.local/state/kuben/kuben.db` |
| `KUBEN_KUBE__REQUIRED` | `false` (`true` in the chart) |
| `KUBEN_KUBE__NAMESPACE` | the pod's namespace. Home of the controller Lease and of `kuben-initial-admin` |
| `KUBEN_KUBE__LEADER_ELECTION` | `false` (`true` in the chart). Run the controllers only on the Lease holder; required when several processes have the controller role |
| `KUBEN_SECURITY__SESSION_TTL_HOURS` | `12` |
| `KUBEN_SECURITY__SESSION_CACHE_TTL_SECS` | `5`. Upper bound for a revoked session to stay valid on another replica |
| `KUBEN_SECURITY__LOGIN_MAX_FAILURES` | `5` per email + client IP |
| `KUBEN_SECURITY__LOGIN_MAX_FAILURES_PER_IP` | `30` |
| `KUBEN_SECURITY__LOGIN_MAX_FAILURES_PER_ACCOUNT` | `100` |
| `KUBEN_SECURITY__LOGIN_WINDOW_SECS` | `900` |
| `KUBEN_SECURITY__TRUST_FORWARDED_FOR` | `false` (`true` in the chart). Only enable behind a proxy that appends `X-Forwarded-For` |
| `KUBEN_SECURITY__PASSWORD_MIN_LENGTH` | `12` |
| `KUBEN_TELEMETRY__LOG_FORMAT` | `json` |
| `KUBEN_BOOTSTRAP__ADMIN_PASSWORD` | generated (see §3) |

## 5. Exposing apps and automatic HTTPS

**Routes.** For every app with an HTTP port, the App controller creates an `HTTPRoute`. Its hostnames are the app's custom domains plus `<app>-<env>.<baseDomain>`. Processes with `protocol: tcp` (databases) get a cluster-internal Service only.

**Listeners.** When `clusterIssuer` is set, Kuben **owns the listeners** of the Gateway named in `KubenConfig.spec.gateway`, so that Gateway must be dedicated to Kuben. Kuben maintains:

- `http` (port 80): only the platform's redirect route and cert-manager's challenge routes may attach. Every other request is redirected to HTTPS.
- one HTTPS listener per hostname, named `h-<hash>`, with the certificate Secret `kuben-tls-<hash>`. A hostname admits routes only from the namespace that claimed it first.
- optionally one wildcard listener `https` for `*.<baseDomain>`, when `wildcardTlsSecret` is set (a DNS-01 certificate). All generated hostnames then share it.

**Certificates.** The Gateway gets the annotation `cert-manager.io/cluster-issuer`, and cert-manager's gateway-shim issues a certificate for every listener.

A minimal Gateway; Kuben fills in the listeners:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: kuben
  namespace: kuben-system
spec:
  gatewayClassName: traefik          # or eg / cilium
  listeners:
    - name: http
      protocol: HTTP
      port: 80
```

A ClusterIssuer that solves HTTP-01 through that Gateway:

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: ops@example.com
    privateKeySecretRef: { name: letsencrypt-account }
    solvers:
      - http01:
          gatewayHTTPRoute:
            parentRefs:
              - { kind: Gateway, name: kuben, namespace: kuben-system, sectionName: http }
```

**Wildcard certificate.** If you use one, issue it with a DNS-01 Certificate whose **name equals the Secret name** in `wildcardTlsSecret`. cert-manager then leaves it alone instead of trying HTTP-01 on the wildcard listener.

**Honest limits:**
- A Gateway holds at most 64 listeners, and Kuben uses at most 60. Beyond that, use the wildcard listener for generated hosts. ListenerSet support (experimental in Gateway API) is on the roadmap.
- TLS termination happens at your Gateway controller. Kuben only configures it.

**Status.** Each app reports its status in the `Exposed` condition:
- `RouteApplied`
- `NoGateway`
- `NoHostname`
- `GatewayAPIMissing`

The *Check DNS* button (`GET …/apps/{app}/domains`) tells you whether each hostname already points at the Gateway.

## 6. Backup and restore

```bash
kuben backup --out ./kuben-backup
```

```bash
kuben restore --from ./kuben-backup
```

- **Backup** exports all Projects, Environments and Apps as YAML.
- **Restore** applies the CRDs first, then re-points old `ownerReference`s at the new Project uids (otherwise the garbage collector would delete the restored objects), then creates the namespaces immediately.
- **Secret values are deliberately not exported.** Back up the database PVC with a VolumeSnapshot, and secrets with Vault, SOPS or a cluster backup tool such as Velero.
- **App volumes are not included either.** They are ordinary PVCs (kept when an app is deleted); back them up with VolumeSnapshots or Velero.

## 7. Upgrading

```bash
helm upgrade kuben oci://ghcr.io/teamtem-dev/charts/kuben -n kuben-system --reuse-values
```

Helm does not update the `crds/` folder on upgrade, so the binary server-side-applies its CRDs on every start (ADR-017). Database migrations also run at boot.

## 8. Security model and its limits

- **Cluster access.**
  - Kuben's service account can manage namespaces, secrets, deployments, cron jobs, PVCs and similar objects cluster-wide, and can patch its Gateway (exact list in `templates/rbac.yaml`).
  - It needs this because it creates one namespace per environment. Treat it like any infrastructure controller.
  - Be clear about what that means: whoever controls Kuben's service account can read every Secret in the cluster, so protect it like cluster-admin (ADR-022). The only namespaced right is the controller Lease.
- **Tenant namespace isolation.**
  - Pod Security Admission: `baseline` is enforced, `restricted` is warned.
  - LoadBalancer and NodePort services are forbidden.
  - Ingress between environments is closed by NetworkPolicy.
- **Authorization.**
  - Every request is checked against the caller's role (owner/admin/developer/viewer).
  - Other orgs' objects return 404, not 403.
  - Deleting a production environment requires the separate `env-delete-protected` permission.
  - Env values are hidden from users without `secret-read`.
- **Sign-in.** Logins are throttled (see §4) against a budget shared by all replicas; the stored buckets are hashes, not emails or addresses. Invited users must replace their temporary password before anything else. A generated admin password goes into a Secret, never into the log.
- **API tokens.**
  - Only a SHA-256 hash is stored, and it is compared in constant time.
  - A token is capped at a role and optionally scoped to a project or environment.
  - Tokens cannot manage tokens, members or passwords.
- **Audit.** Every mutation, denial and login is recorded (never request bodies) and shown under *Audit*. The log is append-only through the API.
- **Secrets.** The API only writes them; it never returns their values, and it touches only Secrets it created itself.

## 9. Troubleshooting

| Tool | Use |
|---|---|
| `kuben doctor` | prerequisites: database, cluster, Gateway API, cert-manager, metrics-server |
| `GET /api/v1/healthz/details` | per-subsystem status (sign-in required) |
| `/livez`, `/readyz` | Kubernetes probes. `/readyz` stays `503` until the informers have synced |
| `kubectl -n kuben-system get lease kuben-controller` | which pod runs the controllers (`holderIdentity`); the others report `controllers: standby` |
| `:9090/metrics` | Prometheus metrics, e.g. `kuben_reconcile_errors_total`, `kuben_audit_write_errors_total`, `kuben_leader` |
| `kubectl get apps,environments -A` | conditions (`Ready`, `Exposed`) with `reason` and `message` |
| Kuben logs: `gateway listener limit reached` / `hostname requested by two namespaces` | listener cap or domain conflict (scenario 9) |
