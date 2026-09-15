//! M1 acceptance on PostgreSQL (plan §18.1 exit criteria, §19.1 invariants):
//! concurrent A/B deploys, the tenant context on a shared connection pool,
//! and the rollback and promotion policy. The agent, cluster and HTTP parts
//! sit next to their code: two clusters on one hub (`kuben-agent` link
//! tests and `kuben-platform::agentlink`), an envelope carried again (the
//! `kuben-agent` runtime tests) and a lost answer (`kuben-api` HTTP tests).

use std::{collections::BTreeMap, time::Duration};

use kuben_core::{
    artifact::Digest,
    ids::{ConfigRevisionId, DeploymentRunId, OrgId, ReleaseId, TargetId},
    ops::{DeployPolicy, Generation, Reject, RunEvent, RunPhase},
};
use serde_json::json;
use sqlx::{AssertSqlSafe, Connection as _};
use uuid::Uuid;

use super::{Advance, NewAudit, PortableRelease, RUN_KIND, RunReason, StartDeployment, Started};
use crate::{
    Store,
    testing::{pg_store, skip},
};

/// What a request handler that panics mid-transaction says.
const PLANNED_PANIC: &str = "a handler panics mid-transaction";
/// The events of a run that is carried out and verified.
const CARRIED_OUT: [RunEvent; 6] = [
    RunEvent::ReadyForDelivery,
    RunEvent::AcceptedByCluster,
    RunEvent::PreflightStarted,
    RunEvent::PreflightPassed,
    RunEvent::Applied,
    RunEvent::Verified,
];

/// One organization's app `web` on production and staging (one placement
/// each on cluster `primary`), two releases and a configuration revision
/// per target.
struct Shop {
    org: OrgId,
    project: kuben_core::ids::ProjectId,
    project_slug: String,
    /// Production, staging.
    targets: [TargetId; 2],
    lifecycle_uids: [Uuid; 2],
    releases: [ReleaseId; 2],
    revisions: [ConfigRevisionId; 2],
}

const PROD: usize = 0;
const STAGING: usize = 1;

fn digest(n: u8) -> Digest {
    format!("sha256:{}", format!("{n:02x}").repeat(32))
        .parse()
        .expect("digest")
}

async fn shop(store: &Store, slug: &str) -> Shop {
    let org = store.create_org(slug, slug).await.expect("org").id;
    let mut t = store.tenant(org).await.expect("tenant");
    let project_slug = format!("{slug}-shop");
    let project = t.create_project(&project_slug, "Shop").await.expect("project");
    let cluster = t.create_cluster("primary").await.expect("cluster");
    let application = t
        .create_application(project, "web", "Web")
        .await
        .expect("application");
    let mut targets = Vec::new();
    for env in ["prod", "staging"] {
        let environment = t
            .create_environment(project, env, env, env == "prod")
            .await
            .expect("environment");
        let placement = t
            .create_placement(project, environment, cluster, &format!("kb-{slug}-{env}"))
            .await
            .expect("placement");
        targets.push(
            t.create_target(project, application, placement)
                .await
                .expect("target"),
        );
    }
    let mut releases = Vec::new();
    for n in [1, 2] {
        let (release, _) = t
            .create_release(
                project,
                &PortableRelease {
                    application,
                    artifacts: BTreeMap::from([("web".to_owned(), digest(n))]),
                    process_contract: json!({}),
                    portable_config: json!({}),
                    renderer_schema: 1,
                    source: Some(json!({ "image_repository": "ghcr.io/acme/web" })),
                    created_by: "user:alice".into(),
                },
            )
            .await
            .expect("release");
        releases.push(release);
    }
    let config = json!({ "runtime": { "processes": { "web": { "port": 8080 } } } });
    let mut revisions = Vec::new();
    let mut lifecycle_uids = Vec::new();
    for &target in &targets {
        let (revision, _) = t
            .create_config_revision(project, target, &config, "user:alice")
            .await
            .expect("revision")
            .expect("target");
        revisions.push(revision);
        lifecycle_uids.push(
            t.target_state(target)
                .await
                .expect("read")
                .expect("target")
                .lifecycle_uid,
        );
    }
    t.commit().await.expect("commit");
    Shop {
        org,
        project,
        project_slug,
        targets: targets.try_into().expect("two targets"),
        lifecycle_uids: lifecycle_uids.try_into().expect("two targets"),
        releases: releases.try_into().expect("two releases"),
        revisions: revisions.try_into().expect("two targets"),
    }
}

impl Shop {
    /// A run of release `release` on target `on` expecting `expected`.
    fn run(
        &self,
        on: usize,
        release: usize,
        expected: u64,
        reason: RunReason,
        hash: &str,
    ) -> StartDeployment {
        StartDeployment {
            project: self.project,
            target: self.targets[on],
            release: self.releases[release],
            config_revision: self.revisions[on],
            render_plan: None,
            expected_generation: Generation(expected),
            lifecycle_uid: self.lifecycle_uids[on],
            reason,
            requested_by: "user:alice".into(),
            input_hash: hash.as_bytes().to_vec(),
        }
    }
}

async fn start(store: &Store, org: OrgId, request: &StartDeployment) -> Started {
    let mut t = store.tenant(org).await.expect("tenant");
    let started = t
        .start_deployment(
            request,
            NewAudit {
                actor_kind: "user".into(),
                actor_id: Some("alice".into()),
                action: "deployment.accepted".into(),
                outcome: "accepted".into(),
                ..NewAudit::default()
            },
            None,
        )
        .await
        .expect("start");
    t.commit().await.expect("commit");
    started
}

fn accepted(started: Started) -> DeploymentRunId {
    let Started::Accepted { run, .. } = started else {
        panic!("not accepted: {started:?}");
    };
    run
}

/// Play the materializer: claim the one due run, move it through `events`
/// under the claim's fence, and settle the operation.
async fn settle(store: &Store, run: DeploymentRunId, events: &[RunEvent]) {
    let claim = store
        .claim_operation("acceptance", &[RUN_KIND], Duration::from_secs(30))
        .await
        .expect("claim")
        .expect("a due run");
    let mut outcome = "succeeded";
    for &event in events {
        match store.advance_run(&claim, run, event).await.expect("advance") {
            Advance::Moved(RunPhase::Failed) => outcome = "failed",
            Advance::Moved(_) => {}
            other => panic!("{event:?}: {other:?}"),
        }
    }
    assert!(
        store
            .finish_operation(&claim, outcome, None)
            .await
            .expect("finish"),
        "the claim still holds"
    );
}

async fn count(store: &Store, sql: &'static str) -> i64 {
    sqlx::query_scalar(sql)
        .fetch_one(store.pool())
        .await
        .expect("count")
}

/// Criterion "A/B race" (I05, I01): racers that expect the same generation
/// start at once; the target's row lock lets exactly one in, the others are
/// told the generation moved, and nothing of theirs is left behind.
#[tokio::test]
async fn concurrent_deploys_of_one_generation_let_exactly_one_in() {
    let Some(store) = pg_store().await else {
        skip("A/B race");
        return;
    };
    let s = shop(&store, "a").await;
    let racers: Vec<_> = (0..8)
        .map(|i| {
            let (store, org) = (store.clone(), s.org);
            let request = s.run(PROD, i % 2, 0, RunReason::Deploy, &format!("racer-{i}"));
            tokio::spawn(async move { start(&store, org, &request).await })
        })
        .collect();
    let (mut won, mut moved) = (0, 0);
    for racer in racers {
        match racer.await.expect("join") {
            Started::Accepted { generation, .. } => {
                assert_eq!(generation, Generation(1));
                won += 1;
            }
            Started::Rejected(Reject::GenerationMoved {
                current: 1,
                expected: 0,
            }) => moved += 1,
            other => panic!("unexpected: {other:?}"),
        }
    }
    assert_eq!((won, moved), (1, 7), "exactly one racer wins");

    let mut t = store.tenant(s.org).await.expect("tenant");
    let state = t
        .target_state(s.targets[PROD])
        .await
        .expect("read")
        .expect("target");
    assert_eq!(state.desired_generation, Generation(1));
    assert_eq!(t.runs(s.targets[PROD], 10).await.expect("runs").len(), 1);
    drop(t);
    assert_eq!(count(&store, "SELECT count(*) FROM operations").await, 1);
    assert_eq!(count(&store, "SELECT count(*) FROM outbox").await, 1);
    assert_eq!(
        count(
            &store,
            "SELECT count(*) FROM audit_events WHERE action = 'deployment.accepted'"
        )
        .await,
        1,
        "one accepted intent, one audit record; the losers wrote nothing"
    );
}

/// Criterion "tenant pool reuse" (I21): many transactions of two
/// organizations share the pool's four connections at once, as the server's
/// requests do; some commit, some fail on an error, some are dropped by a
/// panicking handler. Each sees only its own organization, and afterwards no
/// connection carries a tenant into its next transaction.
#[tokio::test]
async fn the_tenant_context_never_outlives_its_transaction_on_a_shared_pool() {
    let Some(store) = pg_store().await else {
        skip("tenant pool reuse");
        return;
    };
    let shops = [shop(&store, "a").await, shop(&store, "b").await];
    // The test server connects as a superuser, which bypasses row-level
    // security; probe as an ordinary role, as the server runs in production.
    let schema: String = sqlx::query_scalar("SELECT current_schema()")
        .fetch_one(store.pool())
        .await
        .expect("schema");
    // Safe to format: the schema is `t_` followed by hex digits.
    sqlx::raw_sql(AssertSqlSafe(format!(
        "DO $$ BEGIN CREATE ROLE kuben_rls_probe NOLOGIN; \
         EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$; \
         GRANT USAGE ON SCHEMA {schema} TO kuben_rls_probe; \
         GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA {schema} TO kuben_rls_probe;"
    )))
    .execute(store.pool())
    .await
    .expect("probe role");

    let tasks: Vec<_> = (0..24_usize)
        .map(|i| {
            let store = store.clone();
            let (org, own) = (shops[i % 2].org, shops[i % 2].project_slug.clone());
            tokio::spawn(async move {
                let mut t = store.tenant(org).await.expect("tenant");
                sqlx::query("SET LOCAL ROLE kuben_rls_probe")
                    .execute(&mut *t.tx)
                    .await
                    .expect("role");
                let slugs: Vec<String> = sqlx::query_scalar("SELECT slug FROM projects")
                    .fetch_all(&mut *t.tx)
                    .await
                    .expect("projects");
                assert_eq!(slugs, [own], "only its own organization");
                tokio::task::yield_now().await;
                match i % 3 {
                    0 => t.commit().await.expect("commit"),
                    1 => {
                        let failed = sqlx::query("SELECT no_such_column FROM projects")
                            .execute(&mut *t.tx)
                            .await;
                        assert!(failed.is_err(), "the transaction is aborted and dropped");
                    }
                    _ => panic!("{PLANNED_PANIC}"),
                }
            })
        })
        .collect();
    let mut panicked = 0;
    for task in tasks {
        if let Err(e) = task.await {
            let payload = e.into_panic();
            let message = payload
                .downcast_ref::<String>()
                .map(String::as_str)
                .or_else(|| payload.downcast_ref::<&str>().copied())
                .unwrap_or("?");
            assert_eq!(message, PLANNED_PANIC, "only the planned panics");
            panicked += 1;
        }
    }
    assert_eq!(panicked, 8);

    // Every connection of the pool, next transaction, no tenant: nothing.
    let mut connections = Vec::new();
    for _ in 0..4 {
        connections.push(store.pool().acquire().await.expect("connection"));
    }
    for connection in &mut connections {
        let mut tx = connection.begin().await.expect("begin");
        sqlx::query("SET LOCAL ROLE kuben_rls_probe")
            .execute(&mut *tx)
            .await
            .expect("role");
        let visible: i64 = sqlx::query_scalar("SELECT count(*) FROM projects")
            .fetch_one(&mut *tx)
            .await
            .expect("projects");
        assert_eq!(visible, 0, "a tenant context outlived its transaction");
        tx.rollback().await.expect("rollback");
    }
}

/// Criterion "rollback policy" (I08, I11): a rollback and a promotion reuse
/// existing releases (no build, no new release), a rollback pins the target,
/// and a failed deploy keeps its failure as its outcome after the rollback
/// that recovered from it succeeds: the rollback takes the failed run's
/// rights (it is superseded and can no longer recover), never its outcome.
#[tokio::test]
async fn rollback_and_promotion_reuse_releases_and_a_failed_run_stays_failed() {
    let Some(store) = pg_store().await else {
        skip("rollback policy");
        return;
    };
    let s = shop(&store, "a").await;
    let first = accepted(start(&store, s.org, &s.run(PROD, 0, 0, RunReason::Deploy, "deploy-1")).await);
    settle(&store, first, &CARRIED_OUT).await;
    let second = accepted(start(&store, s.org, &s.run(PROD, 1, 1, RunReason::Deploy, "deploy-2")).await);
    settle(&store, second, &[RunEvent::ReadyForDelivery, RunEvent::Failed]).await;
    let rollback = accepted(
        start(
            &store,
            s.org,
            &s.run(PROD, 0, 2, RunReason::Rollback, "rollback-1"),
        )
        .await,
    );
    settle(&store, rollback, &CARRIED_OUT).await;
    let promotion = accepted(
        start(
            &store,
            s.org,
            &s.run(STAGING, 0, 0, RunReason::Promotion, "promote-1"),
        )
        .await,
    );
    settle(&store, promotion, &CARRIED_OUT).await;

    let mut t = store.tenant(s.org).await.expect("tenant");
    let runs = t.runs(s.targets[PROD], 10).await.expect("runs");
    let history: Vec<_> = runs
        .iter()
        .map(|r| (r.generation.0, r.reason.as_str(), r.phase, r.outcome.as_deref()))
        .collect();
    assert_eq!(
        history,
        [
            (3, "rollback", RunPhase::Succeeded, Some("succeeded")),
            (2, "deploy", RunPhase::Superseded, Some("failed")),
            (1, "deploy", RunPhase::Succeeded, Some("succeeded")),
        ],
        "the rollback took the failed deploy's rights, never its failure"
    );
    assert_eq!(
        runs[0].release, s.releases[0],
        "the rollback runs release 1 again"
    );
    let prod = t
        .target_state(s.targets[PROD])
        .await
        .expect("read")
        .expect("target");
    assert_eq!(prod.policy, DeployPolicy::Pinned, "a rollback pins the target");

    let staging = t.runs(s.targets[STAGING], 10).await.expect("runs");
    assert_eq!(staging.len(), 1);
    assert_eq!(
        (staging[0].reason.as_str(), staging[0].release, staging[0].phase),
        ("promotion", s.releases[0], RunPhase::Succeeded),
        "the same release on another target"
    );
    let staging_state = t
        .target_state(s.targets[STAGING])
        .await
        .expect("read")
        .expect("target");
    assert_eq!(
        staging_state.policy,
        DeployPolicy::Auto,
        "a promotion does not pin"
    );
    drop(t);
    assert_eq!(
        count(&store, "SELECT count(*) FROM releases").await,
        2,
        "rollback and promotion made no release: nothing was built"
    );
}
