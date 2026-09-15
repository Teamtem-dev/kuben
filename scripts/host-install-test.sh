#!/usr/bin/env bash
# `kuben setup` on a fresh Linux host with systemd (the host-install job in
# CI): it installs or starts PostgreSQL, makes the role and database kuben
# (peer authentication, not a superuser, owner of its database) and runs
# Kuben against the cluster of the current kubeconfig; a second run changes
# nothing, and `kuben uninstall --purge` drops the database and the role.
# Needs root through sudo and changes the host: run it on a throwaway machine.
#
#   KUBEN_BIN=target/debug/kuben scripts/host-install-test.sh
set -euo pipefail
cd "$(dirname "$0")/.."

BIN=${KUBEN_BIN:-target/debug/kuben}
KUBECONFIG_FILE=${KUBECONFIG:-$HOME/.kube/config}
work=$(mktemp -d)

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

diagnose() {
  local status=$?
  if ((status != 0)); then
    echo "---- kuben.service ----"
    sudo journalctl -u kuben --no-pager -n 60 -o cat 2>/dev/null || true
    echo "---- postgresql ----"
    sudo journalctl -u postgresql --no-pager -n 20 -o cat 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap diagnose EXIT

# One value, as the superuser postgres over the Unix socket.
as_postgres() { sudo runuser -u postgres -- psql -tAc "$1"; }

setup() { sudo "$1" setup --yes --kubeconfig "$KUBECONFIG_FILE" 2>&1 | tee "$work/setup.txt"; }

echo "==> kuben setup"
setup "$BIN"
sudo systemctl is-active --quiet postgresql || fail "postgresql is not running"
[[ $(as_postgres "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = 'kuben'") == f ]] ||
  fail "the role kuben is missing, or a superuser"
[[ $(as_postgres "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'kuben'") == kuben ]] ||
  fail "the database kuben is missing, or not owned by kuben"
for _ in $(seq 90); do
  if curl -fsS http://127.0.0.1:3000/readyz >/dev/null 2>&1; then break; fi
  sleep 1
done
curl -fsS http://127.0.0.1:3000/readyz >/dev/null || fail "kuben.service is not ready"
# As the service user: peer authentication maps the system user to the role.
sudo -u kuben /usr/local/bin/kuben doctor | tee "$work/doctor.txt" || fail "kuben doctor failed"
grep -q '^\[OK  \] database: ' "$work/doctor.txt" || fail "doctor: database not OK"
grep -q '^\[OK  \] database role: ' "$work/doctor.txt" || fail "doctor: the role bypasses row-level security"

echo "==> kuben setup again changes nothing"
setup /usr/local/bin/kuben
curl -fsS http://127.0.0.1:3000/readyz >/dev/null || fail "kuben.service is not ready after the second run"

echo "==> kuben uninstall --purge drops the database and the role"
sudo /usr/local/bin/kuben uninstall --purge --yes
[[ -z $(as_postgres "SELECT 1 FROM pg_database WHERE datname = 'kuben'") ]] || fail "the database kuben stayed"
[[ -z $(as_postgres "SELECT 1 FROM pg_roles WHERE rolname = 'kuben'") ]] || fail "the role kuben stayed"

echo "HOST INSTALL PASSED"
