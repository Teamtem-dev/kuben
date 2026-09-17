//! Image SBOMs and vulnerability scans (M4.6; plan §13.3, §18.1).
//!
//! Every built image is scanned by digest; the scan records the scanner and
//! the age of its vulnerability database, because a scan is only as fresh as
//! its feed and an unavailable feed is never "clean". An environment's gate
//! decides what a deployment may carry: nothing (off), a warning, or a
//! refusal of findings at or above a severity that no unexpired exception
//! covers, and optionally of images without a fresh scan. Provenance,
//! signatures and findings are separate questions; this module answers only
//! the last.

use std::collections::BTreeSet;

use serde::{Deserialize, Serialize};

/// Findings listed by id in a scan, at most.
pub const MAX_LISTED: usize = 40;
/// Longest exception a person may grant.
pub const MAX_EXCEPTION_SECS: i64 = 90 * 24 * 3600;
/// Default age after which a scan no longer counts.
pub const DEFAULT_MAX_AGE_SECS: u32 = 7 * 24 * 3600;

/// A finding's severity, as scanners report it.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Severity {
    Unknown,
    Low,
    Medium,
    High,
    Critical,
}

impl Severity {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Unknown => "unknown",
            Self::Low => "low",
            Self::Medium => "medium",
            Self::High => "high",
            Self::Critical => "critical",
        }
    }

    #[must_use]
    pub fn parse(s: &str) -> Option<Self> {
        Some(match s.to_ascii_lowercase().as_str() {
            "unknown" => Self::Unknown,
            "low" => Self::Low,
            "medium" => Self::Medium,
            "high" => Self::High,
            "critical" => Self::Critical,
            _ => return None,
        })
    }
}

/// Findings per severity.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Counts {
    #[serde(default)]
    pub critical: u32,
    #[serde(default)]
    pub high: u32,
    #[serde(default)]
    pub medium: u32,
    #[serde(default)]
    pub low: u32,
    #[serde(default)]
    pub unknown: u32,
}

impl Counts {
    /// Findings at `severity` or above.
    #[must_use]
    pub const fn at_least(&self, severity: Severity) -> u32 {
        let mut n = self.critical;
        if matches!(
            severity,
            Severity::High | Severity::Medium | Severity::Low | Severity::Unknown
        ) {
            n = n.saturating_add(self.high);
        }
        if matches!(severity, Severity::Medium | Severity::Low | Severity::Unknown) {
            n = n.saturating_add(self.medium);
        }
        if matches!(severity, Severity::Low | Severity::Unknown) {
            n = n.saturating_add(self.low);
        }
        if matches!(severity, Severity::Unknown) {
            n = n.saturating_add(self.unknown);
        }
        n
    }
}

/// Whether a scan could run.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ScanStatus {
    Ok,
    /// The scanner, its feed or the image was not available.
    Unavailable,
}

impl ScanStatus {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Ok => "ok",
            Self::Unavailable => "unavailable",
        }
    }
}

/// One finding by id: `(severity, id)`.
pub type Finding = (Severity, String);

/// What a scan container writes to its termination log (at most 4 KiB).
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct ScanReport {
    pub status: ScanStatus,
    #[serde(default)]
    pub scanner: String,
    /// When the vulnerability database was built (RFC 3339).
    #[serde(default)]
    pub db: Option<String>,
    #[serde(default)]
    pub counts: Counts,
    /// `SEVERITY:ID`, most severe first.
    #[serde(default)]
    pub findings: Vec<String>,
    #[serde(default)]
    pub detail: Option<String>,
}

impl ScanReport {
    /// A report of a scan that could not run.
    #[must_use]
    pub fn unavailable(detail: impl Into<String>) -> Self {
        Self {
            status: ScanStatus::Unavailable,
            scanner: String::new(),
            db: None,
            counts: Counts::default(),
            findings: Vec::new(),
            detail: Some(detail.into()),
        }
    }

    /// The listed findings that parse, at most [`MAX_LISTED`].
    #[must_use]
    pub fn listed(&self) -> Vec<Finding> {
        self.findings
            .iter()
            .filter_map(|f| {
                let (severity, id) = f.split_once(':')?;
                let id = id.trim();
                let valid = !id.is_empty()
                    && id.len() <= 64
                    && id
                        .bytes()
                        .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.' | b':'));
                valid.then_some((Severity::parse(severity)?, id.to_owned()))
            })
            .take(MAX_LISTED)
            .collect()
    }
}

/// A recorded scan of one digest.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ScanSummary {
    pub status: ScanStatus,
    pub scanner: String,
    /// When the vulnerability database was built (Unix milliseconds).
    pub db_updated_at: Option<i64>,
    pub counts: Counts,
    pub findings: Vec<Finding>,
    /// When the scan ran (Unix milliseconds).
    pub scanned_at: i64,
}

/// What an environment's gate does.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum GateMode {
    #[default]
    Off,
    Warn,
    Block,
}

impl GateMode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Off => "off",
            Self::Warn => "warn",
            Self::Block => "block",
        }
    }

    #[must_use]
    pub fn parse(s: &str) -> Option<Self> {
        Some(match s {
            "off" => Self::Off,
            "warn" => Self::Warn,
            "block" => Self::Block,
            _ => return None,
        })
    }
}

/// An environment's vulnerability gate.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ScanGate {
    pub mode: GateMode,
    /// Findings at or above this severity count (`high` or `critical`).
    pub severity: Severity,
    /// An image without a fresh, successful scan counts as a finding.
    pub require_scan: bool,
    /// A scan older than this no longer counts.
    pub max_age_secs: u32,
}

impl Default for ScanGate {
    fn default() -> Self {
        Self::off()
    }
}

impl ScanGate {
    #[must_use]
    pub const fn off() -> Self {
        Self {
            mode: GateMode::Off,
            severity: Severity::Critical,
            require_scan: false,
            max_age_secs: DEFAULT_MAX_AGE_SECS,
        }
    }

    /// Production: known critical findings are refused.
    #[must_use]
    pub const fn production() -> Self {
        Self {
            mode: GateMode::Block,
            ..Self::off()
        }
    }

    #[must_use]
    pub const fn is_valid(&self) -> bool {
        matches!(self.severity, Severity::High | Severity::Critical)
            && self.max_age_secs >= 3600
            && self.max_age_secs <= 90 * 24 * 3600
    }

    /// Whether `self` refuses less than `current`.
    #[must_use]
    pub fn weakens(&self, current: &Self) -> bool {
        self.mode < current.mode
            || self.severity > current.severity
            || (current.require_scan && !self.require_scan)
            || self.max_age_secs > current.max_age_secs
    }
}

/// What the gate decided for a deployment.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum GateVerdict {
    Pass,
    Warn(Vec<String>),
    Block(Vec<String>),
}

/// Decide on the images `scans` (digest and its newest scan) under `gate`
/// at `now` (Unix milliseconds); `excepted` are the finding ids with an
/// unexpired exception.
#[must_use]
pub fn evaluate(
    gate: &ScanGate,
    scans: &[(String, Option<ScanSummary>)],
    excepted: &BTreeSet<String>,
    now: i64,
) -> GateVerdict {
    if gate.mode == GateMode::Off {
        return GateVerdict::Pass;
    }
    let mut reasons = Vec::new();
    for (digest, scan) in scans {
        let short = digest.get(..19).unwrap_or(digest);
        let fresh = scan.as_ref().filter(|s| {
            s.status == ScanStatus::Ok
                && now.saturating_sub(s.scanned_at) <= i64::from(gate.max_age_secs) * 1000
        });
        let Some(scan) = fresh else {
            if gate.require_scan {
                reasons.push(format!("{short}… has no fresh vulnerability scan"));
            }
            continue;
        };
        let counted = scan.counts.at_least(gate.severity);
        let covered = scan
            .findings
            .iter()
            .filter(|(severity, id)| *severity >= gate.severity && excepted.contains(id))
            .count();
        let open = counted.saturating_sub(u32::try_from(covered).unwrap_or(u32::MAX));
        if open > 0 {
            let ids: Vec<&str> = scan
                .findings
                .iter()
                .filter(|(severity, id)| *severity >= gate.severity && !excepted.contains(id))
                .map(|(_, id)| id.as_str())
                .take(5)
                .collect();
            reasons.push(format!(
                "{short}… has {open} {} or worse finding(s) without an exception{}",
                gate.severity.as_str(),
                if ids.is_empty() {
                    String::new()
                } else {
                    format!(": {}", ids.join(", "))
                }
            ));
        }
    }
    match (reasons.is_empty(), gate.mode) {
        (true, _) | (_, GateMode::Off) => GateVerdict::Pass,
        (false, GateMode::Warn) => GateVerdict::Warn(reasons),
        (false, GateMode::Block) => GateVerdict::Block(reasons),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
    const NOW: i64 = 1_800_000_000_000;

    fn scan(counts: Counts, findings: &[(Severity, &str)], age_secs: i64) -> ScanSummary {
        ScanSummary {
            status: ScanStatus::Ok,
            scanner: "trivy 0.74.0".into(),
            db_updated_at: Some(NOW - 3_600_000),
            counts,
            findings: findings.iter().map(|(s, id)| (*s, (*id).to_owned())).collect(),
            scanned_at: NOW - age_secs * 1000,
        }
    }

    fn one(s: Option<ScanSummary>) -> Vec<(String, Option<ScanSummary>)> {
        vec![(DIGEST.to_owned(), s)]
    }

    #[test]
    fn reports_list_only_well_formed_findings() {
        let report: ScanReport = serde_json::from_str(
            r#"{"status":"ok","scanner":"trivy 0.74.0","db":"2026-09-17T00:00:00Z",
                "counts":{"critical":1,"high":2},
                "findings":["CRITICAL:CVE-2026-0001","HIGH:GHSA-abcd-efgh","BOGUS:CVE-1","HIGH:","HIGH:a b"]}"#,
        )
        .expect("report");
        assert_eq!(
            report.listed(),
            [
                (Severity::Critical, "CVE-2026-0001".to_owned()),
                (Severity::High, "GHSA-abcd-efgh".to_owned())
            ]
        );
        assert_eq!(report.counts.at_least(Severity::High), 3);
        assert_eq!(report.counts.at_least(Severity::Critical), 1);
        let down: ScanReport =
            serde_json::from_str(r#"{"status":"unavailable","detail":"no feed"}"#).expect("r");
        assert_eq!(down, ScanReport::unavailable("no feed"));
    }

    #[test]
    fn open_findings_above_the_severity_are_refused() {
        let gate = ScanGate::production();
        let critical = Counts {
            critical: 1,
            high: 4,
            ..Counts::default()
        };
        let findings = [(Severity::Critical, "CVE-1"), (Severity::High, "CVE-2")];
        let verdict = evaluate(
            &gate,
            &one(Some(scan(critical, &findings, 60))),
            &BTreeSet::new(),
            NOW,
        );
        assert!(
            matches!(&verdict, GateVerdict::Block(r) if r[0].contains("1 critical") && r[0].contains("CVE-1"))
        );
        let excepted = BTreeSet::from(["CVE-1".to_owned()]);
        assert_eq!(
            evaluate(&gate, &one(Some(scan(critical, &findings, 60))), &excepted, NOW),
            GateVerdict::Pass,
            "the critical finding has an exception; high ones do not count"
        );
        let high = ScanGate {
            severity: Severity::High,
            ..gate
        };
        assert!(matches!(
            evaluate(&high, &one(Some(scan(critical, &findings, 60))), &excepted, NOW),
            GateVerdict::Block(r) if r[0].contains("4 high")
        ));
        let warn = ScanGate {
            mode: GateMode::Warn,
            ..gate
        };
        assert!(matches!(
            evaluate(
                &warn,
                &one(Some(scan(critical, &findings, 60))),
                &BTreeSet::new(),
                NOW
            ),
            GateVerdict::Warn(_)
        ));
        assert_eq!(
            evaluate(
                &ScanGate::off(),
                &one(Some(scan(critical, &findings, 60))),
                &BTreeSet::new(),
                NOW
            ),
            GateVerdict::Pass
        );
    }

    #[test]
    fn unlisted_findings_cannot_be_excepted() {
        // Two critical findings, only one of them listed and excepted.
        let counts = Counts {
            critical: 2,
            ..Counts::default()
        };
        let excepted = BTreeSet::from(["CVE-1".to_owned()]);
        let s = scan(counts, &[(Severity::Critical, "CVE-1")], 60);
        assert!(matches!(
            evaluate(&ScanGate::production(), &one(Some(s)), &excepted, NOW),
            GateVerdict::Block(r) if r[0].contains("1 critical")
        ));
    }

    #[test]
    fn missing_stale_or_unavailable_scans_count_only_when_required() {
        let gate = ScanGate::production();
        let clean = scan(Counts::default(), &[], 60);
        let stale = scan(Counts::default(), &[], i64::from(gate.max_age_secs) + 1);
        let unavailable = ScanSummary {
            status: ScanStatus::Unavailable,
            ..clean.clone()
        };
        for s in [None, Some(stale.clone()), Some(unavailable.clone())] {
            assert_eq!(
                evaluate(&gate, &one(s.clone()), &BTreeSet::new(), NOW),
                GateVerdict::Pass
            );
            let strict = ScanGate {
                require_scan: true,
                ..gate
            };
            assert!(matches!(
                evaluate(&strict, &one(s), &BTreeSet::new(), NOW),
                GateVerdict::Block(r) if r[0].contains("no fresh vulnerability scan")
            ));
        }
        // A stale scan's findings do not count either: rescans refresh them.
        let stale_critical = ScanSummary {
            counts: Counts {
                critical: 3,
                ..Counts::default()
            },
            ..stale
        };
        assert_eq!(
            evaluate(&gate, &one(Some(stale_critical)), &BTreeSet::new(), NOW),
            GateVerdict::Pass
        );
    }

    #[test]
    fn gates_weaken_in_every_direction() {
        let strict = ScanGate {
            mode: GateMode::Block,
            severity: Severity::High,
            require_scan: true,
            max_age_secs: 86_400,
        };
        assert!(!strict.weakens(&strict));
        for weaker in [
            ScanGate {
                mode: GateMode::Warn,
                ..strict
            },
            ScanGate {
                severity: Severity::Critical,
                ..strict
            },
            ScanGate {
                require_scan: false,
                ..strict
            },
            ScanGate {
                max_age_secs: 172_800,
                ..strict
            },
        ] {
            assert!(weaker.weakens(&strict), "{weaker:?}");
            assert!(!strict.weakens(&weaker));
        }
        assert!(strict.is_valid());
        assert!(
            !ScanGate {
                severity: Severity::Low,
                ..strict
            }
            .is_valid()
        );
        assert!(
            !ScanGate {
                max_age_secs: 10,
                ..strict
            }
            .is_valid()
        );
        assert_eq!(GateMode::parse("block"), Some(GateMode::Block));
        assert_eq!(Severity::parse("HIGH"), Some(Severity::High));
    }
}
