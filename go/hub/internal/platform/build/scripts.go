package build

// The fixed scripts of a build pod, byte for byte the Rust constants of
// crates/kuben-platform/src/build/job.rs (TestScriptsAreRustsBytes pins
// them). Untrusted values reach them only as environment variables.

const (
	// fetchScript fetches exactly one commit with the fetch token, whose only mount
	// is in the fetch container.
	fetchScript = `set -eu
export HOME=/workspace/home
mkdir -p "$HOME" /workspace/src
cd /workspace/src
git init -q .
auth=$(printf 'x-access-token:%s' "$(cat /var/run/kuben/source/token)" | base64 | tr -d '\n')
git -c "http.extraHeader=Authorization: Basic $auth" fetch -q --depth 1 --no-tags "$KUBEN_CLONE_URL" "$KUBEN_COMMIT"
git checkout -q --detach FETCH_HEAD
test "$(git rev-parse HEAD)" = "$KUBEN_COMMIT"
rm -rf .git
`
	// planScript picks the strategy and writes the Railpack plan; a context
	// that leaves the checkout (a symlink) is refused.
	planScript = `set -eu
fail() { printf '%s' "$1" >/dev/termination-log; exit 2; }
inside() { case "$1" in /workspace/src|/workspace/src/*) ;; *) fail "$2 leaves the repository";; esac; }
[ -d "/workspace/src/$KUBEN_CONTEXT" ] || fail "the build context '$KUBEN_CONTEXT' does not exist"
ctx=$(cd "/workspace/src/$KUBEN_CONTEXT" && pwd -P)
inside "$ctx" "the build context"
strategy=$KUBEN_STRATEGY
if [ "$strategy" = auto ]; then
  if [ -f "$ctx/$KUBEN_DOCKERFILE" ]; then strategy=dockerfile; else strategy=railpack; fi
fi
if [ "$strategy" = dockerfile ]; then
  [ -f "$ctx/$KUBEN_DOCKERFILE" ] || fail "there is no '$KUBEN_DOCKERFILE' in the build context"
else
  command -v railpack >/dev/null 2>&1 || fail "the repository has no Dockerfile and Railpack is not configured on this server (build.railpack_image and build.railpack_frontend)"
  mkdir -p /workspace/plan
  railpack prepare "$ctx" --plan-out /workspace/plan/railpack-plan.json
fi
printf '%s' "$strategy" >/workspace/strategy
`
	// buildScript builds and pushes with rootless BuildKit and reports the
	// digest; a context or Dockerfile directory outside the checkout is
	// refused before BuildKit reads it.
	buildScript = `set -eu
fail() { printf '%s' "$1" >/dev/termination-log; exit 2; }
inside() { case "$1" in /workspace/src|/workspace/src/*) ;; *) fail "$2 leaves the repository";; esac; }
strategy=$(cat /workspace/strategy)
ctx=$(cd "/workspace/src/$KUBEN_CONTEXT" && pwd -P)
inside "$ctx" "the build context"
if [ "$strategy" = dockerfile ]; then
  dir=$(cd "$(dirname "$ctx/$KUBEN_DOCKERFILE")" && pwd -P)
  inside "$dir" "the Dockerfile"
  set -- --frontend dockerfile.v0 --local "dockerfile=$dir" --opt "filename=$(basename "$KUBEN_DOCKERFILE")"
else
  set -- --frontend gateway.v0 --opt "source=$KUBEN_RAILPACK_FRONTEND" --local dockerfile=/workspace/plan
fi
output="type=image,name=$KUBEN_IMAGE,push=true"
if [ "$KUBEN_INSECURE_REGISTRY" = true ]; then output="$output,registry.insecure=true"; fi
buildctl-daemonless.sh build "$@" --local "context=$ctx" --output "$output" --metadata-file /workspace/metadata.json
digest=$(tr ',' '\n' </workspace/metadata.json | sed -n 's/.*"containerimage\.digest": *"\(sha256:[0-9a-f]\{64\}\)".*/\1/p' | head -n 1)
[ -n "$digest" ] || fail "the build wrote no image digest"
printf '%s' "$digest" >/workspace/digest
printf '{"digest":"%s","strategy":"%s"}' "$digest" "$strategy" >/dev/termination-log
`
	// ScanScript writes the SBOM of the pushed image and scans it. It always
	// exits 0: a scan that cannot run reports `unavailable`, never a clean
	// image. The SBOM goes to the log (gzip, base64, between markers); the
	// summary to the termination log, which holds 4 KiB.
	//
	// The database date is read with sed's `\1`. Kuben 1.2.0 carried a raw
	// byte 0x01 there instead, so every report held a control character, did
	// not parse, and every scan counted as unavailable (the scan gate never
	// saw a finding); TestTheScanReportCarriesTheDatabaseDate pins the fix.
	ScanScript = `set -u
report() { printf '{"status":"unavailable","scanner":"%s","detail":"%s"}' "${scanner:-}" "$1" >/dev/termination-log; exit 0; }
cd /workspace
export TRIVY_CACHE_DIR=/workspace/trivy TRIVY_NO_PROGRESS=true TRIVY_DISABLE_VEX_NOTICE=true
scanner="trivy $(trivy --version 2>/dev/null | sed -n 's/^Version: *//p' | head -n 1)"
digest=${KUBEN_DIGEST:-$(cat /workspace/digest 2>/dev/null)}
case "$digest" in sha256:*) ;; *) report "no image digest to scan";; esac
insecure=""
if [ "$KUBEN_INSECURE_REGISTRY" = true ]; then insecure="--insecure"; fi
trivy image --quiet $insecure --format cyclonedx --output sbom.json "$KUBEN_REPOSITORY@$digest" 2>scan.err || report "the SBOM could not be written"
trivy sbom --quiet --scanners vuln --format template   --template '{{ range . }}{{ range .Vulnerabilities }}{{ .Severity }}:{{ .VulnerabilityID }}{{ "
" }}{{ end }}{{ end }}'   --output found.txt sbom.json 2>scan.err || report "the vulnerability database is not available"
db=$(trivy version --format json 2>/dev/null | tr ',' '
' | sed -n 's/.*"UpdatedAt": *"\([^"]*\)".*/\1/p' | head -n 1)
sort -u found.txt | tr -cd 'A-Za-z0-9:._
-' >unique.txt
n() { grep -c "^$1:" unique.txt || true; }
findings=$( (grep '^CRITICAL:' unique.txt; grep '^HIGH:' unique.txt) | head -n 40 | sed 's/.*/"&"/' | paste -sd, -)
printf '{"status":"ok","scanner":"%s","db":"%s","counts":{"critical":%s,"high":%s,"medium":%s,"low":%s,"unknown":%s},"findings":[%s]}'   "$scanner" "$db" "$(n CRITICAL)" "$(n HIGH)" "$(n MEDIUM)" "$(n LOW)" "$(n UNKNOWN)" "$findings" >/dev/termination-log
echo kuben-sbom-begin
gzip -c sbom.json | base64 | tr -d '
'
echo
echo kuben-sbom-end
`
)
