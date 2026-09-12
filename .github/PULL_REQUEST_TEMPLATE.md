<!--
Keep the title in the imperative mood, matching the commit convention in
CONTRIBUTING.md: fix(api): reject an empty promotion target
-->

## What this changes

<!-- The behaviour before and after. Link the issue it closes, if there is one. -->

## Why

<!-- The problem being solved. For a bug, what made it possible in the first place. -->

## How it was verified

<!-- Name the commands you ran and what they reported. "bun run ci" alone is enough for
     most changes; say so if you could not run part of it. -->

- [ ] `bun run ci` passes (fmt, clippy with `-D warnings`, tests, drift, Biome, typecheck, web build and budgets)
- [ ] New behaviour has a test that fails without the change
- [ ] `bun run e2e` run, if this touches controllers, CRDs or the Helm chart

## Checklist

- [ ] The invariants in [CONTRIBUTING.md](../CONTRIBUTING.md) still hold
- [ ] Docs updated (`docs/`, README) if behaviour or configuration changed
- [ ] Generated files committed if the API or CRD types changed (`bun run gen`)
- [ ] Breaking changes are labelled `breaking` and explained above
