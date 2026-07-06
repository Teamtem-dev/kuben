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
