//! HTTP API. Everything user-facing goes through here: REST (`/api/v1`),
//! one SSE stream per browser tab (`/api/v1/stream`), health endpoints and
//! the embedded SPA.
//!
//! Transport rule (ADR-014): REST + one SSE per tab + WebSocket only for
//! terminals. No other endpoint streams.

pub mod audit;
pub mod auth;
pub mod authz;
pub mod error;
pub mod openapi;
pub mod routes;
pub mod state;
pub mod stream;
pub mod web;

use std::time::Duration;

use axum::{Router, middleware, routing::get};
use tower_http::{
    compression::CompressionLayer,
    limit::RequestBodyLimitLayer,
    request_id::{MakeRequestUuid, PropagateRequestIdLayer, SetRequestIdLayer},
