#!/usr/bin/env bash
# The Helm chart's own PostgreSQL against a real cluster (the kind job in CI):
# it comes up with the chart, Kuben's role `kuben` owns the `kuben` database
# and is not a superuser (row-level security applies), and `helm uninstall`
# keeps the volume and the password Secret, so a re-install finds its data
# with the same password. The Kuben server itself is not started
# (replicaCount=0): this checks the database part of the chart.
#
#   scripts/chart-postgres-test.sh
set -euo pipefail
cd "$(dirname "$0")/.."

NS=${KUBEN_CHART_TEST_NAMESPACE:-kuben-chart-test}
RELEASE=kuben
PG="${RELEASE}-postgresql"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cleanup() {
  local status=$?
  if ((status != 0)); then
    kubectl -n "$NS" get statefulsets,pods,pvc,secrets 2>/dev/null || true
    kubectl -n "$NS" describe pod "${PG}-0" 2>/dev/null | tail -n 40 || true
    kubectl -n "$NS" logs "${PG}-0" --tail=60 2>/dev/null || true
  fi
  helm -n "$NS" uninstall "$RELEASE" >/dev/null 2>&1 || true
  kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

install() {
  # No KubenConfig: the cluster's singleton belongs to the e2e run before.
  # No agent: the published image of the chart's appVersion predates it.
  # No backup: replicaCount=0, this checks the database part only.
  helm install "$RELEASE" charts/kuben --namespace "$NS" --create-namespace \
    --set replicaCount=0 --set platform.create=false --set agent.enabled=false --set backup.enabled=false --wait --timeout 5m
  kubectl -n "$NS" rollout status "statefulset/${PG}" --timeout=180s
}

# query <sql>: one value, as Kuben connects (role `kuben`, password, TCP).
query() {
  # shellcheck disable=SC2016 # $KUBEN_PASSWORD expands in the pod, not here
  kubectl -n "$NS" exec "${PG}-0" -- \
    sh -c 'PGPASSWORD="$KUBEN_PASSWORD" psql -h 127.0.0.1 -U kuben -d kuben -tAc "$1"' _ "$1"
}

echo "==> install: the chart's PostgreSQL comes up"
install
[[ $(query 'SELECT current_user') == kuben ]] || fail "not connected as kuben"
[[ $(query 'SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user') == f ]] ||
  fail "kuben is a superuser or bypasses row-level security"
[[ $(query "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'kuben'") == kuben ]] ||
  fail "the database kuben is not owned by kuben"
# shellcheck disable=SC2016 # $$…$$ is SQL dollar quoting
query 'CREATE TABLE kept (v text); INSERT INTO kept VALUES ($$before the uninstall$$)' >/dev/null
password=$(kubectl -n "$NS" get secret "$PG" -o jsonpath='{.data.password}')
[[ -n $password ]] || fail "no password in the Secret"

echo "==> helm uninstall keeps the volume and the password Secret"
helm -n "$NS" uninstall "$RELEASE" --wait --timeout 3m
kubectl -n "$NS" wait --for=delete "pod/${PG}-0" --timeout=120s
kubectl -n "$NS" get pvc "data-${PG}-0" >/dev/null || fail "helm uninstall deleted the database volume"
kubectl -n "$NS" get secret "$PG" >/dev/null || fail "helm uninstall deleted the password Secret"

echo "==> a re-install finds its data with the same password"
install
[[ $(kubectl -n "$NS" get secret "$PG" -o jsonpath='{.data.password}') == "$password" ]] ||
  fail "the re-install replaced the password"
[[ $(query 'SELECT v FROM kept') == 'before the uninstall' ]] || fail "the data did not survive"

echo "CHART POSTGRESQL PASSED"
