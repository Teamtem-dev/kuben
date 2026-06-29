//! RFC 9457 `application/problem+json` error responses.

use axum::{
    Json,
    http::{StatusCode, header},
    response::{IntoResponse, Response},
};
use kuben_core::Error;
use serde::Serialize;
use utoipa::ToSchema;

/// Problem Details body.
#[derive(Debug, Serialize, ToSchema)]
pub struct Problem {
    /// Stable machine-readable code, e.g. `forbidden`.
    #[schema(example = "forbidden")]
    pub code: String,
    /// Short human readable title.
    pub title: String,
    /// HTTP status.
    pub status: u16,
    /// Details, if safe to expose.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
}

#[derive(Debug)]
pub struct ApiError(pub Error);

impl From<Error> for ApiError {
    fn from(e: Error) -> Self {
        Self(e)
    }
}

impl From<kuben_store::StoreError> for ApiError {
    fn from(e: kuben_store::StoreError) -> Self {
        Self(Error::Internal(e.to_string()))
    }
}

impl From<anyhow::Error> for ApiError {
    fn from(e: anyhow::Error) -> Self {
        Self(Error::Internal(e.to_string()))
    }
}

impl ApiError {
    #[must_use]
    pub fn status(&self) -> StatusCode {
        match &self.0 {
            Error::NotFound(_) => StatusCode::NOT_FOUND,
            Error::Conflict(_) => StatusCode::CONFLICT,
            Error::Unauthorized => StatusCode::UNAUTHORIZED,
            Error::Forbidden => StatusCode::FORBIDDEN,
            Error::Validation(_) => StatusCode::UNPROCESSABLE_ENTITY,
            Error::Unavailable(_) => StatusCode::SERVICE_UNAVAILABLE,
            Error::RateLimited { .. } => StatusCode::TOO_MANY_REQUESTS,
            Error::Internal(_) => StatusCode::INTERNAL_SERVER_ERROR,
        }
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        let status = self.status();
        if status.is_server_error() {
            tracing::error!(error = %self.0, "request failed");
        }
        let retry_after = match &self.0 {
            Error::RateLimited { retry_after_secs } => Some(*retry_after_secs),
            _ => None,
        };
        let detail = match &self.0 {
            // Never leak internal error text to clients.
            Error::Internal(_) => None,
            e => Some(e.to_string()),
        };
        let body = Problem {
            code: self.0.code().to_owned(),
            title: status.canonical_reason().unwrap_or("Error").to_owned(),
            status: status.as_u16(),
            detail,
        };
        let mut resp = (status, Json(body)).into_response();
        resp.headers_mut().insert(
            header::CONTENT_TYPE,
            "application/problem+json".parse().expect("static header"),
        );
        if let Some(secs) = retry_after {
            resp.headers_mut().insert(header::RETRY_AFTER, secs.into());
        }
        resp
    }
}

pub type ApiResult<T> = Result<T, ApiError>;
