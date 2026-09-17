//! Single sign-on with an OpenID Connect identity provider (M4.3; plan §13,
//! S04): which people may sign in and with which organization role.
//!
//! The role comes from the provider's groups through an explicit mapping and
//! is set again at every sign-in, so a person moved out of a group loses the
//! role the next time they sign in, and a person in no mapped group (and no
//! default role) cannot sign in at all. Email addresses must be verified by
//! the provider and may be limited to domains.

use std::collections::BTreeMap;

use serde::{Deserialize, Deserializer};

use crate::perm::Role;

/// Clock skew tolerated on `exp` and `iat`.
pub const CLOCK_LEEWAY_SECS: i64 = 60;
/// An ID token living longer than this is refused.
pub const MAX_ID_TOKEN_LIFETIME_SECS: i64 = 24 * 3600;
/// How long a started sign-in may take.
pub const LOGIN_WINDOW_SECS: i64 = 10 * 60;

/// The claims of an ID token Kuben reads. Groups come from a configurable
/// claim, so the whole payload is kept.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
pub struct IdClaims {
    pub iss: String,
    #[serde(deserialize_with = "audiences")]
    pub aud: Vec<String>,
    pub sub: String,
    pub exp: i64,
    pub iat: i64,
    #[serde(default)]
    pub nonce: Option<String>,
    #[serde(default)]
    pub email: Option<String>,
    #[serde(default)]
    pub email_verified: Option<bool>,
    #[serde(default)]
    pub name: Option<String>,
    #[serde(flatten)]
    pub other: BTreeMap<String, serde_json::Value>,
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

/// Why an ID token or the person it names was refused.
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
pub enum SsoDenied {
    #[error("the token was issued by another issuer")]
    Issuer,
    #[error("the token is for another client")]
    Audience,
    #[error("the token has expired or lives too long")]
    Expired,
    #[error("the token is not valid yet")]
    NotYetValid,
    #[error("the token does not belong to this sign-in")]
    Nonce,
    #[error("the provider sent no subject or email")]
    Identity,
    #[error("the provider has not verified the email address")]
    Unverified,
    #[error("the email domain is not allowed")]
    Domain,
    #[error("no group of this person is mapped to a role")]
    NoRole,
}

impl IdClaims {
    /// Check issuer, audience, nonce and times at `now` (Unix seconds).
    pub fn check(&self, issuer: &str, client_id: &str, nonce: &str, now: i64) -> Result<(), SsoDenied> {
        if self.iss != issuer {
            return Err(SsoDenied::Issuer);
        }
        if !self.aud.iter().any(|a| a == client_id) {
            return Err(SsoDenied::Audience);
        }
        if self.nonce.as_deref() != Some(nonce) {
            return Err(SsoDenied::Nonce);
        }
        if now >= self.exp.saturating_add(CLOCK_LEEWAY_SECS)
            || self.exp.saturating_sub(self.iat) > MAX_ID_TOKEN_LIFETIME_SECS
        {
            return Err(SsoDenied::Expired);
        }
        if now.saturating_add(CLOCK_LEEWAY_SECS) < self.iat {
            return Err(SsoDenied::NotYetValid);
        }
        Ok(())
    }

    /// The groups in `claim`: a list of strings, or one string.
    #[must_use]
    pub fn groups(&self, claim: &str) -> Vec<String> {
        match self.other.get(claim) {
            Some(serde_json::Value::Array(items)) => items
                .iter()
                .filter_map(|g| g.as_str().map(str::to_owned))
                .collect(),
            Some(serde_json::Value::String(one)) => vec![one.clone()],
            _ => Vec::new(),
        }
    }
}

/// Who may sign in, and as what.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SsoPolicy {
    /// Provider group → organization role.
    pub groups: BTreeMap<String, Role>,
    /// The role of people in no mapped group; `None` refuses them.
    pub default_role: Option<Role>,
    /// Email domains allowed (lowercase); empty for any.
    pub allowed_domains: Vec<String>,
    /// Refuse emails the provider has not verified (or says nothing about).
    pub require_verified_email: bool,
}

/// A person the provider vouched for.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SsoPerson {
    pub subject: String,
    pub email: String,
    pub name: Option<String>,
    pub role: Role,
}

impl SsoPolicy {
    /// The person `claims` names, with the role their `groups` earn.
    pub fn admit(&self, claims: &IdClaims, groups: &[String]) -> Result<SsoPerson, SsoDenied> {
        let email = claims
            .email
            .as_deref()
            .map(|e| e.trim().to_ascii_lowercase())
            .filter(|e| e.contains('@'))
            .ok_or(SsoDenied::Identity)?;
        if claims.sub.is_empty() || claims.sub.len() > 255 {
            return Err(SsoDenied::Identity);
        }
        if self.require_verified_email && claims.email_verified != Some(true) {
            return Err(SsoDenied::Unverified);
        }
        let domain = email.rsplit_once('@').map_or("", |(_, d)| d);
        if !self.allowed_domains.is_empty() && !self.allowed_domains.iter().any(|d| d == domain) {
            return Err(SsoDenied::Domain);
        }
        let role = groups
            .iter()
            .filter_map(|g| self.groups.get(g))
            .copied()
            .max_by_key(|r| r.rank())
            .or(self.default_role)
            .ok_or(SsoDenied::NoRole)?;
        Ok(SsoPerson {
            subject: claims.sub.clone(),
            email,
            name: claims.name.clone().filter(|n| !n.trim().is_empty()),
            role,
        })
    }
}

/// Where to go after signing in: a path on this site, never another site.
#[must_use]
pub fn safe_return_to(path: Option<&str>) -> String {
    match path {
        Some(p)
            if p.starts_with('/')
                && !p.starts_with("//")
                && !p.contains('\\')
                && p.len() <= 512
                && !p.chars().any(char::is_control) =>
        {
            p.to_owned()
        }
        _ => "/".to_owned(),
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    const NOW: i64 = 1_800_000_100;

    fn claims(extra: &serde_json::Value) -> IdClaims {
        let mut base = json!({
            "iss": "https://idp.example.com", "aud": "kuben-console", "sub": "u-1",
            "iat": 1_800_000_000, "exp": 1_800_000_300, "nonce": "n-1",
            "email": "Carol@Example.com", "email_verified": true, "name": "Carol",
            "groups": ["platform-admins", "devs"],
        });
        if let (Some(b), Some(e)) = (base.as_object_mut(), extra.as_object()) {
            b.extend(e.clone());
        }
        serde_json::from_value(base).expect("claims")
    }

    fn policy() -> SsoPolicy {
        SsoPolicy {
            groups: BTreeMap::from([
                ("devs".into(), Role::Developer),
                ("platform-admins".into(), Role::Admin),
            ]),
            default_role: None,
            allowed_domains: vec!["example.com".into()],
            require_verified_email: true,
        }
    }

    #[test]
    fn id_tokens_are_bound_to_issuer_client_nonce_and_time() {
        let c = claims(&json!({}));
        let check = |c: &IdClaims, nonce: &str, now: i64| {
            c.check("https://idp.example.com", "kuben-console", nonce, now)
        };
        assert_eq!(check(&c, "n-1", NOW), Ok(()));
        assert_eq!(check(&c, "n-2", NOW), Err(SsoDenied::Nonce));
        assert_eq!(
            check(&claims(&json!({ "nonce": null })), "n-1", NOW),
            Err(SsoDenied::Nonce)
        );
        assert_eq!(
            check(&claims(&json!({ "iss": "https://evil" })), "n-1", NOW),
            Err(SsoDenied::Issuer)
        );
        assert_eq!(
            check(&claims(&json!({ "aud": ["x", "y"] })), "n-1", NOW),
            Err(SsoDenied::Audience)
        );
        assert_eq!(
            check(&c, "n-1", c.exp + CLOCK_LEEWAY_SECS),
            Err(SsoDenied::Expired)
        );
        assert_eq!(
            check(&c, "n-1", c.iat - CLOCK_LEEWAY_SECS - 1),
            Err(SsoDenied::NotYetValid)
        );
        let long = claims(&json!({ "exp": 1_800_000_000 + MAX_ID_TOKEN_LIFETIME_SECS + 1 }));
        assert_eq!(check(&long, "n-1", NOW), Err(SsoDenied::Expired));
    }

    #[test]
    fn groups_come_from_the_configured_claim() {
        let c = claims(&json!({ "roles": "ops" }));
        assert_eq!(c.groups("groups"), vec!["platform-admins", "devs"]);
        assert_eq!(c.groups("roles"), vec!["ops"]);
        assert!(c.groups("missing").is_empty());
    }

    #[test]
    fn the_strongest_mapped_group_decides_the_role() {
        let c = claims(&json!({}));
        let person = policy().admit(&c, &c.groups("groups")).expect("admitted");
        assert_eq!(person.role, Role::Admin);
        assert_eq!(person.email, "carol@example.com");
        assert_eq!(person.name.as_deref(), Some("Carol"));
        let dev = policy().admit(&c, &["devs".into()]).expect("admitted");
        assert_eq!(dev.role, Role::Developer);
    }

    #[test]
    fn unknown_unverified_and_foreign_people_are_refused() {
        let p = policy();
        let groups = vec!["devs".to_owned()];
        assert_eq!(
            p.admit(&claims(&json!({})), &["marketing".into()]),
            Err(SsoDenied::NoRole)
        );
        let defaulted = SsoPolicy {
            default_role: Some(Role::Viewer),
            ..policy()
        };
        assert_eq!(
            defaulted.admit(&claims(&json!({})), &[]).map(|p| p.role),
            Ok(Role::Viewer)
        );
        assert_eq!(
            p.admit(&claims(&json!({ "email_verified": false })), &groups),
            Err(SsoDenied::Unverified)
        );
        assert_eq!(
            p.admit(&claims(&json!({ "email_verified": null })), &groups),
            Err(SsoDenied::Unverified)
        );
        let trusting = SsoPolicy {
            require_verified_email: false,
            ..policy()
        };
        assert!(
            trusting
                .admit(&claims(&json!({ "email_verified": null })), &groups)
                .is_ok()
        );
        assert_eq!(
            p.admit(&claims(&json!({ "email": "eve@evil.example" })), &groups),
            Err(SsoDenied::Domain)
        );
        assert_eq!(
            p.admit(&claims(&json!({ "email": "eve@sub.example.com" })), &groups),
            Err(SsoDenied::Domain),
            "domains match exactly"
        );
        assert_eq!(
            p.admit(&claims(&json!({ "email": null })), &groups),
            Err(SsoDenied::Identity)
        );
        assert_eq!(
            p.admit(&claims(&json!({ "sub": "" })), &groups),
            Err(SsoDenied::Identity)
        );
    }

    #[test]
    fn sign_ins_only_return_to_this_site() {
        assert_eq!(safe_return_to(Some("/projects/shop")), "/projects/shop");
        for bad in [
            "//evil.example",
            "https://evil.example",
            "/\\evil",
            "projects",
            "/a\nb",
        ] {
            assert_eq!(safe_return_to(Some(bad)), "/", "{bad:?}");
        }
        assert_eq!(safe_return_to(None), "/");
    }
}
