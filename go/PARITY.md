# Rust → Go parity ledger

Baseline: every Rust file as of `86ce940` (origin/main). A file is **done** only when its Go counterpart is reviewed line by line against it, every one of its tests has a Go counterpart, and lint (golangci-lint, NilAway) is clean. `scripts/rust-drift.sh` lists Rust files changed after the baseline.

Status: `todo` · `partial` (the parts a slice needs; the note says what is left) · `ported` (written, tests green, not yet reviewed) · `reviewed` · `dropped` (deliberately not ported; reason given).

| Crate | File | Lines | Rust tests | Go package | Status | Notes |
|---|---|---:|---:|---|---|---|
| kuben | `bootstrap.rs` | 301 | 3 |  | todo | |
| kuben | `bundle.rs` | 184 | 3 |  | todo | |
| kuben | `main.rs` | 85 | 0 | cmd/kuben | partial | serve, setup-token, version; the rest: G5 |
| kuben | `serve.rs` | 867 | 0 | serve | partial | api role only; cluster, builds, background work: S1–S5 |
| kuben | `telemetry.rs` | 36 | 0 |  | todo | |
| kuben | `cli/admin.rs` | 38 | 0 |  | todo | |
| kuben | `cli/agent.rs` | 50 | 0 |  | todo | |
| kuben | `cli/backup.rs` | 561 | 5 |  | todo | |
| kuben | `cli/client.rs` | 685 | 3 |  | todo | |
| kuben | `cli/dns01.rs` | 168 | 2 |  | todo | |
| kuben | `cli/doctor.rs` | 551 | 2 |  | todo | |
| kuben | `cli/mod.rs` | 274 | 2 |  | todo | |
| kuben | `cli/support.rs` | 659 | 4 |  | todo | |
| kuben | `cli/ui.rs` | 372 | 3 |  | todo | |
| kuben | `cli/upgrade.rs` | 134 | 1 |  | todo | |
| kuben | `cli/setup/journal.rs` | 431 | 4 |  | todo | |
| kuben | `cli/setup/mod.rs` | 2210 | 10 |  | todo | |
| kuben | `cli/setup/plan.rs` | 258 | 1 |  | todo | |
| kuben | `cli/setup/platform.rs` | 1138 | 6 |  | todo | |
| kuben-agent | `bootstrap.rs` | 237 | 2 |  | todo | |
| kuben-agent | `enroll.rs` | 903 | 9 |  | todo | |
| kuben-agent | `hub.rs` | 495 | 0 |  | todo | |
| kuben-agent | `lib.rs` | 27 | 0 |  | todo | |
| kuben-agent | `link.rs` | 576 | 3 |  | todo | |
| kuben-agent | `main.rs` | 328 | 3 |  | todo | |
| kuben-agent | `protocol.rs` | 436 | 9 |  | todo | |
| kuben-agent | `runtime.rs` | 657 | 4 |  | todo | |
| kuben-agent | `state.rs` | 351 | 4 |  | todo | |
| kuben-agent | `tls.rs` | 323 | 6 |  | todo | |
| kuben-api | `audit.rs` | 205 | 3 | api (audit.go) | ported |  |
| kuben-api | `authz.rs` | 180 | 1 | api/access | ported |  |
| kuben-api | `client.rs` | 483 | 4 |  | todo | |
| kuben-api | `dns.rs` | 631 | 4 |  | todo | |
| kuben-api | `error.rs` | 100 | 0 | api/problem | ported |  |
| kuben-api | `github.rs` | 750 | 6 |  | todo | |
| kuben-api | `host.rs` | 73 | 2 |  | todo | |
| kuben-api | `image_watch.rs` | 287 | 0 |  | todo | |
| kuben-api | `lib.rs` | 105 | 0 |  | todo | |
| kuben-api | `notify.rs` | 593 | 6 |  | todo | |
| kuben-api | `oci.rs` | 908 | 9 |  | todo | |
| kuben-api | `oidc.rs` | 465 | 5 |  | todo | |
| kuben-api | `openapi.rs` | 231 | 2 | api/gen (ogen) + api/genspec | dropped | spec first: the contract generates the server |
| kuben-api | `previews.rs` | 464 | 0 |  | todo | |
| kuben-api | `setup.rs` | 375 | 2 | api (setup.go) | ported | setup_guide/banner: G5 |
| kuben-api | `sso.rs` | 554 | 5 |  | todo | |
| kuben-api | `state.rs` | 151 | 0 |  | todo | |
| kuben-api | `stream.rs` | 248 | 2 | api/stream | partial | visibility filter and deltas with the projections (S1) |
| kuben-api | `transport.rs` | 157 | 0 |  | todo | |
| kuben-api | `web.rs` | 106 | 1 | api/web | ported |  |
| kuben-api | `bin/openapi.rs` | 23 | 0 |  | todo | |
| kuben-api | `auth/mod.rs` | 462 | 1 | api (session_routes.go), api/httpx | ported | SSO routes: S4 |
| kuben-api | `auth/password.rs` | 75 | 1 | api/auth | ported |  |
| kuben-api | `auth/session.rs` | 202 | 4 | api/auth, api (identity.go) | ported |  |
| kuben-api | `auth/sso.rs` | 260 | 1 |  | todo | |
| kuben-api | `auth/throttle.rs` | 200 | 5 | api (identity.go) | ported |  |
| kuben-api | `routes/access.rs` | 322 | 2 |  | todo | |
| kuben-api | `routes/audit.rs` | 123 | 0 |  | todo | |
| kuben-api | `routes/ci.rs` | 443 | 3 |  | todo | |
| kuben-api | `routes/controls.rs` | 731 | 2 |  | todo | |
| kuben-api | `routes/domains.rs` | 703 | 0 |  | todo | |
| kuben-api | `routes/environments.rs` | 326 | 0 |  | todo | |
| kuben-api | `routes/git.rs` | 298 | 0 |  | todo | |
| kuben-api | `routes/health.rs` | 54 | 0 | api (probes.go) | ported | seq/pods from projections (S1) |
| kuben-api | `routes/incidents.rs` | 511 | 2 |  | todo | |
| kuben-api | `routes/members.rs` | 267 | 0 |  | todo | |
| kuben-api | `routes/mod.rs` | 26 | 0 |  | todo | |
| kuben-api | `routes/policy.rs` | 299 | 2 |  | todo | |
| kuben-api | `routes/previews.rs` | 325 | 0 |  | todo | |
| kuben-api | `routes/projects.rs` | 189 | 0 | api (projects.go) | partial | list and get; create/delete with lifecycle (S1) |
| kuben-api | `routes/registries.rs` | 262 | 2 |  | todo | |
| kuben-api | `routes/request.rs` | 64 | 1 | api (projects.go, access) | partial | duplicate(): S1 |
| kuben-api | `routes/scope.rs` | 301 | 1 | api (projects.go) | partial | project scope; environment/app scopes: S1 |
| kuben-api | `routes/secrets.rs` | 654 | 2 |  | todo | |
| kuben-api | `routes/status.rs` | 322 | 0 |  | todo | |
| kuben-api | `routes/templates.rs` | 543 | 2 |  | todo | |
| kuben-api | `routes/tokens.rs` | 229 | 0 |  | todo | |
| kuben-api | `routes/validate.rs` | 173 | 4 |  | todo | |
| kuben-api | `routes/vulnerabilities.rs` | 267 | 1 |  | todo | |
| kuben-api | `routes/apps/admission.rs` | 190 | 2 |  | todo | |
| kuben-api | `routes/apps/approvals.rs` | 309 | 2 |  | todo | |
| kuben-api | `routes/apps/builds.rs` | 273 | 2 |  | todo | |
| kuben-api | `routes/apps/crud.rs` | 419 | 0 |  | todo | |
| kuben-api | `routes/apps/deployments.rs` | 510 | 2 |  | todo | |
| kuben-api | `routes/apps/doctor.rs` | 231 | 0 |  | todo | |
| kuben-api | `routes/apps/domains.rs` | 90 | 0 |  | todo | |
| kuben-api | `routes/apps/evidence.rs` | 296 | 2 |  | todo | |
| kuben-api | `routes/apps/export.rs` | 544 | 2 |  | todo | |
| kuben-api | `routes/apps/image_policy.rs` | 220 | 0 |  | todo | |
| kuben-api | `routes/apps/jobs.rs` | 105 | 1 |  | todo | |
| kuben-api | `routes/apps/logs.rs` | 616 | 3 |  | todo | |
| kuben-api | `routes/apps/metrics.rs` | 131 | 0 |  | todo | |
| kuben-api | `routes/apps/mod.rs` | 758 | 2 |  | todo | |
| kuben-api | `routes/apps/promote.rs` | 358 | 1 |  | todo | |
| kuben-api | `routes/apps/releases.rs` | 261 | 2 |  | todo | |
| kuben-api | `routes/apps/scans.rs` | 192 | 0 |  | todo | |
| kuben-api | `routes/apps/source.rs` | 453 | 3 |  | todo | |
| kuben-api | `routes/apps/spec.rs` | 689 | 5 |  | todo | |
| kuben-core | `artifact.rs` | 99 | 2 | core/artifact | ported | |
| kuben-core | `authz.rs` | 141 | 1 | core/authz | ported | |
| kuben-core | `capacity.rs` | 334 | 6 | core/capacity | ported | |
| kuben-core | `ci.rs` | 541 | 7 | core/ci | ported | |
| kuben-core | `config.rs` | 1169 | 16 | core/config | ported | |
| kuben-core | `domain.rs` | 146 | 3 | core/domain | ported | |
| kuben-core | `error.rs` | 53 | 0 | core/kerr | ported | |
| kuben-core | `ids.rs` | 147 | 2 | core/ids | ported | |
| kuben-core | `image_policy.rs` | 263 | 4 | core/imagepolicy | ported | |
| kuben-core | `lib.rs` | 28 | 0 | — (module list) | ported | |
| kuben-core | `model.rs` | 221 | 2 | core/model | ported | |
| kuben-core | `perm.rs` | 214 | 5 | core/perm | ported | |
| kuben-core | `policy.rs` | 568 | 11 | core/policy | ported | |
| kuben-core | `preview.rs` | 147 | 4 | core/preview + core/policy (ForPreview) | ported | |
| kuben-core | `scan.rs` | 539 | 5 | core/scan | ported | |
| kuben-core | `source.rs` | 780 | 9 | core/source | ported | |
| kuben-core | `sso.rs` | 342 | 5 | core/sso | ported | |
| kuben-core | `status.rs` | 105 | 2 | core/status | ported | |
| kuben-core | `support.rs` | 237 | 3 | core/support | ported | |
| kuben-core | `time.rs` | 14 | 0 | core/clock | ported | |
| kuben-core | `traits.rs` | 166 | 4 | core/authz (policy); LeaderElector dropped | ported | |
| kuben-core | `upgrade.rs` | 449 | 4 | core/upgrade | ported | |
| kuben-core | `ops/build.rs` | 324 | 11 | core/ops/build | ported | |
| kuben-core | `ops/mod.rs` | 29 | 0 | core/ops | ported | |
| kuben-core | `ops/outcome.rs` | 564 | 9 | core/ops/outcome | ported | |
| kuben-core | `ops/run.rs` | 431 | 10 | core/ops/run | ported | |
| kuben-core | `ops/target.rs` | 358 | 8 | core/ops/target | ported | |
| kuben-crd | `lib.rs` | 93 | 3 |  | todo | |
| kuben-crd | `bin/crdgen.rs` | 24 | 0 |  | todo | |
| kuben-crd | `v1alpha1/app.rs` | 327 | 2 |  | todo | |
| kuben-crd | `v1alpha1/buildrun.rs` | 55 | 0 |  | todo | |
| kuben-crd | `v1alpha1/common.rs` | 63 | 0 |  | todo | |
| kuben-crd | `v1alpha1/config.rs` | 133 | 0 |  | todo | |
| kuben-crd | `v1alpha1/environment.rs` | 112 | 0 |  | todo | |
| kuben-crd | `v1alpha1/mod.rs` | 21 | 0 |  | todo | |
| kuben-crd | `v1alpha1/project.rs` | 65 | 0 |  | todo | |
| kuben-crd | `v1alpha1/release.rs` | 57 | 0 |  | todo | |
| kuben-crd | `v1alpha1/runtime.rs` | 228 | 6 |  | todo | |
| kuben-crd | `v1alpha1/task.rs` | 228 | 4 |  | todo | |
| kuben-platform | `activator.rs` | 22 | 0 |  | todo | |
| kuben-platform | `agentlink.rs` | 580 | 3 |  | todo | |
| kuben-platform | `discovery.rs` | 734 | 5 |  | todo | |
| kuben-platform | `doctor.rs` | 701 | 4 |  | todo | |
| kuben-platform | `duration.rs` | 57 | 2 |  | todo | |
| kuben-platform | `evidence.rs` | 391 | 5 |  | todo | |
| kuben-platform | `health.rs` | 159 | 1 | platform/health | ported |  |
| kuben-platform | `leader.rs` | 313 | 2 |  | todo | |
| kuben-platform | `lib.rs` | 39 | 0 |  | todo | |
| kuben-platform | `local_agent.rs` | 341 | 2 |  | todo | |
| kuben-platform | `registry.rs` | 212 | 4 |  | todo | |
| kuben-platform | `secrets.rs` | 664 | 10 |  | todo | |
| kuben-platform | `supervise.rs` | 133 | 3 |  | todo | |
| kuben-platform | `usage.rs` | 430 | 4 |  | todo | |
| kuben-platform | `render/mod.rs` | 551 | 9 |  | todo | |
| kuben-platform | `materializer/agent.rs` | 365 | 2 |  | todo | |
| kuben-platform | `materializer/detach.rs` | 166 | 0 |  | todo | |
| kuben-platform | `materializer/drift.rs` | 311 | 2 |  | todo | |
| kuben-platform | `materializer/fence.rs` | 99 | 2 |  | todo | |
| kuben-platform | `materializer/lifecycle.rs` | 283 | 0 |  | todo | |
| kuben-platform | `materializer/mod.rs` | 31 | 0 |  | todo | |
| kuben-platform | `materializer/progress.rs` | 134 | 2 |  | todo | |
| kuben-platform | `materializer/render.rs` | 652 | 7 |  | todo | |
| kuben-platform | `materializer/secrets.rs` | 339 | 3 |  | todo | |
| kuben-platform | `materializer/worker.rs` | 663 | 1 |  | todo | |
| kuben-platform | `materializer/write.rs` | 100 | 1 |  | todo | |
| kuben-platform | `controller/app.rs` | 357 | 1 |  | todo | |
| kuben-platform | `controller/crd_apply.rs` | 21 | 0 |  | todo | |
| kuben-platform | `controller/environment.rs` | 227 | 0 |  | todo | |
| kuben-platform | `controller/gateway.rs` | 917 | 11 |  | todo | |
| kuben-platform | `controller/mod.rs` | 268 | 2 |  | todo | |
| kuben-platform | `controller/project.rs` | 71 | 0 |  | todo | |
| kuben-platform | `controller/resources.rs` | 1807 | 16 |  | todo | |
| kuben-platform | `build/evidence.rs` | 151 | 2 |  | todo | |
| kuben-platform | `build/job.rs` | 837 | 9 |  | todo | |
| kuben-platform | `build/mod.rs` | 120 | 0 |  | todo | |
| kuben-platform | `build/observe.rs` | 167 | 4 |  | todo | |
| kuben-platform | `build/rescan.rs` | 268 | 1 |  | todo | |
| kuben-platform | `build/scenarios.rs` | 515 | 17 |  | todo | |
| kuben-platform | `build/steps.rs` | 357 | 9 |  | todo | |
| kuben-platform | `build/worker.rs` | 986 | 4 |  | todo | |
| kuben-platform | `projection/informer.rs` | 273 | 0 |  | todo | |
| kuben-platform | `projection/mod.rs` | 792 | 7 |  | todo | |
| kuben-platform | `projection/views.rs` | 526 | 2 |  | todo | |
| kuben-store | `db.rs` | 195 | 2 | store (store.go, pool.go, errors.go) | ported | 2 → 4 Go tests (+ error messages, id encoding); acquire timeout via a pool wrapper |
| kuben-store | `lib.rs` | 13 | 0 | store (package doc) | ported | |
| kuben-store | `testing.rs` | 55 | 0 | store/pgtest | ported | skip → failure with KUBEN_REQUIRE_PG=1; schema dropped after the test; `Schema` for migrator tests |
| kuben-store | `repo/acceptance.rs` | 451 | 3 |  | todo | |
| kuben-store | `repo/agents.rs` | 875 | 7 |  | todo | |
| kuben-store | `repo/audit.rs` | 130 | 0 | store (audit.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/backups.rs` | 357 | 2 |  | todo | |
| kuben-store | `repo/builds.rs` | 1794 | 11 |  | todo | |
| kuben-store | `repo/capabilities.rs` | 198 | 1 |  | todo | |
| kuben-store | `repo/catalog.rs` | 711 | 2 | store (catalog.go, partial) | todo | environments/environment ported with the projects half of lists_what_the_api_shows; apps, runs, run phases, config revisions, domains, application left |
| kuben-store | `repo/ci.rs` | 471 | 3 |  | todo | |
| kuben-store | `repo/controls.rs` | 707 | 2 |  | todo | |
| kuben-store | `repo/deployments.rs` | 1290 | 4 |  | todo | |
| kuben-store | `repo/detach.rs` | 285 | 0 |  | todo | |
| kuben-store | `repo/domains.rs` | 503 | 2 |  | todo | |
| kuben-store | `repo/image_policies.rs` | 357 | 1 |  | todo | |
| kuben-store | `repo/installs.rs` | 84 | 1 |  | todo | |
| kuben-store | `repo/lifecycle.rs` | 375 | 2 |  | todo | |
| kuben-store | `repo/materialize.rs` | 927 | 6 |  | todo | |
| kuben-store | `repo/mod.rs` | 88 | 0 |  | todo | |
| kuben-store | `repo/notify.rs` | 749 | 3 |  | todo | |
| kuben-store | `repo/operations.rs` | 804 | 5 |  | todo | |
| kuben-store | `repo/orgs.rs` | 320 | 0 | store (orgs.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/policies.rs` | 888 | 8 |  | todo | |
| kuben-store | `repo/previews.rs` | 740 | 2 |  | todo | |
| kuben-store | `repo/product.rs` | 693 | 3 | store (tenant.go) | ported | 3 → 3 |
| kuben-store | `repo/releases.rs` | 144 | 0 |  | todo | |
| kuben-store | `repo/resolve.rs` | 244 | 2 |  | todo | |
| kuben-store | `repo/retention.rs` | 182 | 2 |  | todo | |
| kuben-store | `repo/rollups.rs` | 145 | 1 |  | todo | |
| kuben-store | `repo/scans.rs` | 683 | 3 |  | todo | |
| kuben-store | `repo/secrets.rs` | 1283 | 5 |  | todo | |
| kuben-store | `repo/sessions.rs` | 125 | 0 | store (sessions.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/sso.rs` | 326 | 3 |  | todo | |
| kuben-store | `repo/status.rs` | 244 | 1 |  | todo | |
| kuben-store | `repo/support.rs` | 160 | 1 |  | todo | |
| kuben-store | `repo/throttle.rs` | 64 | 0 | store (throttle.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/tokens.rs` | 154 | 0 | store (tokens.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/upgrades.rs` | 260 | 1 | store (upgrades.go) | ported | 1 → 3 Go tests (+ version order, char truncation) |
| kuben-store | `repo/usage.rs` | 127 | 1 |  | todo | |
| kuben-store | `repo/users.rs` | 140 | 0 | store (users.go) | ported | covered by the tests/matrix.rs port |
| kuben-agent | `tests/link.rs` | 756 | 15 |  | todo | |
| kuben-agent | `tests/runtime.rs` | 297 | 3 |  | todo | |
| kuben-api | `tests/http.rs` | 3927 | 44 |  | todo | |
| kuben-api | `tests/oci.rs` | 41 | 2 |  | todo | |
| kuben-platform | `tests/agent_link_mtls.rs` | 260 | 6 |  | todo | |
| kuben-platform | `tests/execution_crds.rs` | 277 | 2 |  | todo | |
| kuben-platform | `tests/materializer.rs` | 633 | 5 |  | todo | |
| kuben-platform | `tests/two_writer_cas.rs` | 185 | 3 |  | todo | |
| kuben-store | `tests/matrix.rs` | 261 | 1 | store (matrix_test.go, partial) | todo | every identity section ported; the releases section waits for repo/releases.rs |
| kuben-store | `tests/ops_store_pg.rs` | 341 | 1 |  | todo | |

Totals: 231 files, 87964 lines, 685 Rust tests; ported 38.
