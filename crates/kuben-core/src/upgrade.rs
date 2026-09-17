//! Upgrades (M4.8; plan §17.4, S07): which version steps are supported, and
//! what must hold before one starts.
//!
//! Kuben upgrades one minor version at a time (patch releases in between do
//! not count) and never goes back: a database a newer Kuben migrated is not
//! run by an older one. A major step is allowed from the newest minor of the
//! previous major only after its notes were read, so it needs `--major`.
//! The preflight is a list of findings; any failure stops the upgrade, and
//! nothing has changed by then.

use std::{cmp::Ordering, fmt, ops::RangeInclusive};

/// A release version, `MAJOR.MINOR.PATCH[-PRE]`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Version {
    pub major: u64,
    pub minor: u64,
    pub patch: u64,
    pub pre: Option<String>,
}

impl Version {
    /// Parse `v1.2.3`, `1.2.3` or `1.2.3-rc.1` (build metadata is ignored).
    #[must_use]
    pub fn parse(s: &str) -> Option<Self> {
        let s = s.trim().trim_start_matches('v');
        let s = s.split_once('+').map_or(s, |(v, _)| v);
        let (core, pre) = match s.split_once('-') {
            Some((core, pre)) if !pre.is_empty() => (core, Some(pre.to_owned())),
            Some(_) => return None,
            None => (s, None),
        };
        let mut parts = core.split('.').map(|p| p.parse::<u64>().ok());
        let (Some(Some(major)), Some(Some(minor)), Some(Some(patch)), None) =
            (parts.next(), parts.next(), parts.next(), parts.next())
        else {
            return None;
        };
        Some(Self {
            major,
            minor,
            patch,
            pre,
        })
    }
}

impl Ord for Version {
    fn cmp(&self, other: &Self) -> Ordering {
        (self.major, self.minor, self.patch)
            .cmp(&(other.major, other.minor, other.patch))
            // A pre-release comes before its release.
            .then_with(|| match (&self.pre, &other.pre) {
                (None, None) => Ordering::Equal,
                (Some(_), None) => Ordering::Less,
                (None, Some(_)) => Ordering::Greater,
                (Some(a), Some(b)) => a.cmp(b),
            })
    }
}

impl PartialOrd for Version {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

impl fmt::Display for Version {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}.{}.{}", self.major, self.minor, self.patch)?;
        if let Some(pre) = &self.pre {
            write!(f, "-{pre}")?;
        }
        Ok(())
    }
}

/// What kind of step an upgrade is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Step {
    /// The same version: a restart or a repair.
    Same,
    Patch,
    Minor,
    Major,
}

/// Why a step is refused.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum PathError {
    #[error("`{0}` is not a version")]
    Invalid(String),
    #[error("{from} → {to} goes back: a newer Kuben's database is not run by an older one")]
    Downgrade { from: Version, to: Version },
    #[error("{from} → {to} skips minor versions: upgrade to {from_major}.{next_minor} first")]
    SkipsMinor {
        from: Version,
        to: Version,
        from_major: u64,
        next_minor: u64,
    },
    #[error("{from} → {to} is a major upgrade: read its release notes, then pass --major")]
    MajorNeedsConsent { from: Version, to: Version },
    #[error("{from} → {to} skips a major version")]
    SkipsMajor { from: Version, to: Version },
}

/// Whether going from `from` to `to` is supported; `major` is the operator's
/// consent to a major step.
pub fn step(from: &str, to: &str, major: bool) -> Result<Step, PathError> {
    let parse = |s: &str| Version::parse(s).ok_or_else(|| PathError::Invalid(s.to_owned()));
    let (from, to) = (parse(from)?, parse(to)?);
    match to.cmp(&from) {
        Ordering::Less => return Err(PathError::Downgrade { from, to }),
        Ordering::Equal => return Ok(Step::Same),
        Ordering::Greater => {}
    }
    if to.major == from.major {
        return match to.minor - from.minor {
            0 => Ok(Step::Patch),
            1 => Ok(Step::Minor),
            _ => Err(PathError::SkipsMinor {
                from_major: from.major,
                next_minor: from.minor + 1,
                from,
                to,
            }),
        };
    }
    if to.major > from.major + 1 {
        return Err(PathError::SkipsMajor { from, to });
    }
    if major {
        Ok(Step::Major)
    } else {
        Err(PathError::MajorNeedsConsent { from, to })
    }
}

/// How serious a finding is.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Level {
    Ok,
    Warn,
    Fail,
}

/// One preflight finding.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Finding {
    pub level: Level,
    pub check: &'static str,
    pub detail: String,
}

impl Finding {
    fn new(level: Level, check: &'static str, detail: impl Into<String>) -> Self {
        Self {
            level,
            check,
            detail: detail.into(),
        }
    }
}

/// What the preflight looks at.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Facts {
    /// The newest version that ran against this database, if any.
    pub running: Option<String>,
    /// The version about to run.
    pub target: String,
    /// The operator consents to a major step.
    pub major: bool,
    /// The newest migration applied, and the newest the target carries.
    pub schema: i64,
    pub target_schema: i64,
    /// A migration failed half-way.
    pub dirty_schema: bool,
    /// When the newest good backup finished (Unix milliseconds).
    pub last_backup: Option<i64>,
    pub now: i64,
    /// Operations not settled yet.
    pub active_operations: u64,
    /// `(cluster, protocol version)` of every linked, unrevoked agent.
    pub agents: Vec<(String, Option<u32>)>,
    /// The protocol versions the target speaks.
    pub target_protocols: RangeInclusive<u32>,
    /// Free bytes where backups and state go.
    pub free_bytes: Option<u64>,
}

/// A backup older than this does not protect an upgrade.
pub const BACKUP_FRESH_MS: i64 = 24 * 3_600_000;
/// Less free space than this is a warning.
pub const MIN_FREE_BYTES: u64 = 1 << 30;

/// Every finding about `facts`, most serious first.
#[must_use]
pub fn preflight(facts: &Facts) -> Vec<Finding> {
    let mut out = Vec::new();
    out.push(match &facts.running {
        None => Finding::new(Level::Ok, "version", format!("first start of {}", facts.target)),
        Some(running) => match step(running, &facts.target, facts.major) {
            Ok(Step::Same) => Finding::new(Level::Ok, "version", format!("{running} again")),
            Ok(kind) => Finding::new(
                Level::Ok,
                "version",
                format!("{running} → {} ({kind:?})", facts.target),
            ),
            Err(e) => Finding::new(Level::Fail, "version", e.to_string()),
        },
    });
    out.push(if facts.dirty_schema {
        Finding::new(
            Level::Fail,
            "schema",
            "a migration failed half-way: restore the pre-upgrade backup, or fix the database and mark it done",
        )
    } else if facts.schema > facts.target_schema {
        Finding::new(
            Level::Fail,
            "schema",
            format!(
                "the database is at schema {}, newer than {} knows ({}): run a newer Kuben",
                facts.schema, facts.target, facts.target_schema
            ),
        )
    } else {
        Finding::new(
            Level::Ok,
            "schema",
            format!("{} → {} ({} migration(s))", facts.schema, facts.target_schema, facts.target_schema - facts.schema),
        )
    });
    let pending = facts.target_schema > facts.schema;
    out.push(match facts.last_backup {
        Some(at) if facts.now - at <= BACKUP_FRESH_MS => Finding::new(
            Level::Ok,
            "backup",
            format!("the newest good backup is {} h old", (facts.now - at) / 3_600_000),
        ),
        _ if !pending => Finding::new(
            Level::Warn,
            "backup",
            "no backup from the last 24 h (no migrations pending)",
        ),
        _ => Finding::new(
            Level::Fail,
            "backup",
            "migrations are pending and there is no backup from the last 24 h: run `kuben backup`",
        ),
    });
    out.push(if facts.active_operations == 0 {
        Finding::new(Level::Ok, "operations", "none in flight")
    } else {
        Finding::new(
            Level::Warn,
            "operations",
            format!(
                "{} in flight: they resume after the upgrade, but a quiet moment is safer",
                facts.active_operations
            ),
        )
    });
    out.push(agents(facts));
    out.push(disk(facts.free_bytes));
    out.sort_by_key(|f| std::cmp::Reverse(f.level));
    out
}

fn agents(facts: &Facts) -> Finding {
    let stale: Vec<String> = facts
        .agents
        .iter()
        .filter(|(_, p)| p.is_some_and(|p| !facts.target_protocols.contains(&p)))
        .map(|(cluster, p)| format!("{cluster} (protocol {})", p.unwrap_or_default()))
        .collect();
    if stale.is_empty() {
        Finding::new(
            Level::Ok,
            "agents",
            format!("{} linked, all compatible", facts.agents.len()),
        )
    } else {
        Finding::new(
            Level::Fail,
            "agents",
            format!(
                "{} would be refused by {} (it speaks {}–{}): upgrade them first",
                stale.join(", "),
                facts.target,
                facts.target_protocols.start(),
                facts.target_protocols.end()
            ),
        )
    }
}

fn disk(free_bytes: Option<u64>) -> Finding {
    match free_bytes {
        Some(free) if free < MIN_FREE_BYTES => Finding::new(
            Level::Warn,
            "disk",
            format!("only {} MiB free for backups and state", free >> 20),
        ),
        Some(free) => Finding::new(Level::Ok, "disk", format!("{} GiB free", free >> 30)),
        None => Finding::new(Level::Warn, "disk", "free space unknown"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn versions_parse_and_order() {
        let v = |s: &str| Version::parse(s).expect(s);
        assert_eq!(
            v("v1.2.3"),
            Version {
                major: 1,
                minor: 2,
                patch: 3,
                pre: None
            }
        );
        assert_eq!(v("1.2.3-rc.1+build.5").pre.as_deref(), Some("rc.1"));
        assert!(v("1.2.3-rc.1") < v("1.2.3"));
        assert!(v("1.10.0") > v("1.9.9"));
        assert_eq!(v("2.0.0-beta").to_string(), "2.0.0-beta");
        for bad in ["", "1.2", "1.2.3.4", "a.b.c", "1.2.3-", "1..3"] {
            assert_eq!(Version::parse(bad), None, "{bad}");
        }
    }

    #[test]
    fn upgrades_go_forward_one_minor_at_a_time() {
        assert_eq!(step("1.2.3", "1.2.3", false), Ok(Step::Same));
        assert_eq!(step("1.2.3", "1.2.9", false), Ok(Step::Patch));
        assert_eq!(step("1.2.3", "1.3.0", false), Ok(Step::Minor));
        assert_eq!(step("1.3.0-rc.1", "1.3.0", false), Ok(Step::Patch));
        assert!(matches!(
            step("1.3.0", "1.2.9", false),
            Err(PathError::Downgrade { .. })
        ));
        assert!(matches!(
            step("1.2.3", "1.4.0", false),
            Err(PathError::SkipsMinor { next_minor: 3, .. })
        ));
        assert!(matches!(
            step("1.9.0", "2.0.0", false),
            Err(PathError::MajorNeedsConsent { .. })
        ));
        assert_eq!(step("1.9.0", "2.0.0", true), Ok(Step::Major));
        assert!(matches!(
            step("1.9.0", "3.0.0", true),
            Err(PathError::SkipsMajor { .. })
        ));
        assert!(matches!(step("x", "1.0.0", false), Err(PathError::Invalid(_))));
    }

    fn facts() -> Facts {
        Facts {
            running: Some("1.2.0".into()),
            target: "1.3.0".into(),
            major: false,
            schema: 24,
            target_schema: 26,
            dirty_schema: false,
            last_backup: Some(1_000),
            now: 1_000 + 3_600_000,
            active_operations: 0,
            agents: vec![("primary".into(), Some(1))],
            target_protocols: 1..=2,
            free_bytes: Some(10 << 30),
        }
    }

    fn level(findings: &[Finding], check: &str) -> Level {
        findings.iter().find(|f| f.check == check).expect(check).level
    }

    #[test]
    fn a_prepared_upgrade_passes() {
        let findings = preflight(&facts());
        assert!(findings.iter().all(|f| f.level == Level::Ok), "{findings:?}");
    }

    #[test]
    fn what_would_break_the_upgrade_fails_it() {
        let f = preflight(&Facts {
            running: Some("1.4.0".into()),
            ..facts()
        });
        assert_eq!(level(&f, "version"), Level::Fail);
        assert_eq!(f[0].level, Level::Fail, "failures first");
        let ahead = preflight(&Facts {
            schema: 30,
            ..facts()
        });
        assert_eq!(level(&ahead, "schema"), Level::Fail);
        let dirty = preflight(&Facts {
            dirty_schema: true,
            ..facts()
        });
        assert_eq!(level(&dirty, "schema"), Level::Fail);
        let unbacked = preflight(&Facts {
            last_backup: None,
            ..facts()
        });
        assert_eq!(level(&unbacked, "backup"), Level::Fail);
        let old_backup = preflight(&Facts {
            now: 1_000 + BACKUP_FRESH_MS + 1,
            ..facts()
        });
        assert_eq!(level(&old_backup, "backup"), Level::Fail);
        let nothing_pending = preflight(&Facts {
            last_backup: None,
            schema: 26,
            ..facts()
        });
        assert_eq!(level(&nothing_pending, "backup"), Level::Warn);
        let stale_agent = preflight(&Facts {
            target_protocols: 2..=3,
            ..facts()
        });
        assert_eq!(level(&stale_agent, "agents"), Level::Fail);
        let unknown_agent = preflight(&Facts {
            agents: vec![("edge".into(), None)],
            ..facts()
        });
        assert_eq!(level(&unknown_agent, "agents"), Level::Ok, "never linked yet");
        let busy = preflight(&Facts {
            active_operations: 3,
            free_bytes: Some(1 << 20),
            ..facts()
        });
        assert_eq!(
            (level(&busy, "operations"), level(&busy, "disk")),
            (Level::Warn, Level::Warn)
        );
        let first = preflight(&Facts {
            running: None,
            ..facts()
        });
        assert_eq!(level(&first, "version"), Level::Ok);
    }
}
