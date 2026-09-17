//! Image update policies (M5.4; plan §20.2): which tag of a repository an
//! app follows, and how often to look.
//!
//! A policy follows one of:
//! - a SemVer range (`semver:^1.2`, `semver:1.2.*`, `semver:>=1.0, <2`): the
//!   highest release matching it, prereleases only when the range names one;
//! - a tag glob (`tag:release-*`): the highest matching tag by version-aware
//!   order;
//! - one tag (`latest`, or any other name): its current digest.
//!
//! The registry is asked at most every `interval`, and less often after
//! failures (exponential backoff up to a day).

use std::cmp::Ordering;

use semver::{Version, VersionReq};

/// The shortest interval between checks.
pub const MIN_INTERVAL_SECS: u32 = 60;
/// The longest interval between checks, and the backoff's ceiling.
pub const MAX_INTERVAL_SECS: u32 = 86_400;

/// What a policy follows.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Pattern {
    Semver(VersionReq),
    Glob(String),
    Tag(String),
}

/// Why a pattern is not one.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum PatternError {
    #[error("`{0}` is not a SemVer range: {1}")]
    Range(String, String),
    #[error("`{0}` is not a valid tag or tag glob")]
    Tag(String),
}

fn valid_tag(tag: &str, glob: bool) -> bool {
    !tag.is_empty()
        && tag.len() <= 128
        && tag
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'.' | b'-') || (glob && b == b'*'))
        && !tag.starts_with(['.', '-'])
}

impl Pattern {
    /// Parse `semver:<range>`, `tag:<glob>` or a plain tag.
    pub fn parse(text: &str) -> Result<Self, PatternError> {
        let text = text.trim();
        if let Some(range) = text.strip_prefix("semver:") {
            return VersionReq::parse(range.trim())
                .map(Self::Semver)
                .map_err(|e| PatternError::Range(range.to_owned(), e.to_string()));
        }
        if let Some(glob) = text.strip_prefix("tag:") {
            return if valid_tag(glob, true) {
                Ok(Self::Glob(glob.to_owned()))
            } else {
                Err(PatternError::Tag(glob.to_owned()))
            };
        }
        if valid_tag(text, false) {
            Ok(Self::Tag(text.to_owned()))
        } else {
            Err(PatternError::Tag(text.to_owned()))
        }
    }

    /// The pattern as stored and shown.
    #[must_use]
    pub fn text(&self) -> String {
        match self {
            Self::Semver(req) => format!("semver:{req}"),
            Self::Glob(glob) => format!("tag:{glob}"),
            Self::Tag(tag) => tag.clone(),
        }
    }

    /// The tag to follow among `tags`, if any matches. A plain tag needs no
    /// listing: it is itself.
    #[must_use]
    pub fn select(&self, tags: &[String]) -> Option<String> {
        match self {
            Self::Tag(tag) => Some(tag.clone()),
            Self::Semver(req) => {
                let wants_pre = req.comparators.iter().any(|c| !c.pre.is_empty());
                tags.iter()
                    .filter_map(|t| Some((version(t)?, t)))
                    .filter(|(v, _)| (wants_pre || v.pre.is_empty()) && req.matches(v))
                    .max_by(|(a, _), (b, _)| a.cmp(b))
                    .map(|(_, t)| t.clone())
            }
            Self::Glob(glob) => tags
                .iter()
                .filter(|t| glob_matches(glob, t))
                .max_by(|a, b| natural(a, b))
                .cloned(),
        }
    }

    /// Whether the registry must list tags for this pattern.
    #[must_use]
    pub const fn needs_listing(&self) -> bool {
        !matches!(self, Self::Tag(_))
    }
}

/// A tag as a version: `1.2.3`, `v1.2.3`, `1.2` (as `1.2.0`).
fn version(tag: &str) -> Option<Version> {
    let t = tag.strip_prefix('v').unwrap_or(tag);
    Version::parse(t).ok().or_else(|| {
        let dots = t.bytes().filter(|b| *b == b'.').count();
        match dots {
            1 => Version::parse(&format!("{t}.0")).ok(),
            0 if t.bytes().all(|b| b.is_ascii_digit()) => Version::parse(&format!("{t}.0.0")).ok(),
            _ => None,
        }
    })
}

/// `*` matches any run of characters.
fn glob_matches(glob: &str, tag: &str) -> bool {
    let parts: Vec<&str> = glob.split('*').collect();
    let (first, last) = (parts[0], parts[parts.len() - 1]);
    if parts.len() == 1 {
        return glob == tag;
    }
    if !tag.starts_with(first) || !tag.ends_with(last) || tag.len() < first.len() + last.len() {
        return false;
    }
    let mut rest = &tag[first.len()..tag.len() - last.len()];
    for middle in &parts[1..parts.len() - 1] {
        match rest.find(middle) {
            Some(at) => rest = &rest[at + middle.len()..],
            None => return false,
        }
    }
    true
}

/// Order with digit runs compared as numbers: `r-10` after `r-9`.
fn natural(a: &str, b: &str) -> Ordering {
    let chunks = |s: &str| -> Vec<(bool, String)> {
        let mut out: Vec<(bool, String)> = Vec::new();
        for c in s.chars() {
            let digit = c.is_ascii_digit();
            match out.last_mut() {
                Some((d, run)) if *d == digit => run.push(c),
                _ => out.push((digit, c.to_string())),
            }
        }
        out
    };
    let (x, y) = (chunks(a), chunks(b));
    for ((dx, cx), (dy, cy)) in x.iter().zip(&y) {
        let order = if *dx && *dy {
            let (tx, ty) = (cx.trim_start_matches('0'), cy.trim_start_matches('0'));
            tx.len().cmp(&ty.len()).then_with(|| tx.cmp(ty))
        } else {
            cx.cmp(cy)
        };
        if order != Ordering::Equal {
            return order;
        }
    }
    x.len().cmp(&y.len())
}

/// When to look again after `failures` failures in a row, with `interval`
/// between successful checks (or `retry_after` asked by the registry).
#[must_use]
pub fn next_check(now: i64, interval_secs: u32, failures: u32, retry_after: Option<u64>) -> i64 {
    let base = u64::from(interval_secs.clamp(MIN_INTERVAL_SECS, MAX_INTERVAL_SECS));
    let backoff = base
        .saturating_mul(1u64 << failures.min(16))
        .min(u64::from(MAX_INTERVAL_SECS));
    let wait = retry_after.map_or(backoff, |r| backoff.max(r));
    now.saturating_add(i64::try_from(wait.saturating_mul(1000)).unwrap_or(i64::MAX))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tags(list: &[&str]) -> Vec<String> {
        list.iter().map(|t| (*t).to_owned()).collect()
    }

    #[test]
    fn patterns_parse_and_print() {
        assert_eq!(Pattern::parse("latest"), Ok(Pattern::Tag("latest".into())));
        assert_eq!(
            Pattern::parse("tag:release-*").map(|p| p.text()).as_deref(),
            Ok("tag:release-*")
        );
        assert_eq!(
            Pattern::parse("semver:1.2.*").map(|p| p.text()).as_deref(),
            Ok("semver:1.2.*")
        );
        assert!(Pattern::parse("semver:not a range").is_err());
        assert!(Pattern::parse("bad tag").is_err());
        assert!(Pattern::parse("-x").is_err());
        assert!(Pattern::parse("lat*est").is_err(), "globs need the tag: prefix");
        assert!(!Pattern::parse("latest").expect("tag").needs_listing());
    }

    #[test]
    fn semver_ranges_pick_the_highest_release() {
        let all = tags(&[
            "1.2.0",
            "v1.2.9",
            "1.2.10",
            "1.3.0",
            "1.2.11-rc.1",
            "latest",
            "1.2",
        ]);
        let p = Pattern::parse("semver:1.2.*").expect("range");
        assert_eq!(p.select(&all).as_deref(), Some("1.2.10"));
        let p = Pattern::parse("semver:^1").expect("range");
        assert_eq!(p.select(&all).as_deref(), Some("1.3.0"));
        let p = Pattern::parse("semver:>=1.2.11-rc.0, <1.3.0").expect("range");
        assert_eq!(
            p.select(&all).as_deref(),
            Some("1.2.11-rc.1"),
            "prereleases when asked for"
        );
        let p = Pattern::parse("semver:^2").expect("range");
        assert_eq!(p.select(&all), None);
        let p = Pattern::parse("semver:~1.2").expect("range");
        assert_eq!(
            p.select(&tags(&["1.2"])).as_deref(),
            Some("1.2"),
            "short versions count"
        );
    }

    #[test]
    fn globs_pick_the_highest_match() {
        let all = tags(&["release-9", "release-10", "release-2", "nightly-99"]);
        let p = Pattern::parse("tag:release-*").expect("glob");
        assert_eq!(p.select(&all).as_deref(), Some("release-10"));
        assert!(glob_matches("a*b*c", "aXXbYYc"));
        assert!(!glob_matches("a*b*c", "aXXcYYb"));
        assert!(!glob_matches("ab*ba", "aba"));
        assert_eq!(
            Pattern::Tag("latest".into()).select(&[]).as_deref(),
            Some("latest")
        );
    }

    #[test]
    fn checks_back_off_after_failures() {
        assert_eq!(next_check(0, 300, 0, None), 300_000);
        assert_eq!(next_check(0, 300, 2, None), 1_200_000);
        assert_eq!(next_check(0, 300, 30, None), 86_400_000, "a day at most");
        assert_eq!(next_check(0, 10, 0, None), 60_000, "a minute at least");
        assert_eq!(next_check(0, 300, 0, Some(900)), 900_000, "the registry's wish");
    }
}
