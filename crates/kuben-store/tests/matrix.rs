//! Repository tests that run against SQLite (always) and Postgres (when
//! `KUBEN_TEST_PG_URL` is set, e.g. in CI).

use kuben_core::{config::DatabaseCfg, ids::TokenId, model::TokenScope, perm::Role, time::now_ms};
use kuben_store::{
    Store,
    repo::{NewAudit, NewRelease, NewSession, NewToken},
};

#[allow(clippy::too_many_lines)] // one pass over every repository, run against both backends
async fn roundtrip(store: Store) {
    // orgs + users + bindings
    let org = store.create_org("acme", "ACME Inc").await.expect("create org");
    assert_eq!(
        store.find_org_by_slug("acme").await.expect("q").expect("org").id,
        org.id
    );

    let user = store
        .create_user("Alice@Example.com", Some("Alice"), Some("$argon2id$fake"))
        .await
        .expect("user");
    assert_eq!(user.email, "alice@example.com", "emails are normalized");
    assert_eq!(store.count_users().await.expect("count"), 1);

    let creds = store
        .find_user_by_email("ALICE@example.com")
        .await
        .expect("q")
        .expect("found");
    assert_eq!(creds.password_hash.as_deref(), Some("$argon2id$fake"));
    assert!(
        store
            .find_user_by_email("nobody@example.com")
            .await
            .expect("q")
            .is_none()
    );

    store.add_membership(org.id, user.id).await.expect("membership");
    store
        .bind_org_role(org.id, user.id, Role::Developer)
        .await
        .expect("bind");
    let bindings = store.bindings_for_user(user.id).await.expect("bindings");
    assert_eq!(bindings.len(), 1);
    assert_eq!(bindings[0].role, Role::Developer);
    assert_eq!(bindings[0].org_id, org.id);

    // sessions
    let id_hash = vec![7u8; 32];
    store
        .create_session(NewSession {
            id_hash: id_hash.clone(),
            user_id: user.id,
            expires_at: now_ms() + 60_000,
            ip: Some("127.0.0.1".into()),
            ua_hash: None,
