#!/usr/bin/env bash
# Misconfiguration scan of the Helm chart (Trivy), rendered like a real install.
#
#   scripts/scan-chart.sh
#
# Fails on HIGH or CRITICAL findings that .trivyignore.yaml does not justify.
# In GitHub Actions every finding also becomes an annotation on the file.
set -euo pipefail
cd "$(dirname "$0")/.."

# The chart requires Kubernetes >= 1.29; without a version Trivy cannot render
# the templates and would scan only the CRDs.
report=$(trivy config --quiet --format json --severity HIGH,CRITICAL \
  --helm-kube-version 1.32.0 --ignorefile .trivyignore.yaml charts/kuben)

findings=$(jq -r '.Results[]? | .Target as $t | .Misconfigurations[]?
  | select(.Status == "FAIL") | [$t, .Severity, .ID, .Title] | @tsv' <<<"$report")
if [[ -z $findings ]]; then
  echo "chart: no HIGH or CRITICAL misconfigurations"
  exit 0
fi

while IFS=$'\t' read -r target severity id title; do
  echo "${severity} ${id} ${title} (${target})"
  if [[ -n ${GITHUB_ACTIONS:-} ]]; then
    echo "::error file=charts/kuben/${target},title=${id} (${severity})::${title}"
  fi
done <<<"$findings"
echo "fix the chart, or justify the exception in .trivyignore.yaml" >&2
exit 1
