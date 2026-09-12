# Plan: one task graph with Turborepo, Bun and Cargo

**Status:** implemented on `chore/turborepo-bun` · **Date:** 2026-09-12 · **Decision record:** [ADR-024](adr/0024-turborepo-bun-cargo.md)

This plan moves the monorepo from `just` + pnpm + Node to **Turborepo 2.10** +
**Bun 1.4** + **Cargo**. Turborepo becomes the single entry point for every
task in both languages. Cargo still builds Rust. Bun installs, runs and tests
the TypeScript side.

## 1. Goals

1. **One command surface.** Every task, local or in CI, runs as `bun run <verb>` or
   `turbo run <task>`. The `justfile` goes away.
2. **One task graph across languages.** Turborepo knows that the TS client is generated
   from a Rust binary, and that the release binary embeds the web build.
   It orders and caches the tasks accordingly.
3. **Bun replaces pnpm and Node.** Bun is the package manager, the runtime for JS tooling
   and the test runner. No Node or corepack is required anywhere.
4. **CI runs the commands developers run.** Workflows call the same turbo tasks instead
   of duplicating commands.
5. **No regression** in any existing gate: fmt, clippy `-D warnings`, the 4-OS test
   matrix, PostgreSQL matrix, MSRV, cargo-deny, drift, size budgets, e2e,
   shell scripts, release provenance.

Out of scope: application code changes. The only exception is making the two codegen
binaries write their output file themselves (§4.4).

## 2. Audit of the current setup

| Area | Today | Problem |
|---|---|---|
| Task runner | `justfile` with 19 recipes (setup, ci, lint, test, drift, gen, dev, web, build, budgets, e2e, …) | A third tool next to Cargo and pnpm. No caching and no dependency graph. CI does not use it except for `just drift`. |
| JS package manager | pnpm 10 via corepack, `pnpm-workspace.yaml` with a strict `catalog`, `.npmrc` | Needs Node 22.12+ and corepack. The machine default here is Node 16, so `pnpm build` breaks silently. |
| JS runtime | Node 22 (`.nvmrc`, `engines`) | One more toolchain to install and pin in CI, Docker and on dev machines. |
| JS tests | vitest 5 | 3 pure unit-test files. vitest pulls in a large dependency tree for them. |
| Codegen | `cargo run … > file` in the justfile | Shell redirection. The build had to run first so that a failed build would not truncate committed files. |
| CI | Every job repeats its own `pnpm`/`cargo` commands | Commands drift from the local ones, and `just ci` and CI were already different. |
| Docker | `node:22-alpine` + corepack + pnpm stage for the SPA | Replace with the `oven/bun` image. |
| Dependabot | `npm` ecosystem | Bun has its own `bun` ecosystem (GA since 2025-02). |
| Docs | README, CONTRIBUTING, `docs/ci-cd.md` and `deploy/kind.yaml` reference `just` | Must follow the change. |

`KUBEN-GOLDEN-ARCHITECTURE.md` rejected Turborepo for Rust with the argument that
*"Turbo does not understand Cargo's dependency graph."* That was true when it was
written. Turborepo 2.10 now reads Cargo workspaces natively (§3), so the objection no
longer holds. ADR-024 records the reversal.

## 3. Research (2026-09-12)

Versions, from the npm registry and GitHub releases:

| Tool | Version | Notes |
|---|---|---|
| `turbo` | **2.10.12** (latest stable, 2026-08-25) | Pinned exactly. Cargo support sits behind future flags, so every machine must run the same version. |
| Bun | **1.4.2** (2026-09-05) | `packageManager: bun@1.4.2`. `oven-sh/setup-bun` reads it from `package.json`. |
| `oven-sh/setup-bun` | v2.2.0 (`0c5077e5…`) | Pinned by SHA like every other action. |
| `oven/bun` image | `1.4.2-alpine` | Replaces `node:22-alpine` in the source Dockerfile. |

What Turborepo 2.10 provides, from turborepo.dev (Rust guide, configuration
reference, 2.10 release notes, Biome and GitHub Actions guides) and Context7:

- `futureFlags.experimentalCargoWorkspaces` discovers every Cargo workspace member as a
  package, plus one synthetic package named by `[workspace.metadata] name`. It
  registers `build`, `check`, `lint` (clippy), `test`, `format`, and `run`/`dev`
  (for crates with exactly one binary). Unfiltered runs collapse into one
  workspace-wide Cargo command. Hashes cover the crate closure, `Cargo.lock`,
  `rustc -vV` and the Cargo environment.
- `futureFlags.experimentalTaskCommand` lets a task declare a `command` argv. It
  runs without a shell, with the package directory as working directory.
- Cross-package `dependsOn` (`"pkg#task"`) and sidecar tasks (`with`) work
  across languages.
- `cacheMaxAge` / `cacheMaxSize` evict old entries from the local cache (new in 2.10).
- Bun as a package manager is stable since Turborepo 2.6 (`bun.lock` v1), and
  `turbo prune` supports Bun.
- Biome guidance: *"we recommend using a Root Task rather than creating separate
  scripts in each of your packages."*
- Bun supports catalogs in root `package.json` (`workspaces.catalog`),
  `linker = "isolated"` (pnpm-style strictness) and `minimumReleaseAge`
  (supply-chain cooldown).

A throwaway repository (Bun workspace + 3-crate Cargo workspace) confirmed the
behaviour on turbo 2.10.12 before anything changed here. The findings that shape
the design:

| Probe | Result | Consequence |
|---|---|---|
| `turbo ls` | JS packages, crates and the synthetic workspace package share one graph | ✅ |
| `turbo run lint test` unfiltered | Runs once as `cargo clippy --workspace` / `cargo test --workspace` | ✅ no per-crate fan-out |
| `turbo run dev` unfiltered | Also registers `dev` for crates whose only binary is not `main` (here `openapi`, `crdgen`) | `dev` is always filtered to `@kuben/web`, and the API starts as its sidecar |
| Custom `command` task | cwd = crate directory; `outputs` and derived Cargo inputs are **not** added | Every command task declares explicit `inputs`, and `outputs` or `cache: false`. Otherwise a cache hit could skip a compile. |
| `"a#release": {"dependsOn": ["w#build"]}` | Works across languages | The release binary depends on the web build |
| `$TURBO_ROOT$` in `outputs` | Works | Codegen outputs outside the crate can be cached and restored |
| Root `bunfig.toml` `[run] bun = true` | **Not** applied to scripts run in a sub-package | Tools must be forced onto Bun in the script itself |
| `bun --bun vite` / `bunx --bun vite` (Bun 1.4.0 and 1.4.2) | Still runs vite on the `node` found on `PATH` (here Node 16, so it crashes) | Scripts run tools by path: `bun --bun ./node_modules/.bin/vite build` runs on Bun and emits the same bundle hashes as the old Node build |

## 4. Target design

### 4.1 Toolchain

| Concern | Tool |
|---|---|
| Task graph, caching, filtering, `--affected` | Turborepo 2.10.12 |
| Rust build graph | Cargo (unchanged: MSRV 1.94, `rust-toolchain.toml`, `[workspace.dependencies]`) |
| JS package manager | Bun 1.4.2 (`bun.lock`, isolated linker, catalog in `package.json`) |
| JS runtime for tooling (Vite, tsc, size-limit, openapi-typescript) | Bun (`bun --bun`) |
| JS unit tests | `bun test` (replaces vitest) |
| Rust tests | cargo-nextest (unchanged) + `cargo test --doc` |
| Lint and format | clippy + rustfmt, Biome as a root task |

### 4.2 Package graph

```text
//  (root: biome:check, biome:fix)
@kuben/web ──depends──► @kuben/api-client
kuben-cargo  (synthetic: the whole Cargo workspace)
  ├─ kuben ─► kuben-api ─► kuben-platform ─► kuben-store ─► kuben-core
  │                                      └─► kuben-crd
Cross-language task edges:
  kuben-api#gen ─► @kuben/api-client#gen          (openapi.json → schema.d.ts)
  @kuben/web#build ─► kuben#build:release         (dist embedded with rust-embed)
  @kuben/web#dev ──with──► kuben#dev               (API sidecar for Vite's proxy)
```

The synthetic package is named **`kuben-cargo`** (`[workspace.metadata] name`). It
cannot be `kuben` because that name belongs to the binary crate.

### 4.3 Tasks

| Task | Rust | TypeScript | Cached |
|---|---|---|---|
| `build` | `cargo build -p <crate>` (built-in, entrypoints only when unfiltered) | `vite build` → `dist/**` | yes |
| `check` | `cargo check --workspace` (built-in) | `tsc` (app code and tests type-checked separately) | yes |
| `lint` | `kuben-cargo#lint`: `cargo clippy --workspace --all-targets --locked -- -D warnings` | Biome runs as the root task `biome:check` | yes |
| `lint:activator` | `kuben#lint:activator`: clippy with `--features activator` | — | yes |
| `format` | `cargo fmt --all` (built-in; `-- --check` verifies) | root task `biome:fix` | no |
| `test` | `kuben-cargo#test`: `cargo nextest run --workspace --locked` | `bun test` | yes |
| `test:doc` | `kuben-cargo#test:doc`: `cargo test --workspace --doc` | — | yes |
| `test:postgres` | `kuben-store#test:postgres`: nextest `--test matrix` against `KUBEN_TEST_PG_URL` | — | no (external DB) |
| `gen` | `kuben-api#gen` → `packages/api-client/openapi.json`; `kuben-crd#gen` → `charts/kuben/crds/kuben.dev_all.yaml` | `@kuben/api-client#gen`: `openapi-typescript` (after `kuben-api#gen`) | yes (outputs restored) |
| `size` | `kuben#size`: binary budget (after `build:release`) | `size-limit` (after `build`) | yes |
| `build:release` | `kuben#build:release`: `--release --features embed-ui` (after `@kuben/web#build`) | — | no (Cargo's own cache; releases never reuse artifacts) |
| `dev` | `kuben#dev`: `cargo run -- serve --roles=all --dev` | `vite` with `kuben#dev` as its sidecar | no, persistent |
| `e2e` | `kuben#e2e`: `scripts/e2e.sh` (after `kuben#build`) | — | no (needs a live cluster) |
| `transit` | — | Hash-only node so that `check`/`test` re-run when a dependency's sources change | — |

`envMode` is **loose**. In strict mode, a probe showed that tasks see only `HOME`, `PATH`,
`PWD`, `SHELL` and `USER`. That drops `CARGO_HOME`, `RUSTUP_HOME` and the platform
variables Cargo and MSVC need on the 4-OS matrix. Hashing stays explicit instead: every
Rust command task lists the variables that change its result (`RUSTFLAGS`,
`RUSTUP_TOOLCHAIN`, `RUSTC_WRAPPER`, `CARGO_BUILD_*`, `CARGO_TARGET_*`, and
`NEXTEST_PROFILE` for tests). Built-in Cargo tasks hash Turborepo's derived Cargo
environment.

### 4.4 Codegen without shell redirection

`openapi` and `crdgen` take an optional output path. They write to `<path>.tmp` and
rename it over the target. A failed build never runs the binary, and an interrupted
run never truncates a committed file. Without a path they still print to stdout, so
existing usage keeps working. Because turbo restores cached outputs, a stale committed
file is overwritten even on a cache hit, and the drift check stays correct.

### 4.5 Root scripts: the command surface

| `just` recipe | Replacement |
|---|---|
| `just setup` | `bun run setup` (`scripts/setup.sh`: toolchain, `bun install`, cargo-nextest) |
| `just dev` | `bun run dev` |
| `just lint` | `bun run lint` (clippy + Biome) |
| `just fmt` | `bun run format` |
| `just test` | `bun run test` (nextest + doctests + `bun test`) |
| `just test-postgres` | `turbo run kuben-store#test:postgres` |
| `just gen` | `bun run gen` |
| `just drift` | `bun run drift` (`scripts/check-drift.sh`) |
| `just web` | `turbo run build size --filter=@kuben/web` |
| `just build` | `bun run build:release` |
| `just budgets` | `turbo run kuben#size` |
| `just ci` | `bun run ci` (`scripts/ci.sh`, mirrors CI) |
| `just e2e` | `bun run e2e` |
| `just deny` | `cargo deny check` (unchanged, standalone tool) |
| `just ci-changes` | `git diff --name-only origin/main...HEAD \| scripts/ci-changes.sh` |
| `just build-musl`, `just image`, `just crds` | Documented one-liners in `docs/ci-cd.md` (platform tooling, not graph tasks) |

### 4.6 Bun configuration

- `package.json`: `packageManager: bun@1.4.2`, `workspaces.packages` +
  `workspaces.catalog`. The catalog stays the single source of JS versions:
  packages only ever say `catalog:`. `turbo` itself is pinned in the catalog.
- `bunfig.toml`: `linker = "isolated"` gives the strictness pnpm had: a package can
  only import what it declares. `minimumReleaseAge` was tried and **left out**. On Bun
  1.4.0, any re-resolution under it fails with a misleading `<pkg>@catalog: is not in
  the catalog` error, even for unrelated packages; a probe tested linker-only and
  age-only separately. Dependabot's 7-day cooldown stays the supply-chain gate.
- `bun.lock` (text) replaces `pnpm-lock.yaml`. `.npmrc`, `.nvmrc` and
  `pnpm-workspace.yaml` go away.

### 4.7 CI and release

- A composite action, `.github/actions/setup`, installs Bun from `packageManager`,
  runs `bun install --frozen-lockfile`, and optionally sets up Rust plus
  `Swatinem/rust-cache`. The rust-cache still saves only on `main`. Dependabot
  scans the composite action.
- Every job keeps its purpose, timeout and runner. Only the command changes to a
  turbo task:
  - fmt → `turbo run format --filter=kuben-cargo -- --check`
  - clippy → `turbo run kuben-cargo#lint kuben#lint:activator`
  - test matrix → `turbo run kuben-cargo#test` (+ `test:doc` on linux-x64)
  - store matrix → `turbo run kuben-store#test:postgres`
  - MSRV → `RUSTUP_TOOLCHAIN=1.94 turbo run kuben-cargo#check -- --all-targets`
  - web → `turbo run biome:check check test build size` scoped to the JS packages
  - drift → `bun run drift`
  - e2e → `turbo run kuben#e2e`
  - budgets → web via turbo, then the musl zigbuild
- Change detection (`scripts/ci-changes.sh`) stays path-based. Some areas are not
  packages (charts, install script, deploy files, workflows), so turbo's
  `--affected` cannot see them. The rule is extended: anything that changes the
  task runner (`turbo.json`, root `package.json`, `bun.lock`, `bunfig.toml`,
  `.github/`) selects every job.
- Remote cache is opt-in. The workflows pass `TURBO_TOKEN`/`TURBO_TEAM` when
  configured. Pull requests get read-only access
  (`TURBO_CACHE=local:rw,remote:r`), so a PR can never write a cache entry. This
  is the same rule the Rust cache follows.
- The release workflow runs the SPA's package scripts with Bun directly
  (`bun run build && bun run size`), not through turbo. A published artifact is then
  never restored from a task cache, and the job needs no Rust toolchain. Every turbo
  run calls `cargo metadata`, so any job that uses turbo must set up Rust.
- `release.yml` job layout, provenance, images and chart are unchanged.

### 4.8 Files

Added: `turbo.json`, `apps/web/turbo.json`, `packages/api-client/turbo.json`,
`bunfig.toml`, `bun.lock`, `apps/web/tsconfig.test.json`,
`.github/actions/setup/action.yml`, `scripts/setup.sh`, `scripts/check-drift.sh`,
`scripts/ci.sh`, `docs/adr/0024-turborepo-bun-cargo.md`, this plan.

Removed: `justfile`, `pnpm-lock.yaml`, `pnpm-workspace.yaml`, `.npmrc`, `.nvmrc`,
the vitest dependency.

Changed: root and package `package.json` files, `Cargo.toml`
(`[workspace.metadata] name`), the two codegen binaries, `scripts/e2e.sh` (runs from
any directory), `scripts/ci-changes.sh` and its tests, both workflows,
`dependabot.yml`, `Dockerfile`, `.gitignore`, `.dockerignore`, README,
CONTRIBUTING, `docs/ci-cd.md`, `deploy/kind.yaml`, the ADR index.

## 5. Execution steps and exit criteria

| # | Step | Done when |
|---|---|---|
| 1 | Branch `chore/turborepo-bun` from `main` after the v1.0.1 release | ✅ |
| 2 | Bun: manifests, catalog, `bunfig.toml`, lockfile migration, drop pnpm and Node files | `bun install --frozen-lockfile` is clean |
| 3 | vitest → `bun test`; split the test tsconfig | `bun test` passes, and app code cannot see Bun types |
| 4 | `turbo.json` files, `[workspace.metadata]`, codegen binaries | `turbo ls` shows 9 packages, and `turbo run gen` reproduces the committed files byte for byte |
| 5 | Root scripts plus `setup`, `check-drift` and `ci` scripts; `e2e.sh` path fix | `bun run lint`, `check`, `test`, `build`, `drift` and `format -- --check` pass; a second run is a full cache hit |
| 6 | Remove the `justfile`; update change detection and its tests | `scripts/ci-changes.test.sh` passes |
| 7 | Composite action, `ci.yml`, `release.yml`, `dependabot.yml`, Dockerfile | YAML parses, every action is pinned by SHA, shellcheck is clean |
| 8 | Docs: README, CONTRIBUTING, `ci-cd.md`, ADR-024 | No stale `just`/`pnpm`/`.nvmrc` reference remains outside the historical planning documents |
| 9 | Pull request into `main`; release as **v1.0.2** (a patch: users see no behaviour change) | "CI success" is green on the pull request, and the tag passes the release workflow |

## 6. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Cargo support is **experimental** and may change between minor versions | `turbo` is pinned to one exact version in the catalog, and Dependabot upgrades land as PRs that run the full CI (`bun.lock` selects every job). If a future version breaks, the fallback is to turn the flags off and wrap the same Cargo commands in a `package.json` inside a `tools/cargo` package. The task names stay the same. |
| A cache hit hides a compile or test regression | Command tasks declare explicit inputs (all crates, Cargo manifests, lockfile, toolchain and lint config). Built-in Cargo tasks hash `rustc -vV` and the Cargo environment. Releases always run with `--force`. |
| Vite or another tool behaves differently on the Bun runtime | Every script is verified on Bun locally and in CI. Any single tool can drop `--bun` and fall back to Node without touching the rest. |
| `bun test` is not a drop-in for every vitest feature | The current tests only use `describe`/`it`/`expect` matchers that `bun:test` implements. |
| Dependabot's Bun support in workspaces | The `bun` ecosystem is GA. If a PR updates only `package.json`, `--frozen-lockfile` fails CI loudly rather than silently. |
| Contributors without cargo-nextest | `bun run setup` installs it. The error message names the fix. |

## 7. Rollback

The migration is one merge commit. `git revert -m 1 <merge>` restores the justfile,
pnpm and vitest in one step. No application behaviour depends on the build tool.

## 8. Verification (local, 2026-09-12)

| Check | Result |
|---|---|
| `turbo ls` | 9 packages: 2 TS, 6 crates, `kuben-cargo` |
| Lockfile migration | Every resolved version identical to `pnpm-lock.yaml` (vite 8.3.0, react 19.3.0, biome 2.5.13, …); `bun install --frozen-lockfile` clean |
| `bun run gen` | OpenAPI spec, `schema.d.ts` and CRD YAML byte-identical to the committed files |
| `bun run drift` | "generated files are up to date" |
| `@kuben/web#build` on Bun | Same bundle hashes as the old Node build (`index-BcSeoFDm.js`, `index-DYIvwpei.css`) |
| `size` | JS 103.11 kB / 200 kB, CSS 4.59 kB / 25 kB (brotli) |
| `check` | `tsc` clean for the app, its tests and the API client |
| `test` (TS) | `bun test`: 10 passed |
| `lint`, `lint:activator`, `biome:check` | clean (clippy `-D warnings` with pedantic) |
| `format -- --check` | clean |
| `test:doc` | clean |
| Rust tests | 117 passed under `cargo test --workspace`. cargo-nextest could not be built in the restricted verification environment (its `usdt` proc macro panics there). `kuben-cargo#test` runs exactly the command CI already used, and CI exercises it. |
| Dry runs | `bun run dev` → `@kuben/web#dev` + `kuben#dev` only; `lint`, `test` and the CI web-job selections are exactly as designed |
| `bun run dev` (live) | turbo starts the `kuben#dev` sidecar and Vite on the Bun runtime. The sandbox that ran the migration refuses every listening socket (Vite reports any port as "in use" while nothing listens), so serving pages was not observed there. Run `bun run dev` once on a normal machine. |
| Remote cache without credentials | turbo reports it disabled and continues on the local cache, so pull requests are unaffected |
| `scripts/ci-changes.test.sh` | 21/21 cases pass |
