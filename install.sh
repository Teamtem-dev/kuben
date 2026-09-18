#!/bin/sh
# Kuben installer — https://github.com/Teamtem-dev/kuben
#
#   curl -fsSL https://kuben.teamtem.com/install.sh | sh
#
# On a Linux server, as root: installs the kuben binary and runs `kuben setup`,
# which installs k3s when there is no cluster, creates the service user, the
# configuration and the systemd service, opens the firewall and prints the
# link to the setup page. Run it again to upgrade. Anywhere else (a
# workstation, a non-root shell, macOS) it installs the binary only.
#
# Options (flag or environment variable):
#   --version <tag>   KUBEN_VERSION=v1.0.0         release to install (default: latest)
#   --dir <path>      KUBEN_INSTALL_DIR=<path>     install directory (default: /usr/local/bin)
#   --no-sudo         KUBEN_NO_SUDO=1              never escalate; fail if <dir> is not writable
#   --binary-only     KUBEN_BINARY_ONLY=1          install the binary, do not run `kuben setup`
#   --require-signature KUBEN_REQUIRE_SIGNATURE=1  refuse to install without a verified signature
#   --uninstall                                    remove the server set up by `kuben setup`
#   -h, --help
#   Every other option goes to `kuben setup`: --plan, --domain <domain>,
#   --acme-email <email>, --acme-staging, --datastore sqlite|etcd, --port <n>,
#   --kubeconfig <file>, --no-k3s, --bind-local, --allow-http-setup, --yes.
#
# Guarantees:
#   * HTTPS only (TLS >= 1.2), no redirects to plain HTTP.
#   * The archive is verified against the release's checksums.txt (SHA-256)
#     BEFORE anything is extracted or installed. When cosign is installed,
#     checksums.txt itself is verified first: its keyless Sigstore signature
#     must come from this repository's release workflow for that tag.
#   * To read before running: download this file, check it against the
#     release's checksums.txt (and its signature), then run it.
#   * The body is wrapped in main(), which runs only on the last line, so a
#     truncated download cannot execute a partial script.
#   * No shell state is left behind: everything happens in a temp dir that is
#     removed on exit.
#   * Nothing but GitHub (the release; through `kuben setup` also k3s's
#     pinned, checksum-verified installer and the Gateway API CRDs) and, for
#     cert-manager, charts.jetstack.io is contacted. No telemetry.

set -eu

REPO="Teamtem-dev/kuben"
BIN="kuben"

if [ -t 2 ]; then
  BOLD=$(printf '\033[1m')
  RED=$(printf '\033[31m')
  GREEN=$(printf '\033[32m')
  ORANGE=$(printf '\033[38;5;208m')
  DIM=$(printf '\033[2m')
  RESET=$(printf '\033[0m')
else
  BOLD="" RED="" GREEN="" ORANGE="" DIM="" RESET=""
fi

say() { printf '%s✔%s %s\n' "$GREEN" "$RESET" "$*" >&2; }
note() { printf '  %s%s%s\n' "$DIM" "$*" "$RESET" >&2; }
err() {
  printf '%s✖%s %s\n' "$RED" "$RESET" "$*" >&2
  exit 1
}
has() { command -v "$1" >/dev/null 2>&1; }

banner() {
  printf '\n' >&2
  printf '  %s██╗  ██╗██╗   ██╗██████╗ ███████╗███╗   ██╗%s\n' "$ORANGE" "$RESET" >&2
  printf '  %s██║ ██╔╝██║   ██║██╔══██╗██╔════╝████╗  ██║%s\n' "$ORANGE" "$RESET" >&2
  printf '  %s█████╔╝ ██║   ██║██████╔╝█████╗  ██╔██╗ ██║%s\n' "$ORANGE" "$RESET" >&2
  printf '  %s██╔═██╗ ██║   ██║██╔══██╗██╔══╝  ██║╚██╗██║%s\n' "$ORANGE" "$RESET" >&2
  printf '  %s██║  ██╗╚██████╔╝██████╔╝███████╗██║ ╚████║%s\n' "$ORANGE" "$RESET" >&2
  printf '  %s╚═╝  ╚═╝ ╚═════╝ ╚═════╝ ╚══════╝╚═╝  ╚═══╝%s\n' "$ORANGE" "$RESET" >&2
  if [ -n "${1:-}" ]; then
    printf '  %s%s · a Kubernetes PaaS in a single binary · kuben.teamtem.com%s\n\n' "$DIM" "$1" "$RESET" >&2
  else
    printf '  %sa Kubernetes PaaS in a single binary · kuben.teamtem.com%s\n\n' "$DIM" "$RESET" >&2
  fi
}

spin_task() { # <label> <command...>
  label=$1
  shift
  if [ -t 2 ]; then
    "$@" &
    pid=$!
    frames='⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏'
    while kill -0 "$pid" 2>/dev/null; do
      for f in $frames; do
        printf '\r%s%s%s %s' "$ORANGE" "$f" "$RESET" "$label" >&2
        sleep 0.08 2>/dev/null || sleep 1
        kill -0 "$pid" 2>/dev/null || break
      done
    done
    printf '\r\033[2K' >&2
    wait "$pid"
  else
    printf '  %s...\n' "$label" >&2
    "$@"
  fi
}

# Not read from "$0": under `curl … | sh` that is the shell, not this file.
usage() {
  cat <<'EOF'
usage: install.sh [--version <tag>] [--dir <path>] [--no-sudo] [--binary-only] [--uninstall] [setup options]

  --version <tag>   KUBEN_VERSION=v1.0.0       release to install (default: latest)
  --dir <path>      KUBEN_INSTALL_DIR=<path>   install directory (default: /usr/local/bin)
  --no-sudo         KUBEN_NO_SUDO=1            never escalate; fail if <dir> is not writable
  --binary-only     KUBEN_BINARY_ONLY=1        install the binary, do not run `kuben setup`
  --require-signature                          KUBEN_REQUIRE_SIGNATURE=1: fail unless cosign verifies the release
  --uninstall                                  remove the server set up by `kuben setup`

  Other options go to `kuben setup`: --plan, --domain <domain>, --acme-email <email>, --acme-staging,
  --datastore sqlite|etcd, --port <n>, --kubeconfig <file>, --no-k3s, --bind-local, --allow-http-setup, --yes
EOF
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
    err "on Windows download kuben-x86_64-pc-windows-msvc.zip from https://github.com/${REPO}/releases"
    ;;
  *) err "unsupported operating system: $os" ;;
  esac
  case "$arch" in
  x86_64 | amd64) arch_part="x86_64" ;;
  aarch64 | arm64) arch_part="aarch64" ;;
  *) err "unsupported CPU architecture: $arch" ;;
  esac
  echo "${arch_part}-${os_part}"
}

download() { # <url> <dest>
  if has curl; then
    curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
      --retry 3 --retry-connrefused --output "$2" "$1"
  elif has wget; then
    wget --https-only --quiet --tries=3 --output-document="$2" "$1"
  else
    err "curl or wget is required"
  fi
}

latest_tag() {
  if has curl; then
    # Follow the /releases/latest redirect: no API rate limit, no JSON parsing.
    url=$(curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
      --head --output /dev/null --write-out '%{url_effective}' \
      "https://github.com/${REPO}/releases/latest") || err "could not reach github.com"
    tag=${url##*/}
  else
    download "https://api.github.com/repos/${REPO}/releases/latest" "$tmp/latest.json"
    tag=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)
  fi
  case "$tag" in
  v[0-9]*) echo "$tag" ;;
  *) err "could not determine the latest release of ${REPO} (is there a published release yet?)" ;;
  esac
}

sha256_of() {
  if has sha256sum; then
    sha256sum "$1" | cut -d ' ' -f 1
  elif has shasum; then
    shasum -a 256 "$1" | cut -d ' ' -f 1
  elif has openssl; then
    openssl dgst -sha256 -r "$1" | cut -d ' ' -f 1
  else
    err "no SHA-256 tool found (need sha256sum, shasum or openssl)"
  fi
}

install_binary() { # <src> <dir>
  sudo=""
  if [ -d "$2" ]; then
    [ -w "$2" ] || sudo="sudo"
  elif ! mkdir -p "$2" 2>/dev/null; then
    sudo="sudo"
  fi
  if [ -n "$sudo" ]; then
    [ "$no_sudo" = "1" ] && err "$2 is not writable; re-run with --dir \"\$HOME/.local/bin\""
    has sudo || err "$2 is not writable and sudo is not available; use --dir <writable dir>"
    note "$2 is not writable, using sudo"
  fi
  $sudo mkdir -p "$2"
  $sudo install -m 0755 "$1" "$2/$BIN"
}

# checksums.txt of <version>, verified with cosign against the release
# workflow's keyless signature. Without cosign: a note, or a failure when a
# signature is required.
verify_signature() { # <checksums> <version>
  if ! has cosign; then
    [ "$require_signature" = "1" ] && err "cosign is required to verify the release (https://docs.sigstore.dev/cosign/system_config/installation/)"
    note "cosign not found: checksums.txt was not signature-checked (install cosign to have it checked)"
    return 0
  fi
  if ! download "${base}/checksums.txt.sigstore.json" "${tmp}/checksums.txt.sigstore.json" 2>/dev/null; then
    [ "$require_signature" = "1" ] && err "release $2 has no signature (checksums.txt.sigstore.json)"
    note "release $2 is not signed (it predates signed releases): checksums.txt was not signature-checked"
    return 0
  fi
  # The release workflow, run for this tag (or rebuilt from main).
  tag_re=$(printf '%s' "$2" | sed 's/[.+]/\\&/g')
  cosign verify-blob --bundle "${tmp}/checksums.txt.sigstore.json" \
    --certificate-identity-regexp "^https://github[.]com/${REPO}/[.]github/workflows/release[.]yml@refs/(tags/${tag_re}|heads/main)\$" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    "$1" >/dev/null 2>&1 || err "the signature of checksums.txt does not verify for ${REPO} $2: nothing was installed"
  say "Signature verified (Sigstore, ${REPO} release workflow, $2)"
}

# A Linux server with systemd, as root: `kuben setup` can do the rest.
can_setup() {
  [ "$(uname -s)" = Linux ] && [ "$(id -u)" = 0 ] && [ -d /run/systemd/system ]
}

main() {
  version=${KUBEN_VERSION:-}
  dir=${KUBEN_INSTALL_DIR:-/usr/local/bin}
  no_sudo=${KUBEN_NO_SUDO:-0}
  binary_only=${KUBEN_BINARY_ONLY:-0}
  require_signature=${KUBEN_REQUIRE_SIGNATURE:-0}
  uninstall=0
  setup_args=""

  while [ $# -gt 0 ]; do
    case "$1" in
    --version)
      [ $# -ge 2 ] || err "--version needs a value"
      version=$2
      shift 2
      ;;
    --dir)
      [ $# -ge 2 ] || err "--dir needs a value"
      dir=$2
      shift 2
      ;;
    --no-sudo)
      no_sudo=1
      shift
      ;;
    --binary-only)
      binary_only=1
      shift
      ;;
    --require-signature)
      require_signature=1
      shift
      ;;
    --uninstall)
      uninstall=1
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    --port | --kubeconfig | --domain | --acme-email | --datastore)
      [ $# -ge 2 ] || err "$1 needs a value"
      setup_args="$setup_args $1 $2"
      shift 2
      ;;
    --no-k3s | --bind-local | --yes | -y | --plan | --acme-staging | --allow-http-setup)
      setup_args="$setup_args $1"
      shift
      ;;
    *) err "unknown option: $1 (see --help)" ;;
    esac
  done

  if [ "$uninstall" = 1 ]; then
    [ -x "${dir}/${BIN}" ] || err "${dir}/${BIN} is not installed"
    can_setup || err "run the uninstall as root on the server"
    # shellcheck disable=SC2086
    exec "${dir}/${BIN}" uninstall $setup_args
  fi

  for cmd in uname tar mktemp install awk; do has "$cmd" || err "required command not found: $cmd"; done
  umask 022

  tmp=$(mktemp -d "${TMPDIR:-/tmp}/kuben.XXXXXX")
  trap 'rm -rf "$tmp"' EXIT
  trap 'exit 130' INT TERM

  target=$(detect_target)
  banner "${version:-}"
  [ -n "$version" ] || version=$(latest_tag)
  case "$version" in v*) ;; *) version="v${version}" ;; esac

  archive="${BIN}-${target}.tar.gz"
  base="https://github.com/${REPO}/releases/download/${version}"

  spin_task "Downloading ${BIN} ${version} (${target})" download "${base}/${archive}" "${tmp}/${archive}" ||
    err "download failed: ${base}/${archive} — release ${version} has no ${archive}; see https://github.com/${REPO}/releases/tag/${version}"
  download "${base}/checksums.txt" "${tmp}/checksums.txt" || err "download failed: ${base}/checksums.txt"
  verify_signature "${tmp}/checksums.txt" "$version"

  expected=$(awk -v f="$archive" '{ n = $2; sub(/^\*/, "", n) } n == f { print $1; exit }' "${tmp}/checksums.txt")
  [ -n "$expected" ] || err "${archive} is not listed in checksums.txt"
  actual=$(sha256_of "${tmp}/${archive}")
  [ "$expected" = "$actual" ] || err "checksum mismatch for ${archive}: expected ${expected}, got ${actual}"

  tar -xzf "${tmp}/${archive}" -C "$tmp" "$BIN" || err "archive does not contain '${BIN}'"
  install_binary "${tmp}/${BIN}" "$dir"

  say "Installed ${BIN} ${version} (${target}) to ${dir}/${BIN}. ${DIM}sha256 ${actual}${RESET}"

  if [ "$binary_only" != 1 ] && can_setup; then
    KUBEN_INSTALLED="${version} ${target} ${dir}/${BIN} ${actual}"
    KUBEN_BANNER_PRINTED=1
    export KUBEN_INSTALLED KUBEN_BANNER_PRINTED
    # shellcheck disable=SC2086
    exec "${dir}/${BIN}" setup $setup_args
  fi

  case ":${PATH}:" in
  *":${dir}:"*) ;;
  *) note "${dir} is not on your PATH" ;;
  esac
  if [ "$binary_only" != 1 ]; then
    if [ "$(uname -s)" = Linux ]; then
      note "Binary only: run as root on a server with systemd to set it up (sudo ${BIN} setup)."
    else
      note "Binary only: \`${BIN} setup\` sets up a Linux server; here, run \`${BIN} serve\` against a kubeconfig."
    fi
  fi
  printf '%s\n' "${BOLD}Next:${RESET} ${BIN} doctor   ${DIM}(checks the database and the cluster connection)${RESET}" >&2
  note "Guide: https://kuben.teamtem.com/docs/getting-started/binary/"
}

main "$@"
