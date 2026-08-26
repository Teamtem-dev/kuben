#!/bin/sh
# Kuben installer — https://github.com/Teamtem-dev/kuben
#
#   curl -fsSL https://raw.githubusercontent.com/Teamtem-dev/kuben/main/install.sh | bash
#
# Options (flag or environment variable):
#   --version <tag>   KUBEN_VERSION=v0.1.0         release to install (default: latest)
#   --dir <path>      KUBEN_INSTALL_DIR=<path>     install directory (default: /usr/local/bin)
#   --no-sudo         KUBEN_NO_SUDO=1              never escalate; fail if <dir> is not writable
#   -h, --help
#
# Guarantees:
#   * HTTPS only (TLS >= 1.2), no redirects to plain HTTP.
#   * The archive is verified against the release's checksums.txt (SHA-256)
#     BEFORE anything is extracted or installed.
#   * The body is wrapped in main(), which runs only on the last line, so a
#     truncated download cannot execute a partial script.
#   * No shell state is left behind: everything happens in a temp dir that is
#     removed on exit.

set -eu

REPO="Teamtem-dev/kuben"
BIN="kuben"

if [ -t 2 ]; then BOLD=$(printf '\033[1m'); RED=$(printf '\033[31m'); RESET=$(printf '\033[0m'); else BOLD=""; RED=""; RESET=""; fi
