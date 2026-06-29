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
    timeout::TimeoutLayer,
    trace::TraceLayer,
};
use utoipa::OpenApi;
use utoipa_axum::router::OpenApiRouter;
use utoipa_scalar::Servable as _;

pub use state::ApiState;

/// Build the full application router.
pub fn router(state: ApiState) -> Router {
    let timeout = Duration::from_secs(state.cfg.server.request_timeout_secs);
    let body_limit = state.cfg.server.max_body_bytes;

    // REST endpoints: documented, time-limited, body-limited.
    let (rest, openapi) = OpenApiRouter::with_openapi(openapi::ApiDoc::openapi())
        .nest("/api/v1", openapi::api_router())
        .split_for_parts();
    // Runs after routing (`MatchedPath` known) and after the session layer.
    let rest = rest
        .route_layer(middleware::from_fn_with_state(state.clone(), audit::record))
        .layer(TimeoutLayer::with_status_code(
            axum::http::StatusCode::REQUEST_TIMEOUT,
            timeout,
        ))
