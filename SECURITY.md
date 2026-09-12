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

Only the latest release receives security fixes.

## Verifying releases

Release archives and container images carry GitHub build provenance
attestations, and every archive is listed in `checksums.txt`:

```bash
gh attestation verify kuben-x86_64-unknown-linux-musl.tar.gz --repo Teamtem-dev/kuben
gh attestation verify oci://ghcr.io/teamtem-dev/kuben:<version> --repo Teamtem-dev/kuben
```

## What is inside a binary

Release binaries are built with
[cargo-auditable](https://github.com/rust-secure-code/cargo-auditable): the
exact list of crates they contain is embedded in the binary itself (about
6 KiB). Your own scanner can audit what you run, without the source:

```bash
cargo audit bin /usr/local/bin/kuben
trivy image ghcr.io/teamtem-dev/kuben:<version>
```

## How the project checks itself

- **Every pull request:** cargo-deny (licenses, advisories, bans, sources),
  zizmor on the GitHub Actions workflows, Trivy on the Helm chart and on the
  release binary.
- **Every release:** Trivy on the image before it is pushed; nothing is
  published while a HIGH or CRITICAL vulnerability with a fix remains.
- **Every day:** RustSec advisories on `main` and Trivy on the published
  image. See [docs/ci-cd.md](docs/ci-cd.md#4-daily-security-checks).
