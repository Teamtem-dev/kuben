//! The GitHub App (M3): webhook signatures, App JWTs, installation tokens
//! scoped to one repository, and the branch heads a sync reads.
//!
//! Tokens are short-lived by construction (GitHub issues them for an hour)
//! and narrowed further: a build's fetch token can read the contents of its
//! one repository and nothing else, and is revoked when the attempt ends
//! (ADR-028). The App's private key never leaves this process.

use std::{
    fmt,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use bytes::Bytes;
use http::{Method, Request, StatusCode, header};
use http_body_util::{BodyExt, Full, Limited};
use kuben_core::{
    config::GitCfg,
    source::{BranchName, RepoName},
};
use kuben_platform::build::{FetchToken, Head, ProviderError, SourceProvider};
use ring::{hmac, rand::SystemRandom, signature};
use serde::Deserialize;
use serde_json::json;

use crate::transport::{Schemes, Transport, chain};

const TIMEOUT: Duration = Duration::from_secs(15);
const MAX_BODY: usize = 1 << 20;
const ACCEPT: &str = "application/vnd.github+json";
const API_VERSION: &str = "2022-11-28";
/// GitHub refuses JWTs that live longer than ten minutes; clocks drift.
const JWT_BACKDATE: u64 = 60;
const JWT_LIFETIME: u64 = 9 * 60;
/// A cached token is dropped this long before it expires.
const TOKEN_MARGIN: Duration = Duration::from_mins(10);
/// GitHub's tokens live an hour; the cache keeps them for less.
const TOKEN_CACHE_TTL: Duration = Duration::from_mins(50);
/// The header GitHub signs deliveries in.
pub const SIGNATURE_HEADER: &str = "x-hub-signature-256";

/// Whether `signature` (`sha256=<hex>`) is the HMAC-SHA256 of `body` under
/// `secret`, compared in constant time.
#[must_use]
pub fn verify_signature(secret: &[u8], body: &[u8], signature: &str) -> bool {
    let Some(hex) = signature.strip_prefix("sha256=") else {
        return false;
    };
    let Some(tag) = decode_hex(hex) else {
        return false;
    };
    let key = hmac::Key::new(hmac::HMAC_SHA256, secret);
    hmac::verify(&key, body, &tag).is_ok()
}

fn decode_hex(hex: &str) -> Option<Vec<u8>> {
    if hex.len() != 64 {
        return None;
    }
    (0..hex.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(hex.get(i..i + 2)?, 16).ok())
        .collect()
}

/// Why the App could not be set up.
#[derive(Debug, thiserror::Error)]
pub enum AppError {
    #[error(
        "the GitHub App is not configured (git.github_app_id, git.github_private_key_file, git.github_webhook_secret)"
    )]
    NotConfigured,
    #[error("cannot read the GitHub App key {path}: {reason}")]
    Key { path: String, reason: String },
}

/// Signs the App's JWTs.
trait Signer: Send + Sync {
    fn sign(&self, message: &[u8]) -> Result<Vec<u8>, String>;
}

struct RsaSigner(signature::RsaKeyPair);

impl Signer for RsaSigner {
    fn sign(&self, message: &[u8]) -> Result<Vec<u8>, String> {
        let mut out = vec![0; self.0.public().modulus_len()];
        self.0
            .sign(
                &signature::RSA_PKCS1_SHA256,
                &SystemRandom::new(),
                message,
                &mut out,
            )
            .map_err(|_| "signing failed".to_owned())?;
        Ok(out)
    }
}

/// The DER inside a PEM block, and whether it is PKCS#8.
fn pem_der(pem: &str) -> Result<(Vec<u8>, bool), String> {
    let (label, pkcs8) = if pem.contains("-----BEGIN RSA PRIVATE KEY-----") {
        ("RSA PRIVATE KEY", false)
    } else if pem.contains("-----BEGIN PRIVATE KEY-----") {
        ("PRIVATE KEY", true)
    } else {
        return Err("not a PEM RSA private key".into());
    };
    let begin = format!("-----BEGIN {label}-----");
    let end = format!("-----END {label}-----");
    let body = pem
        .split_once(&begin)
        .and_then(|(_, rest)| rest.split_once(&end))
        .map(|(body, _)| body)
        .ok_or("an unterminated PEM block")?;
    let base64: String = body.chars().filter(|c| !c.is_whitespace()).collect();
    let der = base64::engine::general_purpose::STANDARD
        .decode(base64)
        .map_err(|e| e.to_string())?;
    Ok((der, pkcs8))
}

fn signer_from_pem(pem: &str) -> Result<RsaSigner, String> {
    let (der, pkcs8) = pem_der(pem)?;
    let pair = if pkcs8 {
        signature::RsaKeyPair::from_pkcs8(&der)
    } else {
        signature::RsaKeyPair::from_der(&der)
    };
    pair.map(RsaSigner).map_err(|e| e.to_string())
}

fn unix(time: SystemTime) -> u64 {
    time.duration_since(UNIX_EPOCH).map_or(0, |d| d.as_secs())
}

/// The App JWT for `now` (RFC 7519, RS256).
fn app_jwt(signer: &dyn Signer, app_id: u64, now: SystemTime) -> Result<String, String> {
    let header = URL_SAFE_NO_PAD.encode(br#"{"alg":"RS256","typ":"JWT"}"#);
    let now = unix(now);
    let claims = json!({
        "iat": now.saturating_sub(JWT_BACKDATE),
        "exp": now + JWT_LIFETIME,
        "iss": app_id.to_string(),
    });
    let claims = URL_SAFE_NO_PAD.encode(claims.to_string());
    let input = format!("{header}.{claims}");
    let signature = signer.sign(input.as_bytes())?;
    Ok(format!("{input}.{}", URL_SAFE_NO_PAD.encode(signature)))
}

/// Percent-encode one path segment.
fn segment(value: &str) -> String {
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

#[derive(Deserialize)]
struct TokenBody {
    token: String,
    expires_at: String,
}

#[derive(Deserialize)]
struct RepositoryBody {
    id: u64,
}

#[derive(Deserialize)]
struct RefObject {
    sha: String,
}

#[derive(Deserialize)]
struct RefBody {
    object: RefObject,
}

#[derive(Deserialize)]
struct InstallationAccount {
    login: String,
}

#[derive(Deserialize)]
struct InstallationBody {
    account: InstallationAccount,
    #[serde(default)]
    suspended_at: Option<String>,
}

/// An installation as GitHub describes it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Installation {
    pub account: String,
    pub suspended: bool,
}

type Tokens = moka::future::Cache<(u64, String), FetchToken>;

/// The GitHub App Kuben acts as.
#[derive(Clone)]
pub struct GithubApp {
    app_id: u64,
    signer: Arc<dyn Signer>,
    webhook_secret: Arc<[u8]>,
    api: String,
    clone_base: String,
    transport: Transport,
    /// Metadata tokens for head reads, per installation and repository.
    tokens: Tokens,
}

impl fmt::Debug for GithubApp {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("GithubApp")
            .field("app_id", &self.app_id)
            .field("api", &self.api)
            .finish_non_exhaustive()
    }
}

/// When an RFC 3339 `expires_at` is, or an hour from now.
fn expiry(value: &str) -> SystemTime {
    value
        .parse::<jiff::Timestamp>()
        .ok()
        .and_then(|t| u64::try_from(t.as_second()).ok())
        .map_or_else(
            || SystemTime::now() + Duration::from_hours(1),
            |s| UNIX_EPOCH + Duration::from_secs(s),
        )
}

impl GithubApp {
    /// The App of `cfg`, with its key read from disk.
    pub fn from_config(cfg: &GitCfg) -> Result<Self, AppError> {
        if !cfg.github_enabled() {
            return Err(AppError::NotConfigured);
        }
        let (Some(app_id), Some(path), Some(secret)) = (
            cfg.github_app_id,
            cfg.github_private_key_file.as_deref(),
            cfg.github_webhook_secret.as_deref(),
        ) else {
            return Err(AppError::NotConfigured);
        };
        let key_error = |reason: String| AppError::Key {
            path: path.to_owned(),
            reason,
        };
        let pem = std::fs::read_to_string(path).map_err(|e| key_error(e.to_string()))?;
        let signer = signer_from_pem(&pem).map_err(key_error)?;
        Ok(Self::with_signer(
            app_id,
            Arc::new(signer),
            secret.as_bytes(),
            cfg,
        ))
    }

    fn with_signer(app_id: u64, signer: Arc<dyn Signer>, secret: &[u8], cfg: &GitCfg) -> Self {
        let schemes = if cfg.github_api_url.starts_with("http://") {
            Schemes::Any
        } else {
            Schemes::HttpsOnly
        };
        Self {
            app_id,
            signer,
            webhook_secret: Arc::from(secret),
            api: cfg.github_api_url.trim_end_matches('/').to_owned(),
            clone_base: cfg.github_clone_url.trim_end_matches('/').to_owned(),
            transport: Transport::new(schemes, TIMEOUT),
            tokens: moka::future::Cache::builder()
                .max_capacity(10_000)
                .time_to_live(TOKEN_CACHE_TTL)
                .build(),
        }
    }

    /// Whether `body` carries a valid signature from this App's webhook.
    #[must_use]
    pub fn verify(&self, body: &[u8], signature: &str) -> bool {
        verify_signature(&self.webhook_secret, body, signature)
    }

    async fn call(
        &self,
        method: Method,
        path: &str,
        auth: &str,
        body: Option<serde_json::Value>,
    ) -> Result<(StatusCode, Bytes), ProviderError> {
        let unavailable = |e: String| ProviderError::Unavailable(e);
        let payload = body.map_or_else(Bytes::new, |b| Bytes::from(b.to_string()));
        let request = Request::builder()
            .method(method)
            .uri(format!("{}{path}", self.api))
            .header(header::ACCEPT, ACCEPT)
            .header("x-github-api-version", API_VERSION)
            .header(header::AUTHORIZATION, auth)
            .header(header::CONTENT_TYPE, "application/json")
            .header(header::USER_AGENT, concat!("kuben/", env!("CARGO_PKG_VERSION")))
            .body(Full::new(payload))
            .map_err(|e| unavailable(e.to_string()))?;
        let response = self.transport.send(request).await.map_err(unavailable)?;
        let (parts, body) = response.into_parts();
        let body = tokio::time::timeout(TIMEOUT, Limited::new(body, MAX_BODY).collect())
            .await
            .map_err(|_| unavailable("timed out".into()))?
            .map_err(|e| unavailable(chain(&*e)))?
            .to_bytes();
        Ok((parts.status, body))
    }

    fn jwt(&self) -> Result<String, ProviderError> {
        app_jwt(&*self.signer, self.app_id, SystemTime::now()).map_err(ProviderError::Refused)
    }

    /// The installation `id`, read with the App's JWT.
    pub async fn installation(&self, id: u64) -> Result<Installation, ProviderError> {
        let auth = format!("Bearer {}", self.jwt()?);
        let (status, body) = self
            .call(Method::GET, &format!("/app/installations/{id}"), &auth, None)
            .await?;
        let body: InstallationBody = parse(status, &body, &format!("installation {id}"))?;
        Ok(Installation {
            account: body.account.login,
            suspended: body.suspended_at.is_some(),
        })
    }

    /// A new installation token for `repository` with `permissions`.
    async fn mint(
        &self,
        installation: u64,
        repository: &RepoName,
        permissions: serde_json::Value,
    ) -> Result<FetchToken, ProviderError> {
        let auth = format!("Bearer {}", self.jwt()?);
        let body = json!({ "repositories": [repository.name()], "permissions": permissions });
        let (status, body) = self
            .call(
                Method::POST,
                &format!("/app/installations/{installation}/access_tokens"),
                &auth,
                Some(body),
            )
            .await?;
        let body: TokenBody = parse(status, &body, &format!("installation {installation}"))?;
        Ok(FetchToken {
            token: body.token,
            expires_at: expiry(&body.expires_at),
        })
    }

    async fn metadata_token(
        &self,
        installation: u64,
        repository: &RepoName,
    ) -> Result<FetchToken, ProviderError> {
        let key = (installation, repository.as_str().to_owned());
        if let Some(token) = self.tokens.get(&key).await
            && token.expires_at > SystemTime::now() + TOKEN_MARGIN
        {
            return Ok(token);
        }
        let token = self
            .mint(
                installation,
                repository,
                json!({ "contents": "read", "metadata": "read" }),
            )
            .await?;
        self.tokens.insert(key, token.clone()).await;
        Ok(token)
    }
}

/// The JSON of a successful answer, or the matching error.
fn parse<T: for<'de> Deserialize<'de>>(
    status: StatusCode,
    body: &[u8],
    what: &str,
) -> Result<T, ProviderError> {
    match status {
        s if s.is_success() => serde_json::from_slice(body)
            .map_err(|e| ProviderError::Unavailable(format!("{what}: unexpected answer: {e}"))),
        StatusCode::NOT_FOUND | StatusCode::GONE | StatusCode::UNPROCESSABLE_ENTITY => {
            Err(ProviderError::NotFound(what.to_owned()))
        }
        StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN => {
            Err(ProviderError::Refused(format!("{what}: HTTP {status}")))
        }
        other => Err(ProviderError::Unavailable(format!("{what}: HTTP {other}"))),
    }
}

#[async_trait::async_trait]
impl SourceProvider for GithubApp {
    async fn head(
        &self,
        installation: u64,
        repository: &RepoName,
        branch: &BranchName,
    ) -> Result<Head, ProviderError> {
        let token = self.metadata_token(installation, repository).await?;
        let auth = format!("Bearer {}", token.token);
        let repo_path = format!(
            "/repos/{}/{}",
            segment(repository.owner()),
            segment(repository.name())
        );
        let (status, body) = self.call(Method::GET, &repo_path, &auth, None).await?;
        let repo: RepositoryBody = parse(status, &body, repository.as_str())?;
        let branch_path = branch
            .as_str()
            .split('/')
            .map(segment)
            .collect::<Vec<_>>()
            .join("/");
        let (status, body) = self
            .call(
                Method::GET,
                &format!("{repo_path}/git/ref/heads/{branch_path}"),
                &auth,
                None,
            )
            .await?;
        let head: RefBody = parse(status, &body, &format!("{repository}@{branch}"))?;
        let commit = head
            .object
            .sha
            .parse()
            .map_err(|e: kuben_core::source::InvalidSource| ProviderError::Unavailable(e.to_string()))?;
        Ok(Head {
            commit,
            repository_id: repo.id,
        })
    }

    async fn fetch_token(
        &self,
        installation: u64,
        repository: &RepoName,
    ) -> Result<FetchToken, ProviderError> {
        self.mint(installation, repository, json!({ "contents": "read" }))
            .await
    }

    async fn revoke(&self, token: &FetchToken) -> Result<(), ProviderError> {
        let auth = format!("Bearer {}", token.token);
        let (status, _) = self
            .call(Method::DELETE, "/installation/token", &auth, None)
            .await?;
        // An expired or already revoked token is revoked.
        if status.is_success() || status == StatusCode::UNAUTHORIZED {
            Ok(())
        } else {
            Err(ProviderError::Unavailable(format!(
                "revoking a token: HTTP {status}"
            )))
        }
    }

    fn clone_url(&self, repository: &RepoName) -> String {
        format!("{}/{}.git", self.clone_base, repository)
    }
}

#[cfg(test)]
mod tests {
    use std::fmt::Write as _;

    use super::*;

    /// A stand-in for the RSA key: the "signature" is the message reversed.
    struct Mirror;

    impl Signer for Mirror {
        fn sign(&self, message: &[u8]) -> Result<Vec<u8>, String> {
            Ok(message.iter().rev().copied().collect())
        }
    }

    fn hmac_hex(secret: &[u8], body: &[u8]) -> String {
        let tag = hmac::sign(&hmac::Key::new(hmac::HMAC_SHA256, secret), body);
        tag.as_ref().iter().fold(String::new(), |mut hex, b| {
            let _ = write!(hex, "{b:02x}");
            hex
        })
    }

    #[test]
    fn webhook_signatures_are_checked() {
        let body = br#"{"zen":"hello"}"#;
        let good = format!("sha256={}", hmac_hex(b"s3cret", body));
        assert!(verify_signature(b"s3cret", body, &good));
        assert!(!verify_signature(b"other", body, &good), "another secret");
        assert!(!verify_signature(b"s3cret", b"{}", &good), "another body");
        assert!(!verify_signature(
            b"s3cret",
            body,
            &good.replace("sha256=", "sha1=")
        ));
        assert!(!verify_signature(b"s3cret", body, "sha256=zz"));
        assert!(!verify_signature(b"s3cret", body, ""));
        let upper = format!("sha256={}", hmac_hex(b"s3cret", body).to_uppercase());
        assert!(
            verify_signature(b"s3cret", body, &upper),
            "hex case does not matter"
        );
    }

    #[test]
    fn app_jwts_are_short_lived_and_name_the_app() {
        let now = UNIX_EPOCH + Duration::from_secs(1_700_000_000);
        let jwt = app_jwt(&Mirror, 12345, now).expect("jwt");
        let parts: Vec<&str> = jwt.split('.').collect();
        assert_eq!(parts.len(), 3);
        let header: serde_json::Value =
            serde_json::from_slice(&URL_SAFE_NO_PAD.decode(parts[0]).expect("b64")).expect("json");
        assert_eq!(header, json!({ "alg": "RS256", "typ": "JWT" }));
        let claims: serde_json::Value =
            serde_json::from_slice(&URL_SAFE_NO_PAD.decode(parts[1]).expect("b64")).expect("json");
        assert_eq!(claims["iss"], "12345");
        let (iat, exp) = (
            claims["iat"].as_u64().expect("iat"),
            claims["exp"].as_u64().expect("exp"),
        );
        assert_eq!(iat, 1_700_000_000 - JWT_BACKDATE);
        assert!(exp - iat <= 600, "GitHub refuses JWTs longer than ten minutes");
        let signed = URL_SAFE_NO_PAD.decode(parts[2]).expect("b64");
        let input = format!("{}.{}", parts[0], parts[1]);
        assert_eq!(signed, input.bytes().rev().collect::<Vec<_>>());
    }

    #[test]
    fn keys_must_be_rsa_pem() {
        assert!(signer_from_pem("not a key").is_err());
        assert!(
            signer_from_pem("-----BEGIN RSA PRIVATE KEY-----\nAAAA").is_err(),
            "unterminated"
        );
        let garbage = "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n";
        assert!(signer_from_pem(garbage).is_err());
        let (der, pkcs8) =
            pem_der("-----BEGIN PRIVATE KEY-----\nAQID\n-----END PRIVATE KEY-----").expect("pem");
        assert_eq!((der, pkcs8), (vec![1, 2, 3], true));
    }

    #[test]
    fn an_incomplete_configuration_is_refused() {
        assert!(matches!(
            GithubApp::from_config(&GitCfg::default()),
            Err(AppError::NotConfigured)
        ));
        let missing = GitCfg {
            github_app_id: Some(1),
            github_private_key_file: Some("/nonexistent/kuben-github.pem".into()),
            github_webhook_secret: Some("x".into()),
            ..GitCfg::default()
        };
        assert!(matches!(
            GithubApp::from_config(&missing),
            Err(AppError::Key { .. })
        ));
    }

    #[test]
    fn answers_map_to_provider_errors() {
        let ok: Result<RepositoryBody, _> = parse(StatusCode::OK, br#"{"id":7}"#, "r");
        assert_eq!(ok.map(|r| r.id), Ok(7));
        assert!(matches!(
            parse::<RepositoryBody>(StatusCode::NOT_FOUND, b"", "r"),
            Err(ProviderError::NotFound(_))
        ));
        assert!(matches!(
            parse::<RepositoryBody>(StatusCode::FORBIDDEN, b"", "r"),
            Err(ProviderError::Refused(_))
        ));
        assert!(matches!(
            parse::<RepositoryBody>(StatusCode::BAD_GATEWAY, b"", "r"),
            Err(ProviderError::Unavailable(_))
        ));
        assert!(matches!(
            parse::<RepositoryBody>(StatusCode::OK, b"{}", "r"),
            Err(ProviderError::Unavailable(_))
        ));
    }

    #[test]
    fn paths_and_urls_are_built_safely() {
        assert_eq!(segment("feat/x y"), "feat%2Fx%20y");
        let app = GithubApp::with_signer(1, Arc::new(Mirror), b"s", &GitCfg::default());
        assert_eq!(
            app.clone_url(&"acme/shop".parse().expect("repo")),
            "https://github.com/acme/shop.git"
        );
        let ts = expiry("2030-01-01T00:00:00Z");
        assert_eq!(unix(ts), 1_893_456_000);
        assert!(expiry("garbage") > SystemTime::now());
        let token = FetchToken {
            token: "ghs_secret".into(),
            expires_at: ts,
        };
        assert!(!format!("{token:?}").contains("ghs_secret"));
    }
}
