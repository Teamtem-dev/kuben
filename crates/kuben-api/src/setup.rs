//! First run: `GET /setup` says whether an admin account still has to be
//! created, and `POST /setup` creates it from the console and signs in.
//!
//! Anyone who reaches the port before the owner could otherwise claim the
//! installation, so on a non-loopback address the request must carry the
//! setup token: 128 random bits the server writes to [`TOKEN_FILE`] in the
//! state directory (owner-only) and the installer prints as part of the
//! setup link. It expires after [`TOKEN_TTL`]; `kuben setup-token` prints a
//! fresh one. Once a user exists the endpoint answers 404 for good.
//!
//! The token travels in the link's fragment (`/setup#token=…`), which the
//! browser never sends to a server nor puts in a `Referer`, and only in the
//! body of `POST /setup`. The admin's password is never taken over plain
//! HTTP from another machine (ADR-031): the request must come over HTTPS (a
//! trusted proxy's `X-Forwarded-Proto`), from this machine (an SSH tunnel),
//! or to a console that listens on loopback only, unless
//! `security.insecure_setup` says otherwise.

use std::{
    path::PathBuf,
    time::{Duration, SystemTime},
};

use axum::{Json, extract::State, http::HeaderMap};
use axum_extra::extract::CookieJar;
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use kuben_core::{Error, config::Config, perm::Role};
use serde::{Deserialize, Serialize};
use subtle::ConstantTimeEq as _;
use tokio::task::spawn_blocking;
use utoipa::ToSchema;

use crate::{
    auth::{self, UserDto, client_ip},
    error::ApiResult,
    host::{console_url, write_owner_only},
    state::ApiState,
};

/// File that holds the current setup token, in [`Config::state_dir`].
pub const TOKEN_FILE: &str = "setup-token";
/// How long a setup token stays valid.
pub const TOKEN_TTL: Duration = Duration::from_mins(30);

#[must_use]
pub fn token_file(cfg: &Config) -> PathBuf {
    cfg.state_dir().join(TOKEN_FILE)
}

/// A token is needed unless the console is reachable from this machine only.
#[must_use]
pub fn token_required(cfg: &Config) -> bool {
    !cfg.bind_is_loopback()
}

/// Write a fresh token and return it.
pub fn issue_token(cfg: &Config) -> std::io::Result<String> {
    let bytes: [u8; 16] = rand::random();
    let token = URL_SAFE_NO_PAD.encode(bytes);
    let file = token_file(cfg);
    if let Some(dir) = file.parent() {
        std::fs::create_dir_all(dir)?;
    }
    write_owner_only(&file, &format!("{token}\n"))?;
    Ok(token)
}

/// The current token when it is still valid, else a fresh one.
pub fn current_or_new_token(cfg: &Config) -> std::io::Result<String> {
    match read_token(cfg) {
        Some((token, age)) if age <= TOKEN_TTL => Ok(token),
        _ => issue_token(cfg),
    }
}

/// The setup link the operator opens: the console URL plus the token when
/// one is needed.
#[must_use]
pub fn setup_url(cfg: &Config, token: Option<&str>) -> String {
    setup_url_at(&console_url(cfg), token)
}

/// The setup link on `console` (e.g. `http://localhost:3000` through a
/// tunnel); the token rides in the fragment.
#[must_use]
pub fn setup_url_at(console: &str, token: Option<&str>) -> String {
    let fragment = token.map(|t| format!("#token={t}")).unwrap_or_default();
    format!("{}/setup{fragment}", console.trim_end_matches('/'))
}

/// Whether the admin's password may travel over this connection.
#[must_use]
pub fn transport_secure(cfg: &Config, headers: &HeaderMap, peer: Option<std::net::SocketAddr>) -> bool {
    if cfg.bind_is_loopback() || cfg.security.insecure_setup {
        return true;
    }
    let trusted = cfg.security.trusts_forwarded(peer.map(|p| p.ip()));
    let https = trusted
        && headers
            .get("x-forwarded-proto")
            .and_then(|v| v.to_str().ok())
            .is_some_and(|v| {
                v.split(',')
                    .next_back()
                    .is_some_and(|p| p.trim().eq_ignore_ascii_case("https"))
            });
    let local = client_ip(headers, peer, &cfg.security)
        .and_then(|ip| ip.parse::<std::net::IpAddr>().ok())
        .is_some_and(|ip| ip.is_loopback());
    https || local
}

/// Where the operator finishes the setup: the link, and notes on how to
/// reach it. The console's own address when the password may travel there
/// (HTTPS, loopback, or `security.insecure_setup`); otherwise the same page
/// through an SSH tunnel, since the password never crosses plain HTTP from
/// another machine.
#[must_use]
pub fn setup_guide(cfg: &Config, token: Option<&str>) -> (String, Vec<String>) {
    let port = cfg.bind_port();
    let direct = setup_url(cfg, token);
    if cfg.bind_is_loopback() || cfg.security.insecure_setup {
        return (direct, Vec::new());
    }
    let server = crate::host::advertise_ip().map_or_else(|| "<this server>".to_owned(), |ip| ip.to_string());
    let ssh = format!("ssh -L {port}:127.0.0.1:{port} <you>@{server}");
    let tunnel = setup_url_at(&format!("http://localhost:{port}"), token);
    if direct.starts_with("https://") {
        let note = format!(
            "Until DNS and the certificate are ready: run `{ssh}` on your computer, then open {tunnel}"
        );
        return (direct, vec![note]);
    }
    (
        tunnel,
        vec![
            format!("Run `{ssh}` on your computer first; the admin password never travels over plain HTTP."),
            "For an HTTPS console: kuben setup --domain <domain> --acme-email <email>. On a network you \
             trust: kuben setup --allow-http-setup."
                .to_owned(),
        ],
    )
}

/// Why the admin cannot be created over this connection, and what works.
#[must_use]
pub fn insecure_transport_hint(cfg: &Config) -> String {
    let port = cfg.bind_port();
    format!(
        "the admin password is not sent over plain HTTP from another machine. Open the console over HTTPS, or \
         through an SSH tunnel: ssh -L {port}:127.0.0.1:{port} <you>@<this server>, then \
         http://localhost:{port}/setup#token=<token> (kuben setup-token prints it). To allow plain HTTP on a \
         trusted network, set security.insecure_setup = true"
    )
}

fn read_token(cfg: &Config) -> Option<(String, Duration)> {
    let file = token_file(cfg);
    let token = std::fs::read_to_string(&file).ok()?.trim().to_owned();
    let modified = std::fs::metadata(&file).ok()?.modified().ok()?;
    let age = SystemTime::now().duration_since(modified).unwrap_or_default();
    (!token.is_empty()).then_some((token, age))
}

fn verify_token(cfg: &Config, presented: Option<&str>) -> Result<(), Error> {
    if !token_required(cfg) {
        return Ok(());
    }
    let (Some(presented), Some((current, age))) = (presented, read_token(cfg)) else {
        return Err(Error::Forbidden);
    };
    if age > TOKEN_TTL || presented.as_bytes().ct_eq(current.as_bytes()).unwrap_u8() != 1 {
        return Err(Error::Forbidden);
    }
    Ok(())
}

#[derive(Debug, Serialize, ToSchema)]
pub struct SetupStatus {
    /// No admin account exists yet; the console shows `/setup`.
    pub needed: bool,
    /// `POST /setup` must carry the token the installer printed.
    pub token_required: bool,
    /// The admin may be created over this connection (HTTPS, this machine,
    /// or allowed by configuration); otherwise `POST /setup` answers 403.
    pub secure: bool,
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct SetupRequest {
    #[schema(example = "ACME")]
    pub org_name: String,
    #[schema(example = "you@example.com")]
    pub email: String,
    #[schema(value_type = String, format = Password)]
    pub password: secrecy::SecretString,
    /// The setup token from the installer's link, when required.
    pub token: Option<String>,
}

async fn needed(state: &ApiState) -> Result<bool, Error> {
    Ok(state.cfg.setup_wizard() && state.store.count_users().await? == 0)
}

/// Whether the first admin account still has to be created.
#[utoipa::path(get, path = "/setup", operation_id = "setupStatus", tag = "auth", responses(
    (status = 200, body = SetupStatus)
))]
pub async fn status(
    State(state): State<ApiState>,
    peer: Option<axum::Extension<axum::extract::ConnectInfo<std::net::SocketAddr>>>,
    headers: HeaderMap,
) -> ApiResult<Json<SetupStatus>> {
    let needed = needed(&state).await?;
    let peer = peer.map(|axum::Extension(axum::extract::ConnectInfo(addr))| addr);
    Ok(Json(SetupStatus {
        needed,
        token_required: needed && token_required(&state.cfg),
        secure: transport_secure(&state.cfg, &headers, peer),
    }))
}

/// Create the organization and its first admin (owner), and sign in.
#[utoipa::path(
    post,
    path = "/setup", operation_id = "setup",
    tag = "auth",
    request_body = SetupRequest,
    responses(
        (status = 200, description = "Admin created and signed in", body = UserDto),
        (status = 403, description = "Missing, wrong or expired setup token, or plain HTTP from another machine (`insecure_transport`)", body = crate::error::Problem),
        (status = 404, description = "Setup is already complete", body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn complete(
    State(state): State<ApiState>,
    peer: Option<axum::Extension<axum::extract::ConnectInfo<std::net::SocketAddr>>>,
    headers: HeaderMap,
    jar: CookieJar,
    Json(body): Json<SetupRequest>,
) -> ApiResult<(CookieJar, Json<UserDto>)> {
    use secrecy::ExposeSecret;

    // One setup at a time: two browsers cannot both become the first admin.
    let _guard = state.setup_lock.lock().await;
    if !needed(&state).await? {
        return Err(Error::NotFound("setup is complete; sign in instead".into()).into());
    }
    let peer = peer.map(|axum::Extension(axum::extract::ConnectInfo(addr))| addr);
    if !transport_secure(&state.cfg, &headers, peer) {
        return Err(Error::InsecureTransport(insecure_transport_hint(&state.cfg)).into());
    }
    verify_token(&state.cfg, body.token.as_deref())?;

    let email = body.email.trim().to_ascii_lowercase();
    if email.len() < 3 || !email.contains('@') {
        return Err(Error::Validation("enter a valid email address".into()).into());
    }
    let org_name = body.org_name.trim();
    if org_name.is_empty() {
        return Err(Error::Validation("the organization needs a name".into()).into());
    }
    let min = state.cfg.security.password_min_length;
    let password = body.password.expose_secret().to_owned();
    if password.chars().count() < min {
        return Err(Error::Validation(format!("the password needs at least {min} characters")).into());
    }

    let hasher = state.hasher.clone();
    let _permit = state.login_permits.acquire().await.map_err(Error::internal)?;
    let hash = spawn_blocking(move || hasher.hash(&password))
        .await
        .map_err(Error::internal)?
        .map_err(Error::internal)?;

    let slug = &state.cfg.bootstrap.org_slug;
    let org = match state.store.find_org_by_slug(slug).await? {
        Some(org) => org,
        None => state.store.create_org(slug, org_name).await?,
    };
    let user = state.store.create_user(&email, None, Some(&hash)).await?;
    state.store.add_membership(org.id, user.id).await?;
    state.store.bind_org_role(org.id, user.id, Role::Owner).await?;
    std::fs::remove_file(token_file(&state.cfg)).ok();
    tracing::info!(%email, org = %org.slug, "setup complete: admin account created");

    let ip = client_ip(&headers, peer, &state.cfg.security);
    let (cookie, dto) = auth::start_session(&state, user, &headers, ip).await?;
    Ok((jar.add(cookie), Json(dto)))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg_in(dir: &std::path::Path, bind: &str) -> Config {
        let mut cfg = Config::default();
        cfg.server.state_dir = Some(dir.display().to_string());
        cfg.server.bind = bind.into();
        cfg
    }

    #[test]
    fn token_is_checked_on_public_addresses_only() {
        let dir = std::env::temp_dir().join(format!("kuben-setup-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("dir");
        let public = cfg_in(&dir, "0.0.0.0:3000");
        assert!(verify_token(&public, None).is_err(), "no token file yet");
        let token = issue_token(&public).expect("issue");
        assert_eq!(current_or_new_token(&public).expect("reuse"), token);
        assert!(verify_token(&public, Some(&token)).is_ok());
        assert!(verify_token(&public, Some("wrong")).is_err());
        assert!(verify_token(&public, None).is_err());
        assert_eq!(
            setup_url(&public, Some(&token)),
            format!("{}/setup#token={token}", console_url(&public)),
            "the token rides in the fragment"
        );
        assert_eq!(
            setup_url_at("http://localhost:3000/", None),
            "http://localhost:3000/setup"
        );

        let local = cfg_in(&dir, "127.0.0.1:3000");
        assert!(verify_token(&local, None).is_ok(), "loopback needs no token");
        assert!(!token_required(&local));
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn the_password_travels_only_over_a_secure_path() {
        use axum::http::HeaderValue;
        let peer = |s: &str| Some(s.parse::<std::net::SocketAddr>().expect("addr"));
        let mut cfg = Config::default();
        cfg.server.bind = "0.0.0.0:3000".into();
        let plain = HeaderMap::new();
        assert!(
            !transport_secure(&cfg, &plain, peer("203.0.113.9:4000")),
            "plain HTTP from afar"
        );
        assert!(
            transport_secure(&cfg, &plain, peer("127.0.0.1:4000")),
            "an SSH tunnel"
        );
        let mut https = HeaderMap::new();
        https.insert("x-forwarded-proto", HeaderValue::from_static("https"));
        assert!(
            !transport_secure(&cfg, &https, peer("203.0.113.9:4000")),
            "a header anyone can send is not believed"
        );
        cfg.security.trust_forwarded_for = true;
        cfg.security.trusted_proxies = vec!["10.42.0.0/16".into()];
        assert!(
            transport_secure(&cfg, &https, peer("10.42.0.12:4000")),
            "HTTPS through the Gateway"
        );
        assert!(!transport_secure(&cfg, &https, peer("203.0.113.9:4000")));
        let mut spoofed = https.clone();
        spoofed.insert("x-forwarded-for", HeaderValue::from_static("127.0.0.1"));
        assert!(
            !transport_secure(&cfg, &spoofed, peer("203.0.113.9:4000")),
            "a direct client cannot claim to be local"
        );
        cfg.security.insecure_setup = true;
        assert!(
            transport_secure(&cfg, &plain, peer("203.0.113.9:4000")),
            "allowed on purpose"
        );
        cfg.security.insecure_setup = false;
        cfg.server.bind = "127.0.0.1:3000".into();
        assert!(transport_secure(&cfg, &plain, None), "a loopback-only console");
        assert!(insecure_transport_hint(&cfg).contains("ssh -L 3000:127.0.0.1:3000"));
    }
}
