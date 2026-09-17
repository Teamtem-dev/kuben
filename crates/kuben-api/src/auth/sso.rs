//! Browser sign-in with the identity provider (M4.3).
//!
//! `start` remembers the sign-in (state hash, nonce, PKCE verifier) and
//! binds it to this browser with a short-lived cookie; `callback` accepts
//! only a state that matches both, once, then opens an ordinary session.
//! Every failure ends on the sign-in page with the same message; the reason
//! goes to the log and the audit record.

use std::net::SocketAddr;

use axum::{
    Extension, Json,
    extract::{ConnectInfo, Query, State},
    http::{HeaderMap, StatusCode, header},
    response::{IntoResponse, Response},
};
use axum_extra::extract::{
    CookieJar,
    cookie::{Cookie, SameSite},
};
use kuben_core::{
    Error,
    sso::{LOGIN_WINDOW_SECS, safe_return_to},
    time::now_ms,
};
use kuben_store::repo::{PendingSso, SsoSignIn};
use serde::{Deserialize, Serialize};
use subtle::ConstantTimeEq;
use utoipa::{IntoParams, ToSchema};

use super::{audit_login, client_ip, session, start_session};
use crate::{
    error::ApiResult,
    sso::{SsoClient, random_value},
    state::ApiState,
};

/// The cookie that binds a started sign-in to the browser.
pub const STATE_COOKIE: &str = "kuben_sso";
const COOKIE_PATH: &str = "/api/v1/auth/sso";
/// Where a failed sign-in lands.
const FAILED: &str = "/login?error=sso";

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct SsoInfo {
    pub enabled: bool,
    /// The sign-in button's label.
    pub display_name: Option<String>,
    /// Where the button leads.
    pub start_url: Option<String>,
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
#[serde(rename_all = "camelCase")]
pub struct StartQuery {
    /// A path on this site to return to after signing in.
    pub return_to: Option<String>,
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct CallbackQuery {
    pub code: Option<String>,
    pub state: Option<String>,
    /// Set by the provider when the person or the provider refused.
    pub error: Option<String>,
}

fn state_cookie(secure: bool, value: String, max_age_secs: i64) -> Cookie<'static> {
    let mut cookie = Cookie::build((STATE_COOKIE, value))
        .path(COOKIE_PATH)
        .http_only(true)
        // The callback is a top-level navigation from the provider.
        .same_site(SameSite::Lax)
        .max_age(time::Duration::seconds(max_age_secs));
    if secure {
        cookie = cookie.secure(true);
    }
    cookie.build()
}

fn redirect(to: &str) -> Response {
    (StatusCode::SEE_OTHER, [(header::LOCATION, to.to_owned())]).into_response()
}

fn client(state: &ApiState) -> Result<&SsoClient, Error> {
    state
        .sso
        .as_deref()
        .ok_or_else(|| Error::NotFound("single sign-on is not configured".into()))
}

/// Whether single sign-on is offered, for the sign-in page.
#[utoipa::path(
    get,
    path = "/auth/sso", operation_id = "getSsoInfo",
    tag = "auth",
    responses((status = 200, body = SsoInfo))
)]
pub async fn info(State(state): State<ApiState>) -> Json<SsoInfo> {
    Json(state.sso.as_deref().map_or(
        SsoInfo {
            enabled: false,
            display_name: None,
            start_url: None,
        },
        |c| SsoInfo {
            enabled: true,
            display_name: Some(c.display_name().to_owned()),
            start_url: Some(format!("{COOKIE_PATH}/start")),
        },
    ))
}

/// Start signing in: redirects to the identity provider.
#[utoipa::path(
    get,
    path = "/auth/sso/start", operation_id = "startSso",
    tag = "auth",
    params(StartQuery),
    responses(
        (status = 303, description = "To the identity provider"),
        (status = 404, body = crate::error::Problem, description = "Single sign-on is not configured"),
        (status = 503, body = crate::error::Problem),
    )
)]
pub async fn start(
    State(state): State<ApiState>,
    jar: CookieJar,
    Query(q): Query<StartQuery>,
) -> ApiResult<(CookieJar, Response)> {
    let sso = client(&state)?;
    let (raw_state, nonce, verifier) = (random_value(), random_value(), random_value());
    let url = sso
        .authorize_url(&raw_state, &nonce, &verifier)
        .await
        .map_err(|e| Error::Unavailable(e.to_string()))?;
    let pending = PendingSso {
        nonce,
        verifier,
        return_to: safe_return_to(q.return_to.as_deref()),
    };
    state
        .store
        .begin_sso(
            &session::sha256(raw_state.as_bytes()),
            &pending,
            now_ms() + LOGIN_WINDOW_SECS * 1000,
        )
        .await?;
    let jar = jar.add(state_cookie(
        state.cfg.cookie_secure(),
        raw_state,
        LOGIN_WINDOW_SECS,
    ));
    Ok((jar, redirect(&url)))
}

/// The identity provider's answer: signs the person in and redirects.
#[utoipa::path(
    get,
    path = "/auth/sso/callback", operation_id = "finishSso",
    tag = "auth",
    params(CallbackQuery),
    responses((status = 303, description = "Signed in, or back to the sign-in page"))
)]
pub async fn callback(
    State(state): State<ApiState>,
    peer: Option<Extension<ConnectInfo<SocketAddr>>>,
    headers: HeaderMap,
    jar: CookieJar,
    Query(q): Query<CallbackQuery>,
) -> (CookieJar, Response) {
    let ip = client_ip(
        &headers,
        peer.map(|Extension(ConnectInfo(a))| a),
        &state.cfg.security,
    );
    let cookie = jar.get(STATE_COOKIE).map(|c| c.value().to_owned());
    let jar = jar.remove(state_cookie(state.cfg.cookie_secure(), String::new(), 0));
    match finish(&state, &headers, ip.clone(), cookie.as_deref(), &q).await {
        Ok((session_cookie, to)) => (jar.add(session_cookie), redirect(&to)),
        Err(reason) => {
            tracing::warn!(%reason, "a single sign-on was refused");
            audit_login(&state, None, false, "sso", ip, "failure").await;
            (jar, redirect(FAILED))
        }
    }
}

async fn finish(
    state: &ApiState,
    headers: &HeaderMap,
    ip: Option<String>,
    cookie: Option<&str>,
    q: &CallbackQuery,
) -> Result<(Cookie<'static>, String), String> {
    let sso = state.sso.as_deref().ok_or("not configured")?;
    if let Some(error) = &q.error {
        return Err(format!("the provider answered {error}"));
    }
    let (Some(code), Some(returned)) = (q.code.as_deref(), q.state.as_deref()) else {
        return Err("no code or state".into());
    };
    let bound = cookie.is_some_and(|c| bool::from(c.as_bytes().ct_eq(returned.as_bytes())));
    if !bound {
        return Err("the state does not belong to this browser".into());
    }
    let pending = state
        .store
        .take_sso(&session::sha256(returned.as_bytes()))
        .await
        .map_err(|e| e.to_string())?
        .ok_or("the sign-in is unknown, used or expired")?;
    let person = sso
        .sign_in(code, &pending.verifier, &pending.nonce)
        .await
        .map_err(|e| e.to_string())?;
    let org = state
        .store
        .find_org_by_slug(sso.org_slug())
        .await
        .map_err(|e| e.to_string())?
        .ok_or_else(|| format!("organization `{}` does not exist", sso.org_slug()))?;
    let user = match state
        .store
        .sso_sign_in(org.id, sso.issuer(), &person)
        .await
        .map_err(|e| e.to_string())?
    {
        SsoSignIn::SignedIn(user) => user,
        SsoSignIn::Inactive => return Err(format!("{} is deactivated", person.email)),
    };
    audit_login(state, Some(&user), true, &person.email, ip.clone(), "success").await;
    let (cookie, _) = start_session(state, user, headers, ip)
        .await
        .map_err(|e| e.to_string())?;
    Ok((cookie, pending.return_to))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_state_cookie_is_scoped_and_short_lived() {
        let c = state_cookie(true, "abc".into(), LOGIN_WINDOW_SECS);
        assert_eq!(c.path(), Some(COOKIE_PATH));
        assert_eq!(c.http_only(), Some(true));
        assert_eq!(c.same_site(), Some(SameSite::Lax));
        assert_eq!(c.max_age(), Some(time::Duration::minutes(10)));
        assert_eq!(c.secure(), Some(true));
        assert_eq!(state_cookie(false, String::new(), 0).secure(), None);
        let r = redirect("/x");
        assert_eq!(r.status(), StatusCode::SEE_OTHER);
        assert_eq!(r.headers()[header::LOCATION], "/x");
    }
}
