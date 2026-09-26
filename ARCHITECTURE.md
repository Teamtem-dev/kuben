# Architecture

Kuben is one Go module, `github.com/Teamtem-dev/kuben`, at the root of this
repository, plus the web console (`apps/console`, embedded into the hub) and
the website (`apps/site`). This file is the map of the Go code: what each
package is, and the dependency rules the linter enforces. How the code is
written (errors, optional values, state machines, tests) is in
[`go/CONVENTIONS.md`](go/CONVENTIONS.md); which Rust file each package
replaced is in [`go/PARITY.md`](go/PARITY.md).

## Two binaries

- **`kuben`, the hub** (`cmd/kuben`): the server (`kuben serve`: the HTTP API,
  the console, the controllers and every background task) and the command
  line (`kuben setup`, `migrate`, `backup`, `doctor`, and the client commands
  `login`, `apps`, `deploy`, …).
- **`kuben-agent`, the agent** (`cmd/kuben-agent`): runs inside each cluster
  Kuben manages, dials the hub over AgentLink and applies the execution
  envelopes it receives with server-side apply. Nothing else (ADR-027).

Go links only the packages a binary imports, so the agent stays small as long
as it keeps to the packages the rules below allow it.

## Packages

```text
cmd/
  kuben/                      the hub's main: builds the CLI and runs it
  kuben-agent/                the agent's main
api/
  v1alpha1/                   the kuben.dev/v1alpha1 CRD types (kubebuilder layout, generated DeepCopy)
internal/
  agent/                      the cluster agent (doc only)
    bootstrap/                the agent in the hub's own cluster: enrolls without a hand-carried token
    clock/                    the agent's wall clock, the one place that reads it
    link/                     the agent's end of AgentLink: dial, hello, heartbeats, redial with backoff
    runtime/                  the executor: applies an envelope with client-go's dynamic client
    state/                    the agent's state on disk and its identity
  agentlink/                  the hub's end of AgentLink: cluster CA, enrollment, the listener, dispatch
    protocol/                 the AgentLink wire protocol both ends share: messages, framing, TLS, negotiation
  apiclient/                  the Kuben API client the CLI's client commands use
  build/                      Git → isolated build (BuildKit Jobs, evidence, rescans)
  bundlelock/                 the pinned components a release installs besides Kuben (bundle.lock.json)
  cli/                        the `kuben` command line: flags, prompts and output only
    dns01/                    `kuben dns01-issuer`, a cert-manager DNS-01 webhook for Cloudflare
    doctor/                   `kuben doctor`, the preflight checks
    setup/                    `kuben setup` and `uninstall`: this machine becomes a Kuben server
    supportbundle/            `kuben support-bundle`: a local file for a support conversation
    ui/                       the terminal output style of the CLI
    upgrade/                  `kuben upgrade-check`: prints the upgrade preflight
  core/                       the domain: types, rules and state machines; no IO, no goroutines
    artifact/                 OCI artifact references (images by digest)
    ascii/                    ASCII-only text operations
    authz/                    authorization: subjects, scopes, proofs
    capacity/                 quota and scheduling admission arithmetic
    ci/                       trust for external CI (GitHub Actions OIDC)
    clock/                    unix-millisecond time, the Clock interface, saturating arithmetic
    compat/                   the support envelope: which platforms and versions are supported
    config/                   the configuration, the contract with operators
    dnsname/                  DNS names and domain claims
    ids/                      typed UUIDv7 identifiers
    imagepolicy/              image update policies (which tag an app follows)
    kerrors/                  the domain error with a stable code every layer shares
    model/                    identity and audit models owned by SQL
    ops/                      what the durable operation state machines share
      build/                  BuildAttempt phases
      outcome/                what a build Job's state means for its attempt
      run/                    DeploymentRun phases
      target/                 an application target's control state
    opt/                      opt.Val, the optional value that cannot be dereferenced when absent
    perm/                     permissions and the built-in roles
    policy/                   environment policies and deployment approvals
    preview/                  preview environment naming and lifetimes
    scan/                     SBOMs and vulnerability scans
    source/                   Git sources and the GitHub webhook payloads Kuben acts on
    sso/                      who may sign in through OpenID Connect, with which role
    status/                   what a public status page says about an app
    upgrade/                  supported version steps and the upgrade preflight rules
  doctor/                     why an app is or is not reachable, as a list of checks
  evidence/                   the Doctor's evidence graph
  firstrun/                   the hub's first boot: the default org, the admin, the setup token and link
  health/                     the subsystem health registry behind /livez and /readyz
  host/                       the machine Kuben runs on: its advertised address, owner-only files
  httpapi/                    the HTTP API handlers (the server generated from openapi.json)
    access/                   who may do what on a request
    apidocs/                  the API reference page at /api/docs
    auth/                     credentials: argon2id password hashes, sessions, API tokens
    gen/                      generated by ogen from packages/api-client/openapi.json
    genspec/                  prepares the spec for ogen (go generate)
    httpx/                    request plumbing: principal, client address, CSRF guard, recovery
    problem/                  RFC 9457 problem+json responses
    stream/                   the server-sent event stream of the console
    web/                      the embedded console and its Content-Security-Policy
  imagewatch/                 the image update watcher: follows tag patterns, deploys new digests
  install/
    journal/                  the installer's journal: what `kuben setup` did and who owns what
  integrations/               clients of other systems
    dns/                      DNS over HTTPS lookups and the Cloudflare DNS adapter
    github/                   the GitHub App: JWTs, installation tokens, webhooks
    oci/                      OCI registries: tag lists, digests, build output verification
    oidc/                     OpenID Connect token verification (JWKS, RS256)
    outbound/                 the HTTP transport every outbound call goes through
    sso/                      the OpenID Connect client for single sign-on
      ssotest/                a test identity provider
  jsonx/                      strict JSON decoding as serde did it, and canonical JSON for hashes
  keyring/                    the secret keyring: sealing and opening managed secret values
  kube/                       everything that talks to Kubernetes on the hub side
    controller/               the reconcilers (server-side apply, field manager `kuben`)
    discovery/                cluster capability discovery
    envtest/                  test support: a shared envtest API server for a package's tests
    leader/                   leader election (the Lease `kuben-controller`)
    materializer/             turns SQL desired state into cluster objects
    projection/               informers and the in-memory read models the console streams
    registry/                 the clients of the managed clusters
    render/                   an App's spec and the cluster's capabilities become a RenderPlan
  maintenance/
    backup/                   `kuben backup` and `restore`: pg_dump archives, manifest, fencing
    upgrade/                  the journaled migration and the upgrade preflight
  metrics/                    Prometheus metrics
  notify/                     webhook delivery and incidents
  previews/                   preview environments: the pull request lifecycle and its janitor
  server/                     `kuben serve`: wiring, background work, ordered shutdown
  store/                      PostgreSQL persistence (product state, operations, identity, audit)
    migrate/                  runs migrations on sqlx's ledger, as the Rust binary did
    migrations/               the embedded SQL migrations
    pgtest/                   test support: a schema per test on a real PostgreSQL
  supervise/                  runs a subsystem and restarts it with backoff
  usage/                      bounded CPU and memory usage from the Metrics API
  version/                    the build's version, set by the linker
test/
  oracle/                     compares the Go hub with the released Rust 1.2 binary
testdata/                     fixtures several packages share (Rust-written compat files)
tools/                        a separate module pinning the Go tools (`go tool -modfile=tools/go.mod …`)
```

`testdata/` directories next to a package belong to that package.

## Dependency rules

Enforced by depguard in [`.golangci.yml`](.golangci.yml) (direct imports) and
by `internal/server/layering_test.go` (transitive):

1. **`internal/core/**` imports only `internal/core/**` and `internal/jsonx`.**
   Both are pure: no IO, no goroutines, no `net/http`, `database/sql` or `os`
   (config loading is the one exception). Everything else may import core.
2. **The agent keeps to itself.** `internal/agent/**` and `cmd/kuben-agent`
   import only `internal/agent/**`, `internal/agentlink/protocol`,
   `api/v1alpha1`, `internal/core/**`, `internal/jsonx` and `internal/version`
   from this module, and never controller-runtime.
3. **Only `internal/server` imports `internal/httpapi`.** Code that the routes
   and something else both need lives in a package below it (`firstrun` for
   the setup token, `store`, `keyring`, `previews`, …). The subpackages
   (`httpapi/auth`, `httpapi/stream`, …) may be used where they fit.
4. **Nothing imports `internal/cli` except `cmd/kuben`** (and the CLI's own
   packages). The CLI parses flags and prints; logic both the CLI and the
   server use lives in `maintenance/`, `install/`, `firstrun/`, `host/`, and
   so on. The server must not reach `internal/cli` even transitively.

Beyond the rules, the direction is: `cmd` → `cli` / `server` → `httpapi` →
the feature packages (`previews`, `imagewatch`, `notify`, `build`, `kube/…`,
`agentlink`, `integrations/…`) → `store`, `keyring` → `core`, `jsonx`.

## Contracts

The JSON of `packages/api-client/openapi.json`, the CRDs in
`charts/kuben/crds/kuben.dev_all.yaml`, the SQL schema, the AgentLink
protocol, configuration keys, `KUBEN_*` variables and the CLI's commands and
flags are frozen: they are what the console, existing installations and
agents rely on. Package names are free to change; these are not.

## Names you will see

- **hub**: the `kuben` binary and the server it runs; the side of AgentLink
  that listens.
- **agent**: `kuben-agent`, the process in each managed cluster.
- **AgentLink**: the mutually authenticated TLS stream an agent opens to the
  hub, and the framed protocol on it (`internal/agentlink/protocol`).
- **envelope**: one unit of work the hub hands an agent over AgentLink.
- **materializer**: the hub component that turns SQL desired state into
  envelopes and cluster objects (`internal/kube/materializer`).
- **projection**: an in-memory, UI-shaped read model fed by informers, which
  the console's event stream publishes (`internal/kube/projection`).
- **render plan**: what an App becomes in a given cluster, frozen per
  deployment run (`internal/kube/render`).
- **keyring**: the set of keys that seal managed secret values at rest
  (`internal/keyring`); not Kubernetes Secrets.
- **first run**: the hub's first boot and the setup page that creates the
  first admin (`internal/firstrun`).
- **oracle**: the test that sends the same requests to the Go hub and the
  released Rust 1.2 binary and compares the answers (`test/oracle`).
- **envtest**: a real kube-apiserver and etcd without controllers, for tests
  (`internal/kube/envtest`).
