# Contributing to Kuben

Thanks for helping. This file covers the workflow and the review checklist:
the 18 invariants every change must keep.

## Workflow

1. `just setup` once, then `just dev` (API on `:8080`, Vite on `:5173`).
2. Keep changes focused; one concern per pull request.
3. Before pushing: `just ci` (fmt, clippy `-D warnings`, tests, drift check,
   web lint/typecheck/test/build/size). If you touched controllers or the API,
   also run `just e2e` against kind.
4. Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
   (`feat(api): …`, `fix(controller): …`). Label PRs (`feature`, `bug`,
   `security`, `breaking`) — release notes are grouped by label.

Repository rules:

- **Dependencies:** Rust versions live only in `[workspace.dependencies]`;
  JS versions only in the `catalog:` of `pnpm-workspace.yaml`
  (`catalogMode: strict`). Every new crate must pass `cargo deny check`.
- **Generated files are committed:** after changing API types or CRDs run
  `just gen` and commit `packages/api-client/openapi.json`,
  `packages/api-client/src/schema.d.ts` and `charts/kuben/crds/`. CI fails on
  drift.
- **Every handler gets a unique `operation_id`** in `#[utoipa::path]`; the
  TypeScript types are keyed by it.
- **CRD types must stay structural:** no internally tagged enums (use
  one-of structs with optional fields, like `Source { image, git }`).
  `all_crds_have_structural_schema` guards this.
- **Budgets are gates:** binary ≤ 25 MiB, image ≤ 30 MiB, web JS ≤ 200 kB
  brotli. Raising a budget needs a written reason in the PR.

## Review checklist — the 18 invariants

Each invariant closes a class of bugs found in the system Kuben replaces.
Reviewers check the ones a change touches.
