# ADR-024: Turborepo runs every task; Bun replaces pnpm and Node; Cargo stays the Rust build graph

**Status:** decided · **Date:** 2026-09-12 · **Plan:** [turborepo-migration-plan.md](../turborepo-migration-plan.md)

## Context

The monorepo had three tools around its two languages: Cargo for Rust, pnpm
(with Node 22 and corepack) for TypeScript, and a `justfile` tying them
together. `just` had no dependency graph and no cache, and CI repeated its
commands instead of calling them. The golden architecture rejected Turborepo
for Rust because Turborepo "does not understand Cargo's dependency graph."
Turborepo 2.10 has since added native Cargo workspace support, which answers
that objection. Two cross-language edges were implicit: the TypeScript client
is generated from a Rust binary, and the release binary embeds the web build.

## Decision

- **Turborepo 2.10.12, pinned exactly, is the single task runner.** It uses
  `futureFlags.experimentalCargoWorkspaces` and `experimentalTaskCommand`.
  Every crate is a package, and `kuben-cargo` (`[workspace.metadata] name`)
  stands for the whole Cargo workspace. Cargo still schedules and caches the
  Rust compilation itself.
- **The two cross-language edges are explicit tasks.** `kuben-api#gen` →
  `@kuben/api-client#gen`, and `@kuben/web#build` → `kuben#build:release`.
  `@kuben/web#dev` starts `kuben#dev` as a sidecar.
- **Bun 1.4 is the package manager, the JS tool runtime and the test runner.**
  `bun.lock` replaces `pnpm-lock.yaml`. The version catalog moves to
  `workspaces.catalog` in `package.json`. The isolated linker keeps pnpm's
  strictness. Scripts run tools by path, as in
  `bun --bun ./node_modules/.bin/vite build`. On Bun 1.4.0 and 1.4.2, a tool
  named by bin (`bun --bun vite`, `bunx --bun vite`) still follows its
  `#!/usr/bin/env node` shebang whenever a `node` is on `PATH`. Neither a root
  nor a package-level `bunfig.toml` `[run] bun = true` changes that. With the
  path form, the build runs on Bun (`vite … bun-v26.3.0`) and emits the same
  bundle hashes as the old Node build. `bun test` replaces vitest. Node is no
  longer needed anywhere.
- **Biome runs as a root task** (`biome:check`, `biome:fix`), as the Turborepo
  Biome guide recommends.
- **Command tasks declare their inputs.** A task with a custom `command` gets
  neither Cargo's derived inputs nor outputs. It lists every crate, the Cargo
  manifests, the lockfile and the toolchain file. It also either lists its
  outputs or sets `cache: false`, so a cache hit can never skip a compile.
- **`envMode: loose`.** In strict mode a task sees only `HOME`, `PATH`,
  `PWD`, `SHELL` and `USER`. That drops `CARGO_HOME`, `RUSTUP_HOME` and the
  platform variables Cargo and MSVC need on the 4-OS test matrix. Hashing stays
  explicit: every Rust command task lists the variables that change its result
  (`RUSTFLAGS`, `RUSTUP_TOOLCHAIN`, `CARGO_BUILD_*`, …).
- **CI keeps its jobs and its path-based change detection.** Charts, scripts,
  deploy files and workflows are not packages, so `--affected` cannot see them.
  Each job calls the turbo task developers run. Anything that changes the task
  runner (`turbo.json`, root `package.json`, `bun.lock`, `bunfig.toml`) selects
  every job.
- **Releases never reuse a cache.** The release workflow builds the SPA
  directly with Bun, and the Rust release builds stay cache-free as before.

## Consequences

- One command surface: `bun run setup | dev | build | check | lint | format |
  test | gen | drift | ci | e2e | build:release`, and `turbo run <task>
  --filter=<package>` for anything narrower.
- Every `turbo` invocation validates the Cargo workspace with `cargo metadata`.
  Cargo and rustc must therefore be on `PATH` even for JS-only work, so CI
  web jobs set up Rust. The source Dockerfile's web stage calls Bun directly
  and needs no Rust.
- Cargo support is experimental. The exact pin and Dependabot's full-CI rule
  for `bun.lock` contain upgrades. The fallback is to turn the flags off and
  move the same Cargo commands into `package.json` scripts, keeping the task
  names.
- `minimumReleaseAge` is not set in `bunfig.toml`. On Bun 1.4.0 any
  re-resolution under it fails with a misleading "is not in the catalog" error.
  Dependabot's 7-day cooldown remains the supply-chain gate. Revisit when Bun
  reports age-gated versions clearly.
- Contributors need Bun and cargo-nextest instead of Node, corepack and just.
  `bun run setup` installs cargo-nextest.
