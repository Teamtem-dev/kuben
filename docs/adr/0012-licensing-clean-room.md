# ADR-012: Licensing and a clean-room boundary to the system being replaced

**Status:** decided · **Date:** 2026-09-12 (written down; decided during planning)

## Context

Kuben is licensed Apache-2.0. The platform it replaces (called "the
incumbent" throughout the docs) is licensed GPL-3.0. Its source was read
during planning to find the security and design problems Kuben avoids
([KUBEN-GOLDEN-ARCHITECTURE.md](../KUBEN-GOLDEN-ARCHITECTURE.md)). The same
documents proposed bundling or importing the incumbent's template catalog
(more than 160 templates) and flagged that as a licensing risk.

Copying GPL-3.0 code, template definitions, prose, schemas or artwork into
Kuben would subject Kuben's distribution to the GPL.

## Decision

- Nothing expressive crosses the boundary: no code, template definitions,
  descriptions, schemas, icons, screenshots or documentation text from the
  incumbent enter this repository.
- Template catalog entries (`crates/kuben-api/src/routes/templates.rs`) are
  written from each image's own upstream documentation. Functional facts that
  come from there, such as the image name and tag, ports, mount paths,
  environment variable names and sensible volume sizes, are not the
  incumbent's expression and may coincide with it.
- Kuben ships no importer for the incumbent's catalog. A migration tool may
  read a user's existing resources from their own cluster at runtime; it must
  not embed the incumbent's templates.
- A larger community catalog, if one comes, lives in a separate repository
  with its own license, and its content is reviewed before Kuben uses it.
- Planning documents may quote short configuration excerpts from the
  incumbent where they criticise it. They are not part of the binary.

## Consequences

- An audit on 2026-09-12 compared all 8 templates with the incumbent's
  source. None of the descriptions or distinctive values appear there, the
  data models are unrelated, and no image or icon files are shared. The only
  overlap (Vaultwarden's image, port, mount path and volume size) is the
  upstream default.
- Reviewers check this boundary for every change to templates, console text
  or documentation ([CONTRIBUTING.md](../../CONTRIBUTING.md)).
- The catalog grows more slowly than an import would allow. This is accepted.
