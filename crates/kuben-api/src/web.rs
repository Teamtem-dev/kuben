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
