# G0 spikes (2026-09-21)

Both spikes ran on darwin/arm64 with Go 1.27.1, cross-compiling
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w"`.

## Spike 1 — OpenAPI 3.1 server generation → **ogen**

Input: `packages/api-client/openapi.json` unchanged (OpenAPI 3.1.0, 129
operations, 114 schemas, 181 `type: [T, "null"]`, 10 `oneOf`, 23
`additionalProperties: false`).

| | ogen v1.24.0 | oapi-codegen v2.8.0 (std-http, strict) |
|---|---|---|
| Reads the 3.1 spec as is | yes | yes |
| Generated code compiles | **yes**, `go vet` clean | **no**: `OwnerDto` (`oneOf`) has no JSON methods; the SBOM response type is invalid |
| Operations in the handler interface | 129 | 129 |
| Optional/nullable values | `Opt*`/`OptNil*` value types (no pointers: fits NilAway) | `*T` pointers |
| JSON | generated encoders/decoders (`go-faster/jx`), no reflection | `encoding/json` |
| Validation | generated validators (enums, formats, lengths) | none on the server |
| Generation time | ~8 s | ~3 s |

Decision: **ogen**, server only, without OpenTelemetry
(`generator.features: {disable_all: true, enable: [paths/server, ogen/unimplemented]}`).
Size cost over a bare `net/http` binary: **+2.9 MiB** (8.4 vs 5.5 MiB).

Consequences:
- The two `text/event-stream` endpoints (event stream, log follow) are
  declared as JSON in the spec and are served by hand-written handlers
  mounted in front of the generated router.
- Domain types stay separate from the generated `Opt*` DTOs; conversion
  happens in the route layer (CONVENTIONS: pointers and generated shapes
  die at the boundary).

## Spike 2 — binary size with the real dependencies

A `main` that uses each dependency for real (so the linker keeps what
production code keeps), no Kuben code yet:

| Binary | Contents | Size (stripped) |
|---|---|---|
| hub | controller-runtime (manager, leader election), client-go dynamic, k8s.io/metrics, gateway-api types, pgx/v5 pool, BuildKit client, go-containerregistry, go-github + ghinstallation, lego + Cloudflare DNS provider, miekg/dns, argon2id, go-oidc, cobra, Prometheus, golang-lru, automemlimit | **58.3 MiB** |
| agent, with client-go discovery + restmapper | dynamic client, SSA, TLS, automemlimit | 25.4 MiB |
| agent, **without** discovery | the same minus discovery/restmapper | **10.4 MiB** |

Largest contributors to the hub (packages): client-go 275, grpc 66,
apimachinery 59, k8s.io/api 58, controller-runtime 47, BuildKit 44,
protobuf 39, go-containerregistry 25.

Decisions:
- **Agent budget stays a hard 25 MiB, target ≈ 12 MiB**: the agent maps
  kinds to resources with a small resolver over the API server's
  `/api` and `/apis` resource lists (plain REST client, cached), not
  client-go's discovery package, whose OpenAPI dependencies cost 15 MiB.
  The hub-agent protocol does not change.
- **Hub**: expected ≈ 65–70 MiB with Kuben's own code, the generated
  server and the embedded console. The size budget for the hub is a
  warning only (owner's decision); it is set to 75 MiB.
- RSS (idle hub with 200 pods, idle agent) needs a cluster and is
  measured in CI's kind e2e job; the hard gates of plan §11 apply there.
