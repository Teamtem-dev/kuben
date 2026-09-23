# Rust → Go parity ledger

Baseline: every Rust file as of `86ce940` (origin/main). A file is **done** only when its Go counterpart is reviewed line by line against it, every one of its tests has a Go counterpart, and lint (golangci-lint, NilAway) is clean. `scripts/rust-drift.sh` lists Rust files changed after the baseline.

Status: `todo` · `partial` (the parts a slice needs; the note says what is left) · `ported` (written, tests green, not yet reviewed) · `reviewed` · `dropped` (deliberately not ported; reason given).

| Crate | File | Lines | Rust tests | Go package | Status | Notes |
|---|---|---:|---:|---|---|---|
| kuben | `bootstrap.rs` | 301 | 3 |  | todo | |
| kuben | `bundle.rs` | 184 | 3 |  | todo | |
| kuben | `main.rs` | 85 | 0 | cmd/kuben | partial | serve, setup-token, version; the rest: G5 |
| kuben | `serve.rs` | 867 | 0 | serve | partial | api role; with a cluster: informers, readiness on first sync, discovery (controller role), leader election settings (the Lease is campaigned for once reconcilers exist, S1-D). Left: materializer, controllers, AgentLink, builds, background work (S1–S5) |
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
| kuben-api | `oci.rs` | 908 | 9 | api/oci | partial | Parse, Fixed (FixedImages), Registry (RegistryResolver, on go-containerregistry): 5 tests ported (tag_pages split into link and retry halves), 2 dropped (challenge parsing and query encoding are go-containerregistry's; covered by in-process registry tests), 2 left with RegistryVerifier (build slice, S3) |
| kuben-api | `oidc.rs` | 465 | 5 |  | todo | |
| kuben-api | `openapi.rs` | 231 | 2 | api/gen (ogen) + api/genspec | dropped | spec first: the contract generates the server |
| kuben-api | `previews.rs` | 464 | 0 |  | todo | |
| kuben-api | `setup.rs` | 375 | 2 | api (setup.go) | ported | setup_guide/banner: G5 |
| kuben-api | `sso.rs` | 554 | 5 |  | todo | |
| kuben-api | `state.rs` | 151 | 0 |  | todo | |
| kuben-api | `stream.rs` | 248 | 2 | api/stream, platform/projection (source.go) | ported | 2 → 4 Go tests (the Visibility tests + the stream.Source over the projections: org filter, lag → `resync` without id); the source seeds each connection's filter from its own snapshot taken right after subscribing (Serve asks for snapshot and deltas separately) |
| kuben-api | `transport.rs` | 157 | 0 |  | todo | |
| kuben-api | `web.rs` | 106 | 1 | api/web | ported |  |
| kuben-api | `bin/openapi.rs` | 23 | 0 |  | todo | |
| kuben-api | `auth/mod.rs` | 462 | 1 | api (session_routes.go), api/httpx | ported | SSO routes: S4 |
| kuben-api | `auth/password.rs` | 75 | 1 | api/auth | ported |  |
| kuben-api | `auth/session.rs` | 202 | 4 | api/auth, api (identity.go) | ported |  |
| kuben-api | `auth/sso.rs` | 260 | 1 |  | todo | |
| kuben-api | `auth/throttle.rs` | 200 | 5 | api (identity.go) | ported |  |
| kuben-api | `routes/access.rs` | 322 | 2 | api (access_routes.go) | ported |  |
| kuben-api | `routes/audit.rs` | 123 | 0 | api (audit_routes.go) | ported |  |
| kuben-api | `routes/ci.rs` | 443 | 3 |  | todo | |
| kuben-api | `routes/controls.rs` | 731 | 2 |  | todo | |
| kuben-api | `routes/domains.rs` | 703 | 0 |  | todo | |
| kuben-api | `routes/environments.rs` | 326 | 0 | api (environments.go) | ported | list, get, create (placement on `primary`, initial policy, org environment quota), delete (closes a preview, `env-delete-protected` for production); `TestEnvironmentsReadAndWriteSQL` (the environment half of `environments_and_apps_read_from_sql` and `viewers_can_read_but_not_write`) |
| kuben-api | `routes/git.rs` | 298 | 0 |  | todo | |
| kuben-api | `routes/health.rs` | 54 | 0 | api (probes.go) | ported | seq, pods and cluster from the projections and registry |
| kuben-api | `routes/incidents.rs` | 511 | 2 |  | todo | |
| kuben-api | `routes/members.rs` | 267 | 0 | api (members.go) | ported |  |
| kuben-api | `routes/mod.rs` | 26 | 0 |  | todo | |
| kuben-api | `routes/policy.rs` | 299 | 2 | api (policy_routes.go) | ported | 2 → 2 unit tests + tests/http.rs `m4_weakening_protection_takes_an_owner` |
| kuben-api | `routes/previews.rs` | 325 | 0 |  | todo | |
| kuben-api | `routes/projects.rs` | 189 | 0 | api (projects.go) | ported | list, get (readiness from the org's own projection), create, delete; tests/http.rs `projects_are_tenant_scoped` → `TestProjectsAreTenantScoped` (+ duplicate, delete twice, viewer) |
| kuben-api | `routes/registries.rs` | 262 | 2 |  | todo | |
| kuben-api | `routes/request.rs` | 64 | 1 | api (request.go, projects.go, access) | ported | 1 → 1 (`TestTimestampsAreRFC3339`); `actor` is `access.Access.Actor` |
| kuben-api | `routes/scope.rs` | 301 | 1 | api (scope.go) | partial | project and environment scopes; app scope, sql_target, cluster, kube_error with the app routes |
| kuben-api | `routes/secrets.rs` | 654 | 2 |  | todo | |
| kuben-api | `routes/status.rs` | 322 | 0 |  | todo | |
| kuben-api | `routes/templates.rs` | 543 | 2 | api (templates.go) | ported | 2 → 2 unit tests + tests/http.rs `scenario8_template_catalogue` |
| kuben-api | `routes/tokens.rs` | 229 | 0 | api (tokens.go) | ported |  |
| kuben-api | `routes/validate.rs` | 173 | 4 | api (validate.go) | ported | 4 → 4 |
| kuben-api | `routes/vulnerabilities.rs` | 267 | 1 |  | todo | |
| kuben-api | `routes/apps/admission.rs` | 190 | 2 | api (apps_admission.go, environments.go) | ported | 2 → 2 |
| kuben-api | `routes/apps/approvals.rs` | 309 | 2 | api (apps_approvals.go) | ported | 2 → 2; `GetDeploymentApproval`, `ApproveDeployment`, `RejectDeployment`; `TestRefusalsMapToHTTP`, `TestEligibleToDecide`, `TestM4ProtectedDeploysWaitForAnotherApprover` |
| kuben-api | `routes/apps/builds.rs` | 273 | 2 |  | todo | |
| kuben-api | `routes/apps/crud.rs` | 419 | 0 | api (apps_crud.go) | partial | list, create (image), get, update, delete, restart, handover; `TestAppsReadFromSQL`, `TestViewersCanReadAppsButNotWrite`. Git-sourced create answers 501 until builds (S3) |
| kuben-api | `routes/apps/deployments.rs` | 510 | 2 | api (apps_deployments.go) | ported | 2 → 2; `a_deployment_is_accepted_once_and_can_be_polled`, `a_lost_answer_is_given_again_without_a_second_run`, `deployments_need_deploy_rights_a_pinned_image_and_an_app_in_sql` → Go (PostgreSQL); the input hash is the text Rust hashed, so an Idempotency-Key replay matches across the cutover; `Location` through httpx.SetHeader |
| kuben-api | `routes/apps/doctor.rs` | 231 | 0 |  | todo | |
| kuben-api | `routes/apps/domains.rs` | 90 | 0 | api (apps_domains.go) | ported | DNS hostname verification against Gateway addresses (ok, mismatch, unresolved, unknown); injectable DNSResolver |
| kuben-api | `routes/apps/evidence.rs` | 296 | 2 |  | todo | |
| kuben-api | `routes/apps/export.rs` | 544 | 2 |  | todo | |
| kuben-api | `routes/apps/image_policy.rs` | 220 | 0 |  | todo | |
| kuben-api | `routes/apps/jobs.rs` | 105 | 1 | api (apps_jobs.go) | ported | 1 → 1; `manual_job_names_fit_the_limit`, manual Job creation from live CronJob, 503 without a cluster |
| kuben-api | `routes/apps/logs.rs` | 616 | 3 | api (apps_logs.go) | ported | 3 → 3; `an_apps_objects_are_told_apart_from_its_neighbours`, `a_followed_log_line_keeps_its_time_and_is_cut_on_a_character`, `followed_logs_are_capped_per_user_and_freed_when_closed`, GetAppLogs (once or SSE followed, bounded per-user permit), GetAppEvents |
| kuben-api | `routes/apps/metrics.rs` | 131 | 0 |  | todo | |
| kuben-api | `routes/apps/mod.rs` | 758 | 2 | api (apps.go) | ported | 2 → 2; DTOs set every nullable member explicitly (ogen omits an unset one, serde wrote null); registry logins for resolution wait for the keyring (S2) |
| kuben-api | `routes/apps/promote.rs` | 358 | 1 | api (apps_promote.go) | ported | 1 → 1; `promotion_keeps_target_domains_and_scaling_and_reports_changes`, spec diffs without env values, missing secret warnings against live cluster Secrets and store, dry-run with null app |
| kuben-api | `routes/apps/releases.rs` | 261 | 2 | api (apps_releases.go) | ported | 2 → 2; `scenario5_releases_are_newest_first_and_rollback_is_authorized` → Go (PostgreSQL) |
| kuben-api | `routes/apps/scans.rs` | 192 | 0 |  | todo | |
| kuben-api | `routes/apps/source.rs` | 453 | 3 |  | todo | |
| kuben-api | `routes/apps/spec.rs` | 689 | 5 | api (apps_spec.go) | ported | 5 → 5; `port` is range-checked as u16 (the contract says int32, minimum 0) |
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
| kuben-crd | `lib.rs` | 93 | 3 | kubenapi/v1alpha1 (register.go, constants.go) | ported | 3/3 tests (register_test.go; the insta snapshot is compared with the manifest's App CRD) + names/scopes/printer columns, scheme, labels |
| kuben-crd | `bin/crdgen.rs` | 24 | 0 |  | dropped | the manifest charts/kuben/crds/kuben.dev_all.yaml is frozen until 2.1, then controller-gen generates it from the markers |
| kuben-crd | `v1alpha1/app.rs` | 327 | 2 | kubenapi/v1alpha1 (app.go) | ported | 2/2 tests (app_test.go) + 4 zero-value/enum tests; wire form pinned by testdata/objects against the Rust oracle |
| kuben-crd | `v1alpha1/buildrun.rs` | 55 | 0 | kubenapi/v1alpha1 (buildrun.go) | ported | testdata/objects fixtures |
| kuben-crd | `v1alpha1/common.rs` | 63 | 0 | kubenapi/v1alpha1 (common.go, enum.go) | ported | testdata/objects fixtures |
| kuben-crd | `v1alpha1/config.rs` | 133 | 0 | kubenapi/v1alpha1 (config.go) | ported | testdata/objects fixtures; TestDefaultsAreFreshValues |
| kuben-crd | `v1alpha1/environment.rs` | 112 | 0 | kubenapi/v1alpha1 (environment.go) | ported | testdata/objects fixtures |
| kuben-crd | `v1alpha1/mod.rs` | 21 | 0 | kubenapi/v1alpha1 | ported | module list only |
| kuben-crd | `v1alpha1/project.rs` | 65 | 0 | kubenapi/v1alpha1 (project.go) | ported | testdata/objects fixtures |
| kuben-crd | `v1alpha1/release.rs` | 57 | 0 | kubenapi/v1alpha1 (release.go) | ported | testdata/objects fixtures |
| kuben-crd | `v1alpha1/runtime.rs` | 228 | 6 | kubenapi/v1alpha1 (runtime.go) | ported | 6/6 tests (runtime_test.go): CEL and OpenAPI rules of the manifest run by the apiextensions-apiserver validators |
| kuben-crd | `v1alpha1/task.rs` | 228 | 4 | kubenapi/v1alpha1 (task.go) | ported | 4/4 tests (task_test.go), same validators |
| kuben-platform | `activator.rs` | 22 | 0 |  | todo | |
| kuben-platform | `agentlink.rs` | 580 | 3 |  | todo | |
| kuben-platform | `discovery.rs` | 734 | 5 | platform/discovery | ported | 5 → 19 Go tests (+ probes over fake clientsets, the loop against its store, the Watch); tokio `watch` → `discovery.Watch` (SUBSTITUTIONS.md) |
| kuben-platform | `doctor.rs` | 701 | 4 |  | todo | |
| kuben-platform | `duration.rs` | 57 | 2 | platform/controller (duration.go) | ported | 2 → 2 Go tests; unexported, returns whole seconds (u64) so huge grace periods saturate like Rust |
| kuben-platform | `evidence.rs` | 391 | 5 |  | todo | |
| kuben-platform | `health.rs` | 159 | 1 | platform/health | ported |  |
| kuben-platform | `leader.rs` | 313 | 2 | platform/leader | ported | client-go leaderelection (SUBSTITUTIONS.md); `decisions` tested the hand-written protocol and `timing_is_consistent` the constants, which client-go itself refuses when inconsistent → 1 Go test: two replicas over a fake clientset, one leader, hand-over on release |
| kuben-platform | `lib.rs` | 39 | 0 |  | todo | |
| kuben-platform | `local_agent.rs` | 341 | 2 |  | todo | |
| kuben-platform | `registry.rs` | 212 | 4 | platform/registry | ported | 4 → 4 |
| kuben-platform | `secrets.rs` | 664 | 10 |  | todo | |
| kuben-platform | `supervise.rs` | 133 | 3 | platform/supervise | ported | 3 → 3 |
| kuben-platform | `usage.rs` | 430 | 4 |  | todo | |
| kuben-platform | `render/mod.rs` | 551 | 9 | platform/render | ported | 9 → 9 (+ 14 builder tests in build_test.go); the three insta snapshots match byte for byte (testdata/ holds copies, checked identical while the Rust tree exists); objects built as JSON maps, canonical text by wire.CanonicalValue |
| kuben-platform | `materializer/agent.rs` | 365 | 2 | platform/materializer (agent.go) | ported | 2 → 2; the envelope test checks what `kuben_agent::runtime::check` checked (spec decodes, digest, names, namespace) until the agent is ported; `Apply` in kubenapi/protocol |
| kuben-platform | `materializer/detach.rs` | 166 | 0 | platform/materializer (detach.go) | ported | covered by tests/materializer.rs (envtest, CI) |
| kuben-platform | `materializer/drift.rs` | 311 | 2 | platform/materializer (drift.go) | ported | 2 → 2; the kube-rs watcher is a dynamic informer on the managed Apps, checked one at a time in its handler |
| kuben-platform | `materializer/fence.rs` | 99 | 2 | platform/materializer (fence.go) | ported | 2 → 2 |
| kuben-platform | `materializer/lifecycle.rs` | 283 | 0 | platform/materializer (lifecycle.go) | ported | covered by tests/materializer.rs (envtest, CI) |
| kuben-platform | `materializer/mod.rs` | 31 | 0 | platform/materializer (materializer.go) | ported | |
| kuben-platform | `materializer/progress.rs` | 134 | 2 | platform/materializer (progress.go) | ported | 2 → 2 |
| kuben-platform | `materializer/render.rs` | 652 | 7 | platform/materializer (render.go) | ported | 7 → 7; the config revision is decoded by `v1alpha1.DecodeAppSpec`, which refuses missing or null required members as serde did |
| kuben-platform | `materializer/secrets.rs` | 339 | 3 | platform/materializer (secrets.go) | partial | the keyring-less path only (a run bound to a secret fails `SecretsUnavailable`, as Rust without a keyring); writing and collecting revision Secrets and the 3 tests follow with `secrets.rs` (S2) |
| kuben-platform | `materializer/worker.rs` | 663 | 1 | platform/materializer (worker.go, plan.go) | ported | 1 → 1; kube-rs typed Api → dynamic client + serde-compatible JSON of the kubenapi types, server-side apply as `kuben-materializer` |
| kuben-platform | `materializer/write.rs` | 100 | 1 | platform/materializer (kube.go) | ported | 1 → 1 |
| kuben-platform | `controller/app.rs` | 357 | 1 | platform/controller (app.go), platform/render (Build) | ported | 1 → 5 Go tests (+ fake-client reconciles: apply/prune/status, GatewayAPIMissing, build error, hand-over); reads Rust made live go through the API reader; `blockOwnerDeletion: false` stripped to match kube-rs owner refs |
| kuben-platform | `controller/crd_apply.rs` | 21 | 0 | platform/controller (crds.go) | ported | 0 → 2 Go tests; embedded byte-identical copy of charts/kuben/crds/kuben.dev_all.yaml (pinned by a test while the chart file exists) |
| kuben-platform | `controller/environment.rs` | 227 | 0 | platform/controller (environment.go) | ported | 0 → 4 Go tests (provision, conflict, Retain, Delete grace/purge) |
| kuben-platform | `controller/gateway.rs` | 917 | 11 | platform/controller (gateway.go, gateway_run.go), platform/render (domains.go) | ported | 11 → 13 Go tests (+ 2 reconcile passes on the fake client) |
| kuben-platform | `controller/mod.rs` | 268 | 2 | platform/controller (controller.go, run.go) | ported | 2 → 5 Go tests; controller-runtime manager (SUBSTITUTIONS.md); its process-wide logger is set by serve |
| kuben-platform | `controller/project.rs` | 71 | 0 | platform/controller (project.go) | ported | 0 → 1 Go test |
| kuben-platform | `controller/resources.rs` | 1807 | 16 | platform/render (build.go, domains.go, platform.go, errors.go, environment.go) | ported | 16 → 18 Go tests: the App half in build_test.go, the Environment half (namespace, quota, limits, netpol), `demand` and `job_from_cron` in environment_test.go |
| kuben-platform | `build/evidence.rs` | 151 | 2 |  | todo | |
| kuben-platform | `build/job.rs` | 837 | 9 |  | todo | |
| kuben-platform | `build/mod.rs` | 120 | 0 |  | todo | |
| kuben-platform | `build/observe.rs` | 167 | 4 |  | todo | |
| kuben-platform | `build/rescan.rs` | 268 | 1 |  | todo | |
| kuben-platform | `build/scenarios.rs` | 515 | 17 |  | todo | |
| kuben-platform | `build/steps.rs` | 357 | 9 |  | todo | |
| kuben-platform | `build/worker.rs` | 986 | 4 |  | todo | |
| kuben-platform | `projection/informer.rs` | 273 | 0 | platform/projection (informer.go) | ported | 0 → 3 Go tests (fake clientsets: first LIST swap-in and later events, optional kinds once their group is served, first-sync deadline). client-go informers (SUBSTITUTIONS.md) |
| kuben-platform | `projection/mod.rs` | 792 | 7 | platform/projection (projection.go, delta.go) | ported | 7 → 10 Go tests (+ delta JSON, a slow subscriber's lag, route view fallback); per-subscriber bounded queue instead of a broadcast ring |
| kuben-platform | `projection/views.rs` | 526 | 2 | platform/projection (views.go) | ported | 2 → 3 Go tests (+ the JSON of every view pinned) |
| kuben-store | `db.rs` | 195 | 2 | store (store.go, pool.go, errors.go) | ported | 2 → 4 Go tests (+ error messages, id encoding); acquire timeout via a pool wrapper |
| kuben-store | `lib.rs` | 13 | 0 | store (package doc) | ported | |
| kuben-store | `testing.rs` | 55 | 0 | store/pgtest | ported | skip → failure with KUBEN_REQUIRE_PG=1; schema dropped after the test; `Schema` for migrator tests |
| kuben-store | `repo/acceptance.rs` | 451 | 3 | store (acceptance_test.go) | ported | 3 → 3; the panicking handler is a recovered panic with the deferred rollback a Go server runs |
| kuben-store | `repo/agents.rs` | 875 | 7 | store (agents.go, partial) | partial | `Delivery`, `record_runtime_observation`, `target_delivery`, `runtime_observation`, `hand_over_to_agent` (for the catalog and the materializer); enrollment, links, tokens, handover and the 7 tests follow with the agent work |
| kuben-store | `repo/audit.rs` | 130 | 0 | store (audit.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/backups.rs` | 357 | 2 |  | todo | |
| kuben-store | `repo/builds.rs` | 1794 | 11 |  | todo | |
| kuben-store | `repo/capabilities.rs` | 198 | 1 | store (capabilities.go) | ported | 1 → 1 |
| kuben-store | `repo/catalog.rs` | 711 | 2 | store (catalog.go) | ported | 2 → 2 |
| kuben-store | `repo/ci.rs` | 471 | 3 |  | todo | |
| kuben-store | `repo/controls.rs` | 707 | 2 | store (controls.go, partial) | partial | `active_freeze` (a deployment checks it), `target_paused` (the materializer holds runs); freezes, silences, owners and the 2 tests follow with the control routes |
| kuben-store | `repo/deployments.rs` | 1290 | 4 | store (deployments.go) | ported | 4 → 7 Go tests (+ run reasons, emergency guard, serde content texts); INSERT_RUN casts $1, $5, $6, $7, $9, $10, $15 (SUBSTITUTIONS.md) |
| kuben-store | `repo/detach.rs` | 285 | 0 | store (detach.go) | ported | covered by the detach scenarios of tests/http.rs (S4) and tests/materializer.rs |
| kuben-store | `repo/domains.rs` | 503 | 2 | store (domains.go, partial) | partial | `domain_owner` (app creation checks it); claims, DNS providers and the 2 tests follow with the domain work (S2) |
| kuben-store | `repo/image_policies.rs` | 357 | 1 |  | todo | |
| kuben-store | `repo/installs.rs` | 84 | 1 |  | todo | |
| kuben-store | `repo/lifecycle.rs` | 375 | 2 | store (lifecycle.go) | ported | 2 → 3 Go tests (+ the subject's wire form) |
| kuben-store | `repo/materialize.rs` | 927 | 6 | store (materialize.go) | ported | 6 → 6 |
| kuben-store | `repo/mod.rs` | 88 | 0 |  | todo | |
| kuben-store | `repo/notify.rs` | 749 | 3 |  | todo | |
| kuben-store | `repo/operations.rs` | 804 | 5 | store (operations.go) | ported | 5 → 5 |
| kuben-store | `repo/orgs.rs` | 320 | 0 | store (orgs.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/policies.rs` | 888 | 8 | store (policies.go) | ported | 8 → 8; `PolicyRevision`, `environment_policy`, `policy_of_target`, `set_environment_policy`, `RunApproval`, `DecideRun`; all 8 PostgreSQL tests ported in policies_test.go |
| kuben-store | `repo/previews.rs` | 740 | 2 | store (previews.go, partial) | partial | `untrusted_target` (a deployment checks it), `close_preview`; previews and the 2 tests follow with the preview work |
| kuben-store | `repo/product.rs` | 693 | 3 | store (tenant.go) | ported | 3 → 3 |
| kuben-store | `repo/releases.rs` | 144 | 0 | store (releases.go) | ported | 0 tests in the file; covered by the releases section of the tests/matrix.rs port |
| kuben-store | `repo/resolve.rs` | 244 | 2 | store (resolve.go) | ported | 2 → 2; `SqlScope` is `SQLScope` |
| kuben-store | `repo/retention.rs` | 182 | 2 | store (retention.go) | ported | 2 → 2; the DB test writes its incidents and webhook deliveries by hand until repo/notify.rs is ported |
| kuben-store | `repo/rollups.rs` | 145 | 1 |  | todo | |
| kuben-store | `repo/scans.rs` | 683 | 3 | store (scans.go, partial) | partial | `scan_verdict`, `latest_scans` (a deployment checks the gate); recording scans, SBOMs, exceptions and the 3 tests follow with the scan work |
| kuben-store | `repo/secrets.rs` | 1283 | 5 | store (secrets.go, partial) | partial | `secrets` (live secret summaries), `wanted_secrets`, `bind_run_secrets`, `run_secret_bindings`, `SecretBinding`; the secret store mutations and the 5 tests follow with the secret routes |
| kuben-store | `repo/sessions.rs` | 125 | 0 | store (sessions.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/sso.rs` | 326 | 3 |  | todo | |
| kuben-store | `repo/status.rs` | 244 | 1 |  | todo | |
| kuben-store | `repo/support.rs` | 160 | 1 | store (support.go) | ported | 1 → 1; incidents written by hand until repo/notify.rs is ported |
| kuben-store | `repo/throttle.rs` | 64 | 0 | store (throttle.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/tokens.rs` | 154 | 0 | store (tokens.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/upgrades.rs` | 260 | 1 | store (upgrades.go) | ported | 1 → 3 Go tests (+ version order, char truncation) |
| kuben-store | `repo/usage.rs` | 127 | 1 | store (usage.go) | ported | 1 → 1 |
| kuben-store | `repo/users.rs` | 140 | 0 | store (users.go) | ported | covered by the tests/matrix.rs port |
| kuben-agent | `tests/link.rs` | 756 | 15 |  | todo | |
| kuben-agent | `tests/runtime.rs` | 297 | 3 |  | todo | |
| kuben-api | `tests/http.rs` | 3927 | 44 | api (*_test.go) | partial | skeleton, scenarios 2/3/4, m4 project roles (adapted to ported routes) |
| kuben-api | `tests/oci.rs` | 41 | 2 |  | todo | |
| kuben-platform | `tests/agent_link_mtls.rs` | 260 | 6 |  | todo | |
| kuben-platform | `tests/execution_crds.rs` | 277 | 2 |  | todo | |
| kuben-platform | `tests/materializer.rs` | 633 | 5 |  | todo | |
| kuben-platform | `tests/two_writer_cas.rs` | 185 | 3 |  | todo | |
| kuben-store | `tests/matrix.rs` | 261 | 1 | store (matrix_test.go) | ported | 1 → 1, every section |
| kuben-store | `tests/ops_store_pg.rs` | 341 | 1 |  | todo | |

Totals: 231 files, 87964 lines, 685 Rust tests; ported 38.
