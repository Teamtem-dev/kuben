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

say() { printf '%skuben:%s %s\n' "$BOLD" "$RESET" "$*" >&2; }
err() {
  printf '%skuben: error:%s %s\n' "$RED" "$RESET" "$*" >&2
  exit 1
}
has() { command -v "$1" >/dev/null 2>&1; }

usage() {
  sed -n '2,12p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' ||
    echo "usage: install.sh [--version <tag>] [--dir <path>] [--no-sudo]"
}

detect_target() {
  os=$(uname -s)
  arch=$(uname -m)
  case "$os" in
  Linux) os_part="unknown-linux-musl" ;;
  Darwin)
    os_part="apple-darwin"
    # An x86_64 shell under Rosetta 2 on Apple Silicon still gets the native binary.
    if [ "$arch" = "x86_64" ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = "1" ]; then
      arch="arm64"
    fi
    ;;
  MINGW* | MSYS* | CYGWIN*)
