//! End-to-end HTTP tests against an in-memory store, seeded projections and
//! no cluster (writes that need Kubernetes must fail cleanly with 503).

use std::sync::Arc;

use axum::{
    Router,
    body::Body,
    http::{Request, StatusCode, header},
};
use http_body_util::BodyExt;
use kuben_api::{ApiState, auth::CLIENT_HEADER};
use kuben_core::{config::Config, ids::OrgId, perm::Role, traits::StaticPolicy};
use kuben_platform::{
    health::Health,
    projection::{AppView, EnvironmentView, PodPhase, PodView, ProcessView, ProjectView, Projections},
};
use kuben_store::{Store, repo::NewRelease};
use serde_json::json;
use tower::ServiceExt;

struct TestApp {
    router: Router,
    projections: Arc<Projections>,
    org: OrgId,
    store: Store,
}

async fn setup() -> TestApp {
    setup_with(|_| {}).await
}

async fn setup_with(tweak: impl FnOnce(&mut Config)) -> TestApp {
    let mut cfg = Config::default();
    cfg.security.cookie_secure = false;
    tweak(&mut cfg);
    let store = Store::memory().await.expect("store");
    let hasher = kuben_api::auth::password::Hasher::insecure_for_tests();
    let org = store.create_org("acme", "ACME").await.expect("org");
    let alice = store
        .create_user(
            "alice@example.com",
            Some("Alice"),
            Some(&hasher.hash("hunter22").expect("hash")),
        )
        .await
        .expect("user");
    store
        .bind_org_role(org.id, alice.id, Role::Owner)
        .await
        .expect("bind");
    let bob = store
        .create_user(
            "bob@example.com",
            None,
            Some(&hasher.hash("hunter22").expect("hash")),
        )
        .await
        .expect("user");
    store
        .bind_org_role(org.id, bob.id, Role::Viewer)
        .await
        .expect("bind");

    let health = Health::new();
    health.set_ready(true);
    let projections = Arc::new(Projections::new());
    let state = ApiState::new(
        cfg,
        store.clone(),
        None,
        projections.clone(),
        health,
        Arc::new(StaticPolicy),
    );
    TestApp {
        router: kuben_api::router(state),
