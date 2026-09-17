//! The OpenID Connect client for single sign-on (M4.3): discovery, the
//! authorization URL with PKCE, the code exchange and ID token verification.
//!
//! The provider's discovery document must name the configured issuer, and
//! its keys and endpoints are used only from there. ID tokens are accepted
//! only when RS256-signed by one of those keys, for this client and this
//! sign-in's nonce.

use std::{fmt, sync::Arc, time::Duration};

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use bytes::Bytes;
use http::{Method, Request, StatusCode, header};
use http_body_util::{BodyExt, Full, Limited};
use kuben_core::{
    config::SsoCfg,
    sso::{IdClaims, SsoDenied, SsoPerson, SsoPolicy},
};
use secrecy::{ExposeSecret, SecretString};
use serde::Deserialize;
use sha2::{Digest as _, Sha256};
use tokio::sync::OnceCell;

use crate::{
    oidc::{HttpGet, HttpsGet, KeyCache, OidcError},
    transport::chain,
};

const TIMEOUT: Duration = Duration::from_secs(15);
const MAX_DOCUMENT: usize = 256 << 10;

/// Why a sign-in failed. People see one answer; the reason is logged.
#[derive(Debug, thiserror::Error)]
pub enum SsoError {
    #[error("the identity provider is unavailable: {0}")]
    Unavailable(String),
    #[error("the identity provider refused the code: {0}")]
    CodeRefused(String),
    #[error("the ID token was not accepted: {0}")]
    Token(#[from] OidcError),
    #[error("{0}")]
    Denied(#[from] SsoDenied),
    #[error("the SSO configuration is incomplete: {0}")]
    Config(String),
}

/// What sign-in needs from the provider, beyond reading documents.
#[async_trait::async_trait]
pub trait IdentityProvider: HttpGet {
    /// `POST` a form to `url` with HTTP Basic client authentication.
    async fn post_form(&self, url: &str, form: &str, basic: &str) -> Result<(StatusCode, Bytes), String>;
}

#[async_trait::async_trait]
impl IdentityProvider for HttpsGet {
    async fn post_form(&self, url: &str, form: &str, basic: &str) -> Result<(StatusCode, Bytes), String> {
        let request = Request::builder()
            .method(Method::POST)
            .uri(url)
            .header(header::ACCEPT, "application/json")
            .header(header::CONTENT_TYPE, "application/x-www-form-urlencoded")
            .header(header::AUTHORIZATION, format!("Basic {basic}"))
            .header(header::USER_AGENT, concat!("kuben/", env!("CARGO_PKG_VERSION")))
            .body(Full::new(Bytes::from(form.to_owned())))
            .map_err(|e| e.to_string())?;
        let response = self.transport().send(request).await?;
        let (parts, body) = response.into_parts();
        let body = tokio::time::timeout(TIMEOUT, Limited::new(body, MAX_DOCUMENT).collect())
            .await
            .map_err(|_| "timed out".to_owned())?
            .map_err(|e| chain(&*e))?
            .to_bytes();
        Ok((parts.status, body))
    }
}

/// The parts of the provider's discovery document Kuben uses.
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Discovery {
    pub issuer: String,
    pub authorization_endpoint: String,
    pub token_endpoint: String,
    pub jwks_uri: String,
}

struct Provider {
    discovery: Discovery,
    keys: KeyCache,
}

/// Percent-encode a query or form value.
fn encode(value: &str) -> String {
    value
        .bytes()
        .map(|b| {
            if b.is_ascii_alphanumeric() || matches!(b, b'-' | b'.' | b'_' | b'~') {
                char::from(b).to_string()
            } else {
                format!("%{b:02X}")
            }
        })
        .collect()
}

fn https(url: &str) -> bool {
    url.starts_with("https://")
}

/// The PKCE S256 challenge of `verifier`.
#[must_use]
pub fn challenge(verifier: &str) -> String {
    URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()))
}

/// A URL-safe random value of 32 bytes (43 characters).
#[must_use]
pub fn random_value() -> String {
    let bytes: [u8; 32] = rand::random();
    URL_SAFE_NO_PAD.encode(bytes)
}

#[derive(Deserialize)]
struct TokenResponse {
    id_token: Option<String>,
}

/// The single sign-on client of this installation.
pub struct SsoClient {
    issuer: String,
    client_id: String,
    client_secret: SecretString,
    redirect_uri: String,
    scopes: Vec<String>,
    group_claim: String,
    policy: SsoPolicy,
    display_name: String,
    org_slug: String,
    http: Arc<dyn IdentityProvider>,
    provider: OnceCell<Provider>,
    /// Keys given up front (tests).
    fixed_keys: Option<crate::oidc::Jwks>,
    clock: fn() -> i64,
}

impl fmt::Debug for SsoClient {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("SsoClient")
            .field("issuer", &self.issuer)
            .field("client_id", &self.client_id)
            .finish_non_exhaustive()
    }
}

fn system_clock() -> i64 {
    kuben_core::time::now_ms() / 1000
}

impl SsoClient {
    /// The client `cfg` describes, answering at `public_url`, joining people
    /// to `default_org` unless the configuration names another.
    ///
    /// # Errors
    ///
    /// When a required setting is missing or invalid.
    pub fn from_config(cfg: &SsoCfg, public_url: Option<&str>, default_org: &str) -> Result<Self, SsoError> {
        let missing = |what: &str| SsoError::Config(format!("sso.{what} is required"));
        let issuer = cfg.issuer.clone().ok_or_else(|| missing("issuer"))?;
        let client_id = cfg.client_id.clone().ok_or_else(|| missing("client_id"))?;
        let secret = match (&cfg.client_secret_file, &cfg.client_secret) {
            (Some(path), _) => std::fs::read_to_string(path)
                .map_err(|e| SsoError::Config(format!("cannot read sso.client_secret_file {path}: {e}")))?
                .trim()
                .to_owned(),
            (None, Some(secret)) => secret.clone(),
            (None, None) => return Err(missing("client_secret_file")),
        };
        let public_url = public_url
            .filter(|u| https(u))
            .ok_or_else(|| SsoError::Config("server.public_url must be an https URL for SSO".into()))?;
        let policy = cfg.policy().map_err(|e| SsoError::Config(e.to_string()))?;
        if policy.groups.is_empty() && policy.default_role.is_none() {
            return Err(SsoError::Config(
                "map at least one group to a role (sso.groups) or set sso.default_role".into(),
            ));
        }
        Ok(Self {
            issuer: issuer.trim_end_matches('/').to_owned(),
            client_id,
            client_secret: SecretString::from(secret),
            redirect_uri: format!("{}/api/v1/auth/sso/callback", public_url.trim_end_matches('/')),
            scopes: cfg.scopes.clone(),
            group_claim: cfg.group_claim.clone(),
            policy,
            display_name: cfg.display_name.clone(),
            org_slug: cfg.org.clone().unwrap_or_else(|| default_org.to_owned()),
            http: Arc::new(HttpsGet::default()),
            provider: OnceCell::new(),
            fixed_keys: None,
            clock: system_clock,
        })
    }

    /// The same client talking to `http` with fixed keys and `clock` (tests).
    #[must_use]
    pub fn with_provider(
        mut self,
        http: Arc<dyn IdentityProvider>,
        keys: crate::oidc::Jwks,
        clock: fn() -> i64,
    ) -> Self {
        self.http = http;
        self.fixed_keys = Some(keys);
        self.clock = clock;
        self
    }

    #[must_use]
    pub fn display_name(&self) -> &str {
        &self.display_name
    }

    #[must_use]
    pub fn org_slug(&self) -> &str {
        &self.org_slug
    }

    #[must_use]
    pub fn issuer(&self) -> &str {
        &self.issuer
    }

    async fn provider(&self) -> Result<&Provider, SsoError> {
        self.provider
            .get_or_try_init(|| async {
                let url = format!("{}/.well-known/openid-configuration", self.issuer);
                let body = self
                    .http
                    .get(&url, MAX_DOCUMENT)
                    .await
                    .map_err(SsoError::Unavailable)?;
                let discovery: Discovery =
                    serde_json::from_slice(&body).map_err(|e| SsoError::Unavailable(e.to_string()))?;
                if discovery.issuer.trim_end_matches('/') != self.issuer {
                    return Err(SsoError::Config(format!(
                        "the provider calls itself {}, not {}",
                        discovery.issuer, self.issuer
                    )));
                }
                let endpoints = [
                    &discovery.authorization_endpoint,
                    &discovery.token_endpoint,
                    &discovery.jwks_uri,
                ];
                if !endpoints.iter().all(|u| https(u)) {
                    return Err(SsoError::Config("the provider's endpoints must be https".into()));
                }
                let keys = match &self.fixed_keys {
                    Some(jwks) => KeyCache::fixed(jwks.clone()),
                    None => KeyCache::new(discovery.jwks_uri.clone(), self.http.clone() as Arc<dyn HttpGet>),
                };
                Ok(Provider { discovery, keys })
            })
            .await
    }

    /// Where to send the browser to sign in.
    pub async fn authorize_url(&self, state: &str, nonce: &str, verifier: &str) -> Result<String, SsoError> {
        let p = self.provider().await?;
        let separator = if p.discovery.authorization_endpoint.contains('?') {
            '&'
        } else {
            '?'
        };
        Ok(format!(
            "{}{separator}response_type=code&client_id={}&redirect_uri={}&scope={}&state={}&nonce={}\
             &code_challenge={}&code_challenge_method=S256",
            p.discovery.authorization_endpoint,
            encode(&self.client_id),
            encode(&self.redirect_uri),
            encode(&self.scopes.join(" ")),
            encode(state),
            encode(nonce),
            challenge(verifier),
        ))
    }

    /// The person behind `code`, if the provider and the policy admit them.
    pub async fn sign_in(&self, code: &str, verifier: &str, nonce: &str) -> Result<SsoPerson, SsoError> {
        let p = self.provider().await?;
        let form = format!(
            "grant_type=authorization_code&code={}&redirect_uri={}&code_verifier={}&client_id={}",
            encode(code),
            encode(&self.redirect_uri),
            encode(verifier),
            encode(&self.client_id),
        );
        let basic = base64::engine::general_purpose::STANDARD.encode(format!(
            "{}:{}",
            encode(&self.client_id),
            encode(self.client_secret.expose_secret())
        ));
        let (status, body) = self
            .http
            .post_form(&p.discovery.token_endpoint, &form, &basic)
            .await
            .map_err(SsoError::Unavailable)?;
        if !status.is_success() {
            return Err(SsoError::CodeRefused(format!("HTTP {status}")));
        }
        let response: TokenResponse =
            serde_json::from_slice(&body).map_err(|e| SsoError::CodeRefused(e.to_string()))?;
        let id_token = response
            .id_token
            .ok_or_else(|| SsoError::CodeRefused("no id_token".into()))?;
        let payload = p.keys.verify(&id_token).await?;
        let claims: IdClaims = serde_json::from_slice(&payload).map_err(|_| OidcError::Malformed)?;
        claims.check(&self.issuer, &self.client_id, nonce, (self.clock)())?;
        let groups = claims.groups(&self.group_claim);
        Ok(self.policy.admit(&claims, &groups)?)
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use std::{collections::BTreeMap, sync::Mutex};

    use kuben_core::perm::Role;

    use super::*;

    pub(crate) const FIXTURE: &str = include_str!("testdata/sso-oidc.json");
    pub(crate) const NOW: i64 = 1_800_000_100;

    /// A provider answering from memory; the token endpoint hands out the
    /// fixture token named by the code.
    #[derive(Default)]
    pub(crate) struct FakeIdp {
        pub(crate) forms: Mutex<Vec<String>>,
        pub(crate) issuer: Option<String>,
    }

    #[async_trait::async_trait]
    impl HttpGet for FakeIdp {
        async fn get(&self, url: &str, _limit: usize) -> Result<Bytes, String> {
            assert_eq!(url, "https://idp.example.com/.well-known/openid-configuration");
            let issuer = self
                .issuer
                .clone()
                .unwrap_or_else(|| "https://idp.example.com".into());
            Ok(Bytes::from(
                serde_json::json!({
                    "issuer": issuer,
                    "authorization_endpoint": "https://idp.example.com/authorize",
                    "token_endpoint": "https://idp.example.com/token",
                    "jwks_uri": "https://idp.example.com/jwks",
                })
                .to_string(),
            ))
        }
    }

    #[async_trait::async_trait]
    impl IdentityProvider for FakeIdp {
        async fn post_form(&self, url: &str, form: &str, basic: &str) -> Result<(StatusCode, Bytes), String> {
            assert_eq!(url, "https://idp.example.com/token");
            assert!(!basic.is_empty());
            self.forms.lock().expect("lock").push(form.to_owned());
            let code = form
                .split('&')
                .find_map(|kv| kv.strip_prefix("code="))
                .unwrap_or_default();
            let tokens = fixture()["tokens"].clone();
            match tokens.get(code).and_then(|t| t.as_str()) {
                Some(token) => Ok((
                    StatusCode::OK,
                    Bytes::from(serde_json::json!({ "id_token": token, "token_type": "Bearer" }).to_string()),
                )),
                None => Ok((
                    StatusCode::BAD_REQUEST,
                    Bytes::from_static(b"{\"error\":\"invalid_grant\"}"),
                )),
            }
        }
    }

    pub(crate) fn fixture() -> serde_json::Value {
        serde_json::from_str(FIXTURE).expect("fixture")
    }

    pub(crate) fn config() -> SsoCfg {
        SsoCfg {
            enabled: true,
            issuer: Some("https://idp.example.com/".into()),
            client_id: Some("kuben-console".into()),
            client_secret: Some("s3cret".into()),
            groups: BTreeMap::from([
                ("platform-admins".into(), "admin".into()),
                ("devs".into(), "developer".into()),
            ]),
            allowed_domains: vec!["example.com".into()],
            ..SsoCfg::default()
        }
    }

    pub(crate) fn client(idp: Arc<FakeIdp>) -> SsoClient {
        let jwks = serde_json::from_value(fixture()["jwks"].clone()).expect("jwks");
        SsoClient::from_config(&config(), Some("https://kuben.example.com/"), "acme")
            .expect("client")
            .with_provider(idp, jwks, || NOW)
    }

    #[test]
    fn configuration_is_checked_up_front() {
        let ok =
            SsoClient::from_config(&config(), Some("https://kuben.example.com"), "acme").expect("client");
        assert_eq!(ok.org_slug(), "acme");
        assert_eq!(ok.issuer(), "https://idp.example.com");
        assert_eq!(
            ok.redirect_uri,
            "https://kuben.example.com/api/v1/auth/sso/callback"
        );
        assert!(!format!("{ok:?}").contains("s3cret"));
        let cases = [
            (
                SsoCfg {
                    issuer: None,
                    ..config()
                },
                Some("https://kuben.example.com"),
            ),
            (
                SsoCfg {
                    client_secret: None,
                    ..config()
                },
                Some("https://kuben.example.com"),
            ),
            (config(), Some("http://kuben.example.com")),
            (config(), None),
            (
                SsoCfg {
                    groups: BTreeMap::new(),
                    ..config()
                },
                Some("https://kuben.example.com"),
            ),
        ];
        for (cfg, url) in cases {
            assert!(matches!(
                SsoClient::from_config(&cfg, url, "acme"),
                Err(SsoError::Config(_))
            ));
        }
        let defaulted = SsoCfg {
            groups: BTreeMap::new(),
            default_role: Some("viewer".into()),
            ..config()
        };
        assert!(SsoClient::from_config(&defaulted, Some("https://k.example.com"), "acme").is_ok());
    }

    #[tokio::test]
    async fn the_authorization_url_carries_pkce_and_the_nonce() {
        let c = client(Arc::default());
        let url = c
            .authorize_url("st@te", "nonce-1", "verifier-verifier-verifier-verifier-verif")
            .await
            .expect("url");
        assert!(
            url.starts_with("https://idp.example.com/authorize?response_type=code&client_id=kuben-console")
        );
        assert!(
            url.contains("redirect_uri=https%3A%2F%2Fkuben.example.com%2Fapi%2Fv1%2Fauth%2Fsso%2Fcallback")
        );
        assert!(url.contains("scope=openid%20email%20profile"));
        assert!(url.contains("state=st%40te") && url.contains("nonce=nonce-1"));
        assert!(url.contains(&format!(
            "code_challenge={}&code_challenge_method=S256",
            challenge("verifier-verifier-verifier-verifier-verif")
        )));
        assert_eq!(
            challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"),
            "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
        );
        assert_eq!(random_value().len(), 43);
        assert_ne!(random_value(), random_value());
    }

    #[tokio::test]
    async fn a_valid_code_signs_the_mapped_person_in() {
        let idp = Arc::new(FakeIdp::default());
        let c = client(idp.clone());
        let carol = c
            .sign_in("valid", "the-verifier", "nonce-1")
            .await
            .expect("carol");
        assert_eq!(
            (carol.email.as_str(), carol.role),
            ("carol@example.com", Role::Admin)
        );
        let dave = c.sign_in("developer", "v", "nonce-1").await.expect("dave");
        assert_eq!(dave.role, Role::Developer);
        let form = idp.forms.lock().expect("lock")[0].clone();
        assert!(
            form.contains("grant_type=authorization_code") && form.contains("code_verifier=the-verifier")
        );
    }

    #[tokio::test]
    async fn anything_else_is_refused() {
        let c = client(Arc::default());
        let denied = |r: Result<SsoPerson, SsoError>| match r {
            Err(SsoError::Denied(d)) => Some(d),
            _ => None,
        };
        assert_eq!(
            denied(c.sign_in("valid", "v", "nonce-2").await),
            Some(SsoDenied::Nonce)
        );
        assert_eq!(
            denied(c.sign_in("unverified", "v", "nonce-1").await),
            Some(SsoDenied::Unverified)
        );
        assert_eq!(
            denied(c.sign_in("outsider", "v", "nonce-1").await),
            Some(SsoDenied::Domain)
        );
        assert_eq!(
            denied(c.sign_in("no_role", "v", "nonce-1").await),
            Some(SsoDenied::NoRole)
        );
        assert_eq!(
            denied(c.sign_in("wrong_aud", "v", "nonce-1").await),
            Some(SsoDenied::Audience)
        );
        assert!(matches!(
            c.sign_in("unknown", "v", "nonce-1").await,
            Err(SsoError::CodeRefused(_))
        ));
    }

    #[tokio::test]
    async fn a_provider_claiming_another_issuer_is_not_used() {
        let idp = Arc::new(FakeIdp {
            issuer: Some("https://evil.example.com".into()),
            ..FakeIdp::default()
        });
        let c = client(idp);
        assert!(matches!(
            c.sign_in("valid", "v", "nonce-1").await,
            Err(SsoError::Config(_))
        ));
    }
}
