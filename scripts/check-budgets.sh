#!/usr/bin/env bash
# Size budgets are CI gates, not aspirations (blueprint §5.7).
#
#   scripts/check-budgets.sh binary <path> [max MiB]   default: $KUBEN_BUDGET_BINARY_MB or 25
#   scripts/check-budgets.sh image  <ref>  [max MiB]   default: $KUBEN_BUDGET_IMAGE_MB  or 30
#
# The web bundle budget is enforced by size-limit (`pnpm size`).
set -euo pipefail

mib() { awk -v b="$1" 'BEGIN { printf "%.2f", b / 1048576 }'; }

