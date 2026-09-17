//! OIDC token verification for external CI (M4.2).
//!
//! Only RS256 is accepted (no `none`, no HMAC with a public key as secret),
//! the header must name its key, and keys come only from the configured
//! issuer's JWKS URL, never from the token. Keys are cached and refreshed at
//! most once a minute when a token names an unknown key.

use std::{
    fmt,
    time::{Duration, Instant},
};

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use bytes::Bytes;
use http::{Method, Request, StatusCode, header};
use http_body_util::{BodyExt, Full, Limited};
use kuben_core::ci::{GithubClaims, TokenError};
use ring::signature;
use serde::Deserialize;
use tokio::sync::RwLock;

use crate::transport::{Schemes, Transport, chain};

const TIMEOUT: Duration = Duration::from_secs(10);
const MAX_JWKS: usize = 256 << 10;
const MAX_TOKEN: usize = 16 << 10;
/// Keys are read again after this long.
const KEYS_TTL: Duration = Duration::from_mins(10);
/// An unknown key triggers a refresh at most this often.
const REFRESH_FLOOR: Duration = Duration::from_mins(1);

/// Why a token was not accepted. The caller answers every one the same way.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum OidcError {
    #[error("the token is malformed")]
    Malformed,
    #[error("the token is not signed with RS256")]
    Algorithm,
    #[error("the token names no known key")]
    UnknownKey,
    #[error("the token signature is wrong")]
    Signature,
    #[error("{0}")]
    Claims(TokenError),
    #[error("the issuer's keys are unavailable: {0}")]
    Unavailable(String),
}

/// One JSON Web Key.
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Jwk {
    #[serde(default)]
    pub kid: Option<String>,
    pub kty: String,
    #[serde(default)]
    pub alg: Option<String>,
    #[serde(default)]
    pub n: Option<String>,
    #[serde(default)]
    pub e: Option<String>,
}

/// A JSON Web Key Set.
#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
pub struct Jwks {
    pub keys: Vec<Jwk>,
}

#[derive(Deserialize)]
struct Header {
    alg: String,
    #[serde(default)]
    kid: Option<String>,
    #[serde(default)]
    crit: Option<serde_json::Value>,
}

fn part(text: &str) -> Result<Vec<u8>, OidcError> {
    URL_SAFE_NO_PAD.decode(text).map_err(|_| OidcError::Malformed)
}

/// The payload of `token` if its RS256 signature verifies with a key of
/// `jwks`.
pub fn verify_rs256(token: &str, jwks: &Jwks) -> Result<Vec<u8>, OidcError> {
    if token.len() > MAX_TOKEN {
        return Err(OidcError::Malformed);
    }
    let mut parts = token.split('.');
    let (Some(head), Some(body), Some(sig), None) = (parts.next(), parts.next(), parts.next(), parts.next())
    else {
        return Err(OidcError::Malformed);
    };
    let header: Header = serde_json::from_slice(&part(head)?).map_err(|_| OidcError::Malformed)?;
    if header.alg != "RS256" || header.crit.is_some() {
        return Err(OidcError::Algorithm);
    }
    let kid = header.kid.ok_or(OidcError::UnknownKey)?;
    let key = jwks
        .keys
        .iter()
        .find(|k| {
            k.kid.as_deref() == Some(kid.as_str())
                && k.kty == "RSA"
                && k.alg.as_deref().is_none_or(|a| a == "RS256")
        })
        .ok_or(OidcError::UnknownKey)?;
    let (Some(n), Some(e)) = (key.n.as_deref(), key.e.as_deref()) else {
        return Err(OidcError::UnknownKey);
    };
    let public = signature::RsaPublicKeyComponents {
        n: part(n)?,
        e: part(e)?,
    };
    let message = format!("{head}.{body}");
    public
        .verify(
            &signature::RSA_PKCS1_2048_8192_SHA256,
            message.as_bytes(),
            &part(sig)?,
        )
        .map_err(|_| OidcError::Signature)?;
    part(body)
}

struct Keys {
    jwks: Jwks,
    fetched: Option<Instant>,
}

/// Verifies GitHub Actions OIDC tokens for one issuer and audience.
pub struct GithubOidc {
    issuer: String,
    audience: String,
    jwks_url: String,
    transport: Transport,
    keys: RwLock<Keys>,
    /// Keys given up front (tests): never fetched.
    fixed: bool,
    /// Unix seconds now.
    clock: fn() -> i64,
}

fn system_clock() -> i64 {
    kuben_core::time::now_ms() / 1000
}

impl fmt::Debug for GithubOidc {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("GithubOidc")
            .field("issuer", &self.issuer)
            .field("audience", &self.audience)
            .finish_non_exhaustive()
    }
}

impl GithubOidc {
    /// A verifier that reads the keys of `issuer` from its JWKS URL.
    #[must_use]
    pub fn new(issuer: &str, audience: &str) -> Self {
        let issuer = issuer.trim_end_matches('/').to_owned();
        Self {
            jwks_url: format!("{issuer}/.well-known/jwks"),
            issuer,
            audience: audience.to_owned(),
            transport: Transport::new(Schemes::HttpsOnly, TIMEOUT),
            keys: RwLock::new(Keys {
                jwks: Jwks::default(),
                fetched: None,
            }),
            fixed: false,
            clock: system_clock,
        }
    }

    /// The same verifier reading the time from `clock` (tests).
    #[must_use]
    pub fn with_clock(mut self, clock: fn() -> i64) -> Self {
        self.clock = clock;
        self
    }

    /// A verifier with fixed keys, for tests and air-gapped installs.
    #[must_use]
    pub fn with_keys(issuer: &str, audience: &str, jwks: Jwks) -> Self {
        Self {
            keys: RwLock::new(Keys {
                jwks,
                fetched: Some(Instant::now()),
            }),
            fixed: true,
            ..Self::new(issuer, audience)
        }
    }

    #[must_use]
    pub fn issuer(&self) -> &str {
        &self.issuer
    }

    async fn fetch(&self) -> Result<Jwks, OidcError> {
        let unavailable = |e: String| OidcError::Unavailable(e);
        let request = Request::builder()
            .method(Method::GET)
            .uri(&self.jwks_url)
            .header(header::ACCEPT, "application/json")
            .header(header::USER_AGENT, concat!("kuben/", env!("CARGO_PKG_VERSION")))
            .body(Full::new(Bytes::new()))
            .map_err(|e| unavailable(e.to_string()))?;
        let response = self.transport.send(request).await.map_err(unavailable)?;
        let (parts, body) = response.into_parts();
        if parts.status != StatusCode::OK {
            return Err(unavailable(format!("HTTP {}", parts.status)));
        }
        let body = tokio::time::timeout(TIMEOUT, Limited::new(body, MAX_JWKS).collect())
            .await
            .map_err(|_| unavailable("timed out".into()))?
            .map_err(|e| unavailable(chain(&*e)))?
            .to_bytes();
        serde_json::from_slice(&body).map_err(|e| unavailable(e.to_string()))
    }

    /// The current keys; fetched when stale, or when `unknown_key` and the
    /// last fetch is older than the refresh floor.
    async fn keys(&self, unknown_key: bool) -> Result<Jwks, OidcError> {
        {
            let keys = self.keys.read().await;
            let age = keys.fetched.map(|at| at.elapsed());
            let fresh = age.is_some_and(|a| a < KEYS_TTL && !(unknown_key && a >= REFRESH_FLOOR));
            if self.fixed || fresh {
                return Ok(keys.jwks.clone());
            }
        }
        let mut keys = self.keys.write().await;
        match self.fetch().await {
            Ok(jwks) => {
                keys.jwks = jwks;
                keys.fetched = Some(Instant::now());
                Ok(keys.jwks.clone())
            }
            // Keep answering with the old keys while the issuer is down.
            Err(e) if keys.fetched.is_some() && !unknown_key => {
                tracing::warn!(error = %e, "OIDC keys not refreshed; the cached ones stay in use");
                Ok(keys.jwks.clone())
            }
            Err(e) => Err(e),
        }
    }

    /// The claims of `token`, if it is a valid token of this issuer for this
    /// audience now.
    pub async fn verify(&self, token: &str) -> Result<GithubClaims, OidcError> {
        self.verify_at(token, (self.clock)()).await
    }

    /// The claims of `token` at `now` (Unix seconds).
    pub async fn verify_at(&self, token: &str, now: i64) -> Result<GithubClaims, OidcError> {
        let jwks = self.keys(false).await?;
        let payload = match verify_rs256(token, &jwks) {
            Err(OidcError::UnknownKey) if !self.fixed => verify_rs256(token, &self.keys(true).await?)?,
            other => other?,
        };
        let claims: GithubClaims = serde_json::from_slice(&payload).map_err(|_| OidcError::Malformed)?;
        claims
            .check(&self.issuer, &self.audience, now)
            .map_err(OidcError::Claims)?;
        Ok(claims)
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use kuben_core::ci::GITHUB_ACTIONS_ISSUER;

    use super::*;

    const FIXTURE: &str = include_str!("testdata/github-oidc.json");
    /// Between the fixture tokens' `iat` and `exp`.
    pub(crate) const NOW: i64 = 1_800_000_100;
    pub(crate) const AUDIENCE: &str = "https://kuben.example.com";

    pub(crate) fn fixture() -> (Jwks, serde_json::Value) {
        let all: serde_json::Value = serde_json::from_str(FIXTURE).expect("fixture");
        let jwks = serde_json::from_value(all["jwks"].clone()).expect("jwks");
        (jwks, all["tokens"].clone())
    }

    pub(crate) fn verifier() -> GithubOidc {
        GithubOidc::with_keys(GITHUB_ACTIONS_ISSUER, AUDIENCE, fixture().0)
    }

    fn token(name: &str) -> String {
        fixture().1[name].as_str().expect("token").to_owned()
    }

    #[tokio::test]
    async fn a_signed_github_token_is_accepted() {
        let claims = verifier().verify_at(&token("valid"), NOW).await.expect("valid");
        assert_eq!(claims.repository_id, 123_456);
        assert_eq!(claims.git_ref, "refs/heads/main");
        assert_eq!(claims.environment.as_deref(), Some("production"));
    }

    #[tokio::test]
    async fn forged_and_foreign_tokens_are_refused() {
        let v = verifier();
        let refused = [
            ("tampered", OidcError::Signature),
            ("unknown_kid", OidcError::UnknownKey),
            ("wrong_aud", OidcError::Claims(TokenError::Audience)),
            ("wrong_iss", OidcError::Claims(TokenError::Issuer)),
        ];
        for (name, error) in refused {
            assert_eq!(v.verify_at(&token(name), NOW).await, Err(error), "{name}");
        }
        assert_eq!(
            v.verify_at(&token("valid"), 1_800_000_300 + 61).await,
            Err(OidcError::Claims(TokenError::Expired))
        );
    }

    #[test]
    fn only_rs256_with_a_named_key_is_considered() {
        let (jwks, _) = fixture();
        let valid = token("valid");
        let (_, rest) = valid.split_once('.').expect("jwt");
        for header in [
            r#"{"alg":"none","kid":"kuben-test-1"}"#,
            r#"{"alg":"HS256","kid":"kuben-test-1"}"#,
            r#"{"alg":"RS256","kid":"kuben-test-1","crit":["exp"]}"#,
        ] {
            let forged = format!("{}.{rest}", URL_SAFE_NO_PAD.encode(header));
            assert_eq!(
                verify_rs256(&forged, &jwks),
                Err(OidcError::Algorithm),
                "{header}"
            );
        }
        let no_kid = format!("{}.{rest}", URL_SAFE_NO_PAD.encode(r#"{"alg":"RS256"}"#));
        assert_eq!(verify_rs256(&no_kid, &jwks), Err(OidcError::UnknownKey));
        for malformed in ["", "a.b", "a.b.c.d", "!!.!!.!!", &"a".repeat(MAX_TOKEN + 1)] {
            assert_eq!(
                verify_rs256(malformed, &jwks),
                Err(OidcError::Malformed),
                "{malformed:.10}"
            );
        }
        let hmac_key = Jwks {
            keys: vec![Jwk {
                kid: Some("kuben-test-1".into()),
                kty: "oct".into(),
                alg: Some("HS256".into()),
                n: None,
                e: None,
            }],
        };
        assert_eq!(verify_rs256(&valid, &hmac_key), Err(OidcError::UnknownKey));
    }

    #[tokio::test]
    async fn the_clock_decides_expiry() {
        let late = verifier().with_clock(|| 1_900_000_000);
        assert_eq!(
            late.verify(&token("valid")).await,
            Err(OidcError::Claims(TokenError::Expired))
        );
        assert!(
            verifier()
                .with_clock(|| NOW)
                .verify(&token("valid"))
                .await
                .is_ok()
        );
    }

    #[test]
    fn the_jwks_url_comes_from_the_issuer() {
        let v = GithubOidc::new("https://token.actions.githubusercontent.com/", AUDIENCE);
        assert_eq!(
            v.jwks_url,
            "https://token.actions.githubusercontent.com/.well-known/jwks"
        );
        assert_eq!(v.issuer(), GITHUB_ACTIONS_ISSUER);
    }
}
