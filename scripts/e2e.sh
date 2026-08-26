#!/usr/bin/env bash
# End-to-end test: a real cluster (kind), the real binary, the public API.
#
#   scripts/e2e.sh                     # uses ./target/debug/kuben and the current kube context
#   KUBEN_BIN=target/release/kuben scripts/e2e.sh
#
# Exercises: CRD self-apply, the controller Lease, login, project → environment → namespace with
# quota/limits/isolation, app deploy → Deployment/Service rollout, scale,
# logs, restart, and the day-2 scenarios of blueprint §5.9: releases and
# rollback, API tokens, audit, cron jobs with "run now", volumes that survive
# app deletion, templates, promotion, domain checks, team invitations and
# login throttling; then deletes and garbage collection.
set -euo pipefail

BIN=${KUBEN_BIN:-target/debug/kuben}
PORT=${KUBEN_E2E_PORT:-18080}
BASE="http://127.0.0.1:${PORT}/api/v1"
PASSWORD="e2e-$(date +%s)-password"
IMAGE=${KUBEN_E2E_IMAGE:-nginxinc/nginx-unprivileged:1.27-alpine}
JOB_IMAGE=${KUBEN_E2E_JOB_IMAGE:-busybox:1.36}
P=e2e
LEASE_NS=${KUBEN_E2E_LEASE_NAMESPACE:-default}
NS="kb-${P}-dev"
NS_LIVE="kb-${P}-live"
APP="/projects/${P}/environments/dev/apps"

need() { command -v "$1" >/dev/null || { echo "missing: $1" >&2; exit 2; }; }
for c in kubectl curl jq; do need "$c"; done
[[ -x $BIN ]] || { echo "build the binary first: cargo build -p kuben ($BIN)" >&2; exit 2; }

work=$(mktemp -d)
cleanup() {
