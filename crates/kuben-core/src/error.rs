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
    Forbidden,
    #[error("validation failed: {0}")]
    Validation(String),
    #[error("unavailable: {0}")]
    Unavailable(String),
    /// Too many attempts; the caller may retry after this many seconds.
    #[error("too many attempts; retry in {retry_after_secs}s")]
