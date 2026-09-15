#!/usr/bin/env bash
# M0 footprint measurement (ADR-031). Run on the reference server after the
# platform is installed and has been idle for a few minutes:
#
#   sudo scripts/spikes/m0-footprint.sh > footprint.md
#
# Prints a Markdown report: host memory, the resident memory of each
# platform process, per-namespace pod usage from the Metrics API, the size of
# Kuben's PostgreSQL database, and disk used by the datastores. Nothing is
# changed on the host.

set -euo pipefail

KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
export KUBECONFIG

have() { command -v "$1" >/dev/null 2>&1; }
mib() { awk '{ printf "%.0f", $1 / 1024 }'; }

echo "# Kuben footprint — $(hostname) — $(date -u +%Y-%m-%dT%H:%MZ)"
echo
echo "| Item | Value |"
echo "|---|---|"
echo "| OS | $(sed -n 's/^PRETTY_NAME=//p' /etc/os-release 2>/dev/null | tr -d '"') |"
echo "| Kernel | $(uname -r) |"
echo "| CPUs | $(nproc) |"
awk '/MemTotal/ {t=$2} /MemAvailable/ {a=$2} END {
  printf "| Memory total | %.0f MiB |\n| Memory available | %.0f MiB |\n| Memory used | %.0f MiB |\n", t/1024, a/1024, (t-a)/1024 }' /proc/meminfo
if have k3s; then
  echo "| k3s | $(k3s --version | head -1) |"
fi
echo

echo "## Resident memory of platform processes"
echo
echo "| Process | Instances | RSS (MiB) |"
echo "|---|---:|---:|"
for name in k3s-server k3s-agent containerd kuben postgres traefik cert-manager zot buildkitd; do
  rss=$(ps -eo rss=,args= | awk -v n="$name" 'index($0, n) && !/awk/ { s += $1; c++ } END { print c + 0, s + 0 }')
  count=${rss%% *}
  kib=${rss##* }
  [[ $count -gt 0 ]] && echo "| $name | $count | $(echo "$kib" | mib) |"
done
echo

if have kubectl && kubectl top pods -A >/dev/null 2>&1; then
  echo "## Pod usage by namespace (Metrics API)"
  echo
  echo "| Namespace | CPU (m) | Memory (MiB) |"
  echo "|---|---:|---:|"
  kubectl top pods -A --no-headers | awk '{
      cpu = $3; sub(/m$/, "", cpu); mem = $4
      if (mem ~ /Gi$/) { sub(/Gi$/, "", mem); mem *= 1024 } else { sub(/Mi$/, "", mem) }
      c[$1] += cpu; m[$1] += mem
    } END { for (ns in c) printf "| %s | %d | %d |\n", ns, c[ns], m[ns] }' | sort
  echo
else
  echo "_Metrics API unavailable: pod usage skipped._"
  echo
fi

# Kuben's own database (ADR-025), on the host PostgreSQL that `kuben setup`
# prepares; skipped when the server uses another database.
if have psql && systemctl is-active --quiet postgresql 2>/dev/null; then
  size=$(runuser -u postgres -- psql -tAc "SELECT pg_size_pretty(pg_database_size('kuben'))" 2>/dev/null || true)
  if [[ -n $size ]]; then
    echo "## PostgreSQL"
    echo
    echo "| Item | Value |"
    echo "|---|---|"
    echo "| Server | $(runuser -u postgres -- psql -tAc 'SHOW server_version' 2>/dev/null) |"
    echo "| Database kuben | ${size} |"
    echo
  fi
fi

echo "## Disk used by datastores"
echo
echo "| Path | Size |"
echo "|---|---:|"
# /var/lib/pgsql is where RHEL-family distributions keep PostgreSQL.
for path in /var/lib/rancher/k3s/server/db /var/lib/rancher/k3s/agent/containerd /var/lib/postgresql /var/lib/pgsql /var/lib/kuben; do
  [[ -d $path ]] && echo "| $path | $(du -sh "$path" 2>/dev/null | cut -f1) |"
done
echo
echo "_Measure again under a build and with the reference app deployed; record both in ADR-031._"
