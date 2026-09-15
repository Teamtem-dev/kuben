//! What the SQL-backed routes share (ADR-032): who asks for a change, as
//! operations and their audit records name them, and how store answers read.

use k8s_openapi::jiff::Timestamp;
use kuben_core::Error;
use kuben_store::{StoreError, repo::NewAudit};

use crate::{authz::Authz, error::ApiError};

/// The caller as `(kind, "kind:id")`: a user, or one of their API tokens.
#[must_use]
pub fn actor(authz: &Authz) -> (String, String) {
    let kind = if authz.current.token.is_some() {
        "token"
    } else {
        "user"
    };
    (kind.to_owned(), format!("{kind}:{}", authz.current.user.id))
}

/// The audit record of an operation a request asked for. The request itself
/// is audited by the middleware as usual.
#[must_use]
pub fn audit(authz: &Authz, action: &str, target_kind: &str, target_ref: String) -> NewAudit {
    let (actor_kind, _) = actor(authz);
    NewAudit {
        actor_kind,
        actor_id: Some(authz.current.user.id.to_string()),
        action: action.to_owned(),
        target_kind: Some(target_kind.to_owned()),
        target_ref: Some(target_ref),
        outcome: "accepted".into(),
        ..NewAudit::default()
    }
}

/// A write refused as a duplicate is `409`, naming what exists.
#[must_use]
pub fn duplicate(e: StoreError, what: &str) -> ApiError {
    if e.is_unique_violation() {
        ApiError(Error::Conflict(format!("{what} already exists")))
    } else {
        e.into()
    }
}

/// RFC 3339 time of a millisecond timestamp.
#[must_use]
pub fn timestamp(ms: i64) -> String {
    Timestamp::from_millisecond(ms)
        .map(|t| t.to_string())
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn timestamps_are_rfc3339() {
        assert_eq!(timestamp(0), "1970-01-01T00:00:00Z");
        assert_eq!(timestamp(1_757_894_400_000), "2025-09-15T00:00:00Z");
    }
}
