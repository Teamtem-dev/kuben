//! Domain error type shared by all crates. The API layer maps it to
//! RFC 9457 `application/problem+json` responses.

/// Canonical domain error.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("not found: {0}")]
    NotFound(String),
    #[error("conflict: {0}")]
    Conflict(String),
    #[error("unauthorized")]
    Unauthorized,
    #[error("forbidden")]
