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

