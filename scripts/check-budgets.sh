#!/usr/bin/env bash
# Size budgets (blueprint §5.7). Since the Go rewrite they warn instead of
# failing: correctness and standard libraries come before binary size
# (plan v2.1, owner decision); an exceeded budget is annotated, not fatal.
#
#   scripts/check-budgets.sh binary <path> [max MiB]   default: $KUBEN_BUDGET_BINARY_MB or 45
#   scripts/check-budgets.sh image  <ref>  [max MiB]   default: $KUBEN_BUDGET_IMAGE_MB  or 50
#
# The web bundle budget is enforced by size-limit (`turbo run size --filter=@kuben/console`).
set -euo pipefail

mib() { awk -v b="$1" 'BEGIN { printf "%.2f", b / 1048576 }'; }

report() { # <label> <bytes> <limit MiB>
  local label=$1 bytes=$2 limit_mib=$3
  local verdict=ok
  if ((bytes > limit_mib * 1048576)); then verdict=OVER; fi
  local line # declared separately: assigning a command substitution masks its exit status (SC2155)
  line="${label}: $(mib "$bytes") MiB (budget ${limit_mib} MiB) — ${verdict}"
  echo "$line"
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then echo "- ${line}" >>"$GITHUB_STEP_SUMMARY"; fi
  if [[ $verdict == OVER && -n ${GITHUB_ACTIONS:-} ]]; then
    echo "::warning title=Size budget exceeded::${line}"
  fi
}

kind=${1:-}
target=${2:-}
case "$kind" in
binary)
  [[ -f $target ]] || { echo "no such binary: $target" >&2; exit 2; }
  report "binary $(basename "$target")" "$(wc -c <"$target" | tr -d ' ')" "${3:-${KUBEN_BUDGET_BINARY_MB:-45}}"
  # A linked text/template engine calls methods by name, so the linker keeps
  # every exported method of every reachable type: about 30 MiB more for the
  # hub (one cobra SetVersionTemplate did it). That is a defect, not a budget.
  # Function names stay in Go's function table (.gopclntab) even in a
  # stripped (-s -w) binary, so a byte search works without symbols.
  if LC_ALL=C grep -aqF 'text/template.(*Template).Execute' "$target"; then
    echo "binary $(basename "$target") links text/template's Execute: dead-code elimination of methods is off (a cobra Set*Template call or html/template?)" >&2
    if [[ -n ${GITHUB_ACTIONS:-} ]]; then echo "::error title=text/template linked::$(basename "$target") links text/template's Execute"; fi
    exit 1
  fi
  ;;
image)
  bytes=$(docker image inspect --format '{{.Size}}' "$target")
  report "image ${target}" "$bytes" "${3:-${KUBEN_BUDGET_IMAGE_MB:-50}}"
  ;;
*)
  sed -n '3,6p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
  ;;
esac
