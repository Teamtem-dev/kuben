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

## 3. First sign-in

```bash
kubectl -n kuben-system get secret kuben-initial-admin -o jsonpath='{.data.password}' | base64 -d
```

Delete the `kuben-initial-admin` Secret once you have signed in and changed the password. A binary running outside Kubernetes prints the generated password to its terminal instead.

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
