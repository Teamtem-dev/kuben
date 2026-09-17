//! Outgoing HTTP: the system's root certificates, and the proxy the
//! environment names (`HTTPS_PROXY`, `HTTP_PROXY`, `ALL_PROXY`, `NO_PROXY`),
//! as curl does. Registries ([`crate::oci`]) and the API client
//! ([`crate::client`]) share it.

use std::{fmt, sync::Arc, time::Duration};

use bytes::Bytes;
use http::{Request, Response};
use http_body_util::Full;
use hyper::body::Incoming;
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
use rustls::crypto::CryptoProvider;

type Direct = Client<HttpsConnector<HttpConnector>, Full<Bytes>>;
type Tunneled = Client<HttpsConnector<Tunnel<HttpConnector>>, Full<Bytes>>;

/// Where requests may go: `https://` only (registries), or plain `http://`
/// too (a Kuben server on localhost or a private network).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Schemes {
    HttpsOnly,
    Any,
}

#[derive(Clone)]
pub struct Transport {
    direct: Result<Direct, Arc<str>>,
    proxies: Arc<Matcher>,
    schemes: Schemes,
    /// Until the response headers arrive.
    timeout: Duration,
}

impl fmt::Debug for Transport {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Transport")
            .field("ready", &self.direct.is_ok())
            .field("schemes", &self.schemes)
            .finish_non_exhaustive()
    }
}

fn crypto() -> Arc<CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

fn no_roots(e: impl fmt::Display) -> Arc<str> {
    Arc::from(format!("no root certificates: {e}"))
}

macro_rules! connector {
    ($schemes:expr, $inner:expr) => {{
        let builder = hyper_rustls::HttpsConnectorBuilder::new()
            .with_provider_and_native_roots(crypto())
            .map_err(no_roots)?;
        match $schemes {
            Schemes::HttpsOnly => builder.https_only().enable_http1().wrap_connector($inner),
            Schemes::Any => builder.https_or_http().enable_http1().wrap_connector($inner),
        }
    }};
}

fn direct(schemes: Schemes) -> Result<Direct, Arc<str>> {
    let mut http = HttpConnector::new();
    http.enforce_http(false);
    Ok(Client::builder(TokioExecutor::new()).build(connector!(schemes, http)))
}

/// A client whose connections are tunnelled through `proxy` (HTTP CONNECT).
fn tunneled(proxy: &Intercept, schemes: Schemes) -> Result<Tunneled, Arc<str>> {
    let mut tunnel = Tunnel::new(proxy.uri().clone(), HttpConnector::new());
    if let Some(auth) = proxy.basic_auth() {
        tunnel = tunnel.with_auth(auth.clone());
    }
    Ok(Client::builder(TokioExecutor::new()).build(connector!(schemes, tunnel)))
}

/// An error with its sources: hyper's own message is only
/// "client error (Connect)", the cause (DNS, TCP, TLS) is in the chain.
#[must_use]
pub fn chain(e: &(dyn std::error::Error + 'static)) -> String {
    let mut out = e.to_string();
    let mut source = e.source();
    while let Some(cause) = source {
        out.push_str(": ");
        out.push_str(&cause.to_string());
        source = cause.source();
    }
    out
}

impl Transport {
    /// With the environment's proxy rules. When the root certificates cannot
    /// be loaded, every request fails with the reason.
    #[must_use]
    pub fn new(schemes: Schemes, timeout: Duration) -> Self {
        Self::with_proxies(Matcher::from_env(), schemes, timeout)
    }

    /// With these proxy rules instead of the environment's.
    #[must_use]
    pub fn with_proxies(proxies: Matcher, schemes: Schemes, timeout: Duration) -> Self {
        Self {
            direct: direct(schemes),
            proxies: Arc::new(proxies),
            schemes,
            timeout,
        }
    }

    /// Send `request`; the response once its headers arrived (its body is
    /// the caller's to read, with its own limits).
    pub async fn send(&self, request: Request<Full<Bytes>>) -> Result<Response<Incoming>, String> {
        let response = match self.proxies.intercept(request.uri()) {
            None => {
                let client = self.direct.as_ref().map_err(ToString::to_string)?;
                tokio::time::timeout(self.timeout, client.request(request)).await
            }
            Some(proxy) => {
                let client = tunneled(&proxy, self.schemes).map_err(|e| e.to_string())?;
                tokio::time::timeout(self.timeout, client.request(request)).await
            }
        };
        response
            .map_err(|_| "timed out".to_owned())?
            .map_err(|e| chain(&e))
    }

    /// Send `request` and read at most `max_body` bytes of the answer within
    /// the transport's timeout: the status and the body.
    pub async fn fetch(
        &self,
        request: Request<Full<Bytes>>,
        max_body: usize,
    ) -> Result<(http::StatusCode, Bytes), String> {
        use http_body_util::{BodyExt as _, Limited};
        let response = self.send(request).await?;
        let (parts, body) = response.into_parts();
        let body = tokio::time::timeout(self.timeout, Limited::new(body, max_body).collect())
            .await
            .map_err(|_| "timed out reading the answer".to_owned())?
            .map_err(|e| chain(&*e))?
            .to_bytes();
        Ok((parts.status, body))
    }
}
