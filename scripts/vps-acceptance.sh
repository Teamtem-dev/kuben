#!/usr/bin/env bash
# M2 acceptance (plan §18.1) on a real server: a fresh Linux VPS with a public
# address and a wildcard DNS record. It installs Kuben with HTTPS, creates the
# first admin, deploys an app by digest with the kuben client, checks it over
# public HTTPS with a real certificate, stops the control plane and checks the
# app still answers, runs setup again, and measures the footprint.
#
#   # DNS first: *.apps.example.com and kuben.apps.example.com → this server
#   sudo KUBEN_BIN=./kuben DOMAIN=apps.example.com ACME_EMAIL=ops@example.com \
#     scripts/vps-acceptance.sh | tee acceptance.md
#
# KUBEN_BIN: the kuben binary to install (default: the one on PATH).
# ACME_STAGING=1 uses Let's Encrypt's staging server (untrusted certificates;
# the HTTPS checks then skip verification). Run it on a throwaway server: it
# installs k3s, PostgreSQL and a systemd service. The report is Markdown on
# stdout; progress goes to stderr.
set -euo pipefail
cd "$(dirname "$0")/.."

BIN=${KUBEN_BIN:-$(command -v kuben || true)}
DOMAIN=${DOMAIN:?set DOMAIN to the base domain whose wildcard record points here}
ACME_EMAIL=${ACME_EMAIL:?set ACME_EMAIL for Let\'s Encrypt}
IMAGE=${KUBEN_ACCEPTANCE_IMAGE:-nginxinc/nginx-unprivileged:1.27-alpine}
CONSOLE="kuben.${DOMAIN}"
APP_HOST="web-demo-prod.${DOMAIN}"
LOCAL="http://127.0.0.1:3000/api/v1"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
results=()

[[ $(id -u) == 0 ]] || { echo "run as root (sudo)" >&2; exit 2; }
[[ -x $BIN ]] || { echo "no kuben binary: set KUBEN_BIN" >&2; exit 2; }
for c in curl jq dig; do command -v "$c" >/dev/null || { echo "missing: $c (apt-get install -y curl jq dnsutils)" >&2; exit 2; }; done

log() { printf '\033[1m==> %s\033[0m\n' "$*" >&2; }
record() { results+=("| $1 | $2 | $3 |"); }
check() { # <name> <detail> <command…>: record PASS or FAIL
  local name=$1 detail=$2
  shift 2
  if "$@" >"$work/check.out" 2>&1; then
    record "$name" PASS "$detail"
  else
    record "$name" FAIL "$detail — $(tail -n 3 "$work/check.out" | tr '\n' ' ')"
  fi
}
eventually() { # <seconds> <command…>
  local deadline=$((SECONDS + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || return 1
    sleep 5
  done
}
tls=()
[[ ${ACME_STAGING:-0} == 1 ]] && tls=(--insecure)
https_ok() { curl -fsS "${tls[@]}" -o /dev/null --max-time 10 "https://$1/"; }
api() { curl -fsS -H 'content-type: application/json' -H 'x-kuben-client: acceptance' "$@"; }

public_ip=$(curl -fsS --max-time 10 https://api.ipify.org || true)
log "DNS: ${CONSOLE} and ${APP_HOST} → ${public_ip:-this server}"
check "DNS points here" "${CONSOLE}, ${APP_HOST} → ${public_ip}" bash -c \
  "[[ -n '$public_ip' ]] && dig +short '$CONSOLE' | grep -qx '$public_ip' && dig +short '$APP_HOST' | grep -qx '$public_ip'"

log "kuben setup with HTTPS"
staging=()
[[ ${ACME_STAGING:-0} == 1 ]] && staging=(--acme-staging)
start=$SECONDS
"$BIN" setup --yes --domain "$DOMAIN" --acme-email "$ACME_EMAIL" "${staging[@]}" 2>&1 | tee "$work/setup.txt" >&2
record "Fresh install" PASS "kuben setup in $((SECONDS - start))s"
check "Setup link over HTTPS" "the link names https://${CONSOLE} with the token after #" \
  grep -q "https://${CONSOLE}/setup#token=" "$work/setup.txt"

log "the console over public HTTPS with a real certificate"
check "Console certificate" "https://${CONSOLE} within 10 minutes" eventually 600 https_ok "$CONSOLE"

log "first admin (from this server: a secure path)"
password="acceptance-$(head -c 12 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9')"
token=$(cat /var/lib/kuben/setup-token)
api -c "$work/cookies" -X POST "$LOCAL/setup" \
  --data "{\"org_name\":\"Acceptance\",\"email\":\"owner@${DOMAIN}\",\"password\":\"${password}\",\"token\":\"${token}\"}" >/dev/null
api -b "$work/cookies" -X POST "$LOCAL/projects" --data '{"name":"demo","display_name":"Demo"}' >/dev/null
api -b "$work/cookies" -X POST "$LOCAL/projects/demo/environments" --data '{"name":"prod"}' >/dev/null
eventually 120 api -b "$work/cookies" "$LOCAL/projects/demo/environments/prod"
cli_token=$(api -b "$work/cookies" -X POST "$LOCAL/tokens" --data '{"name":"acceptance","role":"developer","project":"demo"}' | jq -r .token)

log "an app deployed by digest with the kuben client, over the console's HTTPS address"
export KUBEN_CONTEXT_FILE="$work/contexts.json"
printf '%s\n' "$cli_token" | "$BIN" login "https://${CONSOLE}" --project demo --environment prod >&2
api -H "authorization: Bearer ${cli_token}" -X POST "$LOCAL/projects/demo/environments/prod/apps" \
  --data "{\"name\":\"web\",\"image\":\"${IMAGE}\",\"port\":8080}" >/dev/null
check "Deploy by digest (kuben deploy)" "${IMAGE} resolved and rolled out" "$BIN" deploy web --image "$IMAGE" --timeout 600
check "App over public HTTPS" "https://${APP_HOST} with a certificate" eventually 600 https_ok "$APP_HOST"
check "HTTP redirects to HTTPS" "http://${APP_HOST}" bash -c \
  "curl -sS -o /dev/null -w '%{http_code}' --max-time 10 'http://${APP_HOST}/' | grep -qx 301"
"$BIN" status web --json >"$work/status.json" 2>&1 || true
doctor=$(jq -r '.doctor.status // "unavailable"' "$work/status.json" 2>/dev/null || echo unavailable)
check "Doctor" "overall ${doctor}" test "$doctor" = ok

log "the control plane stops; the app keeps serving"
systemctl stop kuben
check "App without the control plane" "https://${APP_HOST} with kuben.service stopped" \
  bash -c "for i in 1 2 3 4 5 6; do curl -fsS ${tls[*]} -o /dev/null --max-time 10 'https://${APP_HOST}/' || exit 1; sleep 10; done"
systemctl start kuben
check "Control plane back" "/readyz within 2 minutes" eventually 120 curl -fsS http://127.0.0.1:3000/readyz

log "setup again: nothing changes, nothing of others is touched"
"$BIN" setup --yes --domain "$DOMAIN" --acme-email "$ACME_EMAIL" "${staging[@]}" 2>&1 | tee "$work/again.txt" >&2
check "Second setup" "reports that nothing changed" grep -q 'Nothing changed' "$work/again.txt"
check "App after the second setup" "https://${APP_HOST}" https_ok "$APP_HOST"

log "footprint (idle a minute first)"
sleep 60
bash scripts/footprint.sh >"$work/footprint.md" 2>&1 || true

echo "# Kuben M2 acceptance — ${DOMAIN} — $(date -u +%Y-%m-%dT%H:%MZ)"
echo
"$BIN" version --bundle 2>/dev/null | head -1 || true
echo
echo "| Check | Result | Detail |"
echo "|---|---|---|"
printf '%s\n' "${results[@]}"
echo
echo "From another machine (not this server), run:"
echo
echo '```'
echo "curl -sSI https://${APP_HOST}/ | head -1"
echo "curl -sSI https://${CONSOLE}/ | head -1"
echo '```'
echo
cat "$work/footprint.md"
echo
(umask 077 && printf '%s\n' "$password" >/root/kuben-acceptance-password)
echo "Sign in at https://${CONSOLE} as owner@${DOMAIN}; the password is in /root/kuben-acceptance-password (only root can read it)."
if printf '%s\n' "${results[@]}" | grep -q '| FAIL |'; then exit 1; fi
