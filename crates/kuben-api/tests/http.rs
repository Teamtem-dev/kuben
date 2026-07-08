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
        projections,
        org: org.id,
        store,
    }
}

fn seed(app: &TestApp) {
    let org = Some(app.org.to_string());
    app.projections.upsert_project(ProjectView {
        name: "shop".into(),
        uid: Some(uuid::Uuid::now_v7().to_string()),
        display_name: "Shop".into(),
        description: None,
        org: org.clone(),
        environments: 1,
        ready: true,
        deleting: false,
        created_at: None,
    });
    app.projections.upsert_project(ProjectView {
        name: "secret-project".into(),
        uid: Some(uuid::Uuid::now_v7().to_string()),
        display_name: "Other tenant".into(),
        description: None,
        org: Some(OrgId::new().to_string()),
        environments: 0,
        ready: true,
        deleting: false,
        created_at: None,
    });
    app.projections.upsert_environment(EnvironmentView {
        name: "shop-prod".into(),
        uid: Some(uuid::Uuid::now_v7().to_string()),
        project: "shop".into(),
        org: org.clone(),
        env_type: "production",
        namespace: "kb-shop-prod".into(),
        phase: Some("Ready".into()),
        ready: true,
        message: None,
        deleting: false,
        deletion_scheduled_at: None,
        created_at: None,
    });
    app.projections.upsert_app(AppView {
        key: "kb-shop-prod/api".into(),
        namespace: "kb-shop-prod".into(),
        name: "api".into(),
        uid: Some(uuid::Uuid::now_v7().to_string()),
        org: org.clone(),
        project: Some("shop".into()),
        environment: Some("shop-prod".into()),
        image: Some("nginx:1.27".into()),
        git_repo: None,
        url: Some("https://api.example.com".into()),
        ready: true,
        reason: Some("Available".into()),
        message: None,
        processes: vec![ProcessView {
            name: "web".into(),
            command: vec![],
            port: Some(80),
            size: "small".into(),
            min_replicas: 1,
            max_replicas: 1,
            schedule: None,
            protocol: "http".into(),
        }],
        env: vec![],
        domains: vec!["api.example.com".into()],
        volumes: vec![],
        created_at: None,
    });
    app.projections.upsert_pod(PodView {
        key: "kb-shop-prod/api-web-1".into(),
        namespace: "kb-shop-prod".into(),
        name: "api-web-1".into(),
        org,
        app: Some("api".into()),
        process: Some("web".into()),
        phase: PodPhase::Running,
        ready: true,
        restarts: 0,
        reason: None,
        node: Some("node-1".into()),
        started_at: None,
    });
}

async fn send(app: &Router, req: Request<Body>) -> (StatusCode, serde_json::Value) {
    let resp = app.clone().oneshot(req).await.expect("response");
    let status = resp.status();
    let bytes = resp.into_body().collect().await.expect("body").to_bytes();
    (
        status,
        serde_json::from_slice(&bytes).unwrap_or(serde_json::Value::Null),
    )
}

fn get(path: &str, cookie: &str) -> Request<Body> {
    Request::get(path)
        .header(header::COOKIE, cookie)
        .body(Body::empty())
        .expect("request")
}

fn post(path: &str, cookie: &str, body: &str) -> Request<Body> {
    Request::post(path)
        .header(header::COOKIE, cookie)
        .header(header::CONTENT_TYPE, "application/json")
        .header(CLIENT_HEADER, "test")
        .body(Body::from(body.to_owned()))
        .expect("request")
}

async fn login(app: &Router, email: &str) -> String {
    let req = Request::post("/api/v1/auth/login")
        .header(header::CONTENT_TYPE, "application/json")
        .header(CLIENT_HEADER, "test")
        .body(Body::from(format!(
            r#"{{"email":"{email}","password":"hunter22"}}"#
        )))
        .expect("request");
    let resp = app.clone().oneshot(req).await.expect("response");
    assert_eq!(resp.status(), StatusCode::OK);
    let set_cookie = resp.headers()[header::SET_COOKIE]
        .to_str()
        .expect("cookie")
        .to_owned();
    set_cookie.split(';').next().expect("pair").to_owned()
}

#[tokio::test]
async fn health_endpoints() {
    let app = setup().await;
    let resp = app
        .router
        .clone()
        .oneshot(Request::get("/livez").body(Body::empty()).expect("req"))
        .await
        .expect("resp");
    assert_eq!(resp.status(), StatusCode::OK);
    let resp = app
        .router
        .oneshot(Request::get("/readyz").body(Body::empty()).expect("req"))
        .await
        .expect("resp");
    assert_eq!(resp.status(), StatusCode::OK);
}

#[tokio::test]
async fn unknown_api_route_is_json_404_not_spa() {
    let app = setup().await;
    let resp = app
        .router
        .oneshot(Request::get("/api/v1/nope").body(Body::empty()).expect("req"))
        .await
        .expect("resp");
    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
    assert_eq!(resp.headers()[header::CONTENT_TYPE], "application/problem+json");
}

#[tokio::test]
async fn me_requires_auth() {
    let app = setup().await;
    let (status, problem) = send(
        &app.router,
        Request::get("/api/v1/me").body(Body::empty()).expect("req"),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
    assert_eq!(problem["code"], "unauthorized");
}

#[tokio::test]
async fn login_without_csrf_header_is_forbidden() {
    let app = setup().await;
    let req = Request::post("/api/v1/auth/login")
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(
            r#"{"email":"alice@example.com","password":"hunter22"}"#,
        ))
        .expect("req");
    let (status, _) = send(&app.router, req).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn session_lifecycle() {
    let app = setup().await;
    let req = Request::post("/api/v1/auth/login")
        .header(header::CONTENT_TYPE, "application/json")
        .header(CLIENT_HEADER, "test")
        .body(Body::from(
            r#"{"email":"Alice@Example.com","password":"hunter22"}"#,
        ))
        .expect("req");
    let resp = app.router.clone().oneshot(req).await.expect("resp");
    assert_eq!(resp.status(), StatusCode::OK);
    let set_cookie = resp.headers()[header::SET_COOKIE]
        .to_str()
        .expect("cookie")
        .to_owned();
    assert!(set_cookie.starts_with("kuben_session="));
    assert!(
        set_cookie.contains("HttpOnly")
            && set_cookie.contains("SameSite=Lax")
            && set_cookie.contains("Max-Age")
    );
    let cookie = set_cookie.split(';').next().expect("pair").to_owned();

    let (status, me) = send(&app.router, get("/api/v1/me", &cookie)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(
        (me["email"].as_str(), me["via"].as_str()),
        (Some("alice@example.com"), Some("session"))
    );

    let (status, _) = send(&app.router, post("/api/v1/auth/logout", &cookie, "")).await;
    assert_eq!(status, StatusCode::NO_CONTENT);
    let (status, _) = send(&app.router, get("/api/v1/me", &cookie)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn wrong_password_is_unauthorized() {
    let app = setup().await;
    let req = Request::post("/api/v1/auth/login")
        .header(header::CONTENT_TYPE, "application/json")
        .header(CLIENT_HEADER, "test")
        .body(Body::from(r#"{"email":"alice@example.com","password":"nope"}"#))
        .expect("req");
    let (status, _) = send(&app.router, req).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn projects_are_tenant_scoped() {
    let app = setup().await;
    seed(&app);
    let cookie = login(&app.router, "alice@example.com").await;

    let (status, list) = send(&app.router, get("/api/v1/projects", &cookie)).await;
    assert_eq!(status, StatusCode::OK);
    let names: Vec<&str> = list
        .as_array()
        .expect("array")
        .iter()
        .filter_map(|p| p["name"].as_str())
        .collect();
    assert_eq!(names, vec!["shop"], "other tenants' projects are invisible");

    let (status, project) = send(&app.router, get("/api/v1/projects/shop", &cookie)).await;
    assert_eq!(
        (status, project["display_name"].as_str()),
        (StatusCode::OK, Some("Shop"))
    );
    let (status, _) = send(&app.router, get("/api/v1/projects/secret-project", &cookie)).await;
    assert_eq!(
        status,
        StatusCode::NOT_FOUND,
        "404, not 403: existence must not leak"
    );

    // No cluster: validation runs first, then a clean 503.
    let (status, _) = send(
        &app.router,
        post(
            "/api/v1/projects",
            &cookie,
            r#"{"name":"Bad Name","display_name":"x"}"#,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    let (status, _) = send(
        &app.router,
        post(
            "/api/v1/projects",
            &cookie,
            r#"{"name":"blog","display_name":"Blog"}"#,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE);

    // A project with environments cannot be deleted.
    let req = Request::delete("/api/v1/projects/shop")
        .header(header::COOKIE, &cookie)
        .header(CLIENT_HEADER, "test")
        .body(Body::empty())
        .expect("req");
    let (status, problem) = send(&app.router, req).await;
    assert_eq!(
        (status, problem["code"].as_str()),
        (StatusCode::CONFLICT, Some("conflict"))
    );
}

#[tokio::test]
async fn environments_and_apps_read_from_projections() {
    let app = setup().await;
    seed(&app);
    let cookie = login(&app.router, "alice@example.com").await;

    let (status, envs) = send(&app.router, get("/api/v1/projects/shop/environments", &cookie)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(envs[0]["name"], "prod");
    assert_eq!(envs[0]["resource_name"], "shop-prod");
    assert_eq!(envs[0]["env_type"], "production");

    let (status, apps) = send(
        &app.router,
        get("/api/v1/projects/shop/environments/prod/apps", &cookie),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(apps[0]["name"], "api");
    assert_eq!(apps[0]["environment"], "prod");

    let (status, detail) = send(
        &app.router,
        get("/api/v1/projects/shop/environments/prod/apps/api", &cookie),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(detail["app"]["url"], "https://api.example.com");
    assert_eq!(detail["pods"][0]["name"], "api-web-1");
    assert_eq!(detail["pods"][0]["phase"], "running");

    let (status, _) = send(
        &app.router,
        get("/api/v1/projects/shop/environments/prod/apps/nope", &cookie),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    let (status, _) = send(
        &app.router,
        get("/api/v1/projects/shop/environments/prod/apps/api/logs", &cookie),
    )
    .await;
    assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE);

    // Invalid input is rejected before touching the cluster.
    let bad = r#"{"name":"web","image":"nginx latest"}"#;
    let (status, _) = send(
        &app.router,
        post("/api/v1/projects/shop/environments/prod/apps", &cookie, bad),
    )
    .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    let good = r#"{"name":"web","image":"nginx:1.27","port":80}"#;
    let (status, _) = send(
        &app.router,
        post("/api/v1/projects/shop/environments/prod/apps", &cookie, good),
    )
    .await;
    assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE);
    let (status, _) = send(
        &app.router,
        post(
            "/api/v1/projects/shop/environments",
            &cookie,
            r#"{"name":"staging"}"#,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE);
}

#[tokio::test]
async fn viewers_can_read_but_not_write() {
    let app = setup().await;
    seed(&app);
    let cookie = login(&app.router, "bob@example.com").await;

    let (status, _) = send(&app.router, get("/api/v1/projects/shop/environments", &cookie)).await;
    assert_eq!(status, StatusCode::OK);
    let (status, problem) = send(
        &app.router,
        post("/api/v1/projects/shop/environments", &cookie, r#"{"name":"qa"}"#),
    )
    .await;
    assert_eq!(
        (status, problem["code"].as_str()),
        (StatusCode::FORBIDDEN, Some("forbidden"))
    );
    let body = r#"{"name":"web","image":"nginx:1.27"}"#;
    let (status, _) = send(
        &app.router,
        post("/api/v1/projects/shop/environments/prod/apps", &cookie, body),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    let (status, _) = send(
        &app.router,
        post(
            "/api/v1/projects/shop/environments/prod/apps/api/restart",
            &cookie,
            "",
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn openapi_docs_are_served() {
    let app = setup().await;
    let resp = app
        .router
        .oneshot(Request::get("/api/docs").body(Body::empty()).expect("req"))
        .await
        .expect("resp");
    assert_eq!(resp.status(), StatusCode::OK);
}

// ---------------------------------------------------------------------------
// Scenarios 1–5 and 8 (docs/KUBEN-MASTER-BLUEPRINT.md §5.9)
// ---------------------------------------------------------------------------

enum Auth<'a> {
    Anonymous,
    Cookie(&'a str),
    Bearer(&'a str),
}

async fn call(
    app: &Router,
    method: &str,
    path: &str,
    auth: Auth<'_>,
    body: Option<serde_json::Value>,
    forwarded_for: Option<&str>,
) -> (StatusCode, axum::http::HeaderMap, serde_json::Value) {
    let mut b = Request::builder()
        .method(method)
        .uri(path)
        .header(header::CONTENT_TYPE, "application/json");
    b = match auth {
        Auth::Anonymous => b.header(CLIENT_HEADER, "web"),
        Auth::Cookie(c) => b.header(header::COOKIE, c).header(CLIENT_HEADER, "web"),
        Auth::Bearer(t) => b.header(header::AUTHORIZATION, format!("Bearer {t}")),
    };
    if let Some(xff) = forwarded_for {
        b = b.header("x-forwarded-for", xff);
    }
    let req = b
        .body(body.map_or_else(Body::empty, |v| Body::from(v.to_string())))
        .expect("request");
    let resp = app.clone().oneshot(req).await.expect("response");
    let status = resp.status();
    let headers = resp.headers().clone();
    let bytes = resp.into_body().collect().await.expect("body").to_bytes();
    let json = serde_json::from_slice(&bytes).unwrap_or(serde_json::Value::Null);
    (status, headers, json)
}

async fn sign_in(app: &Router, email: &str, password: &str) -> (StatusCode, String, serde_json::Value) {
    let (status, headers, body) = call(
        app,
        "POST",
        "/api/v1/auth/login",
        Auth::Anonymous,
        Some(json!({ "email": email, "password": password })),
        None,
    )
    .await;
    let cookie = headers
        .get(header::SET_COOKIE)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.split(';').next())
        .unwrap_or_default()
        .to_owned();
    (status, cookie, body)
}

async fn status_of(
    app: &Router,
    method: &str,
    path: &str,
    cookie: &str,
    body: Option<serde_json::Value>,
) -> StatusCode {
    call(app, method, path, Auth::Cookie(cookie), body, None).await.0
}

async fn invite(app: &Router, cookie: &str, email: &str, role: &str) -> (StatusCode, serde_json::Value) {
    let (status, _, body) = call(
        app,
        "POST",
        "/api/v1/members",
        Auth::Cookie(cookie),
        Some(json!({ "email": email, "role": role })),
        None,
    )
    .await;
    (status, body)
}

async fn try_login(app: &Router, password: &str, forwarded_for: &str) -> (StatusCode, axum::http::HeaderMap) {
    let (status, headers, _) = call(
        app,
        "POST",
        "/api/v1/auth/login",
        Auth::Anonymous,
        Some(json!({ "email": "alice@example.com", "password": password })),
        Some(forwarded_for),
    )
    .await;
    (status, headers)
}

#[tokio::test]
async fn scenario1_login_is_throttled_per_client_and_ignores_forged_hops() {
    let t = setup_with(|c| {
        c.security.login_max_failures = 3;
        c.security.trust_forwarded_for = true;
    })
    .await;
    for _ in 0..3 {
        assert_eq!(
            try_login(&t.router, "wrong", "6.6.6.6, 10.0.0.1").await.0,
            StatusCode::UNAUTHORIZED
        );
    }
    // Same proxy-appended hop, forged first hop, correct password: still blocked.
    let (status, headers) = try_login(&t.router, "hunter22", "1.2.3.4, 10.0.0.1").await;
    assert_eq!(status, StatusCode::TOO_MANY_REQUESTS);
    let retry: u64 = headers
        .get(header::RETRY_AFTER)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.parse().ok())
        .expect("Retry-After");
    assert!(retry > 0);
    assert_eq!(
        try_login(&t.router, "hunter22", "6.6.6.6, 10.0.0.2").await.0,
        StatusCode::OK,
        "another client is unaffected"
    );
}

#[tokio::test]
async fn scenario2_every_mutation_is_audited_without_handler_code() {
    let t = setup().await;
    seed(&t);
    let (_, alice, _) = sign_in(&t.router, "alice@example.com", "hunter22").await;
    let (_, bob, _) = sign_in(&t.router, "bob@example.com", "hunter22").await;
    let body = json!({ "name": "blog", "display_name": "Blog" });
    assert_eq!(
        status_of(&t.router, "POST", "/api/v1/projects", &bob, Some(body.clone())).await,
        StatusCode::FORBIDDEN
    );
    assert_eq!(
        status_of(&t.router, "POST", "/api/v1/projects", &alice, Some(body)).await,
        StatusCode::SERVICE_UNAVAILABLE,
        "no cluster in tests"
    );

    let (status, _, page) = call(
        &t.router,
        "GET",
        "/api/v1/audit",
        Auth::Cookie(&alice),
        None,
        None,
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let events = page["events"].as_array().expect("events");
    let seen: Vec<(String, String)> = events
        .iter()
        .map(|e| {
            (
                e["action"].as_str().unwrap_or_default().to_owned(),
                e["outcome"].as_str().unwrap_or_default().to_owned(),
            )
        })
        .collect();
    for expected in [
        ("createProject", "denied"),
        ("createProject", "error"),
        ("login", "success"),
    ] {
        assert!(
            seen.contains(&(expected.0.to_owned(), expected.1.to_owned())),
            "missing {expected:?} in {seen:?}"
        );
    }
    let denied = events.iter().find(|e| e["outcome"] == "denied").expect("denied");
    assert_eq!(denied["actor"], "bob@example.com");
    assert_eq!(denied["status"], 403);
    assert_eq!(
        call(&t.router, "GET", "/api/v1/audit", Auth::Cookie(&bob), None, None)
            .await
            .0,
        StatusCode::FORBIDDEN,
        "viewers cannot read the audit log"
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // one end-to-end story per scenario
async fn scenario3_api_tokens_are_capped_scoped_and_revocable() {
    let t = setup().await;
    seed(&t);
    let (_, alice, _) = sign_in(&t.router, "alice@example.com", "hunter22").await;
    let (status, _, created) = call(
        &t.router,
        "POST",
        "/api/v1/tokens",
        Auth::Cookie(&alice),
        Some(json!({ "name": "ci", "role": "viewer" })),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{created}");
    let token = created["token"].as_str().expect("token").to_owned();
    assert!(token.starts_with("kbn_pat_"));
    assert!(
        created["info"]["prefix"]
            .as_str()
            .is_some_and(|p| token.starts_with(p))
    );

    let (status, _, me) = call(&t.router, "GET", "/api/v1/me", Auth::Bearer(&token), None, None).await;
    assert_eq!((status, me["via"].as_str()), (StatusCode::OK, Some("token")));
    assert_eq!(
        call(
            &t.router,
            "GET",
            "/api/v1/projects",
            Auth::Bearer(&token),
            None,
            None
        )
        .await
        .0,
        StatusCode::OK
    );
    let project = json!({ "name": "blog", "display_name": "Blog" });
    assert_eq!(
        call(
            &t.router,
            "POST",
            "/api/v1/projects",
            Auth::Bearer(&token),
            Some(project),
            None
        )
        .await
        .0,
        StatusCode::FORBIDDEN,
        "viewer cap"
    );
    assert_eq!(
        call(
            &t.router,
            "POST",
            "/api/v1/tokens",
            Auth::Bearer(&token),
            Some(json!({ "name": "x" })),
            None
        )
        .await
        .0,
        StatusCode::FORBIDDEN,
        "tokens cannot mint tokens"
    );
    let mut forged = token.clone();
    let last = forged.pop().expect("char");
    forged.push(if last == 'A' { 'B' } else { 'A' });
    assert_eq!(
        call(&t.router, "GET", "/api/v1/me", Auth::Bearer(&forged), None, None)
            .await
            .0,
        StatusCode::UNAUTHORIZED
    );

    let (_, _, scoped) = call(
        &t.router,
        "POST",
        "/api/v1/tokens",
        Auth::Cookie(&alice),
        Some(json!({ "name": "deploy", "role": "developer", "project": "shop" })),
        None,
    )
    .await;
    let scoped = scoped["token"].as_str().expect("scoped token").to_owned();
    assert_eq!(
        call(
            &t.router,
            "GET",
            "/api/v1/projects/shop/environments",
            Auth::Bearer(&scoped),
            None,
            None
        )
        .await
        .0,
        StatusCode::OK
    );
    assert_eq!(
        call(
            &t.router,
            "GET",
            "/api/v1/members",
            Auth::Bearer(&scoped),
            None,
            None
        )
        .await
        .0,
        StatusCode::FORBIDDEN,
        "a project token has no org-wide authority"
    );

    let id = created["info"]["id"].as_str().expect("id");
    assert_eq!(
        call(
            &t.router,
            "DELETE",
            &format!("/api/v1/tokens/{id}"),
            Auth::Cookie(&alice),
            None,
            None
        )
        .await
        .0,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        call(&t.router, "GET", "/api/v1/me", Auth::Bearer(&token), None, None)
            .await
            .0,
        StatusCode::UNAUTHORIZED
    );
}

async fn change_password(app: &Router, cookie: &str, current: &str, new: &str) -> StatusCode {
    call(
        app,
        "POST",
        "/api/v1/me/password",
        Auth::Cookie(cookie),
        Some(json!({ "current_password": current, "new_password": new })),
        None,
    )
    .await
    .0
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // one end-to-end story per scenario
async fn scenario4_team_members_follow_the_role_rules() {
    let t = setup().await;
    seed(&t);
    let (_, alice, me) = sign_in(&t.router, "alice@example.com", "hunter22").await;
    let alice_id = me["id"].as_str().expect("id").to_owned();

    let (status, invited) = invite(&t.router, &alice, "carol@example.com", "developer").await;
    assert_eq!(status, StatusCode::CREATED, "{invited}");
    let temp = invited["temporary_password"]
        .as_str()
        .expect("temporary password")
        .to_owned();
    let carol_id = invited["member"]["id"].as_str().expect("id").to_owned();
    let (status, carol, me) = sign_in(&t.router, "carol@example.com", &temp).await;
    assert_eq!(
        (status, &me["must_change_password"]),
        (StatusCode::OK, &json!(true))
    );
    assert_eq!(
        call(
            &t.router,
            "GET",
            "/api/v1/projects",
            Auth::Cookie(&carol),
            None,
            None
        )
        .await
        .0,
        StatusCode::FORBIDDEN,
        "the temporary password must be replaced first"
    );
    assert_eq!(
        change_password(&t.router, &carol, &temp, "short").await,
        StatusCode::UNPROCESSABLE_ENTITY
    );
    assert_eq!(
        change_password(&t.router, &carol, "wrong", "correct horse battery").await,
        StatusCode::UNPROCESSABLE_ENTITY
    );
    assert_eq!(
        change_password(&t.router, &carol, &temp, "correct horse battery").await,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        call(
            &t.router,
            "GET",
            "/api/v1/projects",
            Auth::Cookie(&carol),
            None,
            None
        )
        .await
        .0,
        StatusCode::OK
    );

    let role_change = |id: &str, role: &str| (format!("/api/v1/members/{id}"), json!({ "role": role }));
    let (path, body) = role_change(&carol_id, "viewer");
    assert_eq!(
        status_of(&t.router, "PATCH", &path, &alice, Some(body)).await,
        StatusCode::OK
    );
    let (path, body) = role_change(&alice_id, "admin");
    assert_eq!(
        status_of(&t.router, "PATCH", &path, &alice, Some(body)).await,
        StatusCode::CONFLICT,
        "own role"
    );
    assert_eq!(
        invite(&t.router, &carol, "eve@example.com", "viewer").await.0,
        StatusCode::FORBIDDEN,
        "viewers cannot invite"
    );

    let (_, dave) = invite(&t.router, &alice, "dave@example.com", "admin").await;
    let dave_temp = dave["temporary_password"].as_str().expect("temp").to_owned();
    let (_, dave, _) = sign_in(&t.router, "dave@example.com", &dave_temp).await;
    assert_eq!(
        change_password(&t.router, &dave, &dave_temp, "another long password").await,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        invite(&t.router, &dave, "mallory@example.com", "owner").await.0,
        StatusCode::FORBIDDEN,
        "no escalation"
    );
    assert_eq!(
        call(
            &t.router,
            "DELETE",
            &format!("/api/v1/members/{alice_id}"),
            Auth::Cookie(&dave),
            None,
            None
        )
        .await
        .0,
        StatusCode::FORBIDDEN,
        "only owners touch owners"
    );
    assert_eq!(
        invite(&t.router, &dave, "erin@example.com", "developer").await.0,
        StatusCode::CREATED,
        "admins can invite"
    );

    assert_eq!(
        call(
            &t.router,
            "DELETE",
            &format!("/api/v1/members/{carol_id}"),
            Auth::Cookie(&alice),
            None,
            None
        )
        .await
        .0,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        call(&t.router, "GET", "/api/v1/me", Auth::Cookie(&carol), None, None)
            .await
            .0,
        StatusCode::UNAUTHORIZED,
        "removal revokes sessions"
    );
    let (_, _, members) = call(
        &t.router,
        "GET",
        "/api/v1/members",
        Auth::Cookie(&alice),
        None,
        None,
    )
    .await;
    let emails: Vec<&str> = members
        .as_array()
        .expect("members")
        .iter()
        .filter_map(|m| m["email"].as_str())
        .collect();
    assert!(
        emails.contains(&"erin@example.com") && !emails.contains(&"carol@example.com"),
        "{emails:?}"
    );
}

#[tokio::test]
async fn scenario5_releases_are_newest_first_and_rollback_is_authorized() {
    let t = setup().await;
    seed(&t);
    for image in ["nginx:1.26", "nginx:1.27"] {
        t.store
            .record_release(NewRelease {
                org_id: Some(t.org),
                namespace: "kb-shop-prod".into(),
                app: "api".into(),
                image: Some(image.into()),
                spec: json!({
                    "source": { "image": image },
                    "runtime": { "processes": { "web": { "port": 80 } } }
                }),
                reason: "deploy".into(),
                actor_id: None,
                note: None,
            })
            .await
            .expect("release");
    }
    let base = "/api/v1/projects/shop/environments/prod/apps/api";
    let (_, alice, _) = sign_in(&t.router, "alice@example.com", "hunter22").await;
    let (_, bob, _) = sign_in(&t.router, "bob@example.com", "hunter22").await;
    let (status, _, list) = call(
        &t.router,
        "GET",
        &format!("{base}/releases"),
        Auth::Cookie(&bob),
        None,
        None,
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let revisions: Vec<i64> = list
        .as_array()
        .expect("releases")
        .iter()
        .filter_map(|r| r["revision"].as_i64())
        .collect();
    assert_eq!(revisions, vec![2, 1]);
    assert_eq!(list[0]["current"], true);
    assert_eq!(list[0]["image"], "nginx:1.27");

    let rollback = format!("{base}/rollback");
    let to = |revision: i64| Some(json!({ "revision": revision }));
    assert_eq!(
        status_of(&t.router, "POST", &rollback, &bob, to(1)).await,
        StatusCode::FORBIDDEN
    );
    assert_eq!(
        status_of(&t.router, "POST", &rollback, &alice, to(9)).await,
        StatusCode::NOT_FOUND
    );
    assert_eq!(
        status_of(&t.router, "POST", &rollback, &alice, to(1)).await,
        StatusCode::SERVICE_UNAVAILABLE,
        "no cluster in tests"
    );
}

#[tokio::test]
async fn scenario8_template_catalogue() {
    let t = setup().await;
    seed(&t);
    let (_, alice, _) = sign_in(&t.router, "alice@example.com", "hunter22").await;
    let (_, bob, _) = sign_in(&t.router, "bob@example.com", "hunter22").await;
    let (status, _, list) = call(
        &t.router,
        "GET",
        "/api/v1/templates",
        Auth::Cookie(&bob),
        None,
        None,
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let templates = list.as_array().expect("templates");
    assert_eq!(templates.len(), 8);
    let pg = templates
        .iter()
        .find(|t| t["id"] == "postgres")
        .expect("postgres");
    assert_eq!(pg["protocol"], "tcp");
    assert!(
        pg["connection_keys"]
            .as_array()
            .expect("keys")
            .contains(&json!("url"))
    );

    let deploy = "/api/v1/projects/shop/environments/prod/templates/postgres";
    let name = || Some(json!({ "name": "db" }));
    assert_eq!(
        status_of(&t.router, "POST", deploy, &bob, name()).await,
        StatusCode::FORBIDDEN
    );
    assert_eq!(
        status_of(&t.router, "POST", deploy, &alice, name()).await,
        StatusCode::SERVICE_UNAVAILABLE,
        "no cluster in tests"
    );
}
