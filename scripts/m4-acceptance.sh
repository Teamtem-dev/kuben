#!/usr/bin/env bash
# M4 acceptance (plan §19.3, S01–S10) on a real installation: the evidence
# report of the Supported MVP gate. Run it on the Kuben server, as root, after
# scripts/vps-acceptance.sh (or `kuben setup` with HTTPS):
#
#   sudo KUBEN_URL=https://kuben.apps.example.com DOMAIN=apps.example.com \
#     KUBEN_TOKEN=<an org admin's API token> \
#     KUBEN_APPROVER_TOKEN=<a second admin's API token> \
#     REVIEWER="Jane Doe" scripts/m4-acceptance.sh | tee m4-acceptance.md
#
# Optional: WEBHOOK_URL (an HTTPS receiver you can watch, for S02),
# KUBEN_BIN (default: kuben on PATH), KUBEN_ACCEPTANCE_IMAGE and
# KUBEN_ACCEPTANCE_IMAGE2 (two tags of one web image on port 8080).
#
# Every scenario records PASS or FAIL for what can be checked here, and
# MANUAL for what needs a person, a second host or a destructive step: a
# written test is not a result (§19.3). The report is Markdown on stdout;
# progress goes to stderr. It creates a project `accept-<n>` and leaves it
# for the reviewer; the detached app stays running on purpose.
set -euo pipefail
cd "$(dirname "$0")/.."

URL=${KUBEN_URL:?set KUBEN_URL to the console address}
TOKEN=${KUBEN_TOKEN:?set KUBEN_TOKEN to an organization admin API token}
APPROVER=${KUBEN_APPROVER_TOKEN:?set KUBEN_APPROVER_TOKEN to a second admin API token}
DOMAIN=${DOMAIN:?set DOMAIN to the apps base domain}
REVIEWER=${REVIEWER:?set REVIEWER to the person reviewing this run}
BIN=${KUBEN_BIN:-$(command -v kuben || true)}
IMAGE=${KUBEN_ACCEPTANCE_IMAGE:-nginxinc/nginx-unprivileged:1.27-alpine}
IMAGE2=${KUBEN_ACCEPTANCE_IMAGE2:-nginxinc/nginx-unprivileged:1.26-alpine}
API="${URL%/}/api/v1"
P="accept-$((RANDOM % 9000 + 1000))"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
results=()

[[ $(id -u) == 0 ]] || { echo "run as root (sudo)" >&2; exit 2; }
[[ -x $BIN ]] || { echo "no kuben binary: set KUBEN_BIN" >&2; exit 2; }
for c in curl jq kubectl; do command -v "$c" >/dev/null || { echo "missing: $c" >&2; exit 2; }; done

log() { printf '\033[1m==> %s\033[0m\n' "$*" >&2; }
record() { results+=("| $1 | $2 | $3 |"); }
check() { # <scenario> <detail> <command…>
  local id=$1 detail=$2
  shift 2
  if "$@" >"$work/check.out" 2>&1; then
    record "$id" PASS "$detail"
  else
    record "$id" FAIL "$detail — $(tail -n 3 "$work/check.out" | tr '\n' ' ' | cut -c1-300)"
  fi
}
manual() { record "$1" MANUAL "$2"; }
eventually() { # <seconds> <command…>
  local deadline=$((SECONDS + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || return 1
    sleep 5
  done
}
# call <token> <method> <path> [json]: the body on stdout, the status in $work/status.
call() {
  local token=$1 method=$2 path=$3 body=${4:-}
  local args=(-sS -o "$work/body" -w '%{http_code}' -X "$method" -H "authorization: Bearer $token"
    -H 'content-type: application/json' -H 'x-kuben-client: m4-acceptance')
  [[ -n $body ]] && args+=(--data "$body")
  curl "${args[@]}" "$API$path" >"$work/status"
  cat "$work/body"
}
status() { cat "$work/status"; }
expect() { # <status> <token> <method> <path> [json]
  local want=$1
  shift
  call "$@" >/dev/null
  [[ $(status) == "$want" ]] || { echo "got $(status), want $want: $(cat "$work/body")"; return 1; }
}
admin() { call "$TOKEN" "$@"; }
app_path() { echo "/projects/$P/environments/$1/apps/$2"; }
https_ok() { curl -fsS -o /dev/null --max-time 10 "https://$1/"; }
host_of() { echo "$2-$P-$1.$DOMAIN"; } # <env> <app>
generation() { admin GET "$(app_path "$1" "$2")/deployments" | jq -r '[.[].generation] | max // 0'; }
latest_run() { admin GET "$(app_path "$1" "$2")/deployments" | jq -r 'max_by(.generation) | .run'; }
ready() { admin GET "$(app_path "$1" "$2")" | jq -e '.ready == true' >/dev/null; }
current_image() { admin GET "$(app_path "$1" "$2")/releases" | jq -r '.[] | select(.current) | .image'; }

# Database commands run as the service user: PostgreSQL maps the system
# user to its role (peer authentication).
as_kuben() {
  if id kuben >/dev/null 2>&1; then runuser -u kuben -- "$@"; else "$@"; fi
}
chmod 755 "$work"
for d in backups support; do
  if id kuben >/dev/null 2>&1; then install -d -o kuben -m 700 "$work/$d"; else mkdir -p "$work/$d"; fi
done

# Approve the newest run of <env>/<app> as the second admin.
approve_latest() {
  local path run hash
  path=$(app_path "$1" "$2")
  run=$(latest_run "$1" "$2")
  hash=$(admin GET "$path/deployments/$run" | jq -r '.plan_hash // empty')
  [[ -n $hash ]] || return 0 # nothing to approve
  expect 403 "$TOKEN" POST "$path/deployments/$run/approve" "{\"planHash\":\"$hash\"}" || return 1
  expect 204 "$APPROVER" POST "$path/deployments/$run/approve" "{\"planHash\":\"$hash\"}"
}

export API work TOKEN APPROVER P DOMAIN BIN
export -f call status expect admin app_path https_ok host_of generation latest_run ready current_image as_kuben

s01() {
  log "S01 golden path"
  # shellcheck disable=SC2016 # expanded by the inner shell, from the exports
  check S01 "project $P with staging and production" bash -c '
    expect 201 "$TOKEN" POST /projects "{\"name\":\"$P\",\"display_name\":\"Acceptance\"}" &&
    expect 201 "$TOKEN" POST "/projects/$P/environments" "{\"name\":\"staging\"}" &&
    expect 201 "$TOKEN" POST "/projects/$P/environments" "{\"name\":\"prod\",\"env_type\":\"production\"}"' 
  admin POST "/projects/$P/environments/staging/apps" \
    "{\"name\":\"web\",\"image\":\"$IMAGE\",\"port\":8080}" >/dev/null
  admin POST "/projects/$P/environments/staging/apps" \
    "{\"name\":\"worker\",\"image\":\"$IMAGE\",\"command\":[\"sh\",\"-c\",\"sleep 1000000\"]}" >/dev/null
  check S01 "web and worker ready in staging" eventually 300 bash -c "
    ready staging web && ready staging worker"
  check S01 "web over HTTPS in staging" eventually 300 https_ok "$(host_of staging web)"
  admin POST "$(app_path staging web)/promote" '{"to_environment":"prod"}' >/dev/null
  check S01 "promotion to production waits for a second admin (self-approval refused)" approve_latest prod web
  check S01 "web over HTTPS in production" eventually 300 https_ok "$(host_of prod web)"
  check S01 "production runs the digest staging runs" bash -c "
    [[ \$(current_image staging web) == \$(current_image prod web) ]]"
  local gen
  gen=$(generation prod web)
  admin POST "$(app_path prod web)/deployments" "{\"image\":\"$IMAGE2\",\"expected_generation\":$gen}" >/dev/null
  approve_latest prod web >/dev/null 2>&1 || true
  eventually 300 ready prod web || true
  admin POST "$(app_path prod web)/rollback" '{"revision":1}' >/dev/null
  approve_latest prod web >/dev/null 2>&1 || true
  check S01 "rollback returns production to the promoted image" eventually 300 bash -c "
    [[ \$(current_image prod web) == \$(current_image staging web) ]]"
  manual S01 "a second person installs from the docs on the reference profile and repeats this, including a Git build with a build-time config error, with no manual database edit"
}

s02() {
  log "S02 outage and alerts"
  check S02 "incidents can be listed" expect 200 "$TOKEN" GET /incidents
  if [[ -n ${WEBHOOK_URL:-} ]]; then
    local id
    id=$(admin POST /webhooks "{\"name\":\"$P\",\"url\":\"$WEBHOOK_URL\",\"events\":[\"*\"]}" | jq -r .id)
    admin POST "/webhooks/$id/ping" >/dev/null
    check S02 "a signed ping reaches $WEBHOOK_URL" eventually 120 bash -c "
      admin GET /webhooks/$id/deliveries | jq -e 'any(.[]; .status == \"delivered\")'"
  else
    manual S02 "set WEBHOOK_URL to check a signed delivery"
  fi
  manual S02 "an external monitor (outside this host) alerts when the host stops; record the detection time; stop the webhook receiver and check retries, then recovery"
}

s03() {
  log "S03 backup and recovery"
  check S03 "kuben backup writes a backup" as_kuben "$BIN" backup --out "$work/backups"
  local dir
  dir=$(find "$work/backups" -maxdepth 1 -name 'kuben-*' | sort | tail -n 1)
  check S03 "the backup is intact and restorable (restore --check)" as_kuben "$BIN" restore --from "$dir" --check
  manual S03 "copy the backup and keyring off-site, restore on a fresh host, check apps, keys and generations; record the RPO and RTO"
}

s04() {
  log "S04 access revocation"
  local created token id
  created=$(admin POST /tokens '{"name":"s04","role":"viewer","expires_in_days":1}')
  token=$(jq -r .token <<<"$created")
  id=$(jq -r .info.id <<<"$created")
  check S04 "a viewer token reads" expect 200 "$token" GET /projects
  check S04 "a viewer token cannot deploy" expect 403 "$token" POST "$(app_path staging web)/restart"
  admin DELETE "/tokens/$id" >/dev/null
  check S04 "a revoked token is refused" expect 401 "$token" GET /projects
  check S04 "a forged token is refused" expect 401 "kbn_forged" GET /projects
  check S04 "the audit log names no token" bash -c "
    ! admin GET '/audit?limit=200' | grep -qF '$token'"
  manual S04 "remove a member and downgrade a role mid-session; present CI tokens with a wrong issuer, audience, repository or ref, an expired one and a replayed one"
}

s05() {
  log "S05 freeze, pause, silence, emergency rollback"
  local env="/projects/$P/environments/prod" ends freeze gen
  ends=$(date -u -d '+2 hours' +%Y-%m-%dT%H:%M:%SZ)
  freeze=$(admin POST "$env/freezes" "{\"reason\":\"acceptance\",\"endsAt\":\"$ends\"}" | jq -r .id)
  gen=$(generation prod web)
  check S05 "a freeze refuses a release" expect 409 "$TOKEN" POST "$(app_path prod web)/deployments" \
    "{\"image\":\"$IMAGE2\",\"expected_generation\":$gen}"
  check S05 "an emergency rollback passes the freeze" expect 202 "$APPROVER" POST \
    "$(app_path prod web)/emergency-rollback" '{"reason":"acceptance drill"}'
  check S05 "the freeze is lifted once" expect 204 "$TOKEN" DELETE "$env/freezes/$freeze"
  check S05 "a pause holds the app" expect 204 "$TOKEN" POST "$(app_path staging web)/pause" '{"reason":"acceptance"}'
  check S05 "the app shows its pause" bash -c "
    admin GET '$(app_path staging web)' | jq -e '.paused != null'"
  check S05 "resume" expect 204 "$TOKEN" POST "$(app_path staging web)/resume"
  check S05 "a silence is kept" expect 201 "$TOKEN" POST "$env/silences" \
    "{\"reason\":\"acceptance\",\"endsAt\":\"$ends\",\"app\":\"web\"}"
  manual S05 "pause during a rollout and watch that nothing writes after the fence; resume and check that no webhook storm follows"
}

s06() {
  log "S06 pressure"
  check S06 "doctor passes under normal load" as_kuben "$BIN" doctor
  manual S06 "fill the build disk, stop the registry and the ACME issuer, flood builds: apps keep serving, accepted deployments are kept, doctor and incidents name the cause, recovery needs no database edit"
}

s07() {
  log "S07 upgrade and advisories"
  check S07 "upgrade-check passes for this binary" as_kuben "$BIN" upgrade-check
  check S07 "the bundle lock lists pinned digests" bash -c "'$BIN' version --bundle --json | jq -e '.k3s.version and .postgresql.digest'"
  check S07 "the app's scans are reported" expect 200 "$TOKEN" GET "$(app_path prod web)/scans"
  manual S07 "kill an upgrade after its backup and before migrating, then after migrating: the server resumes or refuses clearly; an agent two minors old is refused; every vulnerability exception has an owner and an expiry"
}

s08() {
  log "S08 export, detach, retaining uninstall"
  check S08 "export holds manifests and no secret values" bash -c "
    admin GET '$(app_path staging web)/export' | jq -e '.format == \"kuben.dev/export/v1\" and (.manifests.items | length > 0)'"
  local detached
  detached=$(admin POST "$(app_path staging web)/detach" '{"confirm":"web","reason":"acceptance"}' | jq -r '.id // empty')
  check S08 "detach is accepted" test -n "$detached"
  check S08 "the detach completes" eventually 180 bash -c "
    admin GET '/projects/$P/environments/staging/detached/$detached' | jq -e '.completedAt != null'"
  check S08 "the detached app still serves HTTPS" https_ok "$(host_of staging web)"
  check S08 "its Deployment has no owner left" bash -c "
    kubectl -n '$P-staging' get deploy -l kuben.dev/app=web -o json |
      jq -e '(.items | length > 0) and all(.items[]; (.metadata.ownerReferences // []) == [])'"
  check S08 "its Secrets are released from Kuben" bash -c "
    ! kubectl -n '$P-staging' get secret -l kuben.dev/secret-id,app.kubernetes.io/managed-by=kuben -o name | grep -q ."
  manual S08 "on a disposable server: kuben uninstall --purge --keep-apps; the app keeps pulling, serving and renewing with its new owner; the listed inventory is complete"
}

s09() {
  log "S09 support"
  check S09 "support-bundle --preview writes nothing" bash -c "
    as_kuben '$BIN' support-bundle --preview --out '$work/support' && [[ -z \$(ls -A '$work/support') ]]"
  check S09 "a support bundle is written, private and without the token" bash -c "
    as_kuben '$BIN' support-bundle --out '$work/support' >/dev/null &&
    f=\$(ls '$work'/support/kuben-support-*.json) &&
    [[ \$(stat -c %a \"\$f\") == 600 ]] && ! grep -qF '$TOKEN' \"\$f\""
  check S09 "making a bundle is audited" eventually 30 bash -c "
    admin GET '/audit?limit=200' | grep -q support.bundle.created"
  manual S09 "an operator who did not build Kuben diagnoses a seeded fault with the runbooks and a bundle only; recovery access (reset-admin) shows in the audit log"
}

s10() {
  log "S10 support envelope"
  check S10 "doctor reports the support envelope" bash -c "as_kuben '$BIN' doctor | grep -q 'support envelope'"
  check S10 "bundles carry the envelope, without a high-availability claim" bash -c "
    grep -q 'no high-availability claim' '$work'/support/kuben-support-*.json"
  manual S10 "record hardware and scale measurements (footprint from vps-acceptance.sh), the pilot team, period and outcome (§18.5)"
}

log "M4 acceptance on $URL as project $P"
for s in s01 s02 s03 s04 s05 s06 s07 s08 s09 s10; do
  "$s" || record "${s^^}" FAIL "the scenario stopped early"
done

pass=0 fail=0 open=0
for r in "${results[@]}"; do
  case "$r" in
  *"| PASS |"*) pass=$((pass + 1)) ;;
  *"| FAIL |"*) fail=$((fail + 1)) ;;
  *) open=$((open + 1)) ;;
  esac
done
cat <<EOF
# Kuben M4 acceptance (S01–S10)

| | |
|---|---|
| Date | $(date -u +%Y-%m-%dT%H:%M:%SZ) |
| Kuben | $("$BIN" version | head -n 1) |
| Host | $(uname -srm) |
| Kubernetes | $(kubectl version -o json 2>/dev/null | jq -r '.serverVersion.gitVersion // "unknown"') |
| Console | $URL |
| Project | $P |
| Reviewer | $REVIEWER |
| Result | $pass passed, $fail failed, $open manual |

| Scenario | Result | Check |
|---|---|---|
EOF
printf '%s\n' "${results[@]}"
cat <<'EOF'

Manual rows need their own evidence (logs, timings, screenshots) and the
reviewer's sign-off before the Supported MVP gate is passed.
EOF
((fail == 0))
