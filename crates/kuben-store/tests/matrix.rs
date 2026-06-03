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

    // ---- members (scenario 4) ----
    let members = store.list_members(org.id).await.expect("members");
    assert_eq!(members.len(), 1);
    assert_eq!(members[0].role, Role::Developer);
    store
        .set_org_role(org.id, user.id, Role::Owner)
        .await
        .expect("promote");
    assert_eq!(store.count_owners(org.id).await.expect("owners"), 1);
    let invited = store
        .create_invited_user("Bob@Example.com", None, Some("$argon2id$tmp"))
        .await
        .expect("invite");
    assert!(invited.must_change_password);
    store
        .bind_org_role(org.id, invited.id, Role::Viewer)
        .await
        .expect("bind");
    assert_eq!(store.list_members(org.id).await.expect("members").len(), 2);
    store
        .set_password_hash(invited.id, "$argon2id$new")
        .await
        .expect("password");
    let bob = store.find_user_by_id(invited.id).await.expect("q").expect("bob");
    assert!(!bob.must_change_password, "a new password clears the flag");
    store.remove_member(org.id, invited.id).await.expect("remove");
    assert_eq!(store.list_members(org.id).await.expect("members").len(), 1);

    // ---- releases (scenario 5) ----
    for (i, image) in ["nginx:1.27", "nginx:1.28"].into_iter().enumerate() {
        let r = store
            .record_release(NewRelease {
                org_id: Some(org.id),
                namespace: "kb-shop-prod".into(),
                app: "api".into(),
                image: Some(image.into()),
                spec: serde_json::json!({ "source": { "image": image } }),
                reason: "deploy".into(),
                actor_id: Some(user.id.to_string()),
                note: None,
            })
            .await
            .expect("release");
        assert_eq!(r.revision, i64::try_from(i).expect("small") + 1);
    }
    let releases = store
        .list_releases("kb-shop-prod", "api", 10)
        .await
        .expect("releases");
    assert_eq!(
        releases.iter().map(|r| r.revision).collect::<Vec<_>>(),
        vec![2, 1]
    );
    let first = store
        .find_release("kb-shop-prod", "api", 1)
        .await
        .expect("q")
        .expect("revision 1");
    assert_eq!(first.image.as_deref(), Some("nginx:1.27"));
    assert_eq!(first.spec["source"]["image"], "nginx:1.27");

    // ---- audit pagination (scenario 2) ----
    for action in ["createApp", "updateApp"] {
        store
            .append_audit(NewAudit {
                org_id: Some(org.id),
                actor_kind: "user".into(),
                action: action.into(),
                outcome: "success".into(),
                ..NewAudit::default()
            })
            .await
            .expect("audit");
    }
    let page = store.list_audit(org.id, None, 1).await.expect("page");
    assert_eq!(page.len(), 1);
    assert_eq!(page[0].action, "updateApp");
    let older = store
        .list_audit(org.id, Some(page[0].seq), 10)
        .await
        .expect("older");
    assert!(
        older
            .iter()
            .all(|e| e.seq < page[0].seq && e.org_id == Some(org.id))
    );
    assert!(older.iter().any(|e| e.action == "createApp"));

    // ---- login throttle windows (scenario 1) ----
    let now = now_ms();
    let window = 60_000;
    for i in 0..2 {
        store
            .throttle_record_failure("bucket-a", now + i, now + i - window)
            .await
            .expect("failure");
    }
    let w = store
        .throttle_window("bucket-a")
        .await
        .expect("q")
        .expect("window");
    assert_eq!((w.failures, w.started_at), (2, now));
    let later = now + 2 * window;
    store
        .throttle_record_failure("bucket-a", later, later - window)
        .await
        .expect("failure");
    let w = store
        .throttle_window("bucket-a")
        .await
        .expect("q")
        .expect("window");
    assert_eq!(
        (w.failures, w.started_at),
        (1, later),
        "an expired window restarts"
    );
    store
        .throttle_record_failure("bucket-b", now, now - window)
        .await
        .expect("failure");
    assert_eq!(
        store.throttle_purge(now).await.expect("purge"),
        1,
        "only the window of bucket-b has expired"
    );
    store.throttle_clear("bucket-a").await.expect("clear");
    assert!(store.throttle_window("bucket-a").await.expect("q").is_none());

    store.ping().await.expect("ping");
    store.checkpoint_and_close().await.expect("close");
}

#[tokio::test]
async fn sqlite_memory_roundtrip() {
    let store = Store::memory().await.expect("connect");
    assert_eq!(store.backend(), "sqlite");
    roundtrip(store).await;
}

#[tokio::test]
async fn sqlite_file_roundtrip_uses_separate_reader_pool() {
    let dir = std::env::temp_dir().join(format!("kuben-store-{}", uuid::Uuid::now_v7()));
    std::fs::create_dir_all(&dir).expect("tmp dir");
    let url = format!("sqlite://{}", dir.join("kuben.db").display());
    let store = Store::connect(&DatabaseCfg {
        url,
        max_connections: 2,
    })
    .await
    .expect("connect");
    roundtrip(store).await;
    std::fs::remove_dir_all(&dir).ok();
}

#[tokio::test]
async fn postgres_roundtrip() {
    let Ok(url) = std::env::var("KUBEN_TEST_PG_URL") else {
        eprintln!("KUBEN_TEST_PG_URL not set; skipping postgres matrix test");
        return;
    };
    let store = Store::connect(&DatabaseCfg {
        url,
        max_connections: 4,
    })
    .await
    .expect("connect");
    assert_eq!(store.backend(), "postgres");
    roundtrip(store).await;
}

/// Both backends ship the same migration files, declaring the same tables
/// and columns. New migrations are picked up from the directories.
#[test]
fn schema_parity_between_sqlite_and_postgres() {
    fn columns(sql: &str) -> Vec<(String, Vec<String>)> {
        let mut out = Vec::new();
        let mut current: Option<(String, Vec<String>)> = None;
        for line in sql.lines() {
            let l = line.trim();
            if let Some(rest) = l.strip_prefix("CREATE TABLE ") {
                let name = rest
                    .trim_end_matches(" (")
                    .trim_end_matches('(')
                    .trim()
                    .to_string();
                current = Some((name, Vec::new()));
            } else if l == ");" {
                if let Some(t) = current.take() {
                    out.push(t);
                }
            } else if let Some((_, cols)) = current.as_mut() {
                let first = l.split_whitespace().next().unwrap_or_default();
                if !first.is_empty() && !matches!(first, "PRIMARY" | "UNIQUE" | "--") {
                    cols.push(first.to_string());
                }
            }
        }
        out
    }
    fn migrations(backend: &str) -> (Vec<String>, String) {
        let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("migrations")
            .join(backend);
        let mut files: Vec<_> = std::fs::read_dir(&dir)
            .expect("migrations dir")
            .map(|e| e.expect("entry").path())
            .collect();
        files.sort();
        let names = files
            .iter()
            .map(|p| p.file_name().expect("name").to_string_lossy().into_owned())
            .collect();
        let sql = files
            .iter()
            .map(|p| std::fs::read_to_string(p).expect("read migration"))
            .collect::<Vec<_>>()
            .join("\n");
        (names, sql)
    }
    let (sqlite_files, sqlite) = migrations("sqlite");
    let (postgres_files, postgres) = migrations("postgres");
    assert_eq!(
        sqlite_files, postgres_files,
        "every migration needs a twin for the other backend"
    );
    assert_eq!(
        columns(&sqlite),
        columns(&postgres),
        "sqlite and postgres schemas diverged"
    );
}
