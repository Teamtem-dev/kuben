# Go conventions for Kuben

These rules exist so the Go code keeps what Rust gave for free (no null
dereference, exhaustive state handling, no data races, errors that cannot be
ignored) and drops what was only there because of Rust.

## Layout

- `go/hub` is the hub (the `kuben` binary), `go/agent` the cluster agent, `go/kubenapi`
  what they share (CRD types, protocol). Everything not meant for import lives
  under `internal/`.
- `internal/core/**` performs **no IO**, starts no goroutines and imports
  nothing from `store`, `platform` or `api`. One package per domain concept,
  named after it (`perm`, `authz`, `preview`, `imagepolicy`, `ops/run`, …).
- While the Rust code still exists, each Go package says in its package
  comment which Rust module it replaces, and ports **every** test of it.

## Contracts are frozen

JSON field names, enum strings, error codes, audit action strings, SQL and
CRD shapes are the contract with the console, the database, the agents and
existing installations. Read the serde attributes (`rename_all`, `rename`,
`skip_serializing_if`, `default`, `tag`, `untagged`, `flatten`) of the Rust
type and reproduce the exact wire form; add a test that pins it.

## No nil dereference

- Optional domain values are `opt.Val[T]`, never `*T`. It marshals as the
  value or `null`; with `json:",omitzero"` it is left out (serde's
  `skip_serializing_if = "Option::is_none"`).
- Pointers are for mutation and for large structs passed down a call chain,
  not for "maybe". A function that can fail to find something returns
  `(T, bool)` or `(T, error)`, never a nil `*T` with a nil error.
- Maps and slices are read with the comma-ok form or a length check. Zero
  values must be safe to use: prefer `Valid()`-style checks over constructors
  that callers can bypass.
- CI runs NilAway; a finding is a build failure, not a suggestion.

## Closed sets and state machines

- A Rust enum without data is a named string type with constants that carry
  the wire strings, plus `ParseX(string) (X, error)` returning a
  `kerr.Validation` error. Every `switch` over it lists every constant (the
  `exhaustive` linter fails the build otherwise); `default:` is not a way
  around it.
- A Rust enum **with** data is a sealed interface: an unexported marker
  method, one struct per variant, and `//sumtype:decl` above the interface so
  `gochecksumtype` makes type switches exhaustive.
- State machines declare their transitions as data (`from → allowed to`) and
  expose one `Transition`/`Next` function that refuses everything else. No
  caller sets a state field directly; tests walk the whole table, including
  every refused pair.
- Invalid states must be unrepresentable where Go allows it (unexported
  fields plus a validating constructor) and detected at the boundary where it
  does not (`Validate() error`, called when decoding and before storing).

## Errors

- Domain failures are `*kerr.Error` with a stable code; wrap with
  `fmt.Errorf("doing x: %w", err)` so the chain stays inspectable with
  `errors.Is/As`. A module whose Rust error enum is matched on by callers
  keeps a typed error of its own.
- No error is dropped (`errcheck`), no panic in library code, no `init()`,
  no package-level mutable state.

## Time, numbers, JSON

- Time is `int64` unix milliseconds. Code that needs "now" takes a
  `clock.Clock`. Rust's saturating arithmetic is `clock.SaturatingAdd` and
  friends: overflow must not wrap.
- `serde_json::Value` is `any` decoded with `json.Unmarshal` (objects are
  `map[string]any`); an opaque stored blob is `json.RawMessage`.
- Unsigned Rust integers stay unsigned where the value is a count or a size;
  convert at the SQL boundary.

## Concurrency (outside core)

- Every goroutine has an owner, a `context.Context` and a way to stop; use
  `errgroup` or `internal/platform/supervise`, never a bare `go` statement in
  business code. Shared state is owned by one goroutine or guarded by a
  mutex declared next to the fields it guards.
- All tests run with `-race`.

## What is deliberately *not* ported

- Getter methods for plain fields, `as_str` next to `Display`, `Default`
  impls whose defaults are zero values, `From`/`Into` conversions used once,
  builder macros, `#[must_use]`, `async-trait` indirection, feature flags.
- Traits with a single implementation: use the concrete type until a second
  one exists (tests included).
- Hand-written clients for things Go has a maintained library for (OCI
  registries, DNS, ACME, GitHub, Kubernetes informers, leader election, CRD
  generation). Keep the behaviour and the tests, replace the plumbing.

## Tests

- Table-driven, in the external `x_test` package, standard library plus
  `go-cmp`. `proptest` properties become `testing/quick` checks or fuzz
  targets with a seed corpus. No sleeping: inject the clock.
- Database tests use a real PostgreSQL (`KUBEN_TEST_PG_URL`); Kubernetes
  tests use envtest. No mocks of the database. Without the URL a database
  test skips with a message, and CI sets `KUBEN_REQUIRE_PG=1`, which turns
  that skip into a failure: no database test is ever skipped silently.

## Style

- gofumpt, goimports with the module as local prefix, golangci-lint clean.
- Every exported identifier has a doc comment that says what it means, not
  what its name already says. Comments explain why; code, comments, logs and
  commits are English.
- Functions stay under 100 lines; split along meaning, not to please the
  linter.

## Shared helpers (use them, do not copy them)

- `internal/wire`: strict JSON decoding as serde did it (`Required`,
  `Optional`, `Take`/`TakeOptional` for flattened payloads, `Must[T]` for
  struct fields). Contract-exact encoding (canonical JSON for hashes) lands
  here too.
- `internal/core/ascii`: ASCII-only text operations (Rust's
  `to_ascii_lowercase`); `strings.ToLower` folds all of Unicode.
- `internal/core/clock`: time and saturating integer arithmetic.

## Tools and checks

Tool versions are pinned in `go/tools` (`go tool <name>` from anywhere in the
workspace). golangci-lint is the exception: its authors advise against
building it from source, so CI runs the official action at a pinned version
with `.golangci.yml`.

```bash
cd go/hub
go tool gofumpt -l .                                   # must print nothing
go vet ./...
go tool exhaustive -default-signifies-exhaustive=false -ignore-enum-types '^reflect\.Kind$' ./...
go tool go-check-sumtype -default-signifies-exhaustive=false ./...
go tool errcheck -blank -asserts -ignoretests ./...
go tool nilaway -test=true ./...
go test -race -shuffle=on ./...
go tool govulncheck ./...
```

In the sandbox only, load the mirror first: `. .cache/go/env.sh`
(`/.cache` is gitignored; `.cache/go/gofetch.py` fills the module mirror).

Dependencies are added deliberately, by one person at a time, each with a
row in `go/SUBSTITUTIONS.md` when it replaces Rust behaviour: do not run
`go get` or `go mod tidy` as a side effect of another change.
