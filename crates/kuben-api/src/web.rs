//! Embedded SPA. `/api/*` never falls through to `index.html` (unknown API
//! routes are JSON 404s); every other path serves the SPA shell with a strict
//! CSP. Hashed assets under `/assets/` are immutable-cached.

use axum::{
    http::{HeaderValue, StatusCode, Uri, header},
    response::{IntoResponse, Response},
};
use kuben_core::Error;

use crate::error::ApiError;

const CSP: &str = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; \
     font-src 'self'; connect-src 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'self'";

pub async fn fallback(uri: Uri) -> Response {
    let path = uri.path();
    if path.starts_with("/api/") {
        return ApiError(Error::NotFound(format!("no route for {path}"))).into_response();
    }
    serve_spa(path)
}

#[cfg(feature = "embed-ui")]
fn serve_spa(path: &str) -> Response {
    #[derive(rust_embed::RustEmbed)]
    #[folder = "../../apps/web/dist/"]
    #[exclude = "*.map"]
    struct Assets;

    let rel = path.trim_start_matches('/');
    let (rel, is_asset) = match Assets::get(rel) {
        Some(_) if !rel.is_empty() => (rel.to_owned(), rel.starts_with("assets/")),
        _ => ("index.html".to_owned(), false),
    };
    let Some(file) = Assets::get(&rel) else {
        return (StatusCode::NOT_FOUND, "ui not built").into_response();
    };
    let mime = mime_guess::from_path(&rel).first_or_octet_stream();
    let mut resp = Response::new(axum::body::Body::from(file.data.into_owned()));
    let headers = resp.headers_mut();
    headers.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_str(mime.as_ref()).unwrap_or(HeaderValue::from_static("application/octet-stream")),
    );
    headers.insert(
        header::CACHE_CONTROL,
        HeaderValue::from_static(if is_asset {
            "public, max-age=31536000, immutable"
        } else {
            "no-cache"
        }),
    );
    apply_security_headers(headers);
    resp
}

#[cfg(not(feature = "embed-ui"))]
fn serve_spa(_path: &str) -> Response {
    let mut resp = (
        StatusCode::OK,
        "<!doctype html><title>Kuben</title><p>Kuben API is running. The web UI is not embedded in this build \
         (compile with <code>--features embed-ui</code>) — during development use the Vite dev server.</p>",
    )
        .into_response();
    resp.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("text/html; charset=utf-8"),
    );
    apply_security_headers(resp.headers_mut());
    resp
}

fn apply_security_headers(headers: &mut axum::http::HeaderMap) {
    headers.insert(header::CONTENT_SECURITY_POLICY, HeaderValue::from_static(CSP));
    headers.insert(
        header::X_CONTENT_TYPE_OPTIONS,
        HeaderValue::from_static("nosniff"),
    );
    headers.insert(
        header::REFERRER_POLICY,
        HeaderValue::from_static("strict-origin-when-cross-origin"),
    );
    headers.insert(header::X_FRAME_OPTIONS, HeaderValue::from_static("DENY"));
}
