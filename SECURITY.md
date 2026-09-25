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

Only the latest release receives security fixes. A fix that cannot wait for
the next release ships as a patch release of the latest minor version, with an
advisory naming the affected versions and the upgrade path.

## Handling a report

Every confirmed vulnerability gets an owner, a severity (CVSS) and a target
date: critical within 7 days, high within 14, others with the next release.
When a fix cannot ship in time, the advisory documents the exception: who
owns it, why it is acceptable, the mitigation and the date it expires. An
exception never outlives 90 days without a new review.

## Software bill of materials

Every release publishes a CycloneDX SBOM of its binaries
(`kuben-<target>.cdx.json`, listed in the signed `checksums.txt`), and the
container image carries its SBOM attestation.

Kuben itself scans what it builds: every built image gets an SBOM and a
vulnerability scan by digest, running images are rescanned daily with a fresh
database, and each environment's policy decides whether known findings warn
or block a deployment. A scan that could not run is recorded as
*unavailable*, never as clean; exceptions are owned and expire.

## Verifying releases

Release archives and container images carry GitHub build provenance
attestations, and every archive is listed in `checksums.txt`:

```bash
gh attestation verify kuben-x86_64-unknown-linux-musl.tar.gz --repo Teamtem-dev/kuben
gh attestation verify oci://ghcr.io/teamtem-dev/kuben:<version> --repo Teamtem-dev/kuben
```

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
  image. See [docs/ci-cd.md](docs/ci-cd.md#4-daily-security-checks).
