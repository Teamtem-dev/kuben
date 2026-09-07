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
