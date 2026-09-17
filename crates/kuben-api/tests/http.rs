//! End-to-end HTTP tests against PostgreSQL (the SQL model of ADR-032): apps
//! seeded in SQL, live status seeded in the projections, fixed image digests
//! instead of a registry, and no cluster (what acts on Kubernetes directly
//! must fail cleanly with 503).

use std::{collections::BTreeMap, sync::Arc};

use axum::{
    Router,
    body::Body,
    http::{Request, StatusCode, header},
};
use http_body_util::BodyExt;
use kuben_api::{
    ApiState,
    auth::CLIENT_HEADER,
    oci::{FixedImages, ImageResolver, ResolveError, Resolved},
    oidc::{GithubOidc, Jwks},
};
use kuben_core::{config::Config, ids::OrgId, ops::Generation, perm::Role, traits::StaticPolicy};
use kuben_platform::{
    health::Health,
    projection::{AppView, EnvironmentView, PodPhase, PodView, ProcessView, ProjectView, Projections},
    secrets::RegistryLogin,
};
use kuben_store::{
    Store,
    repo::{EnvironmentKind, NewAudit, PortableRelease, RunReason, StartDeployment},
};
use serde_json::json;
use tower::ServiceExt;

struct TestApp {
    router: Router,
    projections: Arc<Projections>,
    org: OrgId,
    store: Store,
    usage: Arc<kuben_platform::usage::UsageBuffer>,
}

/// The test app on a fresh PostgreSQL schema, or `None` (the test skips)
/// when `KUBEN_TEST_PG_URL` is not set.
async fn setup() -> Option<TestApp> {
    setup_with(|_| {}).await
}

async fn setup_with(tweak: impl FnOnce(&mut Config)) -> Option<TestApp> {
    let mut cfg = Config::default();
    cfg.security.cookie_secure = kuben_core::config::CookieSecure::Fixed(false);
    tweak(&mut cfg);
    let Some(store) = kuben_store::testing::pg_store().await else {
        kuben_store::testing::skip("http");
        return None;
    };
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
    let usage = kuben_platform::usage::UsageBuffer::new();
    let projections = Arc::new(Projections::new());
    // CI trust with the fixture's keys, at a time its tokens are valid.
    let oidc = cfg.github_oidc_audience().map(|audience| {
        GithubOidc::with_keys(&cfg.ci.github_oidc_issuer, &audience, ci_fixture().0).with_clock(|| CI_NOW)
    });
    let sso = cfg.sso.enabled.then(|| {
        let jwks = serde_json::from_value(sso_fixture()["jwks"].clone()).expect("jwks");
        kuben_api::sso::SsoClient::from_config(&cfg.sso, cfg.server.public_url.as_deref(), "acme")
            .expect("sso")
            .with_provider(Arc::new(FakeIdp), jwks, || CI_NOW)
    });
    let state = ApiState::new(
        cfg,
        store.clone(),
        None,
        projections.clone(),
        health,
        Arc::new(StaticPolicy),
    )
    .with_images(images())
    .with_github_oidc(oidc)
    .with_github(Some(kuben_api::github::GithubApp::webhook_only(HOOK_SECRET)))
    .with_dns(Arc::new(FakeDns::default()))
    .with_usage(usage.clone())
    .with_sso(sso)
    .with_keyring(Arc::new(kuben_platform::secrets::Keyring::from_keys([(
        1, [7; 32],
    )])));
    Some(TestApp {
        router: kuben_api::router(state),
        projections,
        org: org.id,
        store,
        usage,
    })
}

/// The digests `nginx:1.27` and `nginx:1.26` resolve to: no registry here.
const NGINX_127: &str = "sha256:1111111111111111111111111111111111111111111111111111111111111111";
const NGINX_126: &str = "sha256:2222222222222222222222222222222222222222222222222222222222222222";

/// The digest `ghcr.io/acme/private:1` resolves to, for bot's login only.
const PRIVATE: &str = "sha256:3333333333333333333333333333333333333333333333333333333333333333";

/// Public images resolve anonymously; `ghcr.io/acme/private:1` only with the
/// login `bot`.
#[derive(Debug)]
struct TestImages(FixedImages);

#[async_trait::async_trait]
impl ImageResolver for TestImages {
    async fn resolve_as(&self, image: &str, login: Option<&RegistryLogin>) -> Result<Resolved, ResolveError> {
        if image != "ghcr.io/acme/private:1" {
            return self.0.resolve_as(image, login).await;
        }
        match login {
            Some(l) if l.username == "bot" && l.password == "token" => Ok(Resolved {
                repository: "ghcr.io/acme/private".into(),
                digest: PRIVATE.parse().expect("digest"),
                given: image.to_owned(),
            }),
            _ => Err(ResolveError::Unauthorized(image.to_owned())),
        }
    }

    async fn list_tags(
        &self,
        repository: &str,
        login: Option<&RegistryLogin>,
    ) -> Result<Vec<String>, ResolveError> {
        self.0.list_tags(repository, login).await
    }
}

fn images() -> Arc<TestImages> {
    Arc::new(TestImages(FixedImages(BTreeMap::from([
        ("nginx:1.27".to_owned(), NGINX_127.parse().expect("digest")),
        ("nginx:1.26".to_owned(), NGINX_126.parse().expect("digest")),
    ]))))
}

/// Project `shop`, production environment `prod` and app `api` (`nginx:1.27`
/// on `api.example.com`, deployed once) in SQL; a project of another
/// organization; and the live status of shop's resources in the projections.
async fn seed(app: &TestApp) {
    seed_sql(app).await;
    seed_projections(app);
}

async fn seed_sql(app: &TestApp) {
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    let project = t.create_project("shop", "Shop").await.expect("project");
    let env = t
        .create_environment_typed(project, "prod", "prod", EnvironmentKind::Production, None)
        .await
        .expect("environment");
    let cluster = t.ensure_cluster("primary").await.expect("cluster");
    let placement = t
        .create_placement(project, env, cluster, "kb-shop-prod")
        .await
        .expect("placement");
    let application = t
        .create_application(project, "api", "api")
        .await
        .expect("application");
    let target = t
        .create_target(project, application, placement)
        .await
        .expect("target");
    let config = json!({
        "runtime": { "processes": { "web": { "port": 80 } } },
        "domains": [{ "host": "api.example.com", "tls": "auto" }],
    });
    let (config_revision, _) = t
        .create_config_revision(project, target, &config, "user:seed")
        .await
        .expect("revision")
        .expect("target");
    let release = PortableRelease {
        application,
        artifacts: BTreeMap::from([("web".to_owned(), NGINX_127.parse().expect("digest"))]),
        process_contract: json!({}),
        portable_config: json!({}),
        renderer_schema: 1,
        source: Some(json!({ "image_repository": "docker.io/library/nginx", "image": "nginx:1.27" })),
        created_by: "user:seed".into(),
    };
    let (release, _) = t.create_release(project, &release).await.expect("release");
    let lifecycle_uid = t
        .target_state(target)
        .await
        .expect("read")
        .expect("target")
        .lifecycle_uid;
    let deploy = StartDeployment {
        project,
        target,
        release,
        config_revision,
        render_plan: None,
        expected_generation: Generation(0),
        lifecycle_uid,
        reason: RunReason::Deploy,
        requested_by: "user:seed".into(),
        input_hash: b"seed".to_vec(),
    };
    let audit = NewAudit {
        actor_kind: "user".into(),
        action: "seed".into(),
        outcome: "accepted".into(),
        ..NewAudit::default()
    };
    t.start_deployment(&deploy, audit, None).await.expect("deploy");
    t.commit().await.expect("commit");

    let other = app.store.create_org("other", "Other").await.expect("org").id;
    let mut t = app.store.tenant(other).await.expect("tenant");
    t.create_project("secret-project", "Other tenant")
        .await
        .expect("project");
    t.commit().await.expect("commit");
}

/// The live status of shop's resources, as the informers would report it.
fn seed_projections(app: &TestApp) {
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
    let Some(app) = setup().await else { return };
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
    let Some(app) = setup().await else { return };
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
    let Some(app) = setup().await else { return };
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
    let Some(app) = setup().await else { return };
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
    let Some(app) = setup().await else { return };
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
    let Some(app) = setup().await else { return };
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
    let Some(app) = setup().await else { return };
    seed(&app).await;
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

    // SQL holds projects: creating one needs no cluster.
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
    assert_eq!(status, StatusCode::CREATED);

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
async fn environments_and_apps_read_from_sql() {
    let Some(app) = setup().await else { return };
    seed(&app).await;
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

    // Invalid input is rejected; a tag is resolved to a digest, and SQL needs
    // no cluster.
    let bad = r#"{"name":"web","image":"nginx latest"}"#;
    let (status, _) = send(
        &app.router,
        post("/api/v1/projects/shop/environments/prod/apps", &cookie, bad),
    )
    .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    let good = r#"{"name":"web","image":"nginx:1.27","port":80}"#;
    let (status, created) = send(
        &app.router,
        post("/api/v1/projects/shop/environments/prod/apps", &cookie, good),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{created}");
    assert_eq!(created["image"], "nginx:1.27");
    assert!(
        !created["ready"].as_bool().expect("ready"),
        "not materialized yet"
    );
    let clash = r#"{"name":"www","image":"nginx:1.27","port":80,"domains":["api.example.com"]}"#;
    let (status, _) = send(
        &app.router,
        post("/api/v1/projects/shop/environments/prod/apps", &cookie, clash),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT, "a domain another app uses");
    let unknown = r#"{"name":"cache","image":"redis:7"}"#;
    let (status, _) = send(
        &app.router,
        post("/api/v1/projects/shop/environments/prod/apps", &cookie, unknown),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "a tag its registry does not know"
    );
    let (status, _) = send(
        &app.router,
        post(
            "/api/v1/projects/shop/environments",
            &cookie,
            r#"{"name":"staging"}"#,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let (_, envs) = send(&app.router, get("/api/v1/projects/shop/environments", &cookie)).await;
    let staging = envs
        .as_array()
        .expect("environments")
        .iter()
        .find(|e| e["name"] == "staging")
        .expect("staging");
    assert_eq!(staging["namespace"], "kb-shop-staging");
    assert_eq!(staging["phase"], "Pending", "its namespace follows");
}

#[tokio::test]
async fn viewers_can_read_but_not_write() {
    let Some(app) = setup().await else { return };
    seed(&app).await;
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
    let Some(app) = setup().await else { return };
    let resp = app
        .router
        .oneshot(Request::get("/api/docs").body(Body::empty()).expect("req"))
        .await
        .expect("resp");
    assert_eq!(resp.status(), StatusCode::OK);
}

// ---------------------------------------------------------------------------
// Day-2 scenarios 1–5 and 8
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
    let Some(t) = setup_with(|c| {
        c.security.login_max_failures = 3;
        c.security.trust_forwarded_for = true;
    })
    .await
    else {
        return;
    };
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
    let Some(t) = setup().await else { return };
    seed(&t).await;
    let (_, alice, _) = sign_in(&t.router, "alice@example.com", "hunter22").await;
    let (_, bob, _) = sign_in(&t.router, "bob@example.com", "hunter22").await;
    let body = json!({ "name": "blog", "display_name": "Blog" });
    assert_eq!(
        status_of(&t.router, "POST", "/api/v1/projects", &bob, Some(body.clone())).await,
        StatusCode::FORBIDDEN
    );
    assert_eq!(
        status_of(&t.router, "POST", "/api/v1/projects", &alice, Some(body)).await,
        StatusCode::CREATED,
        "SQL holds projects"
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
        ("createProject", "success"),
        ("project.apply", "accepted"),
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
    let Some(t) = setup().await else { return };
    seed(&t).await;
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
    let Some(t) = setup().await else { return };
    seed(&t).await;
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

async fn releases(router: &Router, cookie: &str, base: &str) -> (StatusCode, serde_json::Value) {
    let (status, _, list) = call(
        router,
        "GET",
        &format!("{base}/releases"),
        Auth::Cookie(cookie),
        None,
        None,
    )
    .await;
    (status, list)
}

fn revisions(list: &serde_json::Value) -> Vec<i64> {
    list.as_array()
        .expect("releases")
        .iter()
        .filter_map(|r| r["revision"].as_i64())
        .collect()
}

#[tokio::test]
async fn scenario5_releases_are_newest_first_and_rollback_is_authorized() {
    let Some(t) = setup().await else { return };
    seed(&t).await;
    let base = "/api/v1/projects/shop/environments/prod/apps/api";
    let (_, alice, _) = sign_in(&t.router, "alice@example.com", "hunter22").await;
    let (_, bob, _) = sign_in(&t.router, "bob@example.com", "hunter22").await;
    assert_eq!(
        status_of(
            &t.router,
            "PATCH",
            base,
            &alice,
            Some(json!({ "image": "nginx:1.26" }))
        )
        .await,
        StatusCode::OK
    );

    let (status, list) = releases(&t.router, &bob, base).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(revisions(&list), vec![2, 1]);
    assert_eq!(list[0]["current"], true);
    assert_eq!(list[0]["image"], "nginx:1.26");
    assert_eq!(
        (list[0]["reason"].as_str(), list[1]["reason"].as_str()),
        (Some("deploy"), Some("create"))
    );

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
        StatusCode::OK,
        "a rollback is a new run: SQL needs no cluster"
    );
    let (_, list) = releases(&t.router, &alice, base).await;
    assert_eq!(revisions(&list), vec![3, 2, 1]);
    assert_eq!(list[0]["reason"], "rollback");
    assert_eq!(list[0]["image"], "nginx:1.27");
}

#[tokio::test]
async fn scenario8_template_catalogue() {
    let Some(t) = setup().await else { return };
    seed(&t).await;
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

// ---- first run: /setup ----

/// An empty store, as on a fresh install; `bind` decides whether the setup
/// token is required, `dir` is where the token file goes.
async fn empty_app(bind: &str, dir: &std::path::Path) -> Option<Router> {
    empty_app_with(bind, dir, |_| {}).await
}

async fn empty_app_with(
    bind: &str,
    dir: &std::path::Path,
    tweak: impl FnOnce(&mut Config),
) -> Option<Router> {
    let mut cfg = Config::default();
    cfg.security.cookie_secure = kuben_core::config::CookieSecure::Fixed(false);
    cfg.server.bind = bind.into();
    // Where the setup-token file goes; the store is the isolated PostgreSQL
    // schema below.
    cfg.server.state_dir = Some(dir.display().to_string());
    tweak(&mut cfg);
    let Some(store) = kuben_store::testing::pg_store().await else {
        kuben_store::testing::skip("setup");
        return None;
    };
    let health = Health::new();
    health.set_ready(true);
    Some(kuben_api::router(ApiState::new(
        cfg,
        store,
        None,
        Arc::new(Projections::new()),
        health,
        Arc::new(StaticPolicy),
    )))
}

fn scratch_dir(name: &str) -> std::path::PathBuf {
    let dir = std::env::temp_dir().join(format!("kuben-http-{name}-{}", std::process::id()));
    std::fs::create_dir_all(&dir).expect("dir");
    dir
}

const SETUP_BODY: &str =
    r#"{"org_name":"ACME","email":"Owner@Example.com","password":"a-long-first-password"}"#;

#[tokio::test]
async fn setup_creates_the_admin_and_signs_in() {
    if kuben_core::config::in_cluster() {
        return; // the Secret flow applies inside a pod
    }
    let dir = scratch_dir("setup");
    let Some(app) = empty_app("127.0.0.1:3000", &dir).await else {
        return;
    };

    let (status, body) = send(&app, get("/api/v1/setup", "")).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(
        body,
        json!({"needed": true, "token_required": false, "secure": true})
    );

    let resp = app
        .clone()
        .oneshot(post("/api/v1/setup", "", SETUP_BODY))
        .await
        .expect("response");
    assert_eq!(resp.status(), StatusCode::OK);
    let cookie = resp.headers()[header::SET_COOKIE]
        .to_str()
        .expect("cookie")
        .split(';')
        .next()
        .expect("pair")
        .to_owned();
    let (status, me) = send(&app, get("/api/v1/me", &cookie)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(me["email"], "owner@example.com", "signed in as the new admin");

    let (_, body) = send(&app, get("/api/v1/setup", "")).await;
    assert_eq!(body["needed"], false);
    let (status, _) = send(&app, post("/api/v1/setup", "", SETUP_BODY)).await;
    assert_eq!(status, StatusCode::NOT_FOUND, "only ever one first admin");
    std::fs::remove_dir_all(&dir).ok();
}

#[tokio::test]
async fn setup_on_a_public_address_needs_the_installer_token() {
    if kuben_core::config::in_cluster() {
        return;
    }
    let dir = scratch_dir("token");
    let mut cfg = Config::default();
    cfg.server.state_dir = Some(dir.display().to_string());
    let token = kuben_api::setup::issue_token(&cfg).expect("token");
    let right = SETUP_BODY.replace('}', &format!(r#","token":"{token}"}}"#));

    // Plain HTTP from another machine: no password travels, token or not.
    let Some(plain) = empty_app("0.0.0.0:3000", &dir).await else {
        return;
    };
    let (_, body) = send(&plain, get("/api/v1/setup", "")).await;
    assert_eq!(
        body,
        json!({"needed": true, "token_required": true, "secure": false})
    );
    let (status, problem) = send(&plain, post("/api/v1/setup", "", &right)).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert_eq!(problem["code"], "insecure_transport");
    assert!(
        problem["detail"].as_str().is_some_and(|d| d.contains("ssh -L")),
        "{problem}"
    );

    // Behind the TLS proxy: the token decides.
    let Some(app) = empty_app_with("0.0.0.0:3000", &dir, |c| c.security.trust_forwarded_for = true).await
    else {
        return;
    };
    let over_https = |mut req: axum::http::Request<axum::body::Body>| {
        req.headers_mut()
            .insert("x-forwarded-proto", axum::http::HeaderValue::from_static("https"));
        req
    };
    let (_, body) = send(&app, over_https(get("/api/v1/setup", ""))).await;
    assert_eq!(
        body,
        json!({"needed": true, "token_required": true, "secure": true})
    );
    let (status, _) = send(&app, over_https(post("/api/v1/setup", "", SETUP_BODY))).await;
    assert_eq!(status, StatusCode::FORBIDDEN, "no token at all");
    let wrong = SETUP_BODY.replace('}', r#","token":"nope"}"#);
    let (status, problem) = send(&app, over_https(post("/api/v1/setup", "", &wrong))).await;
    assert_eq!(status, StatusCode::FORBIDDEN, "wrong token");
    assert_eq!(problem["code"], "forbidden");

    let (status, _) = send(&app, over_https(post("/api/v1/setup", "", &right))).await;
    assert_eq!(status, StatusCode::OK);
    assert!(
        !kuben_api::setup::token_file(&cfg).exists(),
        "used tokens are removed"
    );
    std::fs::remove_dir_all(&dir).ok();
}

#[tokio::test]
async fn setup_rejects_weak_input() {
    if kuben_core::config::in_cluster() {
        return;
    }
    let dir = scratch_dir("validate");
    let Some(app) = empty_app("127.0.0.1:3000", &dir).await else {
        return;
    };
    for (body, what) in [
        (
            r#"{"org_name":"ACME","email":"nope","password":"a-long-first-password"}"#,
            "email",
        ),
        (
            r#"{"org_name":"ACME","email":"a@b.c","password":"short"}"#,
            "password",
        ),
        (
            r#"{"org_name":"  ","email":"a@b.c","password":"a-long-first-password"}"#,
            "org",
        ),
    ] {
        let (status, _) = send(&app, post("/api/v1/setup", "", body)).await;
        assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY, "{what}");
    }
    let (_, body) = send(&app, get("/api/v1/setup", "")).await;
    assert_eq!(body["needed"], true, "nothing was created");
    std::fs::remove_dir_all(&dir).ok();
}

// ---- deployments on the SQL model (ADR-032) ----

const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
const DEPLOYMENTS: &str = "/api/v1/projects/shop/environments/prod/apps/api/deployments";

/// Project `shop`, environment `prod` and app `api`, in SQL only: the
/// app's target.
async fn sql_app(app: &TestApp) -> kuben_core::ids::TargetId {
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    let project = t.create_project("shop", "Shop").await.expect("project");
    let env = t
        .create_environment(project, "prod", "Production", true)
        .await
        .expect("environment");
    let cluster = t.create_cluster("primary").await.expect("cluster");
    let placement = t
        .create_placement(project, env, cluster, "kb-shop-prod")
        .await
        .expect("placement");
    let application = t
        .create_application(project, "api", "API")
        .await
        .expect("application");
    let target = t
        .create_target(project, application, placement)
        .await
        .expect("target");
    t.commit().await.expect("commit");
    target
}

fn deploy(cookie: &str, expected: u64, key: Option<&str>) -> Request<Body> {
    let body = json!({
        "image": format!("ghcr.io/acme/api@{DIGEST}"),
        "config": { "runtime": { "processes": { "web": { "port": 8080 } } } },
        "expected_generation": expected,
    });
    let mut req = Request::post(DEPLOYMENTS)
        .header(header::COOKIE, cookie)
        .header(header::CONTENT_TYPE, "application/json")
        .header(CLIENT_HEADER, "test");
    if let Some(key) = key {
        req = req.header("idempotency-key", key);
    }
    req.body(Body::from(body.to_string())).expect("request")
}

#[tokio::test]
async fn a_deployment_is_accepted_once_and_can_be_polled() {
    let Some(app) = setup().await else { return };
    sql_app(&app).await;
    let cookie = login(&app.router, "alice@example.com").await;

    let resp = app
        .router
        .clone()
        .oneshot(deploy(&cookie, 0, Some("deploy-1")))
        .await
        .expect("response");
    assert_eq!(resp.status(), StatusCode::ACCEPTED);
    let location = resp.headers()[header::LOCATION]
        .to_str()
        .expect("location")
        .to_owned();
    let bytes = resp.into_body().collect().await.expect("body").to_bytes();
    let first: serde_json::Value = serde_json::from_slice(&bytes).expect("json");
    assert_eq!(first["generation"], 1);
    assert_eq!(first["phase"], "planned");
    assert!(location.ends_with(first["run"].as_str().expect("run")));

    let (status, polled) = send(&app.router, get(&location, &cookie)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(polled["run"], first["run"]);

    let (status, replay) = send(&app.router, deploy(&cookie, 0, Some("deploy-1"))).await;
    assert_eq!(status, StatusCode::ACCEPTED, "{replay}");
    assert_eq!(replay["run"], first["run"], "same key and request: the first run");

    let (status, _) = send(&app.router, deploy(&cookie, 1, Some("deploy-1"))).await;
    assert_eq!(status, StatusCode::CONFLICT, "the same key for another request");

    let (status, problem) = send(&app.router, deploy(&cookie, 0, None)).await;
    assert_eq!(status, StatusCode::CONFLICT, "a stale generation: {problem}");

    let (status, second) = send(&app.router, deploy(&cookie, 1, None)).await;
    assert_eq!(status, StatusCode::ACCEPTED, "{second}");
    assert_eq!(second["generation"], 2);
    let (_, first_now) = send(&app.router, get(&location, &cookie)).await;
    assert_eq!(first_now["phase"], "superseded", "the newer run owns the app");

    // M2.16: the runs, newest first, each with the phases it went through.
    let (status, list) = send(&app.router, get(&format!("{DEPLOYMENTS}?limit=5"), &cookie)).await;
    assert_eq!(status, StatusCode::OK, "{list}");
    let runs = list.as_array().expect("runs");
    assert_eq!(runs.len(), 2);
    assert_eq!(
        (runs[0]["generation"].clone(), runs[1]["run"].clone()),
        (json!(2), first["run"].clone())
    );
    assert_eq!(runs[0]["requested_by"], "alice@example.com");
    let phases: Vec<&str> = runs[1]["timeline"]
        .as_array()
        .expect("timeline")
        .iter()
        .filter_map(|s| s["phase"].as_str())
        .collect();
    assert_eq!(phases.first(), Some(&"planned"), "{phases:?}");
    assert_eq!(phases.last(), Some(&"superseded"), "{phases:?}");
}

/// A deployment whose answer was lost (plan §18.1 crash/ACK replay, I17):
/// the retry with the same key gets the run the first request made, and
/// only one run exists.
#[tokio::test]
async fn a_lost_answer_is_given_again_without_a_second_run() {
    let Some(app) = setup().await else { return };
    let target = sql_app(&app).await;
    let cookie = login(&app.router, "alice@example.com").await;

    // The request is accepted, but its answer never reaches the client.
    let lost = app
        .router
        .clone()
        .oneshot(deploy(&cookie, 0, Some("lost-1")))
        .await
        .expect("response");
    assert_eq!(lost.status(), StatusCode::ACCEPTED);
    drop(lost);

    let (status, retry) = send(&app.router, deploy(&cookie, 0, Some("lost-1"))).await;
    assert_eq!(status, StatusCode::ACCEPTED, "{retry}");
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    let runs = t.runs(target, 10).await.expect("runs");
    assert_eq!(runs.len(), 1, "one intent, one run: {runs:?}");
    assert_eq!(retry["run"], runs[0].run.to_string(), "the first request's run");
    assert_eq!(retry["generation"], 1);
}

#[tokio::test]
async fn deployments_need_deploy_rights_a_pinned_image_and_an_app_in_sql() {
    let Some(app) = setup().await else { return };
    sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;

    let (status, _) = send(&app.router, deploy(&bob, 0, None)).await;
    assert_eq!(status, StatusCode::FORBIDDEN, "a viewer cannot deploy");

    let by_tag =
        json!({ "image": "ghcr.io/acme/api:1.2", "config": {}, "expected_generation": 0 }).to_string();
    let (status, _) = send(&app.router, post(DEPLOYMENTS, &alice, &by_tag)).await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY, "a tag is not a digest");

    let (status, _) = send(
        &app.router,
        post(
            "/api/v1/projects/shop/environments/prod/apps/nope/deployments",
            &alice,
            &by_tag,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);

    let no_config =
        json!({ "image": format!("ghcr.io/acme/api@{DIGEST}"), "expected_generation": 0 }).to_string();
    let (status, _) = send(&app.router, post(DEPLOYMENTS, &alice, &no_config)).await;
    assert_eq!(
        status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "the first deploy brings its configuration"
    );
}

/// A user with `role` in the organization, signed in: their cookie and id.
async fn member(app: &TestApp, email: &str, role: Role) -> (String, String) {
    let hasher = kuben_api::auth::password::Hasher::insecure_for_tests();
    let user = app
        .store
        .create_user(email, None, Some(&hasher.hash("hunter22").expect("hash")))
        .await
        .expect("user");
    app.store.add_membership(app.org, user.id).await.expect("member");
    app.store
        .bind_org_role(app.org, user.id, role)
        .await
        .expect("bind");
    (login(&app.router, email).await, user.id.to_string())
}

const POLICY: &str = "/api/v1/projects/shop/environments/prod/policy";

fn policy(approvals: u8) -> serde_json::Value {
    json!({ "requiredApprovals": approvals, "deployRole": "developer", "approveRole": "admin" })
}

/// Shop's production app with a policy of one approval set by carol (an
/// admin): the router and the cookies of alice (owner), bob (viewer) and
/// carol.
async fn protected(app: &TestApp) -> (String, String, String) {
    sql_app(app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;
    let (carol, _) = member(app, "carol@example.com", Role::Admin).await;
    let (status, _, open) = call(&app.router, "GET", POLICY, Auth::Cookie(&alice), None, None).await;
    assert_eq!((status, open["revision"].clone()), (StatusCode::OK, json!(0)));
    assert_eq!(
        status_of(&app.router, "PUT", POLICY, &bob, Some(policy(1))).await,
        StatusCode::FORBIDDEN,
        "viewers cannot change protection"
    );
    let (status, _, set) = call(
        &app.router,
        "PUT",
        POLICY,
        Auth::Cookie(&carol),
        Some(policy(1)),
        None,
    )
    .await;
    assert_eq!(
        (status, set["revision"].clone()),
        (StatusCode::OK, json!(1)),
        "{set}"
    );
    (alice, bob, carol)
}

/// M4.1: policies are validated and weakening protection takes an owner.
#[tokio::test]
async fn m4_weakening_protection_takes_an_owner() {
    let Some(app) = setup().await else { return };
    let (alice, _, carol) = protected(&app).await;
    assert_eq!(
        status_of(&app.router, "PUT", POLICY, &carol, Some(policy(9))).await,
        StatusCode::UNPROCESSABLE_ENTITY
    );
    assert_eq!(
        status_of(&app.router, "PUT", POLICY, &carol, Some(policy(2))).await,
        StatusCode::OK,
        "stricter is an admin's call"
    );
    assert_eq!(
        status_of(&app.router, "PUT", POLICY, &carol, Some(policy(0))).await,
        StatusCode::FORBIDDEN,
        "weaker is an owner's"
    );
    let (status, _, open) = call(
        &app.router,
        "PUT",
        POLICY,
        Auth::Cookie(&alice),
        Some(policy(0)),
        None,
    )
    .await;
    assert_eq!(
        (status, open["revision"].clone()),
        (StatusCode::OK, json!(3)),
        "{open}"
    );
    let (status, run) = send(&app.router, deploy(&alice, 0, None)).await;
    assert_eq!(status, StatusCode::ACCEPTED, "{run}");
    assert_eq!(
        (run["phase"].clone(), run["approvals_required"].clone()),
        (json!("planned"), json!(0))
    );
}

/// M4.1: a protected deployment waits for someone else with the approve
/// role, who confirms the plan they were shown.
#[tokio::test]
async fn m4_protected_deploys_wait_for_another_approver() {
    let Some(app) = setup().await else { return };
    let (alice, bob, carol) = protected(&app).await;
    let (status, run) = send(&app.router, deploy(&alice, 0, None)).await;
    assert_eq!(status, StatusCode::ACCEPTED, "{run}");
    assert_eq!(run["phase"], "awaitingApproval");
    assert_eq!(run["approvals_required"], 1);
    let hash = run["plan_hash"].as_str().expect("plan hash").to_owned();
    let base = format!("{DEPLOYMENTS}/{}", run["run"].as_str().expect("run"));
    let decide = |hash: &str| Some(json!({ "planHash": hash, "comment": "ship it" }));
    let approve = format!("{base}/approve");

    for (who, why) in [
        (&alice, "the requester never approves"),
        (&bob, "viewers cannot approve"),
    ] {
        assert_eq!(
            status_of(&app.router, "POST", &approve, who, decide(&hash)).await,
            StatusCode::FORBIDDEN,
            "{why}"
        );
    }
    let (_, _, seen) = call(
        &app.router,
        "GET",
        &format!("{base}/approval"),
        Auth::Cookie(&carol),
        None,
        None,
    )
    .await;
    assert_eq!(
        (seen["canDecide"].clone(), seen["requestedBy"].clone()),
        (json!(true), json!("alice@example.com"))
    );
    assert_eq!(
        status_of(&app.router, "POST", &approve, &carol, decide("00")).await,
        StatusCode::CONFLICT,
        "a plan other than the one shown"
    );
    let (status, _, approved) = call(
        &app.router,
        "POST",
        &approve,
        Auth::Cookie(&carol),
        decide(&hash),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{approved}");
    assert_eq!(approved["phase"], "pendingDelivery");
    assert_eq!(approved["decisions"][0]["approver"], "carol@example.com");
    assert_eq!(
        status_of(
            &app.router,
            "POST",
            &format!("{base}/reject"),
            &carol,
            decide(&hash)
        )
        .await,
        StatusCode::CONFLICT,
        "decided already"
    );
}

/// M4.1: roles on one project, granted without escalation; removing a member
/// from the organization takes them away.
#[tokio::test]
async fn m4_project_roles_are_granted_without_escalation() {
    let Some(app) = setup().await else { return };
    sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let (carol, carol_id) = member(&app, "carol@example.com", Role::Admin).await;
    let (dave, dave_id) = member(&app, "dave@example.com", Role::Viewer).await;
    let members = "/api/v1/projects/shop/members";
    let dave_path = format!("{members}/{dave_id}");

    let (status, _) = send(&app.router, deploy(&dave, 0, None)).await;
    assert_eq!(status, StatusCode::FORBIDDEN, "a viewer cannot deploy");

    assert_eq!(
        status_of(
            &app.router,
            "PUT",
            &dave_path,
            &carol,
            Some(json!({ "role": "owner" }))
        )
        .await,
        StatusCode::FORBIDDEN,
        "an admin cannot grant owner"
    );
    assert_eq!(
        status_of(
            &app.router,
            "PUT",
            &format!("{members}/{carol_id}"),
            &carol,
            Some(json!({ "role": "viewer" }))
        )
        .await,
        StatusCode::CONFLICT,
        "nobody changes their own role"
    );
    let (status, _, granted) = call(
        &app.router,
        "PUT",
        &dave_path,
        Auth::Cookie(&carol),
        Some(json!({ "role": "developer" })),
        None,
    )
    .await;
    assert_eq!(
        (status, granted["role"].clone()),
        (StatusCode::OK, json!("developer")),
        "{granted}"
    );
    let (_, _, listed) = call(&app.router, "GET", members, Auth::Cookie(&dave), None, None).await;
    assert_eq!(listed.as_array().map(Vec::len), Some(1), "{listed}");

    let (status, run) = send(&app.router, deploy(&dave, 0, None)).await;
    assert_eq!(status, StatusCode::ACCEPTED, "a project developer deploys: {run}");

    assert_eq!(
        status_of(
            &app.router,
            "PUT",
            &format!("{members}/{}", uuid::Uuid::now_v7()),
            &alice,
            Some(json!({ "role": "viewer" }))
        )
        .await,
        StatusCode::NOT_FOUND,
        "not a member of the organization"
    );
    assert_eq!(
        status_of(
            &app.router,
            "DELETE",
            &format!("/api/v1/members/{dave_id}"),
            &alice,
            None
        )
        .await,
        StatusCode::NO_CONTENT
    );
    let (status, _) = send(&app.router, deploy(&dave, 1, None)).await;
    assert!(
        matches!(status, StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN),
        "removed members lose every role: {status}"
    );
    assert_eq!(
        status_of(&app.router, "DELETE", &dave_path, &alice, None).await,
        StatusCode::NOT_FOUND,
        "the project role went with the membership"
    );
}

/// Signed GitHub Actions tokens and their issuer's keys (M4.2).
const CI_FIXTURE: &str = include_str!("../src/testdata/github-oidc.json");
/// Between the fixture tokens' `iat` and `exp`.
const CI_NOW: i64 = 1_800_000_100;
const CI_EXCHANGE: &str = "/api/v1/ci/github/token";

fn ci_fixture() -> (Jwks, serde_json::Value) {
    let all: serde_json::Value = serde_json::from_str(CI_FIXTURE).expect("fixture");
    (
        serde_json::from_value(all["jwks"].clone()).expect("jwks"),
        all["tokens"].clone(),
    )
}

async fn exchange(app: &TestApp, token: &str, policy: &str) -> (StatusCode, serde_json::Value) {
    let (status, _, body) = call(
        &app.router,
        "POST",
        CI_EXCHANGE,
        Auth::Bearer(token),
        Some(json!({ "policy": policy })),
        None,
    )
    .await;
    (status, body)
}

/// The app on CI trust with shop's policy for `acme/shop`'s main branch,
/// made by alice: the app, alice's cookie and the policy id.
async fn trusted() -> Option<(TestApp, String, String)> {
    let app = setup_with(|cfg| {
        cfg.ci.github_actions = true;
        cfg.ci.github_oidc_audience = Some("https://kuben.example.com".into());
    })
    .await?;
    sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;
    assert_eq!(
        status_of(
            &app.router,
            "POST",
            "/api/v1/ci/trust-policies",
            &bob,
            Some(ci_policy())
        )
        .await,
        StatusCode::FORBIDDEN,
        "viewers cannot trust CI"
    );
    let (status, _, made) = call(
        &app.router,
        "POST",
        "/api/v1/ci/trust-policies",
        Auth::Cookie(&alice),
        Some(ci_policy()),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{made}");
    let id = made["id"].as_str().expect("id").to_owned();
    Some((app, alice, id))
}

fn ci_policy() -> serde_json::Value {
    json!({
        "name": "shop-deploy", "project": "shop", "repository": "acme/shop",
        "repositoryId": 123_456, "repositoryOwnerId": 42, "refs": ["refs/heads/main"],
    })
}

fn ci_token(name: &str) -> String {
    ci_fixture().1[name].as_str().expect("token").to_owned()
}

/// M4.2 (S04): forged, foreign and pull-request tokens get nothing.
#[tokio::test]
async fn m4_untrusted_ci_tokens_get_nothing() {
    let Some((app, _, id)) = trusted().await else {
        return;
    };
    for name in [
        "tampered",
        "wrong_aud",
        "wrong_iss",
        "unknown_kid",
        "pull_request",
    ] {
        let (status, body) = exchange(&app, &ci_token(name), &id).await;
        assert_eq!(status, StatusCode::UNAUTHORIZED, "{name}: {body}");
    }
    let (status, _) = exchange(&app, &ci_token("valid"), &uuid::Uuid::now_v7().to_string()).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED, "an unknown policy");
    let (status, _, _) = call(
        &app.router,
        "POST",
        CI_EXCHANGE,
        Auth::Anonymous,
        Some(json!({ "policy": id })),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED, "no provider token");
}

/// M4.2 (S04): a trusted workflow gets a short-lived, project-scoped token
/// once per provider token; revoking the policy ends it.
#[tokio::test]
async fn m4_trusted_ci_gets_a_scoped_token_once() {
    let Some((app, alice, id)) = trusted().await else {
        return;
    };
    let (status, issued) = exchange(&app, &ci_token("valid"), &id).await;
    assert_eq!(status, StatusCode::CREATED, "{issued}");
    assert_eq!(issued["role"], "developer");
    let ci = issued["token"].as_str().expect("token").to_owned();
    let (status, replay) = exchange(&app, &ci_token("valid"), &id).await;
    assert_eq!(
        status,
        StatusCode::UNAUTHORIZED,
        "a provider token works once: {replay}"
    );

    let as_ci = |method: &'static str, path: &'static str, body: Option<serde_json::Value>| {
        let (ci, router) = (ci.clone(), app.router.clone());
        async move { call(&router, method, path, Auth::Bearer(&ci), body, None).await.0 }
    };
    assert_eq!(
        as_ci("GET", "/api/v1/projects/shop/environments", None).await,
        StatusCode::OK
    );
    let deploy_body = json!({
        "image": format!("ghcr.io/acme/api@{DIGEST}"),
        "config": { "runtime": { "processes": { "web": { "port": 8080 } } } },
        "expected_generation": 0,
    });
    assert_eq!(
        as_ci("POST", DEPLOYMENTS, Some(deploy_body)).await,
        StatusCode::ACCEPTED
    );
    assert_eq!(
        as_ci("GET", "/api/v1/members", None).await,
        StatusCode::FORBIDDEN,
        "project-scoped"
    );
    assert_eq!(
        as_ci("POST", "/api/v1/ci/trust-policies", Some(ci_policy())).await,
        StatusCode::FORBIDDEN,
        "tokens cannot mint trust"
    );

    assert_eq!(
        status_of(
            &app.router,
            "DELETE",
            &format!("/api/v1/ci/trust-policies/{id}"),
            &alice,
            None
        )
        .await,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        as_ci("GET", "/api/v1/projects/shop/environments", None).await,
        StatusCode::UNAUTHORIZED,
        "revoked with its policy"
    );
    let (_, _, listed) = call(
        &app.router,
        "GET",
        "/api/v1/ci/trust-policies",
        Auth::Cookie(&alice),
        None,
        None,
    )
    .await;
    assert!(listed[0]["revokedAt"].is_i64(), "{listed}");
}

/// Signed ID tokens of a test identity provider (M4.3).
const SSO_FIXTURE: &str = include_str!("../src/testdata/sso-oidc.json");

fn sso_fixture() -> serde_json::Value {
    serde_json::from_str(SSO_FIXTURE).expect("fixture")
}

/// An identity provider answering from memory: the token endpoint hands out
/// the fixture ID token named by the code.
struct FakeIdp;

#[async_trait::async_trait]
impl kuben_api::oidc::HttpGet for FakeIdp {
    async fn get(&self, _url: &str, _limit: usize) -> Result<bytes::Bytes, String> {
        Ok(json!({
            "issuer": "https://idp.example.com",
            "authorization_endpoint": "https://idp.example.com/authorize",
            "token_endpoint": "https://idp.example.com/token",
            "jwks_uri": "https://idp.example.com/jwks",
        })
        .to_string()
        .into())
    }
}

#[async_trait::async_trait]
impl kuben_api::sso::IdentityProvider for FakeIdp {
    async fn post_form(
        &self,
        _url: &str,
        form: &str,
        _basic: &str,
    ) -> Result<(StatusCode, bytes::Bytes), String> {
        let code = form
            .split('&')
            .find_map(|kv| kv.strip_prefix("code="))
            .unwrap_or_default();
        Ok(match sso_fixture()["tokens"][code].as_str() {
            Some(token) => (StatusCode::OK, json!({ "id_token": token }).to_string().into()),
            None => (StatusCode::BAD_REQUEST, "{}".into()),
        })
    }
}

async fn sso_app() -> Option<TestApp> {
    setup_with(|cfg| {
        cfg.server.public_url = Some("https://kuben.example.com".into());
        cfg.sso.enabled = true;
        cfg.sso.issuer = Some("https://idp.example.com".into());
        cfg.sso.client_id = Some("kuben-console".into());
        cfg.sso.client_secret = Some("s3cret".into());
        cfg.sso.groups.insert("platform-admins".into(), "admin".into());
        cfg.sso.groups.insert("devs".into(), "developer".into());
        cfg.sso.allowed_domains = vec!["example.com".into()];
    })
    .await
}

/// A sign-in of this browser (state `state`) expecting `nonce`.
async fn pending_sso(app: &TestApp, state: &str, nonce: &str) {
    app.store
        .begin_sso(
            &kuben_api::auth::session::sha256(state.as_bytes()),
            &kuben_store::repo::PendingSso {
                nonce: nonce.into(),
                verifier: "v".repeat(43),
                return_to: "/projects".into(),
            },
            kuben_core::time::now_ms() + 600_000,
        )
        .await
        .expect("begin");
}

/// The callback of a sign-in with `code`, from a browser holding `cookie`:
/// where it redirects and the cookies it sets.
async fn sso_callback(app: &TestApp, code: &str, state: &str, cookie: &str) -> (String, Vec<String>) {
    let path = format!("/api/v1/auth/sso/callback?code={code}&state={state}");
    let (status, headers, _) = call(&app.router, "GET", &path, Auth::Cookie(cookie), None, None).await;
    assert_eq!(status, StatusCode::SEE_OTHER);
    let to = headers[header::LOCATION].to_str().expect("location").to_owned();
    (to, set_cookies(&headers))
}

fn set_cookies(headers: &axum::http::HeaderMap) -> Vec<String> {
    headers
        .get_all(header::SET_COOKIE)
        .iter()
        .filter_map(|v| v.to_str().ok())
        .filter_map(|v| v.split(';').next())
        .map(str::to_owned)
        .collect()
}

/// M4.3: the sign-in page learns about SSO, and starting it redirects to
/// the provider with a state bound to this browser.
#[tokio::test]
async fn m4_sso_start_binds_the_state_to_the_browser() {
    let Some(app) = sso_app().await else { return };
    let (status, _, info) = call(
        &app.router,
        "GET",
        "/api/v1/auth/sso",
        Auth::Anonymous,
        None,
        None,
    )
    .await;
    assert_eq!(
        (status, info["enabled"].clone()),
        (StatusCode::OK, json!(true)),
        "{info}"
    );
    let (status, headers, _) = call(
        &app.router,
        "GET",
        "/api/v1/auth/sso/start?returnTo=//evil.example",
        Auth::Anonymous,
        None,
        None,
    )
    .await;
    assert_eq!(status, StatusCode::SEE_OTHER);
    let location = headers[header::LOCATION].to_str().expect("location");
    assert!(
        location.starts_with("https://idp.example.com/authorize?response_type=code"),
        "{location}"
    );
    assert!(location.contains("code_challenge_method=S256"));
    let state = location
        .split('&')
        .find_map(|kv| kv.strip_prefix("state="))
        .expect("state");
    let cookie = set_cookies(&headers)
        .into_iter()
        .find(|c| c.starts_with("kuben_sso="))
        .expect("state cookie");
    assert_eq!(cookie, format!("kuben_sso={state}"));
}

/// M4.3 (S04): the provider's groups decide the role, and a sign-in works
/// once.
#[tokio::test]
async fn m4_sso_signs_mapped_people_in_once() {
    let Some(app) = sso_app().await else { return };
    let state = "s".repeat(43);
    let browser = format!("kuben_sso={state}");
    pending_sso(&app, &state, "nonce-1").await;
    let (to, cookies) = sso_callback(&app, "valid", &state, &browser).await;
    assert_eq!(to, "/projects");
    let session = cookies
        .into_iter()
        .find(|c| !c.starts_with("kuben_sso="))
        .expect("session cookie");
    let (status, _, me) = call(
        &app.router,
        "GET",
        "/api/v1/me",
        Auth::Cookie(&session),
        None,
        None,
    )
    .await;
    assert_eq!(
        (status, me["email"].clone()),
        (StatusCode::OK, json!("carol@example.com")),
        "{me}"
    );
    let (_, _, members) = call(
        &app.router,
        "GET",
        "/api/v1/members",
        Auth::Cookie(&session),
        None,
        None,
    )
    .await;
    let carol = members
        .as_array()
        .expect("members")
        .iter()
        .find(|m| m["email"] == "carol@example.com")
        .cloned()
        .expect("carol");
    assert_eq!(carol["role"], "admin", "mapped from platform-admins");
    let (to, _) = sso_callback(&app, "valid", &state, &browser).await;
    assert_eq!(to, "/login?error=sso", "a state works once");
}

/// M4.3 (S04): another browser's state, another sign-in's nonce and people
/// the policy refuses get nothing.
#[tokio::test]
async fn m4_sso_refuses_everything_else() {
    let Some(app) = sso_app().await else { return };
    let fresh = |n: usize| format!("{n:0>43}");
    pending_sso(&app, &fresh(0), "nonce-1").await;
    let (to, _) = sso_callback(&app, "valid", &fresh(0), "kuben_sso=other").await;
    assert_eq!(to, "/login?error=sso", "another browser's cookie");
    pending_sso(&app, &fresh(1), "another-nonce").await;
    let (to, _) = sso_callback(&app, "valid", &fresh(1), &format!("kuben_sso={}", fresh(1))).await;
    assert_eq!(to, "/login?error=sso", "a token of another sign-in");
    for (n, code) in ["no_role", "outsider", "unverified", "wrong_aud", "nope"]
        .into_iter()
        .enumerate()
    {
        let state = fresh(n + 2);
        pending_sso(&app, &state, "nonce-1").await;
        let (to, cookies) = sso_callback(&app, code, &state, &format!("kuben_sso={state}")).await;
        assert_eq!(to, "/login?error=sso", "{code}");
        assert!(
            cookies.iter().all(|c| c.starts_with("kuben_sso=")),
            "no session for {code}"
        );
    }
}

const SECRETS: &str = "/api/v1/projects/shop/environments/prod/secrets";

/// A deploy of shop's app reading `DATABASE_URL` from secret `db`.
async fn deploy_with_secret(app: &TestApp, cookie: &str, expected: u64) -> (StatusCode, serde_json::Value) {
    let body = json!({
        "image": format!("ghcr.io/acme/api@{DIGEST}"),
        "config": {
            "runtime": { "processes": { "web": { "port": 8080 } } },
            "env": [{ "name": "DATABASE_URL", "fromSecret": { "name": "db", "key": "url" } }],
        },
        "expected_generation": expected,
    });
    let (status, _, run) = call(
        &app.router,
        "POST",
        DEPLOYMENTS,
        Auth::Cookie(cookie),
        Some(body),
        None,
    )
    .await;
    (status, run)
}

async fn put_secret(app: &TestApp, cookie: &str, url: &str) -> (StatusCode, serde_json::Value) {
    let body = json!({ "data": { "url": url } });
    let (status, _, secret) = call(
        &app.router,
        "PUT",
        &format!("{SECRETS}/db"),
        Auth::Cookie(cookie),
        Some(body),
        None,
    )
    .await;
    (status, secret)
}

async fn bound_revision(app: &TestApp, run: &serde_json::Value) -> Vec<u64> {
    let run: uuid::Uuid = run["run"].as_str().expect("run").parse().expect("uuid");
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    t.run_secret_bindings(kuben_core::ids::DeploymentRunId::from_uuid(run))
        .await
        .expect("bindings")
        .into_iter()
        .map(|b| b.revision)
        .collect()
}

/// M4.4: a secret is a series of encrypted revisions; a new value is rolled
/// out to the apps that reference it, and a revoked one is never deployed.
#[tokio::test]
async fn m4_secret_values_are_revisions_rolled_out_to_their_apps() {
    let Some(app) = setup().await else { return };
    sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;

    let (status, first) = put_secret(&app, &alice, "postgres://one").await;
    assert_eq!(status, StatusCode::OK, "{first}");
    assert_eq!(
        (first["revision"].clone(), first["storage"].clone()),
        (json!(1), json!("encrypted"))
    );
    assert!(first.get("rollouts").is_none(), "no app uses it yet");
    assert_eq!(put_secret(&app, &bob, "x").await.0, StatusCode::FORBIDDEN);
    let (_, _, listed) = call(&app.router, "GET", SECRETS, Auth::Cookie(&alice), None, None).await;
    assert_eq!(listed[0]["keys"], json!(["url"]));
    assert!(
        !listed.to_string().contains("postgres://"),
        "values are never returned"
    );

    let (status, run) = deploy_with_secret(&app, &alice, 0).await;
    assert_eq!(status, StatusCode::ACCEPTED, "{run}");
    assert_eq!(bound_revision(&app, &run).await, [1]);

    let (status, second) = put_secret(&app, &alice, "postgres://two").await;
    assert_eq!(status, StatusCode::OK, "{second}");
    assert_eq!(second["revision"], 2);
    let rollout = &second["rollouts"][0];
    assert_eq!(
        (rollout["app"].clone(), rollout["skipped"].clone()),
        (json!("api"), json!(null))
    );
    let rotation = json!({ "run": rollout["run"] });
    assert_eq!(bound_revision(&app, &rotation).await, [2]);

    let revisions = format!("{SECRETS}/db/revisions");
    let (_, _, history) = call(&app.router, "GET", &revisions, Auth::Cookie(&alice), None, None).await;
    let seen: Vec<_> = history
        .as_array()
        .expect("revisions")
        .iter()
        .map(|r| (r["revision"].clone(), r["current"].clone()))
        .collect();
    assert_eq!(seen, [(json!(2), json!(true)), (json!(1), json!(false))]);
    assert_eq!(
        status_of(&app.router, "DELETE", &format!("{SECRETS}/db"), &alice, None).await,
        StatusCode::CONFLICT,
        "the app references it"
    );

    let revoke = |n: u64| format!("{revisions}/{n}/revoke");
    for (n, expected) in [
        (2, StatusCode::NO_CONTENT),
        (2, StatusCode::CONFLICT),
        (9, StatusCode::NOT_FOUND),
    ] {
        assert_eq!(
            status_of(&app.router, "POST", &revoke(n), &alice, None).await,
            expected,
            "revision {n}"
        );
    }
    let (status, refused) = deploy_with_secret(&app, &alice, 2).await;
    assert_eq!(status, StatusCode::CONFLICT, "{refused}");
    let (status, third) = put_secret(&app, &alice, "postgres://three").await;
    assert_eq!((status, third["revoked"].clone()), (StatusCode::OK, json!(false)));
}

/// M4.4: a production rotation waits for its approval like any change.
#[tokio::test]
async fn m4_production_rotations_wait_for_approval() {
    let Some(app) = setup().await else { return };
    let (alice, bob, _carol) = protected(&app).await;
    put_secret(&app, &alice, "postgres://one").await;
    let (status, run) = deploy_with_secret(&app, &alice, 0).await;
    assert_eq!(
        (status, run["phase"].clone()),
        (StatusCode::ACCEPTED, json!("awaitingApproval"))
    );
    let (status, secret) = put_secret(&app, &alice, "postgres://two").await;
    assert_eq!(status, StatusCode::OK, "{secret}");
    assert_eq!(secret["rollouts"][0]["approvals_required"], 1);
    let body = json!({ "data": { "url": "x" }, "rollout": false });
    assert_eq!(
        status_of(&app.router, "PUT", &format!("{SECRETS}/db"), &bob, Some(body)).await,
        StatusCode::FORBIDDEN
    );
}

const REGISTRIES: &str = "/api/v1/projects/shop/environments/prod/registries";

/// Set shop's login for ghcr.io as alice, after the refusals it gets on the
/// way; its path.
async fn set_ghcr_login(app: &TestApp, alice: &str, bob: &str) -> String {
    let ghcr = format!("{REGISTRIES}/ghcr");
    let login_body = |registry: &str| json!({ "registry": registry, "username": "bot", "password": "token" });
    assert_eq!(
        status_of(&app.router, "PUT", &ghcr, bob, Some(login_body("ghcr.io"))).await,
        StatusCode::FORBIDDEN
    );
    assert_eq!(
        status_of(&app.router, "PUT", &ghcr, alice, Some(login_body("ghcr.io/acme"))).await,
        StatusCode::UNPROCESSABLE_ENTITY
    );
    let (status, _, saved) = call(
        &app.router,
        "PUT",
        &ghcr,
        Auth::Cookie(alice),
        Some(login_body("GHCR.io")),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{saved}");
    assert_eq!(
        (saved["registry"].clone(), saved["revision"].clone()),
        (json!("ghcr.io"), json!(1))
    );
    assert_eq!(
        status_of(
            &app.router,
            "PUT",
            &format!("{REGISTRIES}/other"),
            alice,
            Some(login_body("ghcr.io"))
        )
        .await,
        StatusCode::CONFLICT,
        "one login per registry"
    );
    let secret = json!({ "data": { "url": "x" } });
    assert_eq!(
        status_of(
            &app.router,
            "PUT",
            &format!("{SECRETS}/ghcr"),
            alice,
            Some(secret)
        )
        .await,
        StatusCode::CONFLICT,
        "the name is a login"
    );
    let (_, _, listed) = call(&app.router, "GET", REGISTRIES, Auth::Cookie(alice), None, None).await;
    assert_eq!(listed.as_array().map(Vec::len), Some(1));
    assert!(
        !listed.to_string().contains("token"),
        "passwords are never returned"
    );
    let (_, _, secrets) = call(&app.router, "GET", SECRETS, Auth::Cookie(alice), None, None).await;
    assert_eq!(secrets, json!([]), "logins are not app secrets");
    ghcr
}

/// M4.4: a registry login resolves private tags and is bound to the runs of
/// images from its registry; its password never comes back.
#[tokio::test]
async fn m4_registry_logins_pull_private_images() {
    let Some(app) = setup().await else { return };
    sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;
    let apps = "/api/v1/projects/shop/environments/prod/apps";
    let private = json!({ "name": "private", "image": "ghcr.io/acme/private:1", "port": 80 });
    assert_eq!(
        status_of(&app.router, "POST", apps, &alice, Some(private.clone())).await,
        StatusCode::UNPROCESSABLE_ENTITY,
        "no login yet"
    );

    let ghcr = set_ghcr_login(&app, &alice, &bob).await;

    let (status, _, created) = call(
        &app.router,
        "POST",
        apps,
        Auth::Cookie(&alice),
        Some(private),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{created}");
    let (_, _, runs) = call(
        &app.router,
        "GET",
        &format!("{apps}/private/deployments"),
        Auth::Cookie(&alice),
        None,
        None,
    )
    .await;
    let run = &runs.as_array().expect("runs")[0];
    let bindings = {
        let id: uuid::Uuid = run["run"].as_str().expect("run").parse().expect("uuid");
        let mut t = app.store.tenant(app.org).await.expect("tenant");
        t.run_secret_bindings(kuben_core::ids::DeploymentRunId::from_uuid(id))
            .await
            .expect("bindings")
    };
    assert_eq!(bindings.len(), 1);
    assert_eq!(bindings[0].registry.as_deref(), Some("ghcr.io"));

    assert_eq!(
        status_of(&app.router, "DELETE", &ghcr, &alice, None).await,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        status_of(&app.router, "DELETE", &ghcr, &alice, None).await,
        StatusCode::NOT_FOUND
    );
}

/// M4.5: an app's peak requests must fit its environment's quota, and the
/// organization's apps and environments the installation's quota.
#[tokio::test]
async fn m4_quotas_bound_what_apps_may_request() {
    let Some(app) = setup_with(|cfg| {
        cfg.quota.org_apps = Some(2);
        cfg.quota.org_environments = Some(2);
    })
    .await
    else {
        return;
    };
    let alice = login(&app.router, "alice@example.com").await;
    let created = |status: StatusCode| status == StatusCode::CREATED;
    let project = json!({ "name": "shop", "display_name": "Shop" });
    assert!(created(
        status_of(&app.router, "POST", "/api/v1/projects", &alice, Some(project)).await
    ));
    let environments = "/api/v1/projects/shop/environments";
    for (body, expected) in [
        (
            json!({ "name": "tiny", "quota": { "cpu": "150m" } }),
            StatusCode::CREATED,
        ),
        (
            json!({ "name": "big", "quota": { "cpu": "1e3" } }),
            StatusCode::UNPROCESSABLE_ENTITY,
        ),
        (json!({ "name": "big" }), StatusCode::CREATED),
        (json!({ "name": "third" }), StatusCode::CONFLICT),
    ] {
        let status = status_of(&app.router, "POST", environments, &alice, Some(body.clone())).await;
        assert_eq!(status, expected, "{body}");
    }
    let web = |name: &str| json!({ "name": name, "image": "nginx:1.27", "port": 80 });
    let (status, _, refused) = call(
        &app.router,
        "POST",
        &format!("{environments}/tiny/apps"),
        Auth::Cookie(&alice),
        Some(web("web")),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT, "{refused}");
    assert!(
        refused.to_string().contains("200m CPU"),
        "one replica and its surge pod: {refused}"
    );
    let big = format!("{environments}/big/apps");
    for (name, expected) in [
        ("web", StatusCode::CREATED),
        ("api", StatusCode::CREATED),
        ("third", StatusCode::CONFLICT),
    ] {
        assert_eq!(
            status_of(&app.router, "POST", &big, &alice, Some(web(name))).await,
            expected,
            "{name}"
        );
    }
}

/// M4.6: the environment's scan gate refuses a release with an open
/// critical finding until an owned, expiring exception covers it.
#[tokio::test]
async fn m4_the_scan_gate_refuses_known_critical_findings() {
    let Some(app) = setup().await else { return };
    sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;
    let gate = json!({ "requiredApprovals": 0, "deployRole": "developer", "approveRole": "admin",
        "scan": { "mode": "block", "severity": "critical" } });
    let (status, _, policy) = call(&app.router, "PUT", POLICY, Auth::Cookie(&alice), Some(gate), None).await;
    assert_eq!(status, StatusCode::OK, "{policy}");
    assert_eq!(policy["scan"]["mode"], "block");
    {
        let mut t = app.store.tenant(app.org).await.expect("tenant");
        let scan = kuben_store::repo::NewScan {
            repository: "ghcr.io/acme/api".into(),
            digest: DIGEST.into(),
            summary: kuben_core::scan::ScanSummary {
                status: kuben_core::scan::ScanStatus::Ok,
                scanner: "trivy 0.74.0".into(),
                db_updated_at: None,
                counts: kuben_core::scan::Counts {
                    critical: 1,
                    ..Default::default()
                },
                findings: vec![(kuben_core::scan::Severity::Critical, "CVE-2026-7".into())],
                scanned_at: kuben_core::time::now_ms(),
            },
            detail: None,
            build_attempt: None,
        };
        t.record_scan(&scan).await.expect("scan");
        t.commit().await.expect("commit");
    }
    let (status, refused) = send(&app.router, deploy(&alice, 0, None)).await;
    assert_eq!(status, StatusCode::CONFLICT, "{refused}");
    assert!(refused.to_string().contains("CVE-2026-7"), "{refused}");

    let exceptions = "/api/v1/vulnerability-exceptions";
    let grant = json!({ "vulnerability": "CVE-2026-7", "reason": "not reachable", "owner": "platform", "days": 30, "project": "shop" });
    assert_eq!(
        status_of(&app.router, "POST", exceptions, &bob, Some(grant.clone())).await,
        StatusCode::FORBIDDEN
    );
    let (status, _, granted) = call(
        &app.router,
        "POST",
        exceptions,
        Auth::Cookie(&alice),
        Some(grant),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{granted}");
    assert_eq!(granted["active"], true);
    let (status, run) = send(&app.router, deploy(&alice, 0, None)).await;
    assert_eq!(status, StatusCode::ACCEPTED, "{run}");

    let (_, _, scans) = call(
        &app.router,
        "GET",
        "/api/v1/projects/shop/environments/prod/apps/api/scans",
        Auth::Cookie(&bob),
        None,
        None,
    )
    .await;
    assert_eq!(scans["gate"], "pass", "{scans}");
    assert_eq!(scans["images"][0]["scan"]["critical"], 1);
    assert_eq!(scans["images"][0]["sbom"], false);
    let revoke = format!("{exceptions}/{}", granted["id"].as_str().expect("id"));
    assert_eq!(
        status_of(&app.router, "DELETE", &revoke, &alice, None).await,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        status_of(&app.router, "DELETE", &revoke, &alice, None).await,
        StatusCode::NOT_FOUND
    );
    let (_, _, listed) = call(&app.router, "GET", exceptions, Auth::Cookie(&bob), None, None).await;
    assert_eq!(listed, json!([]), "revoked exceptions are not in force");
}

const PROD: &str = "/api/v1/projects/shop/environments/prod";

/// An RFC 3339 time `hours` from now.
fn hours_ahead(hours: i64) -> String {
    k8s_openapi::jiff::Timestamp::from_millisecond(kuben_core::time::now_ms() + hours * 3_600_000)
        .expect("time")
        .to_string()
}

/// POST `body` to `path` as `cookie`: the status and the answer.
async fn post_json(
    app: &TestApp,
    path: &str,
    cookie: &str,
    body: serde_json::Value,
) -> (StatusCode, serde_json::Value) {
    let (status, _, answer) = call(&app.router, "POST", path, Auth::Cookie(cookie), Some(body), None).await;
    (status, answer)
}

/// M4.9: a freeze refuses releases, an emergency rollback passes it with a
/// reason, and a pause is held and resumed.
#[tokio::test]
async fn m4_freezes_pauses_and_emergency_rollbacks() {
    let Some(app) = setup().await else { return };
    sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;
    let (status, first) = send(&app.router, deploy(&alice, 0, None)).await;
    assert_eq!(status, StatusCode::ACCEPTED, "{first}");
    let first_release = succeed(&app, &first).await;
    let freezes = format!("{PROD}/freezes");
    let freeze = json!({ "reason": "launch", "endsAt": hours_ahead(2) });
    assert_eq!(
        (post_json(&app, &freezes, &bob, freeze.clone()).await).0,
        StatusCode::FORBIDDEN
    );
    let (status, frozen) = post_json(&app, &freezes, &alice, freeze).await;
    assert_eq!(
        (status, frozen["active"].clone()),
        (StatusCode::CREATED, json!(true)),
        "{frozen}"
    );
    let redeploy = json!({
        "image": "ghcr.io/acme/api@sha256:9999999999999999999999999999999999999999999999999999999999999999",
        "expected_generation": 1,
    });
    let (status, refused) = post_json(&app, DEPLOYMENTS, &alice, redeploy).await;
    assert_eq!(status, StatusCode::CONFLICT, "{refused}");
    assert!(refused.to_string().contains("frozen"));

    let api = format!("{PROD}/apps/api");
    let pause = json!({ "reason": "incident 42" });
    assert_eq!(
        post_json(&app, &format!("{api}/pause"), &alice, pause.clone())
            .await
            .0,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        post_json(&app, &format!("{api}/pause"), &alice, pause).await.0,
        StatusCode::CONFLICT
    );
    let (_, _, shown) = call(&app.router, "GET", &api, Auth::Cookie(&alice), None, None).await;
    assert_eq!(shown["app"]["paused"], "incident 42");
    let emergency = format!("{api}/emergency-rollback");
    let why = json!({ "reason": "checkout is down" });
    assert_eq!(
        post_json(&app, &emergency, &alice, why.clone()).await.0,
        StatusCode::NOT_FOUND,
        "no release before the only one"
    );
    let chosen = json!({ "reason": "checkout is down", "release": first_release });
    let (status, run) = post_json(&app, &emergency, &alice, chosen).await;
    assert_eq!(
        (status, run["approvals_required"].clone()),
        (StatusCode::ACCEPTED, json!(0)),
        "{run}"
    );
    assert_eq!(
        post_json(&app, &emergency, &bob, why).await.0,
        StatusCode::FORBIDDEN
    );
    assert_eq!(
        post_json(&app, &format!("{api}/resume"), &alice, json!(null))
            .await
            .0,
        StatusCode::NO_CONTENT
    );
    let lift = format!("{freezes}/{}", frozen["id"].as_str().expect("id"));
    assert_eq!(
        status_of(&app.router, "DELETE", &lift, &alice, None).await,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        status_of(&app.router, "DELETE", &lift, &alice, None).await,
        StatusCode::NOT_FOUND
    );
}

/// M4.9: owners and alert silences are kept.
#[tokio::test]
async fn m4_owners_and_silences_are_kept() {
    let Some(app) = setup().await else { return };
    sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let owner =
        json!({ "owner": "shop-team", "contact": "#shop", "runbookUrl": "https://wiki.example.com/shop" });
    let path = "/api/v1/projects/shop/applications/api/owner";
    let (status, _, saved) = call(&app.router, "PUT", path, Auth::Cookie(&alice), Some(owner), None).await;
    assert_eq!(
        (status, saved["owner"].clone()),
        (StatusCode::OK, json!("shop-team")),
        "{saved}"
    );
    let (_, _, read) = call(&app.router, "GET", path, Auth::Cookie(&alice), None, None).await;
    assert_eq!(read["runbookUrl"], "https://wiki.example.com/shop");
    let silences = format!("{PROD}/silences");
    let silence = json!({ "reason": "maintenance", "endsAt": hours_ahead(1), "app": "api" });
    let (status, made) = post_json(&app, &silences, &alice, silence).await;
    assert_eq!(
        (status, made["active"].clone()),
        (StatusCode::CREATED, json!(true)),
        "{made}"
    );
    let too_long = json!({ "reason": "forever", "endsAt": hours_ahead(24 * 8) });
    assert_eq!(
        post_json(&app, &silences, &alice, too_long).await.0,
        StatusCode::UNPROCESSABLE_ENTITY
    );
}

/// Mark the run in `run` as succeeded; its release.
async fn succeed(app: &TestApp, run: &serde_json::Value) -> uuid::Uuid {
    let id: uuid::Uuid = run["run"].as_str().expect("run").parse().expect("uuid");
    let run = kuben_core::ids::DeploymentRunId::from_uuid(id);
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    t.force_run_phase(run, kuben_core::ops::RunPhase::Succeeded)
        .await
        .expect("phase");
    let release = t.run_release(run).await.expect("read").expect("release");
    t.commit().await.expect("commit");
    *release.as_uuid()
}

/// A webhook receiver on loopback: every request (headers and body) goes
/// to the channel and is answered with 204.
async fn receiver() -> (String, tokio::sync::mpsc::UnboundedReceiver<(String, Vec<u8>)>) {
    use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.expect("bind");
    let url = format!("http://{}/hook", listener.local_addr().expect("addr"));
    let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
    tokio::spawn(async move {
        while let Ok((mut conn, _)) = listener.accept().await {
            let mut buf = Vec::new();
            let mut chunk = [0u8; 4096];
            let (head, body_at) = loop {
                let n = conn.read(&mut chunk).await.expect("read");
                assert!(n > 0, "the request ended early");
                buf.extend_from_slice(&chunk[..n]);
                if let Some(at) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
                    break (String::from_utf8_lossy(&buf[..at]).to_lowercase(), at + 4);
                }
            };
            let length: usize = head
                .lines()
                .find_map(|l| l.strip_prefix("content-length:"))
                .and_then(|v| v.trim().parse().ok())
                .unwrap_or(0);
            while buf.len() < body_at + length {
                let n = conn.read(&mut chunk).await.expect("read");
                assert!(n > 0, "the body ended early");
                buf.extend_from_slice(&chunk[..n]);
            }
            let body = buf[body_at..body_at + length].to_vec();
            conn.write_all(b"HTTP/1.1 204 No Content\r\nconnection: close\r\ncontent-length: 0\r\n\r\n")
                .await
                .expect("write");
            let _ = tx.send((head, body));
        }
    });
    (url, rx)
}

/// The notifier the test app would run.
fn notifier(app: &TestApp) -> kuben_api::notify::Notifier {
    let cfg = kuben_core::config::NotifyCfg {
        allow_private_targets: true,
        allow_http: true,
    };
    kuben_api::notify::Notifier::new(
        app.store.clone(),
        Arc::new(kuben_platform::secrets::Keyring::from_keys([(1, [7; 32])])),
        None,
        cfg,
        Some("https://kuben.example.com".into()),
    )
}

/// Fail the deployment `run` started, as the materializer would.
async fn fail_operation(app: &TestApp) {
    let claim = app
        .store
        .claim_operation(
            "test",
            &[kuben_store::repo::RUN_KIND],
            std::time::Duration::from_mins(1),
        )
        .await
        .expect("claim")
        .expect("an operation");
    assert!(
        app.store
            .finish_operation(&claim, "failed", Some("RolloutFailed"))
            .await
            .expect("finish")
    );
}

/// M4.10: webhooks are created by admins, signed, delivered and listed;
/// a failed deployment opens an incident that can be acknowledged and
/// resolved.
#[tokio::test]
async fn m4_signed_webhooks_and_incidents() {
    let Some(app) = setup_with(|cfg| {
        cfg.notify.allow_private_targets = true;
        cfg.notify.allow_http = true;
    })
    .await
    else {
        return;
    };
    sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;
    let (url, mut received) = receiver().await;
    let endpoint = json!({ "name": "pager", "url": url, "events": ["deployment.failed", "incident.opened"] });
    let hooks = "/api/v1/webhooks";
    assert_eq!(
        post_json(&app, hooks, &bob, endpoint.clone()).await.0,
        StatusCode::FORBIDDEN
    );
    let unknown = json!({ "name": "x", "url": url, "events": ["nope"] });
    assert_eq!(
        post_json(&app, hooks, &alice, unknown).await.0,
        StatusCode::UNPROCESSABLE_ENTITY
    );
    let ftp = json!({ "name": "x", "url": "ftp://example.com/", "events": ["*"] });
    assert_eq!(
        post_json(&app, hooks, &alice, ftp).await.0,
        StatusCode::UNPROCESSABLE_ENTITY
    );
    let (status, made) = post_json(&app, hooks, &alice, endpoint.clone()).await;
    assert_eq!(status, StatusCode::CREATED, "{made}");
    assert_eq!(
        post_json(&app, hooks, &alice, endpoint).await.0,
        StatusCode::CONFLICT
    );
    let secret = made["secret"].as_str().expect("secret").to_owned();
    assert!(secret.starts_with("whsec_"));
    let (_, listed) = send(&app.router, get(hooks, &alice)).await;
    assert_eq!(listed.as_array().map(Vec::len), Some(1));
    assert!(listed[0].get("secret").is_none(), "shown once");
    let id = made["id"].as_str().expect("id");
    let ping = format!("{hooks}/{id}/ping");
    assert_eq!(
        post_json(&app, &ping, &alice, json!({})).await.0,
        StatusCode::ACCEPTED
    );

    let (status, _) = send(&app.router, deploy(&alice, 0, None)).await;
    assert_eq!(status, StatusCode::ACCEPTED);
    fail_operation(&app).await;
    let notifier = notifier(&app);
    assert!(
        notifier.consume().await.expect("consume") >= 2,
        "accepted and settled"
    );
    assert_eq!(
        notifier.deliver().await.expect("deliver"),
        3,
        "ping, failure, incident"
    );
    let mut events = Vec::new();
    for _ in 0..3 {
        let (head, body) = received.recv().await.expect("delivery");
        let t: i64 = head
            .lines()
            .find_map(|l| l.strip_prefix("kuben-signature: t="))
            .and_then(|v| v.split(',').next())
            .and_then(|v| v.parse().ok())
            .expect("signed");
        let expected = kuben_api::notify::signature(secret.as_bytes(), t, &body).to_lowercase();
        assert!(head.contains(&format!("kuben-signature: {expected}")), "{head}");
        let event = head
            .lines()
            .find_map(|l| l.strip_prefix("kuben-event: "))
            .expect("event");
        events.push(event.to_owned());
    }
    events.sort();
    assert_eq!(events, ["deployment.failed", "incident.opened", "ping"]);
    let (_, deliveries) = send(&app.router, get(&format!("{hooks}/{id}/deliveries"), &alice)).await;
    assert!(
        deliveries
            .as_array()
            .expect("list")
            .iter()
            .all(|d| d["status"] == "delivered"),
        "{deliveries}"
    );
    incidents_are_handled(&app, &alice, &bob).await;
}

/// The failed deployment's incident: listed, acknowledged and resolved.
async fn incidents_are_handled(app: &TestApp, alice: &str, bob: &str) {
    let (_, open) = send(&app.router, get("/api/v1/incidents", alice)).await;
    let open = open.as_array().expect("list").clone();
    assert_eq!(open.len(), 1, "{open:?}");
    assert_eq!(
        (
            open[0]["kind"].clone(),
            open[0]["severity"].clone(),
            open[0]["detail"].clone()
        ),
        (
            json!("deployment.failed"),
            json!("critical"),
            json!("RolloutFailed")
        )
    );
    let incident = open[0]["id"].as_str().expect("id");
    let ack = format!("/api/v1/incidents/{incident}/acknowledge");
    assert_eq!(
        post_json(app, &ack, bob, json!({})).await.0,
        StatusCode::FORBIDDEN
    );
    assert_eq!(
        post_json(app, &ack, alice, json!({})).await.0,
        StatusCode::NO_CONTENT
    );
    let resolve = format!("/api/v1/incidents/{incident}/resolve");
    assert_eq!(
        post_json(app, &resolve, alice, json!({})).await.0,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        post_json(app, &resolve, alice, json!({})).await.0,
        StatusCode::CONFLICT
    );
    let (_, open) = send(&app.router, get("/api/v1/incidents", alice)).await;
    assert_eq!(open, json!([]));
    let (_, all) = send(&app.router, get("/api/v1/incidents?all=true", alice)).await;
    assert_eq!(
        all[0]["acknowledgedBy"].as_str().map(|s| s.starts_with("user:")),
        Some(true),
        "{all}"
    );
}

/// Deliver the run in `run` as the materializer would: freeze a plan with a
/// Deployment and a route, and succeed.
async fn deliver_with_plan(app: &TestApp, run: &serde_json::Value) {
    let id: uuid::Uuid = run["run"].as_str().expect("run").parse().expect("uuid");
    let run_id = kuben_core::ids::DeploymentRunId::from_uuid(id);
    let claim = app
        .store
        .claim_operation(
            "test",
            &[kuben_store::repo::RUN_KIND],
            std::time::Duration::from_mins(1),
        )
        .await
        .expect("claim")
        .expect("an operation");
    let resources = json!([
        { "apiVersion": "apps/v1", "kind": "Deployment", "metadata": { "name": "api-web" } },
        { "apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
          "metadata": { "name": "api" }, "spec": { "hostnames": ["api.example.com"] } },
    ]);
    let capabilities = json!({ "sizes": [], "gateway": "kuben-system/kuben" });
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    t.freeze_run_plan(&claim, run_id, "kuben-renderer/2", &capabilities, &resources)
        .await
        .expect("freeze")
        .expect("frozen");
    t.commit().await.expect("commit");
    assert!(
        app.store
            .finish_operation(&claim, "succeeded", None)
            .await
            .expect("finish")
    );
    succeed(app, run).await;
}

/// M4.11: an app is exported, detached with its export frozen, and
/// released once the materializer let go of it.
#[tokio::test]
async fn m4_export_detach_and_release() {
    let Some(app) = setup().await else { return };
    let target = sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;
    let base = format!("{PROD}/apps/api");
    let (status, _) = send(&app.router, get(&format!("{base}/export"), &alice)).await;
    assert_eq!(status, StatusCode::CONFLICT, "nothing delivered yet");
    let (status, run) = send(&app.router, deploy(&alice, 0, None)).await;
    assert_eq!(status, StatusCode::ACCEPTED, "{run}");
    deliver_with_plan(&app, &run).await;
    let (status, export) = send(&app.router, get(&format!("{base}/export"), &alice)).await;
    assert_eq!(status, StatusCode::OK, "{export}");
    assert_eq!(export["format"], "kuben.dev/export/v1");
    assert_eq!(export["manifests"]["items"].as_array().map(Vec::len), Some(2));
    assert_eq!(export["references"]["hostnames"], json!(["api.example.com"]));
    assert!(export["release"]["artifacts"].is_object());

    let detach = format!("{base}/detach");
    let body = json!({ "confirm": "api", "reason": "moving to our own GitOps" });
    assert_eq!(
        post_json(&app, &detach, &bob, body.clone()).await.0,
        StatusCode::FORBIDDEN
    );
    let wrong = json!({ "confirm": "web", "reason": "x" });
    assert_eq!(
        post_json(&app, &detach, &alice, wrong).await.0,
        StatusCode::UNPROCESSABLE_ENTITY
    );
    let (status, detached) = post_json(&app, &detach, &alice, body.clone()).await;
    assert_eq!(status, StatusCode::ACCEPTED, "{detached}");
    assert_eq!(detached["export"]["manifests"], export["manifests"]);
    assert_eq!(
        post_json(&app, &detach, &alice, body).await.0,
        StatusCode::CONFLICT
    );
    let id = detached["id"].as_str().expect("id").to_owned();
    assert_eq!(id, target.as_uuid().to_string());

    let (_, list) = send(&app.router, get(&format!("{PROD}/detached"), &alice)).await;
    assert_eq!(list.as_array().map(Vec::len), Some(1), "{list}");
    assert!(list[0]["completedAt"].is_null() && list[0].get("export").is_none());
    let release = format!("{PROD}/detached/{id}/release");
    assert_eq!(
        post_json(&app, &release, &alice, json!({})).await.0,
        StatusCode::CONFLICT,
        "not finished"
    );
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    assert_eq!(
        t.detached_held(target_environment(&app).await)
            .await
            .expect("held"),
        1
    );
    assert!(t.complete_detach(target).await.expect("complete"));
    t.commit().await.expect("commit");
    assert_eq!(
        post_json(&app, &release, &bob, json!({})).await.0,
        StatusCode::FORBIDDEN
    );
    assert_eq!(
        post_json(&app, &release, &alice, json!({})).await.0,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        post_json(&app, &release, &alice, json!({})).await.0,
        StatusCode::CONFLICT
    );
    let (status, one) = send(&app.router, get(&format!("{PROD}/detached/{id}"), &alice)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(one["export"]["format"], "kuben.dev/export/v1");
    assert!(one["releasedBy"].as_str().is_some_and(|b| b.starts_with("user:")));
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    assert_eq!(
        t.detached_held(target_environment(&app).await)
            .await
            .expect("held"),
        0
    );
}

/// The id of the test app's environment.
async fn target_environment(app: &TestApp) -> kuben_core::ids::EnvironmentId {
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    let project = t
        .projects()
        .await
        .expect("projects")
        .into_iter()
        .find(|p| p.slug == "shop")
        .expect("shop");
    t.environments(project.id)
        .await
        .expect("environments")
        .into_iter()
        .find(|e| e.slug == "prod")
        .expect("prod")
        .id
}

/// The webhook secret of the test GitHub App.
const HOOK_SECRET: &[u8] = b"hook-secret";
const HEAD: &str = "89abcdef0123456789abcdef0123456789abcdef";

/// POST a signed GitHub delivery.
async fn github_delivery(
    app: &TestApp,
    event: &str,
    body: &serde_json::Value,
) -> (StatusCode, serde_json::Value) {
    use std::fmt::Write as _;
    let bytes = body.to_string();
    let key = ring::hmac::Key::new(ring::hmac::HMAC_SHA256, HOOK_SECRET);
    let tag = ring::hmac::sign(&key, bytes.as_bytes());
    let hex = tag.as_ref().iter().fold(String::new(), |mut s, b| {
        let _ = write!(s, "{b:02x}");
        s
    });
    let req = Request::post("/api/v1/webhooks/github")
        .header(header::CONTENT_TYPE, "application/json")
        .header("x-hub-signature-256", format!("sha256={hex}"))
        .header("x-github-event", event)
        .header("x-github-delivery", uuid::Uuid::now_v7().to_string())
        .body(Body::from(bytes))
        .expect("request");
    send(&app.router, req).await
}

fn pull(action: &str, number: u64, head_repo: &str, updated_at: &str) -> serde_json::Value {
    json!({
        "action": action,
        "number": number,
        "pull_request": {
            "number": number,
            "state": if action == "closed" { "closed" } else { "open" },
            "updated_at": updated_at,
            "head": { "sha": HEAD, "ref": "feature", "repo": { "full_name": head_repo } }
        },
        "repository": { "id": 42, "full_name": "acme/shop" },
        "installation": { "id": 77 }
    })
}

/// The test app, built from GitHub: a linked installation and a binding.
async fn git_app(app: &TestApp) {
    let target = sql_app(app).await;
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    assert!(t.link_installation(77, "acme").await.expect("link"));
    let project = t.projects().await.expect("projects")[0].id;
    let config = json!({
        "runtime": { "processes": { "web": { "port": 8080 } } },
        "env": [{ "name": "DB", "fromSecret": { "name": "db", "key": "url" } }],
        "domains": [{ "host": "shop.example.com" }]
    });
    t.create_config_revision(project, target, &config, "user:test")
        .await
        .expect("config")
        .expect("target");
    let binding = kuben_store::repo::NewBinding {
        installation_id: 77,
        repository: "acme/shop".parse().expect("repo"),
        branch: "main".parse().expect("branch"),
        recipe: kuben_core::source::BuildRecipe::default(),
        image_repository: "registry.local/acme/shop".into(),
        pull_request: None,
    };
    t.bind_source(project, target, &binding).await.expect("bind");
    t.commit().await.expect("commit");
}

/// M5.1: pull requests open, follow, close and reopen previews; forks
/// need the project's consent and never get secrets.
#[tokio::test]
async fn m5_previews_follow_pull_requests() {
    let Some(app) = setup().await else { return };
    git_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;
    let policy = "/api/v1/projects/shop/previews/policy";
    let settings = json!({ "enabled": true, "sourceEnvironment": "prod", "ttlHours": 2, "maxActive": 5 });
    let (status, _, _) = call(
        &app.router,
        "PUT",
        policy,
        Auth::Cookie(&bob),
        Some(settings.clone()),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    let (status, _, saved) = call(
        &app.router,
        "PUT",
        policy,
        Auth::Cookie(&alice),
        Some(settings),
        None,
    )
    .await;
    assert_eq!(
        (status, saved["sourceEnvironment"].clone()),
        (StatusCode::OK, json!("prod")),
        "{saved}"
    );

    let (status, answer) = github_delivery(
        &app,
        "pull_request",
        &pull("opened", 12, "acme/shop", "2026-09-17T10:00:00Z"),
    )
    .await;
    assert_eq!(status, StatusCode::ACCEPTED, "{answer}");
    let previews = "/api/v1/projects/shop/previews";
    let (_, listed) = send(&app.router, get(previews, &alice)).await;
    assert_eq!(listed[0]["environment"], "pr12-1", "{listed}");
    assert_eq!(
        (listed[0]["trusted"].clone(), listed[0]["state"].clone()),
        (json!(true), json!("active"))
    );
    assert!(listed[0]["remainingSeconds"].as_i64().is_some_and(|s| s > 7000));
    let (_, apps) = send(
        &app.router,
        get("/api/v1/projects/shop/environments/pr12-1/apps", &alice),
    )
    .await;
    assert_eq!(apps.as_array().map(Vec::len), Some(1), "{apps}");
    let (_, copied) = send(
        &app.router,
        get("/api/v1/projects/shop/environments/pr12-1/apps/api", &alice),
    )
    .await;
    assert_eq!(copied["app"]["env"], json!([]), "no secret references: {copied}");
    assert_eq!(copied["app"]["domains"], json!([]), "no custom domains");

    let (_, answer) = github_delivery(
        &app,
        "pull_request",
        &pull("synchronize", 12, "acme/shop", "2026-09-17T09:00:00Z"),
    )
    .await;
    assert_eq!(
        answer["outcome"], "previews",
        "stale events are accepted but change nothing: {answer}"
    );
    let (_, answer) = github_delivery(
        &app,
        "pull_request",
        &pull("opened", 13, "mallory/shop", "2026-09-17T10:00:00Z"),
    )
    .await;
    assert_eq!(answer["outcome"], "previews");
    let (_, listed) = send(&app.router, get(previews, &alice)).await;
    assert_eq!(
        listed.as_array().map(Vec::len),
        Some(1),
        "forks need consent: {listed}"
    );

    close_and_reopen(&app, &alice, previews).await;
    forks_and_manual_actions(&app, &alice, policy, previews).await;
}

/// Closing ends a preview; a late event does not revive it; reopening makes a new one.
async fn close_and_reopen(app: &TestApp, alice: &str, previews: &str) {
    github_delivery(
        app,
        "pull_request",
        &pull("closed", 12, "acme/shop", "2026-09-17T11:00:00Z"),
    )
    .await;
    let (_, listed) = send(&app.router, get(&format!("{previews}?all=true"), alice)).await;
    assert_eq!(
        (listed[0]["state"].clone(), listed[0]["closeReason"].clone()),
        (json!("closed"), json!("closed"))
    );
    github_delivery(
        app,
        "pull_request",
        &pull("synchronize", 12, "acme/shop", "2026-09-17T10:30:00Z"),
    )
    .await;
    let (_, active) = send(&app.router, get(previews, alice)).await;
    assert_eq!(active, json!([]), "a late event does not bring it back");
    github_delivery(
        app,
        "pull_request",
        &pull("reopened", 12, "acme/shop", "2026-09-17T12:00:00Z"),
    )
    .await;
    let (_, active) = send(&app.router, get(previews, alice)).await;
    assert_eq!(active[0]["environment"], "pr12-2", "a new epoch: {active}");
}

async fn forks_and_manual_actions(app: &TestApp, alice: &str, policy: &str, previews: &str) {
    let settings = json!({ "enabled": true, "sourceEnvironment": "prod", "allowForks": true });
    call(
        &app.router,
        "PUT",
        policy,
        Auth::Cookie(alice),
        Some(settings),
        None,
    )
    .await;
    github_delivery(
        app,
        "pull_request",
        &pull("opened", 13, "mallory/shop", "2026-09-17T10:00:00Z"),
    )
    .await;
    let (_, active) = send(&app.router, get(previews, alice)).await;
    let fork = active
        .as_array()
        .and_then(|a| a.iter().find(|p| p["pullRequest"] == 13))
        .expect("fork preview")
        .clone();
    assert_eq!(
        (fork["environment"].clone(), fork["trusted"].clone()),
        (json!("pr13-1"), json!(false))
    );
    let secret = "/api/v1/projects/shop/environments/pr13-1/secrets/db";
    let (status, _, body) = call(
        &app.router,
        "PUT",
        secret,
        Auth::Cookie(alice),
        Some(json!({ "data": { "url": "x" } })),
        None,
    )
    .await;
    assert_eq!(
        status,
        StatusCode::CONFLICT,
        "no secrets in a fork's preview: {body}"
    );

    let extend = format!("{previews}/pr13-1/extend");
    let (status, extended) = post_json(app, &extend, alice, json!({ "hours": 5, "keep": true })).await;
    assert_eq!(
        (status, extended["autoDelete"].clone()),
        (StatusCode::OK, json!(false)),
        "{extended}"
    );
    assert!(extended["remainingSeconds"].as_i64() > fork["remainingSeconds"].as_i64());
    assert_eq!(
        post_json(app, &extend, alice, json!({ "hours": 0 })).await.0,
        StatusCode::UNPROCESSABLE_ENTITY
    );
    let destroy = format!("{previews}/pr13-1");
    assert_eq!(
        status_of(&app.router, "DELETE", &destroy, alice, None).await,
        StatusCode::ACCEPTED
    );
    assert_eq!(
        status_of(&app.router, "DELETE", &destroy, alice, None).await,
        StatusCode::NOT_FOUND
    );
    let (_, all) = send(&app.router, get(&format!("{previews}?all=true"), alice)).await;
    let closed = all
        .as_array()
        .and_then(|a| a.iter().find(|p| p["environment"] == "pr13-1"))
        .expect("closed");
    assert_eq!(closed["closeReason"], "manual");
}

/// DNS for the tests: TXT answers from `TXT_VALUE`, and a provider whose
/// token `good` holds the zone `example.com`.
#[derive(Debug, Default)]
struct FakeDns {
    records: Arc<std::sync::Mutex<Vec<kuben_api::dns::ProviderRecord>>>,
}

/// The TXT value the fake resolver answers for every challenge name.
static TXT_VALUE: std::sync::Mutex<String> = std::sync::Mutex::new(String::new());

#[async_trait::async_trait]
impl kuben_api::dns::DnsBackend for FakeDns {
    async fn txt(&self, _name: &str) -> Result<Vec<String>, kuben_api::dns::DnsError> {
        let value = TXT_VALUE.lock().expect("lock").clone();
        Ok(if value.is_empty() { vec![] } else { vec![value] })
    }

    async fn ns(&self, _name: &str) -> Result<Vec<String>, kuben_api::dns::DnsError> {
        Ok(vec!["ada.ns.cloudflare.com".into()])
    }

    fn provider(&self, kind: &str, token: &str) -> Option<Arc<dyn kuben_api::dns::DnsProvider>> {
        (kind == "cloudflare").then(|| {
            Arc::new(FakeProvider {
                good: token == "good",
                records: self.records.clone(),
            }) as Arc<dyn kuben_api::dns::DnsProvider>
        })
    }
}

#[derive(Debug)]
struct FakeProvider {
    good: bool,
    records: Arc<std::sync::Mutex<Vec<kuben_api::dns::ProviderRecord>>>,
}

#[async_trait::async_trait]
impl kuben_api::dns::DnsProvider for FakeProvider {
    async fn verify(&self) -> Result<(), kuben_api::dns::DnsError> {
        if self.good {
            Ok(())
        } else {
            Err(kuben_api::dns::DnsError::Refused("bad token".into()))
        }
    }

    async fn zone_for(&self, name: &str) -> Result<Option<kuben_api::dns::Zone>, kuben_api::dns::DnsError> {
        Ok(
            (name == "example.com" || name.ends_with(".example.com")).then(|| kuben_api::dns::Zone {
                id: "z1".into(),
                name: "example.com".into(),
                name_servers: vec![],
            }),
        )
    }

    async fn records(
        &self,
        _zone: &kuben_api::dns::Zone,
        name: &str,
    ) -> Result<Vec<kuben_api::dns::ProviderRecord>, kuben_api::dns::DnsError> {
        let all = self.records.lock().expect("lock");
        Ok(all.iter().filter(|r| r.name == name).cloned().collect())
    }

    async fn create(
        &self,
        _zone: &kuben_api::dns::Zone,
        spec: &kuben_api::dns::RecordSpec,
        tag: &str,
    ) -> Result<kuben_api::dns::ProviderRecord, kuben_api::dns::DnsError> {
        let mut all = self.records.lock().expect("lock");
        let record = kuben_api::dns::ProviderRecord {
            id: format!("r{}", all.len() + 1),
            name: spec.name.clone(),
            record_type: spec.record_type.clone(),
            content: spec.content.clone(),
            proxied: false,
            comment: Some(tag.into()),
        };
        all.push(record.clone());
        Ok(record)
    }

    async fn update(
        &self,
        zone: &kuben_api::dns::Zone,
        id: &str,
        spec: &kuben_api::dns::RecordSpec,
        tag: &str,
    ) -> Result<kuben_api::dns::ProviderRecord, kuben_api::dns::DnsError> {
        self.delete(zone, id).await?;
        self.create(zone, spec, tag).await
    }

    async fn delete(&self, _zone: &kuben_api::dns::Zone, id: &str) -> Result<(), kuben_api::dns::DnsError> {
        self.records.lock().expect("lock").retain(|r| r.id != id);
        Ok(())
    }
}

/// M5.2: domains are claimed and verified by TXT or a provider, another
/// organization's domain is refused, and an app's records are written once.
#[tokio::test]
async fn m5_domain_claims_and_dns_records() {
    let Some(app) = setup_with(|cfg| cfg.domains.cname_target = Some("lb.example.net".into())).await else {
        return;
    };
    let target = sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;
    let body = json!({ "domain": "Shop.Example.com." });
    assert_eq!(
        post_json(&app, "/api/v1/domains", &bob, body.clone()).await.0,
        StatusCode::FORBIDDEN
    );
    let (status, claim) = post_json(&app, "/api/v1/domains", &alice, body.clone()).await;
    assert_eq!(
        (status, claim["domain"].clone()),
        (StatusCode::CREATED, json!("shop.example.com")),
        "{claim}"
    );
    assert_eq!(claim["challengeName"], "_kuben-challenge.shop.example.com");
    assert_eq!(
        post_json(&app, "/api/v1/domains", &alice, body).await.0,
        StatusCode::CONFLICT
    );
    let verify = format!("/api/v1/domains/{}/verify", claim["id"].as_str().expect("id"));
    let (_, pending) = post_json(&app, &verify, &alice, json!({})).await;
    assert_eq!(pending["status"], "pending");
    assert!(
        pending["lastError"]
            .as_str()
            .is_some_and(|e| e.contains("no TXT record")),
        "{pending}"
    );
    *TXT_VALUE.lock().expect("lock") = claim["challengeValue"].as_str().expect("value").to_owned();
    let (_, verified) = post_json(&app, &verify, &alice, json!({})).await;
    assert_eq!(
        (verified["status"].clone(), verified["method"].clone()),
        (json!("verified"), json!("txt"))
    );
    TXT_VALUE.lock().expect("lock").clear();

    let providers = "/api/v1/dns-providers";
    let bad = json!({ "name": "cf", "kind": "cloudflare", "token": "bad" });
    assert_eq!(
        post_json(&app, providers, &alice, bad).await.0,
        StatusCode::UNPROCESSABLE_ENTITY
    );
    let unknown = json!({ "name": "cf", "kind": "route53", "token": "good" });
    assert_eq!(
        post_json(&app, providers, &alice, unknown).await.0,
        StatusCode::UNPROCESSABLE_ENTITY
    );
    let (status, _) = post_json(
        &app,
        providers,
        &alice,
        json!({ "name": "cf", "kind": "cloudflare", "token": "good" }),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let (_, api_claim) = post_json(
        &app,
        "/api/v1/domains",
        &alice,
        json!({ "domain": "api.example.com" }),
    )
    .await;
    let verify = format!("/api/v1/domains/{}/verify", api_claim["id"].as_str().expect("id"));
    let (_, by_provider) = post_json(&app, &verify, &alice, json!({ "provider": "cf" })).await;
    assert_eq!(by_provider["method"], "cloudflare", "{by_provider}");
    app_records(&app, &alice, target).await;
}

async fn app_records(app: &TestApp, alice: &str, target: kuben_core::ids::TargetId) {
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    let project = t.projects().await.expect("projects")[0].id;
    let config = json!({ "runtime": { "processes": { "web": { "port": 8080 } } }, "domains": [{ "host": "shop.example.com" }] });
    t.create_config_revision(project, target, &config, "user:test")
        .await
        .expect("config");
    t.commit().await.expect("commit");
    let dns = "/api/v1/projects/shop/environments/prod/apps/api/dns";
    let (status, changes) = post_json(app, dns, alice, json!({ "provider": "cf" })).await;
    assert_eq!(status, StatusCode::OK, "{changes}");
    assert_eq!(
        (
            changes[0]["action"].clone(),
            changes[0]["recordType"].clone(),
            changes[0]["content"].clone()
        ),
        (json!("created"), json!("CNAME"), json!("lb.example.net"))
    );
    let (_, again) = post_json(app, dns, alice, json!({ "provider": "cf" })).await;
    assert_eq!(again[0]["action"], "unchanged", "{again}");
    let other = app.store.create_org("rival", "Rival").await.expect("org").id;
    let mut t = app.store.tenant(other).await.expect("tenant");
    let claim = uuid::Uuid::now_v7();
    t.create_claim(claim, "rival.io", &"x".repeat(32), "user:r")
        .await
        .expect("claim");
    t.verify_claim(claim, "txt", None).await.expect("verify");
    t.commit().await.expect("commit");
    let taken = json!({ "name": "web2", "image": "nginx:1.27", "port": 8080, "domains": ["www.rival.io"] });
    let (status, body) = post_json(app, "/api/v1/projects/shop/environments/prod/apps", alice, taken).await;
    assert_eq!(
        status,
        StatusCode::CONFLICT,
        "another organization's domain: {body}"
    );
    let (status, _) = post_json(app, "/api/v1/domains", alice, json!({ "domain": "rival.io" })).await;
    assert_eq!(status, StatusCode::CONFLICT);
}

/// M5.3: a published status page is public, cached, and leaks nothing
/// internal.
#[tokio::test]
async fn m5_public_status_pages_show_only_public_facts() {
    let Some(app) = setup().await else { return };
    let target = sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;
    let public = "/api/v1/public/status/shop-status";
    let (status, _, _) = call(&app.router, "GET", public, Auth::Anonymous, None, None).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    let incident = kuben_store::repo::NewIncident {
        project: None,
        environment: None,
        target: Some(target),
        kind: "deployment.failed".into(),
        severity: "critical",
        dedupe_key: "k".into(),
        title: "The deployment of shop/prod/api failed".into(),
        detail: Some("secret internal detail".into()),
    };
    t.open_incident(&incident).await.expect("incident");
    t.commit().await.expect("commit");

    let page = "/api/v1/projects/shop/status-page";
    let body = json!({ "slug": "shop-status", "title": "Shop", "environments": ["prod"] });
    let (status, _, _) = call(
        &app.router,
        "PUT",
        page,
        Auth::Cookie(&bob),
        Some(body.clone()),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    let bad = json!({ "slug": "Shop Status", "title": "Shop", "environments": ["prod"] });
    let (status, _, _) = call(&app.router, "PUT", page, Auth::Cookie(&alice), Some(bad), None).await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    let unknown = json!({ "slug": "shop-status", "title": "Shop", "environments": ["nope"] });
    let (status, _, _) = call(
        &app.router,
        "PUT",
        page,
        Auth::Cookie(&alice),
        Some(unknown),
        None,
    )
    .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    let (status, _, saved) = call(&app.router, "PUT", page, Auth::Cookie(&alice), Some(body), None).await;
    assert_eq!(
        (status, saved["path"].clone()),
        (StatusCode::OK, json!("/status/shop-status")),
        "{saved}"
    );

    let (status, headers, shown) = call(&app.router, "GET", public, Auth::Anonymous, None, None).await;
    assert_eq!(status, StatusCode::OK, "{shown}");
    assert_eq!(headers[header::CACHE_CONTROL], "public, max-age=15");
    assert_eq!(shown["title"], "Shop");
    assert_eq!(
        shown["components"],
        json!([{ "name": "API", "status": "degraded" }])
    );
    assert_eq!(shown["status"], "degraded");
    assert_eq!(shown["incidents"][0]["severity"], "critical");
    assert_eq!(shown["incidents"][0]["component"], "API");
    let text = shown.to_string();
    for leak in [
        "secret internal detail",
        "shop/prod/api",
        "kb-shop-prod",
        &target.to_string(),
        "deployment.failed",
    ] {
        assert!(!text.contains(leak), "{leak} leaked: {text}");
    }
    let (status, _, _) = call(&app.router, "DELETE", page, Auth::Cookie(&alice), None, None).await;
    assert_eq!(status, StatusCode::NO_CONTENT);
    let (status, _, _) = call(&app.router, "GET", public, Auth::Anonymous, None, None).await;
    assert_eq!(status, StatusCode::NOT_FOUND, "taken down at once");
}

/// M5.4: an app follows a SemVer range of its repository; a new digest is
/// deployed and waits for approval in production; the same digest is not
/// deployed twice.
#[tokio::test]
async fn m5_image_policies_deploy_new_digests() {
    let Some(app) = setup().await else { return };
    let target = sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let bob = login(&app.router, "bob@example.com").await;
    let (status, _) = send(&app.router, deploy(&alice, 0, None)).await;
    assert_eq!(status, StatusCode::ACCEPTED);
    let path = "/api/v1/projects/shop/environments/prod/apps/api/image-policy";
    let policy = json!({ "repository": "nginx", "pattern": "semver:>=1.26", "intervalSecs": 300 });
    let put = |cookie: &str, body: serde_json::Value| {
        let cookie = cookie.to_owned();
        let router = app.router.clone();
        async move { call(&router, "PUT", path, Auth::Cookie(&cookie), Some(body), None).await }
    };
    assert_eq!(put(&bob, policy.clone()).await.0, StatusCode::FORBIDDEN);
    let bad = json!({ "repository": "nginx", "pattern": "semver:nope" });
    assert_eq!(put(&alice, bad).await.0, StatusCode::UNPROCESSABLE_ENTITY);
    let fast = json!({ "repository": "nginx", "pattern": "latest", "intervalSecs": 5 });
    assert_eq!(put(&alice, fast).await.0, StatusCode::UNPROCESSABLE_ENTITY);
    let (status, _, saved) = put(&alice, policy.clone()).await;
    assert_eq!(
        (status, saved["repository"].clone()),
        (StatusCode::OK, json!("docker.io/library/nginx")),
        "{saved}"
    );

    let keyring = Arc::new(kuben_platform::secrets::Keyring::from_keys([(1, [7; 32])]));
    let watcher = kuben_api::image_watch::Watcher::new(app.store.clone(), images(), Some(keyring));
    assert_eq!(watcher.pass().await.expect("pass"), 1);
    let (_, followed) = send(&app.router, get(path, &alice)).await;
    assert_eq!(
        (followed["lastTag"].clone(), followed["lastDigest"].clone()),
        (json!("1.27"), json!(NGINX_127)),
        "{followed}"
    );
    assert!(followed["lastRun"].is_string() && followed["lastError"].is_null());
    let (_, runs) = send(&app.router, get(DEPLOYMENTS, &alice)).await;
    let newest = &runs.as_array().expect("runs")[0];
    assert_eq!(
        newest["phase"], "awaitingApproval",
        "production still needs an approval: {newest}"
    );
    assert_eq!(
        newest["requested_by"],
        target.to_string(),
        "the policy asked: {newest}"
    );
    let count = runs.as_array().map(Vec::len);
    assert_eq!(watcher.pass().await.expect("pass"), 0, "not due yet");
    put(&alice, policy).await;
    assert_eq!(watcher.pass().await.expect("pass"), 1);
    let (_, again) = send(&app.router, get(DEPLOYMENTS, &alice)).await;
    assert_eq!(
        again.as_array().map(Vec::len),
        count,
        "the same digest is not deployed twice"
    );
    assert_eq!(
        status_of(&app.router, "DELETE", path, &alice, None).await,
        StatusCode::NO_CONTENT
    );
    assert_eq!(
        send(&app.router, get(path, &alice)).await.0,
        StatusCode::NOT_FOUND
    );
}

/// M5.5: usage is shown when measured and reported unavailable otherwise,
/// never as zero.
#[tokio::test]
async fn m5_metrics_are_never_invented() {
    let Some(app) = setup().await else { return };
    let target = sql_app(&app).await;
    let alice = login(&app.router, "alice@example.com").await;
    let path = "/api/v1/projects/shop/environments/prod/apps/api/metrics";
    let (status, empty) = send(&app.router, get(path, &alice)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(
        (empty["available"].clone(), empty["points"].clone()),
        (json!(false), json!([])),
        "{empty}"
    );
    assert!(empty["reason"].as_str().is_some_and(|r| r.contains("no samples")));
    let now = kuben_core::time::now_ms();
    let key = kuben_platform::usage::SeriesKey {
        namespace: "kb-shop-prod".into(),
        app: "api".into(),
        org: app.org.to_string(),
    };
    let sample = kuben_platform::usage::Sample {
        at: now,
        cpu_millis: 250,
        memory_bytes: 64 << 20,
        pods: 2,
    };
    app.usage.record(key, sample);
    let (_, live) = send(&app.router, get(path, &alice)).await;
    assert_eq!(live["available"], true, "{live}");
    assert_eq!(
        (
            live["points"][0]["cpuMillis"].clone(),
            live["points"][0]["pods"].clone()
        ),
        (json!(250), json!(2))
    );

    let week = format!("{path}?window=7d");
    let (_, none) = send(&app.router, get(&week, &alice)).await;
    assert_eq!(none["available"], false);
    let mut t = app.store.tenant(app.org).await.expect("tenant");
    let hour = kuben_platform::usage::hour_of(now) - 3_600_000;
    t.keep_usage(target, (hour, 100, 400, 1_000, 2_000, 120))
        .await
        .expect("keep");
    t.commit().await.expect("commit");
    let (_, rolled) = send(&app.router, get(&week, &alice)).await;
    assert_eq!(
        (rolled["available"].clone(), rolled["points"][0]["cpuMax"].clone()),
        (json!(true), json!(400)),
        "{rolled}"
    );
    assert_eq!(
        send(&app.router, get(&format!("{path}?window=2d"), &alice))
            .await
            .0,
        StatusCode::UNPROCESSABLE_ENTITY
    );
}
