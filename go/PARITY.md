# Rust → Go parity ledger

Baseline: every Rust file as of `86ce940` (origin/main). A file is **done** only when its Go counterpart is reviewed line by line against it, every one of its tests has a Go counterpart, and lint (golangci-lint, NilAway) is clean. `scripts/rust-drift.sh` lists Rust files changed after the baseline.

Status: `todo` · `partial` (the parts a slice needs; the note says what is left) · `ported` (written, tests green, not yet reviewed) · `reviewed` · `dropped` (deliberately not ported; reason given).

| Crate | File | Lines | Rust tests | Go package | Status | Notes |
|---|---|---:|---:|---|---|---|
| kuben | `bootstrap.rs` | 301 | 3 | bootstrap, serve (firstAdmin) | ported | 3 → 4 (+ random passwords); the Secret is server-side applied by client-go with the `kuben` field manager, force; serve creates the admin (or announces the setup wizard) as serve.rs did |
| kuben | `bundle.rs` | 184 | 3 | bundle | ported | 3 → 5 (+ the embedded copy is byte-identical to the root `bundle.lock.json`, the summary lists every pin) |
| kuben | `main.rs` | 85 | 0 | cmd/kuben, cli | partial | every command is declared (cli.Root); serve, setup-token, version, copy-self run; the others answer "not available in this build yet" until their G5 port. anyhow's `Error: …` on failure; RUST_LOG/telemetry: G5 |
| kuben | `serve.rs` | 867 | 0 | serve | partial | api role; with a cluster: informers, readiness on first sync, discovery (controller role), leader election settings (the Lease is campaigned for once reconcilers exist, S1-D). Left: materializer, controllers, AgentLink, builds, background work (S1–S5) |
| kuben | `telemetry.rs` | 36 | 0 | serve (Logger, LogLevel), platform/metrics | ported | 0 → 2; slog JSON/text; RUST_LOG over telemetry.log_level, of an EnvFilter directive list only the global level; Prometheus (client_golang, a registry per process, no default registry) on server.metrics_bind with Rust's six metric names: subsystem failures/panics (supervise), reconcile errors (controller policy), kuben_leader (leader), SSE lag (projection source), audit write errors |
| kuben | `cli/admin.rs` | 38 | 0 | cli (cmd_admin.go) | ported | 0 → 0; the admin is created through bootstrap.EnsureAdmin when missing; every session of the account is revoked |
| kuben | `cli/agent.rs` | 50 | 0 | cli (cmd_admin.go) | ported | 0 → 0; same lines as Rust; the cluster CA is made in the state directory if the hub has not started yet |
| kuben | `cli/backup.rs` | 561 | 5 |  | todo | |
| kuben | `cli/client.rs` | 685 | 3 |  | todo | |
| kuben | `cli/dns01.rs` | 168 | 2 |  | todo | |
| kuben | `cli/doctor.rs` | 551 | 2 |  | todo | |
| kuben | `cli/mod.rs` | 274 | 2 | cli (root.go, cmd_serve.go, cmd_version.go) | partial | 2 → 6; cobra: clap's `env =` fallbacks by an annotation applied before each command (flag, then variable, then default), `--roles` comma list, `--dev`, version string with Rust's OS/arch names (`macos`, `x86_64`, `aarch64`). Left: the option structs of the commands still to port |
| kuben | `cli/support.rs` | 659 | 4 |  | todo | |
| kuben | `cli/ui.rs` | 372 | 3 |  | todo | |
| kuben | `cli/upgrade.rs` | 134 | 1 | cli/upgrade, cli (cmd_upgrade.go), serve | ported | 1 → 1; serve and `kuben migrate` migrate through upgrade.Migrate (a backup first when migrations are pending and pg_dump is installed) |
| kuben | `cli/setup/journal.rs` | 431 | 4 |  | todo | |
| kuben | `cli/setup/mod.rs` | 2210 | 10 |  | todo | |
| kuben | `cli/setup/plan.rs` | 258 | 1 |  | todo | |
| kuben | `cli/setup/platform.rs` | 1138 | 6 |  | todo | |
| kuben-agent | `bootstrap.rs` | 237 | 2 | agent/bootstrap | ported | 2 → 2 |
| kuben-agent | `enroll.rs` | 903 | 9 | kubenapi/protocol (enroll.go), platform/agentlink (enroll.go, ca.go) | ported | 9 → 9 (+ the certificate shape rcgen gave) |
| kuben-agent | `hub.rs` | 495 | 0 | platform/agentlink (hub.go, server.go) | ported | 0 in Rust; covered by the tests/link.rs port |
| kuben-agent | `lib.rs` | 27 | 0 | agent (doc.go) | ported |  |
| kuben-agent | `link.rs` | 576 | 3 | agent/link | ported | 3 → 3 |
| kuben-agent | `main.rs` | 328 | 3 | agent/cmd/kuben-agent | ported | 3 → 3; cobra and slog JSON (message texts and log field names differ; `RUST_LOG` only as a plain level) |
| kuben-agent | `protocol.rs` | 436 | 9 | kubenapi/protocol | ported | 9 → 9 (+ byte and strictness pins; byte fixtures hand-written from the serde attributes, to regenerate with the Rust compat generator) |
| kuben-agent | `runtime.rs` | 657 | 4 | agent/runtime | ported | 4 → 4 |
| kuben-agent | `state.rs` | 351 | 4 | agent/state | ported | 4 → 4 |
| kuben-agent | `tls.rs` | 323 | 6 | kubenapi/protocol (tls.go) | ported | 6 → 6 |
| kuben-api | `audit.rs` | 205 | 3 | api (audit.go) | ported |  |
| kuben-api | `authz.rs` | 180 | 1 | api/access | ported |  |
| kuben-api | `client.rs` | 483 | 4 |  | todo | |
| kuben-api | `dns.rs` | 631 | 4 | api/dns | ported | 4 → 7; DoH lookups, change plan, Cloudflare adapter (hand-written: ownership needs record comments and ids, which libdns lacks), public backend, gateway records |
| kuben-api | `error.rs` | 100 | 0 | api/problem | ported |  |
| kuben-api | `github.rs` | 750 | 6 | api/github | ported | 6 → 21 (a fake GitHub on httptest: token requests, JWT checked against the public key, caching, error classes, body cap, revoke, installation, pull request state, commit status) |
| kuben-api | `host.rs` | 73 | 2 |  | todo | |
| kuben-api | `image_watch.rs` | 287 | 0 | api (image_watch.go), serve (background.go) | ported | 0 in Rust; covered by the tests/http.rs port below; runs on every replica as serve.rs spawn_background |
| kuben-api | `lib.rs` | 105 | 0 | api (server.go) | partial | router: session, CSRF, audit, timeout (followed logs exempt), body limit, gzip compression (SUBSTITUTIONS), `/api/docs` (api/apidocs), probes, console fallback. The GitHub webhook (S3) and CI exchange (S4) routes follow with their slices |
| kuben-api | `notify.rs` | 593 | 6 | api/notify (plan.go, notifier.go, sealing.go) | ported | 6 → 8 (+ signatures vs testdata/compat, + the http crate's reason phrases); is_private is outbound.IsPrivate; the notifier runs on every replica (serve/background.go) with the GitHub App for commit statuses |
| kuben-api | `oci.rs` | 908 | 9 | api/oci | ported | Parse, Fixed, Registry, Verifier (RegistryVerifier): 7 tests ported, 2 dropped (challenge parsing and query encoding belong to go-containerregistry), + 4 in-process registry tests for the verifier |
| kuben-api | `oidc.rs` | 465 | 5 | api/oidc | ported | 5 → 6 (+ key fetch, cache, refresh and outage against an httptest issuer); the shared JWKS/RS256 parts serve SSO in S4 |
| kuben-api | `openapi.rs` | 231 | 2 | api/gen (ogen) + api/genspec | dropped | spec first: the contract generates the server |
| kuben-api | `previews.rs` | 464 | 0 |  | todo | |
| kuben-api | `setup.rs` | 375 | 2 | api (setup.go) | ported | 2 → 2; setup_guide/setup_url are SetupGuide/SetupURL (the advertised address is passed in; host::console_url) |
| kuben-api | `sso.rs` | 554 | 5 | api/sso | ported | 5 → 5; reuses api/oidc (JWKS, RS256) |
| kuben-api | `state.rs` | 151 | 0 |  | todo | |
| kuben-api | `stream.rs` | 248 | 2 | api/stream, platform/projection (source.go) | ported | 2 → 4 Go tests (the Visibility tests + the stream.Source over the projections: org filter, lag → `resync` without id); the source seeds each connection's filter from its own snapshot taken right after subscribing (Serve asks for snapshot and deltas separately) |
| kuben-api | `transport.rs` | 157 | 0 | api/outbound | ported | net/http with the same rules (https only unless allowed, proxy from env, header and body timeouts, body cap, no redirects); notify.rs is_private is outbound.IsPrivate |
| kuben-api | `web.rs` | 106 | 1 | api/web | ported | 1 → 1; content types from a fixed table of mime_guess 2.0.5 (Go's mime package differs and reads /etc/mime.types) |
| kuben-api | `bin/openapi.rs` | 23 | 0 | api/gen (ogen) + api/genspec | dropped | spec first: packages/api-client/openapi.json is the frozen contract the server is generated from, so nothing writes it |
| kuben-api | `auth/mod.rs` | 462 | 1 | api (session_routes.go), api/httpx | ported | SSO routes: S4 |
| kuben-api | `auth/password.rs` | 75 | 1 | api/auth | ported |  |
| kuben-api | `auth/session.rs` | 202 | 4 | api/auth, api (identity.go) | ported |  |
| kuben-api | `auth/sso.rs` | 260 | 1 | api (sso routes) | ported | 1 → 1 + tests/http.rs `m4_sso_*` (3) |
| kuben-api | `auth/throttle.rs` | 200 | 5 | api (identity.go) | ported |  |
| kuben-api | `routes/access.rs` | 322 | 2 | api (access_routes.go) | ported |  |
| kuben-api | `routes/audit.rs` | 123 | 0 | api (audit_routes.go) | ported |  |
| kuben-api | `routes/ci.rs` | 443 | 3 | api (ci.go) | ported | 3 → 3 + tests/http.rs `m4_untrusted_ci_tokens_get_nothing`, `m4_trusted_ci_gets_a_scoped_token_once` + `TestTheExchangeIsMountedOnItsOwn`; `repositoryId`/`repositoryOwnerId` are int64 and `tokenTtlSecs` int32 in the contract (Rust u64/u32): an out-of-range value is a 422 from ogen instead of Rust's 500 or the policy's text |
| kuben-api | `routes/controls.rs` | 731 | 2 | api (controls.go) | ported | 2 → 2 unit tests + tests/http.rs `m4_freezes_pauses_and_emergency_rollbacks`, `m4_owners_and_silences_are_kept`; OwnerDto answers without Rust's `updatedBy`/`updatedAt`, which the frozen contract does not have (the console never read them) |
| kuben-api | `routes/domains.rs` | 703 | 0 | api (domains.go, domains_sync.go) | ported | 0 → 4 (+ http.rs m5_domain_claims_and_dns_records): claims list/create/verify (TXT or provider)/revoke, DNS providers list/create (token verified, sealed)/delete, syncAppDns (plan + apply, records remembered); DNS backend injectable via Deps.DNS (default dns.NewPublic) |
| kuben-api | `routes/environments.rs` | 326 | 0 | api (environments.go) | ported | list, get, create (placement on `primary`, initial policy, org environment quota), delete (closes a preview, `env-delete-protected` for production); `TestEnvironmentsReadAndWriteSQL` (the environment half of `environments_and_apps_read_from_sql` and `viewers_can_read_but_not_write`) |
| kuben-api | `routes/git.rs` | 298 | 0 | api (git.go) | partial | 0 → 3 (webhook refusals/outcomes, mounting and the 5 MiB limit, provider errors); mounted beside the CI exchange (inside the gzip/request-id/recovery wrappers; an oversized body is an empty 413). Pull requests reach previews::on_pull with the preview port |
| kuben-api | `routes/health.rs` | 54 | 0 | api (probes.go) | ported | seq, pods and cluster from the projections and registry |
| kuben-api | `routes/incidents.rs` | 511 | 2 | api (incidents.go, incidents_webhooks.go) | ported | 2 → 2 + http.rs m4_signed_webhooks_and_incidents |
| kuben-api | `routes/members.rs` | 267 | 0 | api (members.go) | ported |  |
| kuben-api | `routes/mod.rs` | 26 | 0 | api | ported | a module list: the routes are files of package api, mounted through the ogen handler (server.go) |
| kuben-api | `routes/policy.rs` | 299 | 2 | api (policy_routes.go) | ported | 2 → 2 unit tests + tests/http.rs `m4_weakening_protection_takes_an_owner` |
| kuben-api | `routes/previews.rs` | 325 | 0 |  | todo | |
| kuben-api | `routes/projects.rs` | 189 | 0 | api (projects.go) | ported | list, get (readiness from the org's own projection), create, delete; tests/http.rs `projects_are_tenant_scoped` → `TestProjectsAreTenantScoped` (+ duplicate, delete twice, viewer) |
| kuben-api | `routes/registries.rs` | 262 | 2 | api (registries.go) | ported | 2 → 2 + tests/http.rs `m4_registry_logins_pull_private_images` |
| kuben-api | `routes/request.rs` | 64 | 1 | api (request.go, projects.go, access) | ported | 1 → 1 (`TestTimestampsAreRFC3339`); `actor` is `access.Access.Actor` |
| kuben-api | `routes/scope.rs` | 301 | 1 | api (scope.go) | ported | 1 → 1 (`short_names` → `TestEnvironmentShortNames`); project, environment and app scopes, `deleting` (environment or project), cluster, kube_error |
| kuben-api | `routes/secrets.rs` | 654 | 2 | api (secrets*.go) | ported | 2 → 2 + tests/http.rs `m4_secret_values_are_revisions_rolled_out_to_their_apps`, `m4_production_rotations_wait_for_approval`; a negative revision in the revoke path is 422 (axum: 400) |
| kuben-api | `routes/status.rs` | 322 | 0 | api (status_routes.go) | ported | 0 unit tests in Rust; tests/http.rs `m5_public_status_pages_show_only_public_facts` + TestStatusPageDto ported |
| kuben-api | `routes/templates.rs` | 543 | 2 | api (templates.go) | ported | 2 → 2 unit tests + tests/http.rs `scenario8_template_catalogue` |
| kuben-api | `routes/tokens.rs` | 229 | 0 | api (tokens.go) | ported |  |
| kuben-api | `routes/validate.rs` | 173 | 4 | api (validate.go) | ported | 4 → 4 |
| kuben-api | `routes/vulnerabilities.rs` | 267 | 1 | api (vulnerabilities.go) | ported | 1 → 2 + http.rs m4_the_scan_gate_refuses_known_critical_findings; exception expiry saturates |
| kuben-api | `routes/apps/admission.rs` | 190 | 2 | api (apps_admission.go, environments.go) | ported | 2 → 2 |
| kuben-api | `routes/apps/approvals.rs` | 309 | 2 | api (apps_approvals.go) | ported | 2 → 2; `GetDeploymentApproval`, `ApproveDeployment`, `RejectDeployment`; `TestRefusalsMapToHTTP`, `TestEligibleToDecide`, `TestM4ProtectedDeploysWaitForAnotherApprover` |
| kuben-api | `routes/apps/builds.rs` | 273 | 2 | api (apps_builds.go) | ported | 2 → 2 |
| kuben-api | `routes/apps/crud.rs` | 419 | 0 | api (apps_crud.go) | ported | list, create (image and Git), get, update, delete, restart, handover; `TestAppsReadFromSQL`, `TestViewersCanReadAppsButNotWrite` |
| kuben-api | `routes/apps/deployments.rs` | 510 | 2 | api (apps_deployments.go) | ported | 2 → 2; `a_deployment_is_accepted_once_and_can_be_polled`, `a_lost_answer_is_given_again_without_a_second_run`, `deployments_need_deploy_rights_a_pinned_image_and_an_app_in_sql` → Go (PostgreSQL); the input hash is the text Rust hashed, so an Idempotency-Key replay matches across the cutover; `Location` through httpx.SetHeader |
| kuben-api | `routes/apps/doctor.rs` | 231 | 0 | api (apps_doctor.go) | partial | 0 → 8; delegation from the DNS backend's NS, proxy from the organization's provider accounts (unknown without keyring), agent from the store's cluster agent (none/revoked/last seen, stale after max(3×heartbeat, 30) s, saturating). The graph and findings stay empty until evidence.rs |
| kuben-api | `routes/apps/domains.rs` | 90 | 0 | api (apps_domains.go) + platform/doctor | ported | DNS checks of an app's hosts, concurrent with a 3 s timeout each, addresses ordered as IpAddr; resolver injectable |
| kuben-api | `routes/apps/evidence.rs` | 296 | 2 |  | todo | |
| kuben-api | `routes/apps/export.rs` | 544 | 2 | api (apps_export.go, apps_detach.go) | ported | 2 → 5 (+ runbook text, whole wire form, no-Gateway case) + http.rs m4_export_detach_and_release; members as wire.CanonicalValue; top-level member order is ogen's map order |
| kuben-api | `routes/apps/image_policy.rs` | 220 | 0 | api (apps_image_policy.go) | ported | tests/http.rs `m5_image_policies_deploy_new_digests` → `TestM5ImagePoliciesDeployNewDigests` |
| kuben-api | `routes/apps/jobs.rs` | 105 | 1 | api (apps_jobs.go) | ported | 1 → 1; `manual_job_names_fit_the_limit`, manual Job creation from live CronJob, 503 without a cluster |
| kuben-api | `routes/apps/logs.rs` | 616 | 3 | api (apps_logs.go) | ported | 3 → 3; `an_apps_objects_are_told_apart_from_its_neighbours`, `a_followed_log_line_keeps_its_time_and_is_cut_on_a_character`, `followed_logs_are_capped_per_user_and_freed_when_closed`, GetAppLogs (once or SSE followed, bounded per-user permit), GetAppEvents |
| kuben-api | `routes/apps/metrics.rs` | 131 | 0 | api (apps_metrics.go) | ported | tests/http.rs `m5_metrics_are_never_invented` → `TestM5MetricsAreNeverInvented` (HTTP) + `TestLiveMetricsAreNeverInvented` (the live window through `LiveMetrics`) |
| kuben-api | `routes/apps/mod.rs` | 758 | 2 | api (apps.go) | ported | 2 → 2; DTOs set every nullable member explicitly (ogen omits an unset one, serde wrote null); registry logins for resolution wait for the keyring (S2) |
| kuben-api | `routes/apps/promote.rs` | 358 | 1 | api (apps_promote.go) | ported | 1 → 1; `promotion_keeps_target_domains_and_scaling_and_reports_changes`, spec diffs without env values, missing secret warnings against live cluster Secrets and store, dry-run with null app |
| kuben-api | `routes/apps/releases.rs` | 261 | 2 | api (apps_releases.go) | ported | 2 → 2; `scenario5_releases_are_newest_first_and_rollback_is_authorized` → Go (PostgreSQL) |
| kuben-api | `routes/apps/scans.rs` | 192 | 0 | api (apps_scans.go) | ported | 0 → 3 (JSON pins, SBOM bytes as stored); the SBOM's Content-Encoding/Content-Disposition go into the shared header map so the compressor passes the gzip body through |
| kuben-api | `routes/apps/source.rs` | 453 | 3 | api (apps_source.go) | ported | 3 → 5 (+ refusal texts, SourceDto nulls); includes create_git_app |
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
| kuben-platform | `agentlink.rs` | 580 | 3 | platform/agentlink | ported | 3 → 3 |
| kuben-platform | `discovery.rs` | 734 | 5 | platform/discovery | ported | 5 → 19 Go tests (+ probes over fake clientsets, the loop against its store, the Watch); tokio `watch` → `discovery.Watch` (SUBSTITUTIONS.md) |
| kuben-platform | `doctor.rs` | 701 | 4 | platform/doctor | ported | 4 → 9: every check, the overall verdict, the port probe (injectable dialer), the lookup, the KubenConfig and Gateway readers |
| kuben-platform | `duration.rs` | 57 | 2 | platform/controller (duration.go) | ported | 2 → 2 Go tests; unexported, returns whole seconds (u64) so huge grace periods saturate like Rust |
| kuben-platform | `evidence.rs` | 391 | 5 | platform/evidence | ported | 5 → 6 |
| kuben-platform | `health.rs` | 159 | 1 | platform/health | ported |  |
| kuben-platform | `leader.rs` | 313 | 2 | platform/leader | ported | client-go leaderelection (SUBSTITUTIONS.md); `decisions` tested the hand-written protocol and `timing_is_consistent` the constants, which client-go itself refuses when inconsistent → 1 Go test: two replicas over a fake clientset, one leader, hand-over on release |
| kuben-platform | `lib.rs` | 39 | 0 | internal/platform/* | ported | a module list: each Go package under internal/platform says in its package comment which Rust module it replaces |
| kuben-platform | `local_agent.rs` | 341 | 2 | platform/agentlink (local.go) | ported | 2 → 2 |
| kuben-platform | `registry.rs` | 212 | 4 | platform/registry | ported | 4 → 4 |
| kuben-platform | `secrets.rs` | 664 | 10 | platform/secrets | ported | 10 → 12 (+ opening the Rust-sealed `testdata/compat/secrets.json`, + `Prepare` against PostgreSQL) |
| kuben-platform | `supervise.rs` | 133 | 3 | platform/supervise | ported | 3 → 3 |
| kuben-platform | `usage.rs` | 430 | 4 | platform/usage | ported | 4 → 5 (+ one collector pass with the hourly rollup and a missing Metrics API); started by serve on API replicas with a cluster |
| kuben-platform | `render/mod.rs` | 551 | 9 | platform/render | ported | 9 → 9 (+ 14 builder tests in build_test.go); the three insta snapshots match byte for byte (testdata/ holds copies, checked identical while the Rust tree exists); objects built as JSON maps, canonical text by wire.CanonicalValue |
| kuben-platform | `materializer/agent.rs` | 365 | 2 | platform/materializer (agent.go) | ported | 2 → 2; the envelope test checks what `kuben_agent::runtime::check` checked (spec decodes, digest, names, namespace) until the agent is ported; `Apply` in kubenapi/protocol |
| kuben-platform | `materializer/detach.rs` | 166 | 0 | platform/materializer (detach.go) | ported | covered by tests/materializer.rs (envtest, CI) |
| kuben-platform | `materializer/drift.rs` | 311 | 2 | platform/materializer (drift.go) | ported | 2 → 2; the kube-rs watcher is a dynamic informer on the managed Apps, checked one at a time in its handler |
| kuben-platform | `materializer/fence.rs` | 99 | 2 | platform/materializer (fence.go) | ported | 2 → 2 |
| kuben-platform | `materializer/lifecycle.rs` | 283 | 0 | platform/materializer (lifecycle.go) | ported | covered by tests/materializer.rs (envtest, CI) |
| kuben-platform | `materializer/mod.rs` | 31 | 0 | platform/materializer (materializer.go) | ported | |
| kuben-platform | `materializer/progress.rs` | 134 | 2 | platform/materializer (progress.go) | ported | 2 → 2 |
| kuben-platform | `materializer/render.rs` | 652 | 7 | platform/materializer (render.go) | ported | 7 → 7; the config revision is decoded by `v1alpha1.DecodeAppSpec`, which refuses missing or null required members as serde did |
| kuben-platform | `materializer/secrets.rs` | 339 | 3 | platform/materializer (secrets.go) | ported | 3 → 3; writing and collecting revision Secrets against an API server were untested in Rust too (envtest later) |
| kuben-platform | `materializer/worker.rs` | 663 | 1 | platform/materializer (worker.go, plan.go) | ported | 1 → 1; kube-rs typed Api → dynamic client + serde-compatible JSON of the kubenapi types, server-side apply as `kuben-materializer` |
| kuben-platform | `materializer/write.rs` | 100 | 1 | platform/materializer (kube.go) | ported | 1 → 1 |
| kuben-platform | `controller/app.rs` | 357 | 1 | platform/controller (app.go), platform/render (Build) | ported | 1 → 5 Go tests (+ fake-client reconciles: apply/prune/status, GatewayAPIMissing, build error, hand-over); reads Rust made live go through the API reader; `blockOwnerDeletion: false` stripped to match kube-rs owner refs |
| kuben-platform | `controller/crd_apply.rs` | 21 | 0 | platform/controller (crds.go) | ported | 0 → 2 Go tests; embedded byte-identical copy of charts/kuben/crds/kuben.dev_all.yaml (pinned by a test while the chart file exists) |
| kuben-platform | `controller/environment.rs` | 227 | 0 | platform/controller (environment.go) | ported | 0 → 4 Go tests (provision, conflict, Retain, Delete grace/purge) |
| kuben-platform | `controller/gateway.rs` | 917 | 11 | platform/controller (gateway.go, gateway_run.go), platform/render (domains.go) | ported | 11 → 13 Go tests (+ 2 reconcile passes on the fake client) |
| kuben-platform | `controller/mod.rs` | 268 | 2 | platform/controller (controller.go, run.go) | ported | 2 → 5 Go tests; controller-runtime manager (SUBSTITUTIONS.md); its process-wide logger is set by serve |
| kuben-platform | `controller/project.rs` | 71 | 0 | platform/controller (project.go) | ported | 0 → 1 Go test |
| kuben-platform | `controller/resources.rs` | 1807 | 16 | platform/render (build.go, domains.go, platform.go, errors.go, environment.go) | ported | 16 → 18 Go tests: the App half in build_test.go, the Environment half (namespace, quota, limits, netpol), `demand` and `job_from_cron` in environment_test.go |
| kuben-platform | `build/evidence.rs` | 151 | 2 | platform/build (evidence.go) | ported | 2 → 4 (+ control-byte report, Collect on a fake client) |
| kuben-platform | `build/job.rs` | 837 | 9 | platform/build (job.go, scripts.go) | ported | 9 → 10 (+ TestScriptsAreRustsBytes: the scripts are Rust's bytes, the 0x01 in SCAN_SCRIPT included — see HANDOFF owner decisions); a budget that is not a quantity fails before the Job (InvalidBudget) |
| kuben-platform | `build/mod.rs` | 120 | 0 | platform/build | ported | 0 → 3; provider and verifier contracts and their errors |
| kuben-platform | `build/observe.rs` | 167 | 4 | platform/build (observe.go) | ported | 4 → 4; injected clock |
| kuben-platform | `build/rescan.rs` | 268 | 1 | platform/build (rescan.go) | ported | 1 → 1; the deadline on the injected clock |
| kuben-platform | `build/scenarios.rs` | 515 | 17 | platform/build (scenarios_test.go) | ported | 16 → 16 (15 tests + the proptest as a testing/quick property, 512 cases) |
| kuben-platform | `build/steps.rs` | 357 | 9 | platform/build (steps.go) | ported | 9 → 9 |
| kuben-platform | `build/worker.rs` | 986 | 4 | platform/build (worker.go, attempt.go, cluster.go) | ported | 4 → 4 (+ 5 fake-client tests in objects_test.go, 1 envtest test, 2 PostgreSQL end-to-end flows); an undecodable BuildRun is skipped by the sweep |
| kuben-platform | `projection/informer.rs` | 273 | 0 | platform/projection (informer.go) | ported | 0 → 3 Go tests (fake clientsets: first LIST swap-in and later events, optional kinds once their group is served, first-sync deadline). client-go informers (SUBSTITUTIONS.md) |
| kuben-platform | `projection/mod.rs` | 792 | 7 | platform/projection (projection.go, delta.go) | ported | 7 → 10 Go tests (+ delta JSON, a slow subscriber's lag, route view fallback); per-subscriber bounded queue instead of a broadcast ring |
| kuben-platform | `projection/views.rs` | 526 | 2 | platform/projection (views.go) | ported | 2 → 3 Go tests (+ the JSON of every view pinned) |
| kuben-store | `db.rs` | 195 | 2 | store (store.go, pool.go, errors.go) | ported | 2 → 4 Go tests (+ error messages, id encoding); acquire timeout via a pool wrapper |
| kuben-store | `lib.rs` | 13 | 0 | store (package doc) | ported | |
| kuben-store | `testing.rs` | 55 | 0 | store/pgtest | ported | skip → failure with KUBEN_REQUIRE_PG=1; schema dropped after the test; `Schema` for migrator tests |
| kuben-store | `repo/acceptance.rs` | 451 | 3 | store (acceptance_test.go) | ported | 3 → 3; the panicking handler is a recovered panic with the deferred rollback a Go server runs |
| kuben-store | `repo/agents.rs` | 875 | 7 | store (agents.go) | ported | 7 → 7 |
| kuben-store | `repo/audit.rs` | 130 | 0 | store (audit.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/backups.rs` | 357 | 2 | store (backups.go) | ported | 2 → 2 |
| kuben-store | `repo/builds.rs` | 1794 | 11 | store (builds.go) | ported | 11 → 12 (+ `TestBuildHelpersMatchRust`); `AssertSqlSafe(format!(…))` queries are Go constant expressions with the same text |
| kuben-store | `repo/capabilities.rs` | 198 | 1 | store (capabilities.go) | ported | 1 → 1 |
| kuben-store | `repo/catalog.rs` | 711 | 2 | store (catalog.go) | ported | 2 → 2 |
| kuben-store | `repo/ci.rs` | 471 | 3 | store (ci.go) | ported | 3 → 3 |
| kuben-store | `repo/controls.rs` | 707 | 2 | store (controls.go) | ported | 2 → 2 |
| kuben-store | `repo/deployments.rs` | 1290 | 4 | store (deployments.go) | ported | 4 → 7 Go tests (+ run reasons, emergency guard, serde content texts); INSERT_RUN casts $1, $5, $6, $7, $9, $10, $15 (SUBSTITUTIONS.md) |
| kuben-store | `repo/detach.rs` | 285 | 0 | store (detach.go) | ported | covered by the detach scenarios of tests/http.rs (S4) and tests/materializer.rs |
| kuben-store | `repo/domains.rs` | 503 | 2 | store (domains.go) | ported | 2 → 3: claims (advisory lock on the last two labels, Verified sum type), DNS providers (sealed token), records upsert/forget, domain_owner; + the 1024-character failure cap |
| kuben-store | `repo/image_policies.rs` | 357 | 1 | store (image_policies.go) | ported | 1 → 1 |
| kuben-store | `repo/installs.rs` | 84 | 1 | store (installs.go) | ported | 1 → 1 |
| kuben-store | `repo/lifecycle.rs` | 375 | 2 | store (lifecycle.go) | ported | 2 → 3 Go tests (+ the subject's wire form) |
| kuben-store | `repo/materialize.rs` | 927 | 6 | store (materialize.go) | ported | 6 → 6 |
| kuben-store | `repo/mod.rs` | 88 | 0 | store | ported | a module list and re-exports: the Go store is one package, the re-exported types are its exported ones |
| kuben-store | `repo/notify.rs` | 749 | 3 | store (notify.go) | ported | 3 → 3 (PostgreSQL); OpenIncident moved here from status.go; retention/support tests use the repository |
| kuben-store | `repo/operations.rs` | 804 | 5 | store (operations.go) | ported | 5 → 5 |
| kuben-store | `repo/orgs.rs` | 320 | 0 | store (orgs.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/policies.rs` | 888 | 8 | store (policies.go) | ported | 8 → 8; `PolicyRevision`, `environment_policy`, `policy_of_target`, `set_environment_policy`, `RunApproval`, `DecideRun`; all 8 PostgreSQL tests ported in policies_test.go |
| kuben-store | `repo/previews.rs` | 740 | 2 | store (previews.go, partial) | partial | `untrusted_target` (a deployment checks it), `close_preview`; previews and the 2 tests follow with the preview work |
| kuben-store | `repo/product.rs` | 693 | 3 | store (tenant.go) | ported | 3 → 3 |
| kuben-store | `repo/releases.rs` | 144 | 0 | store (releases.go) | ported | 0 tests in the file; covered by the releases section of the tests/matrix.rs port |
| kuben-store | `repo/resolve.rs` | 244 | 2 | store (resolve.go) | ported | 2 → 2; `SqlScope` is `SQLScope` |
| kuben-store | `repo/retention.rs` | 182 | 2 | store (retention.go) | ported | 2 → 2; the DB test writes its incidents and webhook deliveries by hand until repo/notify.rs is ported |
| kuben-store | `repo/rollups.rs` | 145 | 1 | store (rollups.go) | ported | 1 → 1 |
| kuben-store | `repo/scans.rs` | 683 | 3 | store (scans.go) | ported | 3 → 3 |
| kuben-store | `repo/secrets.rs` | 1283 | 5 | store (secrets*.go) | ported | 5 → 5; `SecretKind` is a sealed interface (opaque, registry host) |
| kuben-store | `repo/sessions.rs` | 125 | 0 | store (sessions.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/sso.rs` | 326 | 3 | store (sso.go) | ported | 3 → 3 |
| kuben-store | `repo/status.rs` | 244 | 1 | store (status.go) | ported | 1 → 1 (`status_pages_are_found_by_slug_only_when_enabled`) |
| kuben-store | `repo/support.rs` | 160 | 1 | store (support.go) | ported | 1 → 1; incidents written by hand until repo/notify.rs is ported |
| kuben-store | `repo/throttle.rs` | 64 | 0 | store (throttle.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/tokens.rs` | 154 | 0 | store (tokens.go) | ported | covered by the tests/matrix.rs port |
| kuben-store | `repo/upgrades.rs` | 260 | 1 | store (upgrades.go) | ported | 1 → 3 Go tests (+ version order, char truncation) |
| kuben-store | `repo/usage.rs` | 127 | 1 | store (usage.go) | ported | 1 → 1 |
| kuben-store | `repo/users.rs` | 140 | 0 | store (users.go) | ported | covered by the tests/matrix.rs port |
| kuben-agent | `tests/link.rs` | 756 | 15 | platform/agentlink (link_test.go) | ported | 15 → 15: the real agent link loop against the real hub (the hub module requires go/agent for it) |
| kuben-agent | `tests/runtime.rs` | 297 | 3 | agent/runtime | partial | 3 → 3 written, never run: they need a cluster with controllers (kind), as Rust's `#[ignore]`; skipped unless `KUBEN_TEST_KUBE=1` (a kind CI job is still to add) |
| kuben-api | `tests/http.rs` | 3927 | 44 | api (*_test.go) | partial | 43 of 44: skeleton (10), scenarios 1–5 and 8, deployments (3), m4 policy, approval, roles, quotas, m5 status pages, m4 controls (2), m4 CI trust (2), m5 metrics, m4 secrets, rotations and registry logins (3), m5 image policies, m4 SSO (3), m5 domain claims and DNS records, m4 signed webhooks and incidents, m4 export and detach, m4 scan gate. Left: previews (S5) |
| kuben-api | `tests/oci.rs` | 41 | 2 | api/oci (network_test.go) | ported | 2 → 2, run only with KUBEN_TEST_NETWORK=1 (Rust: --ignored) |
| kuben-platform | `tests/agent_link_mtls.rs` | 260 | 6 | platform/agentlink (mtls_test.go) | ported | 6 → 6 |
| kuben-platform | `tests/execution_crds.rs` | 277 | 2 | platform/controller (execution_crds_test.go) + platform/kubetest | ported | 2 → 2 against envtest's API server |
| kuben-platform | `tests/materializer.rs` | 633 | 5 | platform/materializer (cluster_test.go) | ported | 5 → 5 against envtest and PostgreSQL; plus a controller smoke test (platform/controller run_cluster_test.go) |
| kuben-platform | `tests/two_writer_cas.rs` | 185 | 3 | platform/kubetest (cas_test.go) | ported | 3 → 3 against envtest's API server |
| kuben-store | `tests/matrix.rs` | 261 | 1 | store (matrix_test.go) | ported | 1 → 1, every section |
| kuben-store | `tests/ops_store_pg.rs` | 341 | 1 | store (spike_pg_test.go) | ported | 1 → 1 (five subtests on throwaway `m0_*` tables in the test's own schema; runs in CI with PostgreSQL) |

Totals: 231 files, 87964 lines, 685 Rust tests; dropped 3, partial 9, ported 202, todo 17.
