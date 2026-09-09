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

Kuben is pre-1.0: only the latest release receives security fixes.

## Verifying releases

Release archives and container images carry GitHub build provenance
attestations, and every archive is listed in `checksums.txt`:

```bash
gh attestation verify kuben-x86_64-unknown-linux-musl.tar.gz --repo Teamtem-dev/kuben
gh attestation verify oci://ghcr.io/teamtem-dev/kuben:<version> --repo Teamtem-dev/kuben
```
