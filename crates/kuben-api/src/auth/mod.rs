//! Authentication: password login, opaque cookie sessions, bearer API tokens,
//! CSRF guard, login throttling. JWT/PASETO are deliberately **not** used for
//! browser sessions (ADR-005): opaque ids can be revoked instantly and are
//! never readable by JavaScript.

pub mod password;
pub mod session;
pub mod throttle;

use std::net::SocketAddr;

use axum::{
    Extension, Json, Router,
    extract::{ConnectInfo, FromRequestParts, Request, State},
    http::{HeaderMap, Method, StatusCode, header, request::Parts},
    middleware::Next,
    response::{IntoResponse, Response},
    routing::{get, post},
};
use axum_extra::extract::CookieJar;
use kuben_core::{
    Error,
    ids::{OrgId, TokenId},
    model::{TokenScope, User},
    time::now_ms,
};
use kuben_store::repo::{NewAudit, NewSession};
use serde::{Deserialize, Serialize};
use tokio::task::spawn_blocking;
use utoipa::ToSchema;
use utoipa_axum::{router::OpenApiRouter, routes};

pub use session::{COOKIE_NAME_DEV, COOKIE_NAME_SECURE, cookie_name};

use crate::{
    error::{ApiError, ApiResult},
    state::ApiState,
};

/// Header the SPA must send on every mutating request (CSRF defense in depth).
pub const CLIENT_HEADER: &str = "x-kuben-client";

/// What an API token grants (resolved from the `api_tokens` row).
#[derive(Clone, Debug)]
pub struct TokenGrant {
    pub id: TokenId,
    pub org: OrgId,
    pub scope: TokenScope,
}

/// Authenticated principal attached to the request.
#[derive(Clone, Debug)]
pub struct CurrentUser {
    pub user: User,
    /// `session` or `token`.
    pub via: &'static str,
    /// Set when authenticated with an API token.
    pub token: Option<TokenGrant>,
}

impl<S: Send + Sync> FromRequestParts<S> for CurrentUser {
    type Rejection = ApiError;

    async fn from_request_parts(parts: &mut Parts, _state: &S) -> Result<Self, Self::Rejection> {
        parts
            .extensions
            .get::<Self>()
            .cloned()
            .ok_or(ApiError(Error::Unauthorized))
    }
}

/// Resolve the session cookie (or bearer token) into a [`CurrentUser`]
/// extension. Never rejects by itself — handlers decide via the extractor.
pub async fn session_middleware(State(state): State<ApiState>, mut req: Request, next: Next) -> Response {
    if let Some(user) = resolve_user(&state, req.headers()).await {
        req.extensions_mut().insert(user);
    }
    next.run(req).await
}

async fn resolve_user(state: &ApiState, headers: &HeaderMap) -> Option<CurrentUser> {
    if let Some(bearer) = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
    {
        return session::user_from_api_token(state, bearer.trim()).await;
    }
    let jar = CookieJar::from_headers(headers);
    let raw = jar.get(cookie_name(&state.cfg))?.value().to_owned();
    session::user_from_session(state, &raw).await
}

/// Fetch-Metadata + custom-header CSRF guard for cookie-authenticated
/// mutations. Bearer-token requests are exempt (no ambient credentials).
pub async fn csrf_guard(req: Request, next: Next) -> Response {
    let mutating = !matches!(*req.method(), Method::GET | Method::HEAD | Method::OPTIONS);
    let has_bearer = req.headers().get(header::AUTHORIZATION).is_some();
    if mutating && !has_bearer {
        let site_ok = req
            .headers()
            .get("sec-fetch-site")
            .and_then(|v| v.to_str().ok())
            .is_none_or(|s| matches!(s, "same-origin" | "none"));
        let header_ok = req.headers().contains_key(CLIENT_HEADER);
        if !site_ok || !header_ok {
            return ApiError(Error::Forbidden).into_response();
        }
    }
    next.run(req).await
}

/// Client address for throttling and audit. Behind the Gateway the TCP peer
/// is the proxy, so the **last** `X-Forwarded-For` hop (appended by that
/// proxy) is used. The first hops are client-controlled and never trusted.
#[must_use]
pub fn client_ip(headers: &HeaderMap, peer: Option<SocketAddr>, trust_forwarded_for: bool) -> Option<String> {
    if trust_forwarded_for {
        let last = headers
            .get_all("x-forwarded-for")
            .iter()
            .filter_map(|v| v.to_str().ok())
            .flat_map(|v| v.split(','))
            .map(str::trim)
            .rfind(|s| !s.is_empty());
        if let Some(ip) = last {
            return Some(ip.to_owned());
        }
    }
    peer.map(|p| p.ip().to_string())
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

#[derive(Debug, Deserialize, ToSchema)]
pub struct LoginRequest {
    #[schema(example = "admin@kuben.local")]
    pub email: String,
    #[schema(example = "correct horse battery staple")]
    #[schema(value_type = String, format = Password)]
    pub password: secrecy::SecretString,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct UserDto {
    pub id: String,
    pub email: String,
    pub display_name: Option<String>,
    /// How the request was authenticated: `session` or `token`.
    pub via: &'static str,
    /// The temporary password must be replaced (`POST /me/password`) before
    /// anything else is allowed.
    pub must_change_password: bool,
}

impl From<&CurrentUser> for UserDto {
    fn from(c: &CurrentUser) -> Self {
        Self {
            id: c.user.id.to_string(),
            email: c.user.email.clone(),
            display_name: c.user.display_name.clone(),
            via: c.via,
            must_change_password: c.user.must_change_password,
        }
    }
}

/// Audit record for a login attempt (the middleware skips `login`, because
/// only the handler knows the attempted email).
async fn audit_login(
    state: &ApiState,
    account: Option<&User>,
    authenticated: bool,
    email: &str,
    ip: Option<String>,
    outcome: &str,
) {
    let org = match account {
        Some(user) => state
            .store
            .bindings_for_user(user.id)
            .await
            .ok()
            .and_then(|b| b.first().map(|b| b.org_id)),
        None => None,
    };
    let _ = state
        .store
        .append_audit(NewAudit {
            org_id: org,
            actor_kind: if authenticated { "user" } else { "anonymous" }.into(),
            actor_id: account.filter(|_| authenticated).map(|u| u.id.to_string()),
            action: "login".into(),
            outcome: outcome.into(),
            target_kind: Some("user".into()),
            target_ref: Some(email.to_owned()),
            ip,
            ..NewAudit::default()
        })
        .await;
}

/// Log in with email + password. Sets an `HttpOnly` session cookie.
/// Repeated failures are throttled (`429` with `Retry-After`).
#[utoipa::path(
    post,
    path = "/auth/login", operation_id = "login",
    tag = "auth",
    request_body = LoginRequest,
    responses(
        (status = 200, description = "Logged in", body = UserDto),
        (status = 401, description = "Bad credentials", body = crate::error::Problem),
        (status = 429, description = "Too many failed attempts", body = crate::error::Problem),
    )
)]
pub async fn login(
    State(state): State<ApiState>,
    peer: Option<Extension<ConnectInfo<SocketAddr>>>,
    headers: HeaderMap,
    jar: CookieJar,
    Json(body): Json<LoginRequest>,
) -> ApiResult<(CookieJar, Json<UserDto>)> {
    use secrecy::ExposeSecret;

    let email = body.email.trim().to_ascii_lowercase();
    let peer = peer.map(|Extension(ConnectInfo(addr))| addr);
    let ip = client_ip(&headers, peer, state.cfg.security.trust_forwarded_for);
    if let Err(retry_after_secs) = state.throttle.check(&email, ip.as_deref()).await {
        audit_login(&state, None, false, &email, ip, "throttled").await;
        return Err(ApiError(Error::RateLimited { retry_after_secs }));
    }
    let creds = state.store.find_user_by_email(&email).await?;

    // Always run a hash verification so timing does not reveal whether the
    // account exists.
    let hash = creds
        .as_ref()
        .and_then(|c| c.password_hash.clone())
        .unwrap_or_else(|| state.hasher.dummy_hash());
    let password = body.password.expose_secret().to_owned();
    let hasher = state.hasher.clone();
    let _permit = state.login_permits.acquire().await.map_err(Error::internal)?;
    let ok = spawn_blocking(move || hasher.verify(&password, &hash))
        .await
        .map_err(Error::internal)?;

    let account = creds.as_ref().map(|c| c.user.clone());
    let Some(creds) = creds.filter(|c| ok && c.user.is_active && c.password_hash.is_some()) else {
        state.throttle.record_failure(&email, ip.as_deref()).await;
        audit_login(&state, account.as_ref(), false, &email, ip, "failure").await;
        return Err(ApiError(Error::Unauthorized));
    };
    state.throttle.record_success(&email, ip.as_deref()).await;

    let (raw, id_hash) = session::new_session_id();
    let expires_at = kuben_core::time::plus_hours(now_ms(), state.cfg.security.session_ttl_hours);
    state
        .store
        .create_session(NewSession {
            id_hash,
            user_id: creds.user.id,
            expires_at,
            ip: ip.clone(),
            ua_hash: headers
                .get(header::USER_AGENT)
                .map(|ua| session::sha256(ua.as_bytes())),
        })
        .await?;
    audit_login(&state, Some(&creds.user), true, &email, ip, "success").await;

    let current = CurrentUser {
        user: creds.user,
        via: "session",
        token: None,
    };
    let dto = UserDto::from(&current);
    Ok((jar.add(session::build_cookie(&state.cfg, raw)), Json(dto)))
}

/// Log out: revoke the current session and clear the cookie.
#[utoipa::path(post, path = "/auth/logout", operation_id = "logout", tag = "auth", responses((status = 204, description = "Logged out")))]
pub async fn logout(State(state): State<ApiState>, jar: CookieJar) -> ApiResult<(CookieJar, StatusCode)> {
    if let Some(cookie) = jar.get(cookie_name(&state.cfg)) {
        let id_hash = session::sha256(cookie.value().as_bytes());
        state.store.revoke_session(&id_hash).await?;
        state.session_cache.invalidate(&id_hash).await;
    }
    Ok((
        jar.remove(session::removal_cookie(&state.cfg)),
        StatusCode::NO_CONTENT,
    ))
}

/// The authenticated user.
#[utoipa::path(get, path = "/me", operation_id = "getMe", tag = "auth", responses(
    (status = 200, body = UserDto),
    (status = 401, description = "Not authenticated", body = crate::error::Problem)
))]
pub async fn me(current: CurrentUser) -> Json<UserDto> {
    Json(UserDto::from(&current))
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct ChangePassword {
    #[schema(value_type = String, format = Password)]
    pub current_password: secrecy::SecretString,
    #[schema(value_type = String, format = Password)]
    pub new_password: secrecy::SecretString,
}

/// Replace the caller's password. Every other session is signed out.
#[utoipa::path(
    post,
    path = "/me/password", operation_id = "changePassword",
    tag = "auth",
    request_body = ChangePassword,
    responses(
        (status = 204, description = "Password changed"),
        (status = 403, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn change_password(
    State(state): State<ApiState>,
    current: CurrentUser,
    jar: CookieJar,
    Json(body): Json<ChangePassword>,
) -> ApiResult<StatusCode> {
    use secrecy::ExposeSecret;

    if current.token.is_some() {
        return Err(ApiError(Error::Forbidden));
    }
    let new = body.new_password.expose_secret().to_owned();
    let old = body.current_password.expose_secret().to_owned();
    let min = state.cfg.security.password_min_length;
    if new.chars().count() < min {
        return Err(Error::Validation(format!("the new password needs at least {min} characters")).into());
    }
    if new == old {
        return Err(Error::Validation("the new password must differ from the current one".into()).into());
    }
    let stored = state
        .store
        .find_user_by_email(&current.user.email)
        .await?
        .and_then(|c| c.password_hash)
        .unwrap_or_else(|| state.hasher.dummy_hash());
    let hasher = state.hasher.clone();
    let _permit = state.login_permits.acquire().await.map_err(Error::internal)?;
    let (ok, hash) = spawn_blocking(move || {
        let ok = hasher.verify(&old, &stored);
        (ok, ok.then(|| hasher.hash(&new)))
    })
    .await
    .map_err(Error::internal)?;
    let hash = match (ok, hash) {
        (true, Some(Ok(hash))) => hash,
        (true, Some(Err(e))) => return Err(Error::internal(e).into()),
        _ => return Err(Error::Validation("the current password is incorrect".into()).into()),
    };
    state.store.set_password_hash(current.user.id, &hash).await?;
    let keep = jar
        .get(cookie_name(&state.cfg))
        .map(|c| session::sha256(c.value().as_bytes()))
        .unwrap_or_default();
    state.store.revoke_other_sessions(current.user.id, &keep).await?;
    state.session_cache.invalidate_all();
    Ok(StatusCode::NO_CONTENT)
}

pub fn openapi_router() -> OpenApiRouter<ApiState> {
    OpenApiRouter::new()
        .routes(routes!(login))
        .routes(routes!(logout))
        .routes(routes!(me))
        .routes(routes!(change_password))
}

/// Plain router (used by tests that do not need OpenAPI).
pub fn plain_router() -> Router<ApiState> {
    Router::new()
        .route("/auth/login", post(login))
        .route("/auth/logout", post(logout))
        .route("/me", get(me))
        .route("/me/password", post(change_password))
}

#[cfg(test)]
mod tests {
    use axum::http::HeaderValue;

    use super::*;

    #[test]
    fn client_ip_trusts_only_the_last_forwarded_hop() {
        let mut h = HeaderMap::new();
        h.insert("x-forwarded-for", HeaderValue::from_static("1.2.3.4, 10.0.0.7"));
        let peer: SocketAddr = "10.1.1.1:5000".parse().expect("addr");
        assert_eq!(client_ip(&h, Some(peer), true).as_deref(), Some("10.0.0.7"));
        assert_eq!(
            client_ip(&h, Some(peer), false).as_deref(),
            Some("10.1.1.1"),
            "not trusted → peer"
        );
        assert_eq!(client_ip(&HeaderMap::new(), None, true), None);
    }
}
