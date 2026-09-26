#!/usr/bin/env bash
# `kuben setup` on a fresh Linux host with systemd (the host-install job in
# CI): it installs or starts PostgreSQL, makes the role and database kuben
# (peer authentication, not a superuser, owner of its database) and runs
# Kuben against the cluster of the current kubeconfig; `--plan` beforehand
# changes nothing, the install journal records the run and who owns what
# (M2.6), a second run changes nothing, and `kuben uninstall --purge` drops the
# database and the role it created, but keeps a database and role that were
# there before setup.
# Needs root through sudo and changes the host: run it on a throwaway machine.
#
#   KUBEN_BIN=bin/kuben scripts/host-install-test.sh   # after bun turbo run go:build
set -euo pipefail
cd "$(dirname "$0")/.."

BIN=${KUBEN_BIN:-bin/kuben}
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
in_kuben_db() { sudo runuser -u postgres -- psql -d kuben -tAc "$1"; }

JOURNAL=/var/lib/kuben/install/journal.json
journal() { sudo jq -r "$1" "$JOURNAL"; }
owner_of() { journal "[.resources[] | select(.kind == \"$1\" and .name == \"$2\") | .owner][0] // \"none\""; }

setup() { sudo "$1" setup --yes --kubeconfig "$KUBECONFIG_FILE" 2>&1 | tee "$work/setup.txt"; }

echo "==> kuben setup --plan changes nothing"
sudo "$BIN" setup --plan --kubeconfig "$KUBECONFIG_FILE" 2>&1 | tee "$work/plan.txt"
grep -q 'kuben setup would' "$work/plan.txt" || fail "--plan printed no plan"
for path in /etc/systemd/system/kuben.service /etc/kuben/config.toml "$JOURNAL"; do
  [[ ! -e $path ]] || fail "--plan created $path"
done

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

echo "==> the install journal records the run and what setup created"
[[ $(journal '.runs[-1].succeeded') == true ]] || fail "the journal has no successful run: $(journal '.runs[-1]')"
for resource in "postgresDatabase kuben" "postgresRole kuben" "systemUser kuben" "systemdUnit kuben.service" "file /etc/kuben/config.toml"; do
  # shellcheck disable=SC2086
  [[ $(owner_of $resource) == kuben ]] || fail "$resource is not recorded as created by setup"
done
sudo -u kuben test -r "$JOURNAL" || fail "the service user cannot read the journal"
[[ $(in_kuben_db "SELECT count(*) FROM install_journals") == 1 ]] || fail "kuben serve did not record the journal"

echo "==> kuben setup again changes nothing"
setup /usr/local/bin/kuben
curl -fsS http://127.0.0.1:3000/readyz >/dev/null || fail "kuben.service is not ready after the second run"
grep -q 'Nothing changed' "$work/setup.txt" || fail "the second run did not say it changed nothing"
[[ $(journal '[.runs[-1].steps[] | select(.result != "unchanged")] | length') == 0 ]] ||
  fail "the second run changed: $(journal '[.runs[-1].steps[] | select(.result != "unchanged")]')"

echo "==> kuben uninstall --purge drops the database and the role"
sudo /usr/local/bin/kuben uninstall --purge --yes
[[ -z $(as_postgres "SELECT 1 FROM pg_database WHERE datname = 'kuben'") ]] || fail "the database kuben stayed"
[[ -z $(as_postgres "SELECT 1 FROM pg_roles WHERE rolname = 'kuben'") ]] || fail "the role kuben stayed"
[[ ! -e $JOURNAL ]] || fail "the journal stayed"

echo "==> a database and role that were there first survive uninstall --purge (I12)"
as_postgres "CREATE ROLE kuben LOGIN" >/dev/null
as_postgres "CREATE DATABASE kuben OWNER kuben" >/dev/null
in_kuben_db "CREATE TABLE operator_marker (id int)" >/dev/null
setup "$BIN"
[[ $(owner_of postgresDatabase kuben) == preexisting ]] || fail "the operator's database is recorded as setup's"
[[ $(owner_of postgresRole kuben) == preexisting ]] || fail "the operator's role is recorded as setup's"
sudo /usr/local/bin/kuben uninstall --purge --yes
[[ -n $(as_postgres "SELECT 1 FROM pg_database WHERE datname = 'kuben'") ]] || fail "purge dropped a database setup did not create"
[[ -n $(as_postgres "SELECT 1 FROM pg_roles WHERE rolname = 'kuben'") ]] || fail "purge dropped a role setup did not create"
[[ $(in_kuben_db "SELECT to_regclass('operator_marker') IS NOT NULL") == t ]] || fail "the operator's table is gone"
as_postgres "DROP DATABASE kuben WITH (FORCE)" >/dev/null
as_postgres "DROP ROLE kuben" >/dev/null

echo "HOST INSTALL PASSED"
