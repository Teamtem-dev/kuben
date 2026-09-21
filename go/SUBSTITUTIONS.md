# Substitution ledger

Every place where the Go code replaces Rust code or a Rust crate with a
different library, or with a port whose semantics could drift, has a row
here. A pull request that adds a dependency or swaps an implementation
without a row is not merged (plan §3, principle 3: a substitution is a
behaviour change until proven otherwise).

| Area | Rust | Go | Known differences and how they are closed | Pinned by |
|---|---|---|---|---|
| Semver ranges (image policies) | `semver` crate `VersionReq` | Port of the crate's range grammar, `Display` and matching (`core/imagepolicy/req.go`); `Masterminds/semver/v3` only parses tag versions (`StrictNewVersion`) | Masterminds reads bare `1.2.3` as exact (Rust: caret), treats spaces as AND, has `\|\|`, matches pre-releases more widely → not used for ranges. Masterminds rejects versions over 256 bytes; OCI tags are at most 128. | `imagepolicy_test.go`, 2 fuzz targets |
| Tag globs | hand-written matcher | byte-for-byte port | `path.Match` not used (it treats `/`, `?` and classes specially) | `imagepolicy_test.go` |
| Domain names (IDNA) | `idna::domain_to_ascii` | `golang.org/x/net/idna` custom profile: MapForLookup, BidiRule, CheckJoiners, no hyphen/STD3/DNS-length checks, then the Rust LDH and length checks | `idna.Lookup` rejects `ab--cd.com` and applies STD3 → not used. Unicode table versions differ (x/net: Unicode 17): a newly assigned code point could map differently; x/net is pinned and existing domains are checked before cutover C1. | `domain_test.go` (+ quick property) |
| Advisory lock keys | `domain::lock_key` | same bytes | none | `domain_test.go` |
| Timestamps from GitHub | `jiff::Timestamp::from_str` | `time.Parse(time.RFC3339Nano)` | Go rejects lowercase `t`/`z`, space separator, `:60`, RFC 9557 suffixes; GitHub sends none. Pre-1970 fractions floor instead of truncate. | `source_test.go` |
| Configuration | `figment` (defaults → /etc/kuben/config.toml → ./kuben.toml searched upward → `KUBEN_*` with `__`) | `koanf/v2` + a port of figment's env-value grammar + own nesting (koanf's `maps.Unflatten` clobbers tables) + strict mapstructure decoding | Decode error texts differ. A bare number/boolean for a text setting from env is accepted as text (figment refused it unquoted) — accepted by the owner 2026-09-21. | `config` tests, `FuzzEnvValue` |
| Secrets in configuration | plain `String` | `config.Secret` (redacted in fmt/%v/%+v/%#v/%d/JSON) | `sso.client_secret = ""` means unset — accepted 2026-09-21. | `config` tests |
| JSON output | `serde_json` | `encoding/json` through `internal/wire` (to do) | `encoding/json` escapes `<`, `>`, `&`; serde does not. Every contract JSON (render plans, hashes, webhooks) must go through the non-escaping encoder. | to do (G0/S1) |
| Rust `{:?}` in messages | Debug formatting | `%q` / local `rustQuote` | differs only for control and grapheme-extend characters | package tests |
| HTTP API server | axum + utoipa (code first) | ogen v1.24 (spec first, server only) from the unchanged `openapi.json` | Generated `Opt*`/`OptNil*` DTOs instead of domain types: converted at the route layer. The two SSE endpoints are hand-written handlers. ogen validates requests from the spec (enums, formats, required members). | `go/SPIKES.md`; route tests (S1+) |
| Agent kind→resource mapping | kube-rs discovery | small resolver over `/api` and `/apis` resource lists (no client-go discovery: −15 MiB) | Same answers for served resources; no OpenAPI schema download. | agent tests (S2) |
