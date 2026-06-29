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
