# Security policy

## Reporting a vulnerability

Please **do not open a public issue**. Report vulnerabilities privately through
GitHub: *Security → Report a vulnerability* on
[Teamtem-dev/kuben](https://github.com/Teamtem-dev/kuben/security/advisories/new).

Include the affected version (`kuben version`), the impact, and steps or a
proof of concept to reproduce it. We acknowledge reports within 3 working days
and aim to ship a fix for confirmed high-severity issues within 14 days. We
credit reporters in the advisory unless you ask us not to.

## Supported versions

| Version | Status | Receives |
|---|---|---|
| 2.x pre-releases (`2.0.0-alpha.N`, `-beta.N`, `-rc.N`) | testing, from `main` | fixes in the next pre-release; no patch releases of a pre-release |
| 1.2.x | stable | security and critical fixes as 1.2.N patch releases from the [`release/1.2`](https://github.com/Teamtem-dev/kuben/tree/release/1.2) branch, until six months after 2.0.0 is released; then none |
| 1.1 and older | unsupported | nothing: upgrade to 1.2.x |

Once 2.0.0 is out, the newest 2.x minor receives security fixes as patch
releases. A fix that cannot wait for the next release ships as a patch
release of each supported line, with an advisory naming the affected
versions and the upgrade path.

## Handling a report

Every confirmed vulnerability gets an owner, a severity (CVSS) and a target
date: critical within 7 days, high within 14, others with the next release.
When a fix cannot ship in time, the advisory documents the exception: who
owns it, why it is acceptable, the mitigation and the date it expires. An
exception never outlives 90 days without a new review.

## Software bill of materials

Every release publishes a CycloneDX SBOM of each archive
(`kuben-<target>.cdx.json`, listed in the signed `checksums.txt`), and the
container image carries its SBOM and provenance attestations.

Kuben itself scans what it builds: every built image gets an SBOM and a
vulnerability scan by digest, running images are rescanned daily with a fresh
database, and each environment's policy decides whether known findings warn
or block a deployment. A scan that could not run is recorded as
*unavailable*, never as clean; exceptions are owned and expire.

## Verifying releases

Every archive, SBOM, `install.sh` and `bundle.lock.json` of a release is
listed in `checksums.txt`, which is signed keyless with Sigstore
(`checksums.txt.sigstore.json`; the certificate names the release workflow
and the tag). The image and the Helm chart are signed the same way, and the
archives and the image carry GitHub build provenance attestations:

```bash
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github[.]com/Teamtem-dev/kuben/[.]github/workflows/release[.]yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
sha256sum --check --ignore-missing checksums.txt

gh attestation verify kuben-x86_64-unknown-linux-musl.tar.gz --repo Teamtem-dev/kuben
gh attestation verify oci://ghcr.io/teamtem-dev/kuben:<version> --repo Teamtem-dev/kuben
```

`install.sh` runs the same signature check whenever cosign is installed
(`--require-signature` makes it mandatory).

## What is inside a binary

Kuben 2.x is written in Go, and Go embeds the exact list of modules (with
their versions and checksums) in every binary it builds. Your own scanner can
audit what you run, without the source:

```bash
go version -m /usr/local/bin/kuben
govulncheck -mode=binary /usr/local/bin/kuben
trivy image ghcr.io/teamtem-dev/kuben:<version>
```

The 1.x binaries (Rust) embed their crate list with cargo-auditable instead;
`cargo audit bin` reads it.

## How the project checks itself

- **Every pull request:** govulncheck on the Go modules, zizmor on the GitHub
  Actions workflows, Trivy on the Helm chart and on the release binary.
- **Every release:** Trivy on the image before it is pushed; nothing is
  published while a HIGH or CRITICAL vulnerability with a fix remains.
- **Every day:** govulncheck on `main` and Trivy on the published
  image. See [CI/CD and releases](https://kuben.teamtem.com/docs/contributing/ci-cd/#every-day).
