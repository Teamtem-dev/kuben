//! Embedded SPA. `/api/*` never falls through to `index.html` (unknown API
//! routes are JSON 404s); every other path serves the SPA shell with a strict
//! CSP. Hashed assets under `/assets/` are immutable-cached.

use axum::{
    http::{HeaderValue, StatusCode, Uri, header},
    response::{IntoResponse, Response},
};
