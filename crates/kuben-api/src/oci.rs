//! Image references resolved to digests (plan §8.1, I03).
//!
//! A release pins every image by digest. When the API is given a tag, it asks
//! the image's registry once, anonymously, which digest the tag names now,
//! and records both: the digest is what gets deployed, the tag is provenance
//! (the maintainer's decision on 2026-09-15, option A). A reference that is
//! already `repository@sha256:…` needs no registry at all.
//!
//! Registries that refuse anonymous pulls are not supported yet; the caller
//! gives a digest instead. Registries are reached through the proxy the
//! environment names (`HTTPS_PROXY`, `NO_PROXY`), as curl does.

use std::{collections::BTreeMap, fmt, fmt::Write as _, future::Future, sync::Arc, time::Duration};

use bytes::Bytes;
use http::{HeaderMap, Method, Request, StatusCode, header};
use http_body_util::{BodyExt, Empty, Limited};
use hyper_rustls::HttpsConnector;
use hyper_util::{
    client::{
        legacy::{
            Client,
            connect::{HttpConnector, proxy::Tunnel},
        },
        proxy::matcher::{Intercept, Matcher},
    },
    rt::TokioExecutor,
};
use kuben_core::artifact::Digest;
use rustls::crypto::CryptoProvider;
use serde::Deserialize;
use sha2::{Digest as _, Sha256};

/// Registry host of Docker Hub references without one.
const DOCKER_HUB: &str = "docker.io";
/// Where Docker Hub's registry API answers.
const DOCKER_HUB_API: &str = "registry-1.docker.io";
const MANIFEST_TYPES: &str = "application/vnd.oci.image.index.v1+json, \
     application/vnd.docker.distribution.manifest.list.v2+json, \
     application/vnd.oci.image.manifest.v1+json, \
     application/vnd.docker.distribution.manifest.v2+json";
const DIGEST_HEADER: &str = "docker-content-digest";
const TIMEOUT: Duration = Duration::from_secs(10);
/// Manifests and token responses are small; anything bigger is refused.
const MAX_BODY: usize = 4 << 20;

/// What a reference names: a tag, or a digest.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Reference {
    Tag(String),
    Digest(Digest),
}

/// A parsed image reference, normalized like Docker does it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ImageRef {
    /// `docker.io`, `ghcr.io`, `registry.example.com:5000`, …
    pub registry: String,
    /// The path in the registry: `library/nginx`, `acme/web`, …
    pub path: String,
    pub reference: Reference,
}

impl ImageRef {
    /// `registry/path`, the repository a digest is deployed from.
    #[must_use]
    pub fn repository(&self) -> String {
        format!("{}/{}", self.registry, self.path)
    }

    fn api_host(&self) -> &str {
        if self.registry == DOCKER_HUB {
            DOCKER_HUB_API
        } else {
            &self.registry
        }
    }
}

/// An image pinned by digest, and the reference it was given as.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Resolved {
    /// `registry/path`.
    pub repository: String,
    pub digest: Digest,
    /// The reference as the caller gave it, e.g. `nginx:1.27`.
    pub given: String,
}

impl Resolved {
    /// `repository@digest`.
    #[must_use]
    pub fn pinned(&self) -> String {
        format!("{}@{}", self.repository, self.digest.as_str())
    }
}

/// Why a reference was not resolved.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum ResolveError {
    #[error("`{0}` is not an image reference")]
    Invalid(String),
    #[error("the registry has no image `{0}`")]
    NotFound(String),
    #[error(
        "the registry of `{0}` refuses anonymous pulls; private registries are not supported yet, give repository@sha256:… instead"
    )]
    Unauthorized(String),
    #[error("cannot reach the registry of `{image}`: {reason}")]
    Unreachable { image: String, reason: String },
}

/// Parse `[registry/]path[:tag][@digest]`. Without a registry the image is on
/// Docker Hub, and a one-part path there is under `library/`. Without a tag or
/// digest the tag is `latest`.
pub fn parse(image: &str) -> Result<ImageRef, ResolveError> {
    let invalid = || ResolveError::Invalid(image.to_owned());
    let (name, digest) = match image.split_once('@') {
        Some((name, digest)) => (name, Some(digest.parse::<Digest>().map_err(|_| invalid())?)),
        None => (image, None),
    };
    let (name, tag) = match name.rsplit_once(':') {
        Some((n, t)) if !t.contains('/') => (n, Some(t)),
        _ => (name, None),
    };
    let (registry, path) = match name.split_once('/') {
        Some((first, rest)) if first.contains(['.', ':']) || first == "localhost" => {
            (first.to_owned(), rest.to_owned())
        }
        _ => (DOCKER_HUB.to_owned(), name.to_owned()),
    };
    let path = if registry == DOCKER_HUB && !path.contains('/') {
        format!("library/{path}")
    } else {
        path
    };
    let path_ok = !path.is_empty()
        && path.split('/').all(|part| {
            !part.is_empty()
                && part
                    .bytes()
                    .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || matches!(b, b'.' | b'_' | b'-'))
        });
    let tag_ok = tag.is_none_or(|t| {
        !t.is_empty()
            && t.len() <= 128
            && t.bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
    });
    if !path_ok || !tag_ok || registry.is_empty() {
        return Err(invalid());
    }
    let reference = match digest {
        Some(d) => Reference::Digest(d),
        None => Reference::Tag(tag.unwrap_or("latest").to_owned()),
    };
    Ok(ImageRef {
        registry,
        path,
        reference,
    })
}

/// A bearer challenge (`WWW-Authenticate: Bearer realm=…,service=…,scope=…`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Challenge {
    pub realm: String,
    pub service: Option<String>,
    pub scope: Option<String>,
}

/// Parse a bearer challenge; any other scheme is `None`.
#[must_use]
pub fn parse_challenge(value: &str) -> Option<Challenge> {
    let (scheme, params) = value.trim().split_once(' ')?;
    if !scheme.eq_ignore_ascii_case("bearer") {
        return None;
    }
    let mut found = BTreeMap::new();
    let mut rest = params.trim();
    while !rest.is_empty() {
        let (key, after) = rest.split_once('=')?;
        let after = after.trim_start();
        let (value, tail) = if let Some(quoted) = after.strip_prefix('"') {
            let end = quoted.find('"')?;
            (&quoted[..end], &quoted[end + 1..])
        } else {
            after.split_once(',').unwrap_or((after, ""))
        };
        found.insert(key.trim().to_ascii_lowercase(), value.to_owned());
        rest = tail.trim_start_matches([',', ' ']);
    }
    Some(Challenge {
        realm: found.remove("realm")?,
        service: found.remove("service"),
        scope: found.remove("scope"),
    })
}

/// Resolves image references to digests.
#[async_trait::async_trait]
pub trait ImageResolver: Send + Sync + fmt::Debug {
    async fn resolve(&self, image: &str) -> Result<Resolved, ResolveError>;
}

/// A reference already pinned by digest resolves to itself.
fn pinned(image: &str) -> Result<Option<Resolved>, ResolveError> {
    let r = parse(image)?;
    Ok(match &r.reference {
        Reference::Digest(d) => Some(Resolved {
            repository: r.repository(),
            digest: d.clone(),
            given: image.to_owned(),
        }),
        Reference::Tag(_) => None,
    })
}

/// Fixed answers, for tests and installations without registry access:
/// references not listed resolve only when they carry a digest.
#[derive(Clone, Debug, Default)]
pub struct FixedImages(pub BTreeMap<String, Digest>);

#[async_trait::async_trait]
impl ImageResolver for FixedImages {
    async fn resolve(&self, image: &str) -> Result<Resolved, ResolveError> {
        if let Some(resolved) = pinned(image)? {
            return Ok(resolved);
        }
        let digest = self
            .0
            .get(image)
            .ok_or_else(|| ResolveError::NotFound(image.to_owned()))?;
        Ok(Resolved {
            repository: parse(image)?.repository(),
            digest: digest.clone(),
            given: image.to_owned(),
        })
    }
}

type Direct = Client<HttpsConnector<HttpConnector>, Empty<Bytes>>;
type Tunneled = Client<HttpsConnector<Tunnel<HttpConnector>>, Empty<Bytes>>;

/// Asks the image's registry over HTTPS (OCI distribution API). Like curl,
/// it goes through the proxy `HTTPS_PROXY` (or `ALL_PROXY`) names, unless
/// `NO_PROXY` exempts the registry.
#[derive(Clone)]
pub struct RegistryResolver {
    direct: Result<Direct, Arc<str>>,
    proxies: Arc<Matcher>,
}

impl fmt::Debug for RegistryResolver {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("RegistryResolver")
            .field("ready", &self.direct.is_ok())
            .finish_non_exhaustive()
    }
}

impl Default for RegistryResolver {
    fn default() -> Self {
        Self::new()
    }
}

#[derive(Deserialize)]
struct TokenResponse {
    token: Option<String>,
    access_token: Option<String>,
}

fn no_roots(e: impl fmt::Display) -> Arc<str> {
    Arc::from(format!("no root certificates: {e}"))
}

fn crypto() -> Arc<CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

fn direct() -> Result<Direct, Arc<str>> {
    let connector = hyper_rustls::HttpsConnectorBuilder::new()
        .with_provider_and_native_roots(crypto())
        .map_err(no_roots)?
        .https_only()
        .enable_http1()
        .build();
    Ok(Client::builder(TokioExecutor::new()).build(connector))
}

/// A client whose connections are tunnelled through `proxy` (HTTP CONNECT).
fn tunneled(proxy: &Intercept) -> Result<Tunneled, Arc<str>> {
    let mut tunnel = Tunnel::new(proxy.uri().clone(), HttpConnector::new());
    if let Some(auth) = proxy.basic_auth() {
        tunnel = tunnel.with_auth(auth.clone());
    }
    let connector = hyper_rustls::HttpsConnectorBuilder::new()
        .with_provider_and_native_roots(crypto())
        .map_err(no_roots)?
        .https_only()
        .enable_http1()
        .wrap_connector(tunnel);
    Ok(Client::builder(TokioExecutor::new()).build(connector))
}

/// `future` within the timeout, its error told with its sources.
async fn within<T, E>(future: impl Future<Output = Result<T, E>>) -> Result<T, String>
where
    E: std::error::Error + 'static,
{
    tokio::time::timeout(TIMEOUT, future)
        .await
        .map_err(|_| "timed out".to_owned())?
        .map_err(|e| chain(&e))
}

impl RegistryResolver {
    /// A resolver trusting the system's root certificates, with the proxy
    /// rules of the environment. When the certificates cannot be loaded,
    /// every resolution fails as unreachable, with the reason.
    #[must_use]
    pub fn new() -> Self {
        Self::with_proxies(Matcher::from_env())
    }

    /// A resolver with these proxy rules instead of the environment's.
    #[must_use]
    pub fn with_proxies(proxies: Matcher) -> Self {
        Self {
            direct: direct(),
            proxies: Arc::new(proxies),
        }
    }

    async fn send(
        &self,
        method: Method,
        url: &str,
        bearer: Option<&str>,
        image: &str,
    ) -> Result<(StatusCode, HeaderMap, Bytes), ResolveError> {
        let unreachable = |reason: String| ResolveError::Unreachable {
            image: image.to_owned(),
            reason,
        };
        let mut request = Request::builder()
            .method(method)
            .uri(url)
            .header(header::ACCEPT, MANIFEST_TYPES)
            .header(header::USER_AGENT, concat!("kuben/", env!("CARGO_PKG_VERSION")));
        if let Some(token) = bearer {
            request = request.header(header::AUTHORIZATION, format!("Bearer {token}"));
        }
        let request = request
            .body(Empty::new())
            .map_err(|e| unreachable(e.to_string()))?;
        let response = match self.proxies.intercept(request.uri()) {
            None => {
                let client = self.direct.as_ref().map_err(|e| unreachable(e.to_string()))?;
                within(client.request(request)).await
            }
            Some(proxy) => {
                let client = tunneled(&proxy).map_err(|e| unreachable(e.to_string()))?;
                within(client.request(request)).await
            }
        }
        .map_err(unreachable)?;
        let (parts, body) = response.into_parts();
        let body = tokio::time::timeout(TIMEOUT, Limited::new(body, MAX_BODY).collect())
            .await
            .map_err(|_| unreachable("timed out".into()))?
            .map_err(|e| unreachable(chain(&*e)))?
            .to_bytes();
        Ok((parts.status, parts.headers, body))
    }

    /// An anonymous pull token for the challenge's realm.
    async fn token(&self, challenge: &Challenge, r: &ImageRef, image: &str) -> Result<String, ResolveError> {
        if !challenge.realm.starts_with("https://") {
            return Err(ResolveError::Unauthorized(image.to_owned()));
        }
        let scope = challenge
            .scope
            .clone()
            .unwrap_or_else(|| format!("repository:{}:pull", r.path));
        let mut url = format!("{}?scope={}", challenge.realm, encode(&scope));
        if let Some(service) = &challenge.service {
            url.push_str("&service=");
            url.push_str(&encode(service));
        }
        let (status, _, body) = self.send(Method::GET, &url, None, image).await?;
        if !status.is_success() {
            return Err(ResolveError::Unauthorized(image.to_owned()));
        }
        let response: TokenResponse =
            serde_json::from_slice(&body).map_err(|_| ResolveError::Unauthorized(image.to_owned()))?;
        response
            .token
            .or(response.access_token)
            .ok_or_else(|| ResolveError::Unauthorized(image.to_owned()))
    }

    /// Ask for the manifest of `r`: HEAD first (no rate-limit cost on Docker
    /// Hub); a registry that sends no digest header gets a GET, and the digest
    /// is computed over the manifest it returns.
    async fn manifest_digest(&self, r: &ImageRef, tag: &str, image: &str) -> Result<Digest, ResolveError> {
        let url = format!("https://{}/v2/{}/manifests/{tag}", r.api_host(), r.path);
        let mut bearer = None;
        for method in [Method::HEAD, Method::GET] {
            let (mut status, mut headers, mut body) =
                self.send(method.clone(), &url, bearer.as_deref(), image).await?;
            if status == StatusCode::UNAUTHORIZED && bearer.is_none() {
                let challenge = headers
                    .get(header::WWW_AUTHENTICATE)
                    .and_then(|v| v.to_str().ok())
                    .and_then(parse_challenge)
                    .ok_or_else(|| ResolveError::Unauthorized(image.to_owned()))?;
                bearer = Some(self.token(&challenge, r, image).await?);
                (status, headers, body) = self.send(method.clone(), &url, bearer.as_deref(), image).await?;
            }
            match status {
                StatusCode::OK => {}
                StatusCode::NOT_FOUND => return Err(ResolveError::NotFound(image.to_owned())),
                StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN => {
                    return Err(ResolveError::Unauthorized(image.to_owned()));
                }
                other => {
                    return Err(ResolveError::Unreachable {
                        image: image.to_owned(),
                        reason: format!("HTTP {other}"),
                    });
                }
            }
            if let Some(digest) = headers
                .get(DIGEST_HEADER)
                .and_then(|v| v.to_str().ok())
                .and_then(|v| v.parse::<Digest>().ok())
            {
                return Ok(digest);
            }
            if method == Method::GET {
                let computed = Sha256::digest(&body)
                    .iter()
                    .fold(String::from("sha256:"), |mut hex, byte| {
                        let _ = write!(hex, "{byte:02x}");
                        hex
                    });
                return computed
                    .parse()
                    .map_err(|_| ResolveError::Invalid(image.to_owned()));
            }
        }
        Err(ResolveError::Unreachable {
            image: image.to_owned(),
            reason: "no manifest digest".into(),
        })
    }
}

#[async_trait::async_trait]
impl ImageResolver for RegistryResolver {
    async fn resolve(&self, image: &str) -> Result<Resolved, ResolveError> {
        if let Some(resolved) = pinned(image)? {
            return Ok(resolved);
        }
        let r = parse(image)?;
        let Reference::Tag(tag) = &r.reference else {
            return Err(ResolveError::Invalid(image.to_owned()));
        };
        let digest = self.manifest_digest(&r, tag, image).await?;
        Ok(Resolved {
            repository: r.repository(),
            digest,
            given: image.to_owned(),
        })
    }
}

/// An error with its sources: hyper's own message is only
/// "client error (Connect)", the cause (DNS, TCP, TLS) is in the chain.
fn chain(e: &(dyn std::error::Error + 'static)) -> String {
    let mut out = e.to_string();
    let mut source = e.source();
    while let Some(cause) = source {
        out.push_str(": ");
        out.push_str(&cause.to_string());
        source = cause.source();
    }
    out
}

/// Percent-encode a query value (RFC 3986 unreserved characters stay).
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

#[cfg(test)]
mod tests {
    use super::*;

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    fn tag(t: &str) -> Reference {
        Reference::Tag(t.into())
    }

    #[test]
    fn references_are_normalized_like_docker() {
        let cases = [
            ("nginx", "docker.io", "library/nginx", tag("latest")),
            ("nginx:1.27", "docker.io", "library/nginx", tag("1.27")),
            (
                "nginxinc/nginx-unprivileged:1.27-alpine",
                "docker.io",
                "nginxinc/nginx-unprivileged",
                tag("1.27-alpine"),
            ),
            ("ghcr.io/acme/web:v2", "ghcr.io", "acme/web", tag("v2")),
            (
                "registry.example.com:5000/team/api",
                "registry.example.com:5000",
                "team/api",
                tag("latest"),
            ),
            ("localhost/web:dev", "localhost", "web", tag("dev")),
        ];
        for (image, registry, path, reference) in cases {
            let r = parse(image).expect(image);
            assert_eq!(
                (r.registry.as_str(), r.path.as_str(), &r.reference),
                (registry, path, &reference),
                "{image}"
            );
        }
        let pinned = parse(&format!("ghcr.io/acme/web:v2@{DIGEST}")).expect("pinned");
        assert_eq!(
            pinned.reference,
            Reference::Digest(DIGEST.parse().expect("digest"))
        );
        assert_eq!(pinned.repository(), "ghcr.io/acme/web");
        assert_eq!(parse("nginx").expect("hub").api_host(), "registry-1.docker.io");
    }

    #[test]
    fn malformed_references_are_refused() {
        for bad in [
            "",
            "nginx latest",
            "Nginx",
            "nginx:",
            "nginx@latest",
            "ghcr.io/",
            "a//b",
            &format!("nginx:{}", "x".repeat(129)),
        ] {
            assert!(parse(bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn bearer_challenges_are_read() {
        let hub = r#"Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull""#;
        assert_eq!(
            parse_challenge(hub),
            Some(Challenge {
                realm: "https://auth.docker.io/token".into(),
                service: Some("registry.docker.io".into()),
                scope: Some("repository:library/nginx:pull".into()),
            })
        );
        assert_eq!(
            parse_challenge(r#"Bearer realm="https://ghcr.io/token""#).map(|c| c.service),
            Some(None)
        );
        assert_eq!(parse_challenge(r#"Basic realm="registry""#), None);
        assert_eq!(parse_challenge("Bearer service=x"), None, "no realm");
    }

    #[test]
    fn query_values_are_encoded() {
        assert_eq!(
            encode("repository:library/nginx:pull"),
            "repository%3Alibrary%2Fnginx%3Apull"
        );
    }

    #[tokio::test]
    async fn pinned_references_and_fixed_answers_need_no_registry() {
        let image = format!("ghcr.io/acme/web@{DIGEST}");
        let resolved = RegistryResolver::new().resolve(&image).await.expect("pinned");
        assert_eq!(resolved.pinned(), image);

        let fixed = FixedImages(BTreeMap::from([(
            "nginx:1.27".to_owned(),
            DIGEST.parse().expect("digest"),
        )]));
        let nginx = fixed.resolve("nginx:1.27").await.expect("listed");
        assert_eq!(nginx.repository, "docker.io/library/nginx");
        assert_eq!(nginx.given, "nginx:1.27");
        assert_eq!(nginx.pinned(), format!("docker.io/library/nginx@{DIGEST}"));
        assert_eq!(
            fixed.resolve("redis:7").await,
            Err(ResolveError::NotFound("redis:7".into()))
        );
    }
}
