//! Trust for external CI (M4.2; plan §13, S04): which GitHub Actions
//! workflows may exchange their OIDC token for a short-lived Kuben token.
//!
//! A trust policy names the repository and its owner by their immutable
//! numeric ids (a renamed or re-created repository is someone else), the
//! refs, environments and events it accepts, the role the token is capped at
//! and how long it lives. Everything is denied unless a policy allows it;
//! pull-request refs never carry deploy authority, so a fork cannot use a
//! policy by opening a pull request.

use serde::{Deserialize, Deserializer, Serialize};

use crate::perm::Role;

/// The issuer of GitHub Actions OIDC tokens.
pub const GITHUB_ACTIONS_ISSUER: &str = "https://token.actions.githubusercontent.com";
/// Default and longest life of an exchanged token.
pub const DEFAULT_CI_TOKEN_TTL_SECS: u32 = 15 * 60;
pub const MIN_CI_TOKEN_TTL_SECS: u32 = 60;
pub const MAX_CI_TOKEN_TTL_SECS: u32 = 60 * 60;
/// Clock skew tolerated on `exp`, `nbf` and `iat`.
pub const CLOCK_LEEWAY_SECS: i64 = 60;
/// A provider token living longer than this is refused.
pub const MAX_OIDC_LIFETIME_SECS: i64 = 60 * 60;
const MAX_PATTERNS: usize = 20;
/// The events accepted when a policy names none.
pub const DEFAULT_EVENTS: [&str; 3] = ["push", "workflow_dispatch", "release"];

/// The claims of a GitHub Actions OIDC token Kuben reads.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
pub struct GithubClaims {
    pub iss: String,
    #[serde(deserialize_with = "audiences")]
    pub aud: Vec<String>,
    pub sub: String,
    pub jti: String,
    pub iat: i64,
    #[serde(default)]
    pub nbf: Option<i64>,
    pub exp: i64,
    pub repository: String,
    #[serde(deserialize_with = "numeric_id")]
    pub repository_id: u64,
    pub repository_owner: String,
    #[serde(deserialize_with = "numeric_id")]
    pub repository_owner_id: u64,
    #[serde(rename = "ref")]
    pub git_ref: String,
    pub event_name: String,
    #[serde(default)]
    pub environment: Option<String>,
    #[serde(default)]
    pub workflow_ref: Option<String>,
    #[serde(default)]
    pub run_id: Option<String>,
    #[serde(default)]
    pub actor: Option<String>,
}

fn audiences<'de, D: Deserializer<'de>>(d: D) -> Result<Vec<String>, D::Error> {
    #[derive(Deserialize)]
    #[serde(untagged)]
    enum Aud {
        One(String),
        Many(Vec<String>),
    }
    Ok(match Aud::deserialize(d)? {
        Aud::One(a) => vec![a],
        Aud::Many(a) => a,
    })
}

/// GitHub sends numeric ids as strings.
fn numeric_id<'de, D: Deserializer<'de>>(d: D) -> Result<u64, D::Error> {
    #[derive(Deserialize)]
    #[serde(untagged)]
    enum Id {
        Text(String),
        Number(u64),
    }
    match Id::deserialize(d)? {
        Id::Number(n) => Ok(n),
        Id::Text(s) => s.parse().map_err(serde::de::Error::custom),
    }
}

/// Why a provider token is not acceptable, whatever the policy.
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
pub enum TokenError {
    #[error("the token was issued by another issuer")]
    Issuer,
    #[error("the token is for another audience")]
    Audience,
    #[error("the token has expired")]
    Expired,
    #[error("the token is not valid yet")]
    NotYetValid,
    #[error("the token lives too long")]
    TooLong,
    #[error("the token has no id")]
    NoId,
}

impl GithubClaims {
    /// Check issuer, audience and times at `now` (Unix seconds).
    pub fn check(&self, issuer: &str, audience: &str, now: i64) -> Result<(), TokenError> {
        if self.iss != issuer {
            return Err(TokenError::Issuer);
        }
        if !self.aud.iter().any(|a| a == audience) {
            return Err(TokenError::Audience);
        }
        if self.jti.is_empty() || self.jti.len() > 256 {
            return Err(TokenError::NoId);
        }
        if now >= self.exp.saturating_add(CLOCK_LEEWAY_SECS) {
            return Err(TokenError::Expired);
        }
        let not_before = self.nbf.unwrap_or(self.iat).max(self.iat);
        if now.saturating_add(CLOCK_LEEWAY_SECS) < not_before {
            return Err(TokenError::NotYetValid);
        }
        if self.exp.saturating_sub(self.iat) > MAX_OIDC_LIFETIME_SECS {
            return Err(TokenError::TooLong);
        }
        Ok(())
    }
}

/// Why a policy was refused.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum InvalidPolicy {
    #[error("a policy names 1 to {MAX_PATTERNS} refs")]
    RefCount,
    #[error("ref pattern {0:?} must start with refs/heads/ or refs/tags/, with `*` only at the end")]
    Ref(String),
    #[error("a policy names at most {MAX_PATTERNS} environments and events")]
    ListLength,
    #[error("unknown or unsafe event {0:?}")]
    Event(String),
    #[error("CI tokens are capped at developer or admin, not {0}")]
    Role(Role),
    #[error("CI tokens live between {MIN_CI_TOKEN_TTL_SECS} and {MAX_CI_TOKEN_TTL_SECS} seconds")]
    Ttl,
    #[error("repository and owner ids must be set")]
    Ids,
}

/// Why a token did not match a policy.
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
pub enum Denied {
    #[error("another repository")]
    Repository,
    #[error("another repository owner")]
    Owner,
    #[error("a ref the policy does not allow")]
    Ref,
    #[error("an environment the policy does not allow")]
    Environment,
    #[error("an event the policy does not allow")]
    Event,
}

/// Events whose workflows may run code a pull request controls.
const UNSAFE_EVENTS: [&str; 3] = ["pull_request", "pull_request_target", "workflow_run"];
const KNOWN_EVENTS: [&str; 6] = [
    "push",
    "workflow_dispatch",
    "release",
    "schedule",
    "deployment",
    "merge_group",
];

/// A trust policy for one GitHub repository.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TrustPolicy {
    pub repository_id: u64,
    pub repository_owner_id: u64,
    /// `refs/heads/main`, `refs/tags/v*`, …
    pub refs: Vec<String>,
    /// GitHub environments allowed; empty for any (or none).
    #[serde(default)]
    pub environments: Vec<String>,
    /// Workflow events allowed.
    pub events: Vec<String>,
    /// The role exchanged tokens are capped at.
    pub role: Role,
    pub token_ttl_secs: u32,
}

fn pattern_ok(pattern: &str) -> bool {
    let body = pattern.strip_suffix('*').unwrap_or(pattern);
    (pattern.starts_with("refs/heads/") || pattern.starts_with("refs/tags/"))
        && !body.contains('*')
        && pattern.len() <= 255
        && !pattern.chars().any(|c| c.is_whitespace() || c.is_control())
}

/// Whether `git_ref` matches `pattern`: exact, or a prefix before a final `*`.
#[must_use]
pub fn ref_matches(pattern: &str, git_ref: &str) -> bool {
    pattern
        .strip_suffix('*')
        .map_or(pattern == git_ref, |prefix| git_ref.starts_with(prefix))
}

impl TrustPolicy {
    pub fn validate(&self) -> Result<(), InvalidPolicy> {
        if self.repository_id == 0 || self.repository_owner_id == 0 {
            return Err(InvalidPolicy::Ids);
        }
        if self.refs.is_empty() || self.refs.len() > MAX_PATTERNS {
            return Err(InvalidPolicy::RefCount);
        }
        if let Some(bad) = self.refs.iter().find(|r| !pattern_ok(r)) {
            return Err(InvalidPolicy::Ref(bad.clone()));
        }
        if self.environments.len() > MAX_PATTERNS
            || self.events.is_empty()
            || self.events.len() > MAX_PATTERNS
        {
            return Err(InvalidPolicy::ListLength);
        }
        if let Some(bad) = self.events.iter().find(|e| !KNOWN_EVENTS.contains(&e.as_str())) {
            return Err(InvalidPolicy::Event(bad.clone()));
        }
        if !matches!(self.role, Role::Developer | Role::Admin) {
            return Err(InvalidPolicy::Role(self.role));
        }
        if !(MIN_CI_TOKEN_TTL_SECS..=MAX_CI_TOKEN_TTL_SECS).contains(&self.token_ttl_secs) {
            return Err(InvalidPolicy::Ttl);
        }
        Ok(())
    }

    /// Whether `claims` satisfy this policy. Deny by default.
    pub fn evaluate(&self, claims: &GithubClaims) -> Result<(), Denied> {
        if claims.repository_id != self.repository_id {
            return Err(Denied::Repository);
        }
        if claims.repository_owner_id != self.repository_owner_id {
            return Err(Denied::Owner);
        }
        if UNSAFE_EVENTS.contains(&claims.event_name.as_str()) || !self.events.contains(&claims.event_name) {
            return Err(Denied::Event);
        }
        if claims.git_ref.starts_with("refs/pull/")
            || !self.refs.iter().any(|p| ref_matches(p, &claims.git_ref))
        {
            return Err(Denied::Ref);
        }
        if !self.environments.is_empty()
            && !claims
                .environment
                .as_ref()
                .is_some_and(|env| self.environments.contains(env))
        {
            return Err(Denied::Environment);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    const NOW: i64 = 1_800_000_100;

    fn claims() -> GithubClaims {
        serde_json::from_value(json!({
            "iss": GITHUB_ACTIONS_ISSUER, "aud": "https://kuben.example.com", "sub": "repo:acme/shop:ref:refs/heads/main",
            "jti": "j-1", "iat": 1_800_000_000, "nbf": 1_800_000_000, "exp": 1_800_000_300,
            "repository": "acme/shop", "repository_id": "123456", "repository_owner": "acme",
            "repository_owner_id": "42", "ref": "refs/heads/main", "event_name": "push",
            "environment": "production",
        }))
        .expect("claims")
    }

    fn policy() -> TrustPolicy {
        TrustPolicy {
            repository_id: 123_456,
            repository_owner_id: 42,
            refs: vec!["refs/heads/main".into(), "refs/tags/v*".into()],
            environments: vec![],
            events: DEFAULT_EVENTS.iter().map(|e| (*e).to_owned()).collect(),
            role: Role::Developer,
            token_ttl_secs: DEFAULT_CI_TOKEN_TTL_SECS,
        }
    }

    #[test]
    fn claims_parse_github_shapes() {
        let c = claims();
        assert_eq!((c.repository_id, c.repository_owner_id), (123_456, 42));
        assert_eq!(c.aud, vec!["https://kuben.example.com".to_owned()]);
        let many: GithubClaims = serde_json::from_value(json!({
            "iss": "i", "aud": ["a", "b"], "sub": "s", "jti": "j", "iat": 1, "exp": 2,
            "repository": "r", "repository_id": 7, "repository_owner": "o",
            "repository_owner_id": "8", "ref": "refs/heads/x", "event_name": "push",
        }))
        .expect("claims");
        assert_eq!(
            (many.aud.len(), many.repository_id, many.environment),
            (2, 7, None)
        );
        assert!(serde_json::from_value::<GithubClaims>(json!({ "iss": "i" })).is_err());
    }

    #[test]
    fn tokens_are_checked_for_issuer_audience_and_time() {
        let c = claims();
        let aud = "https://kuben.example.com";
        assert_eq!(c.check(GITHUB_ACTIONS_ISSUER, aud, NOW), Ok(()));
        assert_eq!(c.check("https://evil", aud, NOW), Err(TokenError::Issuer));
        assert_eq!(
            c.check(GITHUB_ACTIONS_ISSUER, "other", NOW),
            Err(TokenError::Audience)
        );
        assert_eq!(
            c.check(GITHUB_ACTIONS_ISSUER, aud, c.exp + CLOCK_LEEWAY_SECS),
            Err(TokenError::Expired)
        );
        assert_eq!(
            c.check(GITHUB_ACTIONS_ISSUER, aud, c.exp + 10),
            Ok(()),
            "within the leeway"
        );
        assert_eq!(
            c.check(GITHUB_ACTIONS_ISSUER, aud, c.iat - CLOCK_LEEWAY_SECS - 1),
            Err(TokenError::NotYetValid)
        );
        let long = GithubClaims {
            exp: c.iat + MAX_OIDC_LIFETIME_SECS + 1,
            ..c.clone()
        };
        assert_eq!(
            long.check(GITHUB_ACTIONS_ISSUER, aud, NOW),
            Err(TokenError::TooLong)
        );
        let anonymous = GithubClaims {
            jti: String::new(),
            ..c
        };
        assert_eq!(
            anonymous.check(GITHUB_ACTIONS_ISSUER, aud, NOW),
            Err(TokenError::NoId)
        );
    }

    #[test]
    fn ref_patterns_are_exact_or_trailing_prefixes() {
        assert!(ref_matches("refs/heads/main", "refs/heads/main"));
        assert!(!ref_matches("refs/heads/main", "refs/heads/main2"));
        assert!(ref_matches("refs/tags/v*", "refs/tags/v1.2.3"));
        assert!(!ref_matches("refs/tags/v*", "refs/heads/v1"));
    }

    #[test]
    fn a_matching_workflow_is_trusted() {
        assert_eq!(policy().evaluate(&claims()), Ok(()));
        let tag = GithubClaims {
            git_ref: "refs/tags/v2.0.0".into(),
            event_name: "release".into(),
            ..claims()
        };
        assert_eq!(policy().evaluate(&tag), Ok(()));
    }

    #[test]
    fn everything_else_is_denied() {
        let p = policy();
        let cases = [
            (
                GithubClaims {
                    repository_id: 1,
                    ..claims()
                },
                Denied::Repository,
            ),
            (
                GithubClaims {
                    repository_owner_id: 1,
                    ..claims()
                },
                Denied::Owner,
            ),
            (
                GithubClaims {
                    git_ref: "refs/heads/feature".into(),
                    ..claims()
                },
                Denied::Ref,
            ),
            (
                GithubClaims {
                    event_name: "pull_request".into(),
                    ..claims()
                },
                Denied::Event,
            ),
            (
                GithubClaims {
                    event_name: "schedule".into(),
                    ..claims()
                },
                Denied::Event,
            ),
        ];
        for (c, denied) in cases {
            assert_eq!(p.evaluate(&c), Err(denied), "{c:?}");
        }
        let everything = TrustPolicy {
            refs: vec!["refs/heads/*".into()],
            events: vec!["push".into()],
            ..policy()
        };
        let pr = GithubClaims {
            git_ref: "refs/pull/7/merge".into(),
            ..claims()
        };
        assert_eq!(
            everything.evaluate(&pr),
            Err(Denied::Ref),
            "pull-request refs never match"
        );
        let target = GithubClaims {
            event_name: "pull_request_target".into(),
            ..claims()
        };
        let permissive = TrustPolicy {
            events: vec!["pull_request_target".into()],
            ..policy()
        };
        assert_eq!(permissive.evaluate(&target), Err(Denied::Event));
    }

    #[test]
    fn environments_narrow_a_policy() {
        let p = TrustPolicy {
            environments: vec!["production".into()],
            ..policy()
        };
        assert_eq!(p.evaluate(&claims()), Ok(()));
        let staging = GithubClaims {
            environment: Some("staging".into()),
            ..claims()
        };
        assert_eq!(p.evaluate(&staging), Err(Denied::Environment));
        let none = GithubClaims {
            environment: None,
            ..claims()
        };
        assert_eq!(p.evaluate(&none), Err(Denied::Environment));
    }

    #[test]
    fn policies_are_validated() {
        policy().validate().expect("valid");
        let cases = [
            (
                TrustPolicy {
                    refs: vec![],
                    ..policy()
                },
                InvalidPolicy::RefCount,
            ),
            (
                TrustPolicy {
                    refs: vec!["main".into()],
                    ..policy()
                },
                InvalidPolicy::Ref("main".into()),
            ),
            (
                TrustPolicy {
                    refs: vec!["refs/pull/*".into()],
                    ..policy()
                },
                InvalidPolicy::Ref("refs/pull/*".into()),
            ),
            (
                TrustPolicy {
                    refs: vec!["refs/heads/*/x".into()],
                    ..policy()
                },
                InvalidPolicy::Ref("refs/heads/*/x".into()),
            ),
            (
                TrustPolicy {
                    events: vec![],
                    ..policy()
                },
                InvalidPolicy::ListLength,
            ),
            (
                TrustPolicy {
                    events: vec!["pull_request".into()],
                    ..policy()
                },
                InvalidPolicy::Event("pull_request".into()),
            ),
            (
                TrustPolicy {
                    role: Role::Owner,
                    ..policy()
                },
                InvalidPolicy::Role(Role::Owner),
            ),
            (
                TrustPolicy {
                    role: Role::Viewer,
                    ..policy()
                },
                InvalidPolicy::Role(Role::Viewer),
            ),
            (
                TrustPolicy {
                    token_ttl_secs: 7200,
                    ..policy()
                },
                InvalidPolicy::Ttl,
            ),
            (
                TrustPolicy {
                    repository_id: 0,
                    ..policy()
                },
                InvalidPolicy::Ids,
            ),
        ];
        for (p, error) in cases {
            assert_eq!(p.validate(), Err(error), "{p:?}");
        }
    }
}
