# Kuben CI/CD and release process

This document describes the GitHub Actions setup of `Teamtem-dev/kuben`:
- what runs on every pull request
- how a release is built and published
- what `install.sh` guarantees

All commands run from the root of `kuben-monorepo`.

## 1. Files

| File | Purpose |
|---|---|
| `.github/workflows/ci.yml` | Runs on every push to `main`, every PR and `merge_group`. A change-detection job, 11 check jobs and the aggregate job **CI success**. |
| `.github/workflows/release.yml` | Runs on a pushed `v*` tag. Publishes binaries for 5 platforms, `checksums.txt`, attestations, the GitHub Release, a multi-arch GHCR image and the OCI Helm chart. |
| `.github/dependabot.yml` | Weekly updates of Actions, crates and npm packages with a 7-day cooldown (a shield against freshly published malicious packages). |
| `.github/release.yml` | Automatic release-note categories by label. |
| `install.sh` | One-line binary install on Linux and macOS. |
| `deploy/release.Dockerfile` | The release image, built from the very musl binaries that were checksummed. |
| `Dockerfile` | Full build from source (cross-compiled with zig, no QEMU). |
| `scripts/check-budgets.sh` | Binary and image size gates. |
| `scripts/ci-changes.sh` (+ `.test.sh`) | Decides which job groups a pull request needs (see below). |
| `.config/nextest.toml` | cargo-nextest profiles (`default` locally, `ci` in CI). |
| `scripts/e2e.sh` | End-to-end test on kind. |

## 2. CI jobs

| Job | What it does | Why |
|---|---|---|
| **Detect changes** | `scripts/ci-changes.sh` on the PR's changed files | selects the job groups below; pushes to `main`, the merge queue and manual runs select everything |
| **Rust format** | `cargo fmt --all --check` | |
| **Clippy** | `cargo clippy --workspace --all-targets --locked -- -D warnings` with `clippy::pedantic`, then again for `-p kuben --features activator` | workspace lints live in `Cargo.toml` (`unwrap_used = deny`, `unsafe_code = forbid`, …); the second run keeps the activator placeholder, which default builds leave out, compiling |
| **Test** (4-OS matrix) | `cargo nextest run --workspace --locked --profile ci` on linux-x64, linux-arm64, macos-arm64 and windows-x64, plus doctests (`cargo test --doc`) on linux-x64 | binaries ship for all 5 targets, so all of them are tested. nextest runs every test in its own process and reports all failures, not just the first |
| **Store matrix** | `kuben-store` tests against PostgreSQL 17 (service container), via nextest | the same repositories must behave identically on SQLite and Postgres |
| **MSRV** | `cargo +1.94 check` | `rust-version = 1.94` in `Cargo.toml` is a promise to users |
| **Supply chain** | `cargo-deny`: licenses, advisories, bans, sources | e.g. `openssl-sys` and `serde_yaml` are banned |
| **Web** | biome, `tsc`, vitest, build, **size-limit** | JS budget 200 kB and CSS 25 kB (brotli) |
| **Generated files** | `just drift` regenerates `openapi.json`, `schema.d.ts` and the CRDs and diffs them | the TS client can never fall behind the API |
| **Shell scripts** | execute bits in git, shellcheck, `install.sh` under `sh`/`dash`/`bash`, change-detection tests, `helm lint --strict` and `helm template` | a script committed without its execute bit fails CI with exit code 126 |
| **Binary size budget** | static musl release build with the embedded UI (built in the same job), gated at **25 MiB** | the shipped binary is measured, not a debug build |
| **End-to-end (kind)** | `scripts/e2e.sh` on a real kind cluster | login → project → environment → namespace with quota/NetworkPolicy → deploy → scale → logs → restart → delete and GC |
| **CI success** | fails if any job above failed or was cancelled | **make only this job required in branch protection.** Adding or removing jobs then never requires touching repository settings |

Which jobs a pull request runs:

| Group | Selected by changes to | Jobs |
|---|---|---|
| `rust` | `crates/`, Cargo manifests and lockfile, toolchain and lint configs | fmt, clippy, test, store matrix, MSRV, e2e, budgets |
| `deps` | `Cargo.toml`, `crates/*/Cargo.toml`, `Cargo.lock`, `deny.toml` | cargo-deny |
| `web` | `apps/`, `packages/`, pnpm and biome configs | web, budgets |
| `codegen` | any `rust` change, `packages/api-client/`, `charts/kuben/crds/` | generated files (drift) |
| `scripts` | `scripts/`, `install.sh`, `charts/`, `deploy/`, `Dockerfile` | shell scripts and Helm, e2e |

- A documentation-only PR runs just the change detection and **CI success**.
- A change to `.github/` or the `justfile` runs everything. So does an empty or unreadable change list: detection fails open.
- Skipped jobs count as passed in **CI success**, but a failed change-detection job fails it.
- `just ci-changes` shows locally which groups your branch would run.

Details:

- Every action is pinned to a **commit SHA** (with the tag in a comment), and Dependabot keeps them current.
- `permissions: contents: read` is the default. Each job requests only what it needs. `persist-credentials: false` is set on every checkout.
- `concurrency` cancels superseded runs on PRs, never on `main`.
- The Rust cache is saved only on `main` (`save-if`), so PRs cannot plant a poisoned cache.

## 3. Release process

```text
tag v0.2.0 ─► plan (validate tag == Cargo version)
               ├─► web (pnpm build + size-limit) ─► build ×5 ─┬─► publish (checksums + attestations + GitHub Release)
               │                                               │        └─► verify-install ×3 (install.sh against the real release)
               │                                               └─► image (budget ≤ 30 MiB → push amd64+arm64 + SBOM + provenance)
               │                                                        └─► chart (helm push oci://ghcr.io/teamtem-dev/charts)
```

Steps:

1. Bump the version in `Cargo.toml` (`[workspace.package] version`), run `cargo check` so `Cargo.lock` follows, and commit.
2. Tag and push:
   ```bash
   git tag -s v0.2.0 -m "v0.2.0"
   ```
   ```bash
   git push origin v0.2.0
   ```
3. The **plan** job checks that the tag equals the version of the `kuben` crate; otherwise nothing is released.
4. Artifacts:

| Target | Runner | File |
|---|---|---|
| `x86_64-unknown-linux-musl` | ubuntu-24.04 (zigbuild) | `kuben-x86_64-unknown-linux-musl.tar.gz` |
| `aarch64-unknown-linux-musl` | ubuntu-24.04-arm (zigbuild) | `kuben-aarch64-unknown-linux-musl.tar.gz` |
| `aarch64-apple-darwin` | macos-15 | `kuben-aarch64-apple-darwin.tar.gz` |
| `x86_64-apple-darwin` | macos-15 (cross) | `kuben-x86_64-apple-darwin.tar.gz` |
| `x86_64-pc-windows-msvc` | windows-2025 | `kuben-x86_64-pc-windows-msvc.zip` |

   Alongside the archives:
   - a `.sha256` file for each archive
   - `checksums.txt`
   - `install.sh`
   - the image `ghcr.io/teamtem-dev/kuben:{X.Y.Z, X.Y, latest}` (`latest` and `X.Y` only for stable versions)
   - the chart at `oci://ghcr.io/teamtem-dev/charts/kuben`
5. Tags such as `v0.2.0-rc.1` automatically become **pre-releases** and never move `latest`.
6. To rebuild an existing tag: *Actions → Release → Run workflow* with the tag; assets are replaced with `--clobber`.

Design decisions:

- **The image is built from the published binaries, not from source.** `deploy/release.Dockerfile` is a single `COPY`. The multi-arch build takes seconds, needs no QEMU, and the bytes in the image are exactly the ones that were checksummed and attested.
- **Releases build without a cache**, so a poisoned cache can never reach a published artifact.
- **The image size gate runs before the push.** `linux/amd64` is loaded and measured first, then the multi-arch image is pushed.
- **`USER 65532:65532` is numeric.** Without a numeric UID, Kubernetes cannot verify `runAsNonRoot: true` and rejects the pod.

## 4. `install.sh`

```bash
curl -fsSL https://raw.githubusercontent.com/Teamtem-dev/kuben/main/install.sh | bash
```

| Option | Environment variable | Default |
|---|---|---|
| `--version v0.2.0` | `KUBEN_VERSION` | latest stable release |
| `--dir <path>` | `KUBEN_INSTALL_DIR` | `/usr/local/bin` |
| `--no-sudo` | `KUBEN_NO_SUDO=1` | uses sudo when the directory is not writable and sudo exists |

Guarantees:

- **HTTPS only**, TLS ≥ 1.2 (`--proto '=https' --tlsv1.2`).
- **Checksum first.** The archive's SHA-256 is compared with `checksums.txt` **before anything is extracted or installed**, using whichever of `sha256sum`, `shasum` or `openssl` exists.
- **No partial execution.** All logic lives in `main()`, called on the last line, so a truncated download executes nothing.
- **Architecture detection.** It detects the architecture (`x86_64`/`aarch64`, Linux/macOS). An x86_64 shell under Rosetta on Apple Silicon gets the native ARM binary.
- **No API calls.** It finds the latest version through the `releases/latest` redirect, so there is no API rate limit and no `jq`.
- **Verified after every release.** The **verify-install** job runs the script on ubuntu (x64 and arm64) and macOS and checks `kuben --version`.

For more assurance, verify the provenance:

```bash
gh attestation verify kuben-x86_64-unknown-linux-musl.tar.gz --repo Teamtem-dev/kuben
```

## 5. First-push checklist

1. Create `Teamtem-dev/kuben` and push the contents of `kuben-monorepo/` as its root.
2. In *Settings → Branches*, protect `main` and make **CI success** required.
3. In *Settings → Actions → General*, set *Workflow permissions* to **Read**; each workflow declares what it needs.
4. After the first release, make the `kuben` and `charts/kuben` packages **Public** under *Packages*. GHCR creates new packages as private.
5. `ubuntu-24.04-arm` runners are free for public repositories. For a private repository, use a paid plan or cross-compile arm64 with zigbuild on x64.
6. In *Settings → Code security*, enable *Private vulnerability reporting* (referenced by `SECURITY.md`) and Dependabot alerts.

## 6. Local equivalents

```bash
just ci
```

Runs fmt, clippy, the tests, the drift check and the web build/size check. `cargo-deny` and `shellcheck` run when installed.

```bash
just e2e
```

Starts a kind cluster and runs `scripts/e2e.sh`.

```bash
just budgets
```

Builds the release binary with the embedded UI and checks the binary size budget.

```bash
just ci-changes
```

Prints which CI job groups a pull request of the current branch would run (compared with `origin/main`).
