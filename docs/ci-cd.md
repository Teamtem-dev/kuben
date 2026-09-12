# Kuben CI/CD and release process

This document describes the GitHub Actions setup of `Teamtem-dev/kuben`:
- what runs on every pull request
- how a release is built and published
- what is checked every day, independent of any change
- what `install.sh` guarantees

All commands run from the root of `kuben-monorepo`. Tasks in both languages
run through Turborepo ([ADR-024](adr/0024-turborepo-bun-cargo.md)). CI jobs
call the same `turbo` tasks developers run locally.

## 1. Files

| File | Purpose |
|---|---|
| `.github/workflows/ci.yml` | Runs on every push to `main`, every PR and `merge_group`. A change-detection job, 12 check jobs and the aggregate job **CI success**. |
| `.github/workflows/release.yml` | Runs on a pushed `v*` tag. Publishes binaries for 5 platforms, `checksums.txt`, attestations, the GitHub Release, a multi-arch GHCR image and the OCI Helm chart. |
| `.github/workflows/security.yml` | Runs daily and on demand: RustSec advisories on `main` and a Trivy scan of the published image. A failure opens one issue (later failures comment on it). |
| `.github/workflows/site.yml` | Builds `apps/site` (kuben.teamtem.com) on pull requests that touch it; deploys it on pushes to `main` to the Cloudflare Worker `kuben` (static assets, `apps/site/wrangler.jsonc`) with `wrangler deploy`. Deploying needs the variable `CLOUDFLARE_ACCOUNT_ID` and the secret `CLOUDFLARE_API_TOKEN`; without the variable the deploy job is skipped. |
| `.github/actions/setup` | Composite action shared by every job. It installs Bun (version from `packageManager`), runs `bun install --frozen-lockfile`, and optionally sets up a Rust toolchain and `Swatinem/rust-cache`. |
| `.github/dependabot.yml` | Weekly updates of Actions (workflows and the composite action), crates and Bun packages, with a 7-day cooldown (a shield against freshly published malicious packages). |
| `.github/release.yml` | Automatic release-note categories by label. |
| `turbo.json` (+ `apps/console/turbo.json`, `packages/api-client/turbo.json`) | The task graph for Rust and TypeScript. |
| `install.sh` | One-line binary install on Linux and macOS. |
| `deploy/release.Dockerfile` | The release image, built from the very musl binaries that were checksummed. |
| `Dockerfile` | Full build from source (cross-compiled with zig, no QEMU). |
| `scripts/check-budgets.sh` | Binary and image size gates. |
| `scripts/check-drift.sh` | Regenerates the committed OpenAPI spec, TS types and CRDs with `turbo run gen` and fails on any difference. |
| `scripts/scan-chart.sh` + `.trivyignore.yaml` | Trivy misconfiguration scan of the rendered Helm chart. Every accepted finding is justified in the ignore file. |
| `scripts/scan-binary.sh` | Trivy scan of a release binary: its embedded dependency list must be present, and no HIGH or CRITICAL vulnerability with a fix may ship. |
| `scripts/ci-changes.sh` (+ `.test.sh`) | Decides which job groups a pull request needs (see below). |
| `scripts/ci.sh` | The local equivalent of CI (`bun run ci`). |
| `.config/nextest.toml` | cargo-nextest profiles (`default` locally, `ci` in CI via `NEXTEST_PROFILE`). |
| `scripts/e2e.sh` | End-to-end test on kind. |

## 2. CI jobs

| Job | What it runs | Why |
|---|---|---|
| **Detect changes** | `scripts/ci-changes.sh` on the PR's changed files | selects the job groups below; pushes to `main`, the merge queue and manual runs select everything |
| **Rust format** | `turbo run format --filter=kuben-cargo -- --check` (`cargo fmt --all -- --check`) | |
| **Clippy** | `turbo run kuben-cargo#lint kuben#lint:activator`: clippy on every crate and target with `-D warnings` and `clippy::pedantic`, then again for `-p kuben --features activator` | workspace lints live in `Cargo.toml` (`unwrap_used = deny`, `unsafe_code = forbid`, …); the second run keeps the activator placeholder, which default builds leave out, compiling |
| **Test** (4-OS matrix) | `turbo run kuben-cargo#test` (cargo-nextest, profile `ci`) on linux-x64, linux-arm64, macos-arm64 and windows-x64, plus `kuben-cargo#test:doc` on linux-x64 | binaries ship for all 5 targets, so all of them are tested. nextest runs every test in its own process and reports all failures, not just the first |
| **Store matrix** | `turbo run kuben-store#test:postgres` against PostgreSQL 17 (service container) | the same repositories must behave identically on SQLite and Postgres |
| **MSRV** | `RUSTUP_TOOLCHAIN=1.94 turbo run kuben-cargo#check -- --all-targets` | `rust-version = 1.94` in `Cargo.toml` is a promise to users |
| **Supply chain** | `cargo-deny`: licenses, advisories, bans, sources | e.g. `openssl-sys` and `serde_yaml` are banned |
| **Web** | `turbo run biome:check check test build size` for the root and `@kuben/*`: Biome, `tsc`, `bun test`, Vite build, **size-limit** | JS budget 200 kB and CSS 25 kB (brotli) |
| **Generated files** | `bun run drift` regenerates `openapi.json`, `schema.d.ts` and the CRDs and diffs them | the TS client can never fall behind the API |
| **Shell scripts** | execute bits in git, shellcheck, `install.sh` under `sh`/`dash`/`bash`, change-detection tests, `helm lint --strict`, `helm template`, and the Trivy misconfiguration scan of the chart (`scripts/scan-chart.sh`) | a script committed without its execute bit fails CI with exit code 126; the chart's RBAC and pod security are what users install |
| **Binary size budget** | the SPA via `turbo run @kuben/console#build`, then a static musl release build with the embedded UI and dependency list (`cargo auditable zigbuild`), gated at **26 MiB**, then `scripts/scan-binary.sh` | the shipped binary is measured and scanned, not a debug build |
| **End-to-end (kind)** | `turbo run kuben#e2e` (`scripts/e2e.sh` after `kuben#build`) on a real kind cluster | login → project → environment → namespace with quota/NetworkPolicy → deploy → scale → logs → restart → delete and GC |
| **Workflow security** | [zizmor](https://docs.zizmor.sh) on the workflows, the setup action and `dependabot.yml`, including its online audits | template injection, excessive permissions, cache poisoning, impostor or unpinned actions. It takes seconds, so it runs on every PR instead of joining a group |
| **CI success** | fails if any job above failed or was cancelled | **make only this job required in branch protection.** Adding or removing jobs then never requires touching repository settings |

Which jobs a pull request runs:

| Group | Selected by changes to | Jobs |
|---|---|---|
| `rust` | `crates/`, Cargo manifests and lockfile, toolchain and lint configs | fmt, clippy, test, store matrix, MSRV, e2e, budgets |
| `deps` | `Cargo.toml`, `crates/*/Cargo.toml`, `Cargo.lock`, `deny.toml` | cargo-deny |
| `web` | `apps/`, `packages/`, `biome.json`, `tsconfig.base.json` | web, budgets |
| `codegen` | any `rust` change, `packages/api-client/`, `charts/kuben/crds/` | generated files (drift) |
| `scripts` | `scripts/`, `install.sh`, `charts/`, `deploy/`, `Dockerfile`, `.trivyignore.yaml` | shell scripts and Helm, e2e |

- A documentation-only PR runs just the change detection, workflow security and **CI success**.
- A change to `.github/` or to the task runner every job uses (`turbo.json`, the root `package.json`, `bun.lock`, `bunfig.toml`) runs everything. So does an empty or unreadable change list: detection fails open.
- Skipped jobs count as passed in **CI success**, but a failed change-detection job fails it.
- `git diff --name-only origin/main...HEAD | scripts/ci-changes.sh` shows locally which groups your branch would run.

Details:

- Every action is pinned to a **commit SHA** (with the tag in a comment), and Dependabot keeps them current. The repository's own setup action is referenced as `$/.github/actions/setup` (self-repository syntax), so it always comes from the commit that is running.
- Rust itself is installed with the runner's own `rustup`, so no third-party action touches the toolchain. Third-party tools (cargo-nextest, cargo-zigbuild, cargo-auditable, zizmor, Trivy) are installed with an exact version by `taiki-e/install-action`, which verifies every download against a checksum. Trivy's own GitHub Actions are deliberately not used: their tags were hijacked in March 2026 ([GHSA-69fq-xp46-6x23](https://github.com/aquasecurity/trivy/security/advisories/GHSA-69fq-xp46-6x23)).
- `permissions: contents: read` is the default. Each job requests only what it needs. `persist-credentials: false` is set on every checkout.
- `concurrency` cancels superseded runs on PRs, never on `main`.
- The Rust cache is saved only on `main` (`save-if`), so PRs cannot plant a poisoned cache.
- Every `turbo` run validates the Cargo workspace (`cargo metadata`), so jobs that only touch the web UI still set up Rust.
- Turborepo's remote cache is opt-in: set the repository variable `TURBO_TEAM` and the secret `TURBO_TOKEN`. Pushes to `main` read and write it. Pull requests only read it, following the same rule as the Rust cache.
- When the end-to-end suite fails, the failing step and the end of the server log become an annotation on the pull request.

## 3. Release process

```text
tag v0.2.0 ─► plan (validate tag == Cargo version)
               ├─► web (bun: vite build + size-limit) ─► build ×5 (cargo auditable) ─┬─► publish (checksums + attestations + GitHub Release)
               │                                                                     │        └─► verify-install ×3 (install.sh against the real release)
               │                                                                     └─► image (budget ≤ 30 MiB → Trivy → push amd64+arm64 + SBOM + provenance)
               │                                                                              └─► chart (helm push oci://ghcr.io/teamtem-dev/charts)
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
6. To rebuild an existing tag: *Actions → Release → Run workflow* with the tag; assets are replaced with `--clobber`. For a tag from before the Turborepo/Bun migration (v1.0.1 and earlier), choose the tag itself under *Use workflow from*, so that the workflow matches the tag's pnpm-based tree.

Design decisions:

- **The image is built from the published binaries, not from source.** `deploy/release.Dockerfile` is a single `COPY`. The multi-arch build takes seconds, needs no QEMU, and the bytes in the image are exactly the ones that were checksummed and attested.
- **Every binary carries its dependency list.** `cargo auditable` embeds the exact crate list (about 6 KiB for Kuben's 378 crates) in a linker section, so `trivy`, `grype`, `osv-scanner` or `cargo audit bin` can audit a deployed binary or the image. The Linux binaries are scanned before they are packaged, and the amd64 image before it is pushed.
- **Releases build without a cache**, so a poisoned cache can never reach a published artifact. The web job calls the package scripts with Bun directly instead of turbo, so no task cache is involved either, and `setup-zig` runs with `use-cache: false`.
- **The image gates run before the push.** `linux/amd64` is loaded, measured against the size budget and scanned by Trivy first; only then is the multi-arch image pushed.
- **`USER 65532:65532` is numeric.** Without a numeric UID, Kubernetes cannot verify `runAsNonRoot: true` and rejects the pod.

## 4. Daily security checks

Advisories are published after code is merged: a lockfile that passed CI
yesterday can be vulnerable today. `.github/workflows/security.yml` therefore
runs every day at 05:17 UTC (and on demand from *Actions → Security*):

| Job | What it runs |
|---|---|
| **RustSec advisories** | `cargo deny check advisories` on `main` |
| **Published image** | `trivy image` on `ghcr.io/teamtem-dev/kuben:latest`: HIGH and CRITICAL vulnerabilities with a fix available |
| **Report failure** | when a scheduled run fails, opens the issue *Scheduled security check failed*, or comments on it if it is still open |

To act on a failure: update the affected dependency (or add a justified
`ignore` entry to `deny.toml`), merge, and cut a patch release so the image
follows.

## 5. `install.sh`

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

## 6. First-push checklist

1. Create `Teamtem-dev/kuben` and push the contents of `kuben-monorepo/` as its root.
2. In *Settings → Branches*, protect `main` and make **CI success** required.
3. In *Settings → Actions → General*, set *Workflow permissions* to **Read**; each workflow declares what it needs.
4. After the first release, make the `kuben` and `charts/kuben` packages **Public** under *Packages*. GHCR creates new packages as private.
5. `ubuntu-24.04-arm` runners are free for public repositories. For a private repository, use a paid plan or cross-compile arm64 with zigbuild on x64.
6. In *Settings → Code security*, enable *Private vulnerability reporting* (referenced by `SECURITY.md`) and Dependabot alerts.

## 7. Local equivalents

```bash
bun run ci
```

Runs clippy, Biome, the tests and doctests, the TypeScript checks, the rustfmt check, the drift check and the web build/size check. `cargo-deny` and `shellcheck` run when installed.

```bash
kind create cluster --config deploy/kind.yaml
```

```bash
bun run e2e
```

Builds the binary and runs `scripts/e2e.sh` against the current kube context.

```bash
bun turbo run kuben#size
```

Builds the release binary with the embedded UI (after the web build) and checks the binary size budget.

```bash
git diff --name-only origin/main...HEAD | scripts/ci-changes.sh
```

Prints which CI job groups a pull request of the current branch would run.

Security tooling outside the task graph (each needs the tool installed):

```bash
zizmor .
```

Audits the workflows, the setup action and `dependabot.yml`.

```bash
scripts/scan-chart.sh
```

Trivy misconfiguration scan of the Helm chart.

Platform tooling outside the task graph:

```bash
cargo auditable zigbuild -p kuben --release --locked --features embed-ui --target x86_64-unknown-linux-musl
```

Builds a static musl binary exactly like the release, dependency list included. It needs zig, cargo-zigbuild and cargo-auditable, and `apps/console/dist` from `bun turbo run @kuben/console#build`. `scripts/scan-binary.sh target/x86_64-unknown-linux-musl/release/kuben` then scans it.

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/teamtem-dev/kuben:dev .
```

Builds a multi-arch image from source.

```bash
kubectl apply --server-side -f charts/kuben/crds/
```

Applies the generated CRDs to the current kube context.
