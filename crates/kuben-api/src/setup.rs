//! First run: `GET /setup` says whether an admin account still has to be
//! created, and `POST /setup` creates it from the console and signs in.
//!
//! Anyone who reaches the port before the owner could otherwise claim the
//! installation, so on a non-loopback address the request must carry the
//! setup token: 128 random bits the server writes to [`TOKEN_FILE`] in the
//! state directory (owner-only) and the installer prints as part of the
//! setup link. It expires after [`TOKEN_TTL`]; `kuben setup-token` prints a
//! fresh one. Once a user exists the endpoint answers 404 for good.

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
    let query = token.map(|t| format!("?token={t}")).unwrap_or_default();
    format!("{}/setup{query}", console_url(cfg))
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
pub async fn status(State(state): State<ApiState>) -> ApiResult<Json<SetupStatus>> {
    let needed = needed(&state).await?;
    Ok(Json(SetupStatus {
        needed,
        token_required: needed && token_required(&state.cfg),
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
        (status = 403, description = "Missing, wrong or expired setup token", body = crate::error::Problem),
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

    let peer = peer.map(|axum::Extension(axum::extract::ConnectInfo(addr))| addr);
    let ip = client_ip(&headers, peer, state.cfg.security.trust_forwarded_for);
    let (cookie, dto) = auth::start_session(&state, user, &headers, ip).await?;
    Ok((jar.add(cookie), Json(dto)))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg_in(dir: &std::path::Path, bind: &str) -> Config {
        let mut cfg = Config::default();
        cfg.database.url = format!("sqlite://{}", dir.join("kuben.db").display());
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
            format!("{}/setup?token={token}", console_url(&public))
        );

        let local = cfg_in(&dir, "127.0.0.1:3000");
        assert!(verify_token(&local, None).is_ok(), "loopback needs no token");
        assert!(!token_required(&local));
        std::fs::remove_dir_all(&dir).ok();
    }
}
