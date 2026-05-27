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
        })
        .await
        .expect("session");
    let s = store.find_session(&id_hash).await.expect("q").expect("session");
    assert_eq!(s.user_id, user.id);
    assert!(s.is_valid_at(now_ms()));
    store.revoke_session(&id_hash).await.expect("revoke");
    let s = store.find_session(&id_hash).await.expect("q").expect("session");
    assert!(!s.is_valid_at(now_ms()));
    assert!(store.find_session(&[1u8; 32]).await.expect("q").is_none());

    // audit
    let id = store
        .append_audit(NewAudit {
            org_id: Some(org.id),
            actor_kind: "user".into(),
            actor_id: Some(user.id.to_string()),
            action: "login".into(),
            outcome: "success".into(),
            data: Some(serde_json::json!({"method": "password"})),
            ..NewAudit::default()
        })
        .await
        .expect("audit");
    let recent = store.recent_audit(10).await.expect("recent");
    assert_eq!(recent.len(), 1);
    assert_eq!(recent[0].id, id);
    assert_eq!(
        recent[0].data.as_ref().and_then(|d| d["method"].as_str()),
        Some("password")
    );

    // ---- api tokens (scenario 3) ----
    let token = store
        .create_token(NewToken {
            id: TokenId::new(),
            org_id: org.id,
            owner: user.id,
            name: "ci".into(),
            prefix: "kbn_pat_test".into(),
            secret_hash: vec![9u8; 32],
            scope: TokenScope {
                role: Role::Developer,
                project: None,
                environment: None,
            },
            expires_at: None,
        })
        .await
        .expect("token");
    let found = store.find_token(token.id).await.expect("q").expect("found");
    assert_eq!(found.scope.role, Role::Developer);
    assert_eq!(found.secret_hash, vec![9u8; 32]);
    assert_eq!(store.list_tokens(user.id).await.expect("list").len(), 1);
    store.touch_token(token.id).await.expect("touch");
    assert!(store.revoke_token(token.id, user.id).await.expect("revoke"));
    assert!(!store.revoke_token(token.id, user.id).await.expect("revoke twice"));
    let revoked = store.find_token(token.id).await.expect("q").expect("found");
    assert!(!revoked.is_usable_at(now_ms()));
