//! Domain names and claims (M5.2; plan §14.3).
//!
//! A host is compared only in canonical form: lower case, IDNA (punycode),
//! no trailing dot. A verified claim on `example.com` covers the name itself
//! and every name below it, so two organizations' claims must never
//! overlap.

/// The TXT record that proves a claim on `domain`.
pub const CHALLENGE_LABEL: &str = "_kuben-challenge";

/// Why a name is not a claimable domain.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum DomainError {
    #[error("`{0}` is not a valid domain name")]
    Invalid(String),
    #[error("`{0}` needs at least two labels")]
    TooShort(String),
    #[error("wildcards are not claimed: claim `{0}`, which covers every name below it")]
    Wildcard(String),
}

/// `name` in canonical form: IDNA (punycode), lower case, no trailing dot.
pub fn canonical(name: &str) -> Result<String, DomainError> {
    let trimmed = name.trim().trim_end_matches('.');
    if let Some(rest) = trimmed.strip_prefix("*.") {
        return Err(DomainError::Wildcard(rest.to_ascii_lowercase()));
    }
    let ascii = idna::domain_to_ascii(trimmed).map_err(|_| DomainError::Invalid(name.to_owned()))?;
    let valid = !ascii.is_empty()
        && ascii.len() <= 253
        && ascii.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && label.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')
                && !label.starts_with('-')
                && !label.ends_with('-')
        });
    if !valid {
        return Err(DomainError::Invalid(name.to_owned()));
    }
    if !ascii.contains('.') {
        return Err(DomainError::TooShort(ascii));
    }
    Ok(ascii)
}

/// Whether a claim on `claim` covers `host` (both canonical).
#[must_use]
pub fn covers(claim: &str, host: &str) -> bool {
    host == claim
        || host
            .strip_suffix(claim)
            .is_some_and(|prefix| prefix.ends_with('.'))
}

/// Whether claims on `a` and `b` (both canonical) overlap.
#[must_use]
pub fn overlaps(a: &str, b: &str) -> bool {
    covers(a, b) || covers(b, a)
}

/// The name whose lock serializes claims that could overlap `domain`: its
/// last two labels.
#[must_use]
pub fn lock_key(domain: &str) -> &str {
    let mut dots = domain.rmatch_indices('.');
    dots.next();
    match dots.next() {
        Some((at, _)) => &domain[at + 1..],
        None => domain,
    }
}

/// The TXT record name for `domain`.
#[must_use]
pub fn challenge_name(domain: &str) -> String {
    format!("{CHALLENGE_LABEL}.{domain}")
}

/// Every name that could hold a claim covering `host`, from the host itself
/// up to its last two labels (the candidates to look up).
#[must_use]
pub fn ancestors(host: &str) -> Vec<&str> {
    let mut out = vec![host];
    let mut rest = host;
    while let Some((_, parent)) = rest.split_once('.') {
        if !parent.contains('.') {
            break;
        }
        out.push(parent);
        rest = parent;
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names_are_made_canonical() {
        assert_eq!(canonical("App.Example.COM.").as_deref(), Ok("app.example.com"));
        assert_eq!(
            canonical("bücher.example").as_deref(),
            Ok("xn--bcher-kva.example")
        );
        assert_eq!(
            canonical("localhost"),
            Err(DomainError::TooShort("localhost".into()))
        );
        assert!(matches!(canonical("*.example.com"), Err(DomainError::Wildcard(d)) if d == "example.com"));
        for bad in [
            "",
            "a..b",
            "-a.com",
            "a-.com",
            "a_b.com",
            "a b.com",
            &format!("{}.com", "a".repeat(64)),
        ] {
            assert!(canonical(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn claims_cover_their_subdomains_only() {
        assert!(covers("example.com", "example.com"));
        assert!(covers("example.com", "a.b.example.com"));
        assert!(!covers("example.com", "badexample.com"));
        assert!(!covers("a.example.com", "example.com"));
        assert!(overlaps("a.example.com", "example.com"));
        assert!(!overlaps("a.example.com", "b.example.com"));
    }

    #[test]
    fn locks_and_ancestors_follow_the_last_two_labels() {
        assert_eq!(lock_key("a.b.example.com"), "example.com");
        assert_eq!(lock_key("example.com"), "example.com");
        assert_eq!(
            ancestors("a.b.example.com"),
            ["a.b.example.com", "b.example.com", "example.com"]
        );
        assert_eq!(ancestors("example.com"), ["example.com"]);
        assert_eq!(challenge_name("example.com"), "_kuben-challenge.example.com");
    }
}
