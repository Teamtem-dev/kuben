//! The support envelope (M4.12; plan §18.5): what a Supported MVP
//! installation runs on, and what is claimed about it.
//!
//! `doctor` checks an installation against it and a support bundle carries
//! it. A test keeps the bundle lock inside it. A version outside
//! [`VersionRange::tested`] can still work ("untested"). A version below
//! [`VersionRange::works_from`] is unsupported.

use serde::Serialize;

/// A `major.minor` version.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct Minor(pub u32, pub u32);

impl std::fmt::Display for Minor {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}.{}", self.0, self.1)
    }
}

impl Minor {
    /// The `major.minor` of a version string such as `v1.36.4+k3s1`,
    /// `1.36`, `17.4` or `17`.
    #[must_use]
    pub fn parse(version: &str) -> Option<Self> {
        let v = version.trim().trim_start_matches('v');
        let mut parts = v.split(['.', '-', '+']);
        let major = parts.next()?.parse().ok()?;
        let minor = parts
            .next()
            .and_then(|m| {
                let digits: String = m.chars().take_while(char::is_ascii_digit).collect();
                digits.parse().ok()
            })
            .unwrap_or(0);
        Some(Self(major, minor))
    }
}

/// The versions of one dependency.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct VersionRange {
    pub name: &'static str,
    /// Older than this is not supported.
    pub works_from: Minor,
    /// Tested in CI and in the acceptance runs, both ends included.
    pub tested: (Minor, Minor),
    /// Only major versions matter (PostgreSQL).
    pub majors: bool,
}

/// How a version fits the envelope.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub enum Fit {
    Supported,
    /// Newer than tested, or between `works_from` and the tested range.
    Untested,
    Unsupported,
}

impl VersionRange {
    #[must_use]
    pub fn fit(&self, version: Minor) -> Fit {
        if version < self.works_from {
            Fit::Unsupported
        } else if version >= self.tested.0 && version <= self.tested.1 {
            Fit::Supported
        } else {
            Fit::Untested
        }
    }

    fn show(&self, v: Minor) -> String {
        if self.majors {
            v.0.to_string()
        } else {
            v.to_string()
        }
    }

    fn json(&self) -> serde_json::Value {
        serde_json::json!({
            "name": self.name,
            "worksFrom": self.show(self.works_from),
            "tested": [self.show(self.tested.0), self.show(self.tested.1)],
        })
    }

    /// A line for `doctor`: how `version` fits, and why.
    #[must_use]
    pub fn describe(&self, version: Minor) -> (Fit, String) {
        let (lo, hi) = (self.show(self.tested.0), self.show(self.tested.1));
        let fit = self.fit(version);
        let (version, works_from) = (self.show(version), self.show(self.works_from));
        let text = match fit {
            Fit::Supported => format!("{} {version} is supported (tested {lo} to {hi})", self.name),
            Fit::Untested => format!(
                "{} {version} is outside the tested range {lo} to {hi}; it may work, but it is not supported",
                self.name
            ),
            Fit::Unsupported => format!(
                "{} {version} is not supported: use {works_from} or newer (tested {lo} to {hi})",
                self.name
            ),
        };
        (fit, text)
    }
}

pub const KUBERNETES: VersionRange = VersionRange {
    name: "Kubernetes",
    works_from: Minor(1, 29),
    tested: (Minor(1, 31), Minor(1, 36)),
    majors: false,
};

pub const POSTGRESQL: VersionRange = VersionRange {
    name: "PostgreSQL",
    works_from: Minor(15, 0),
    tested: (Minor(15, 0), Minor(18, u32::MAX)),
    majors: true,
};

pub const GATEWAY_API: VersionRange = VersionRange {
    name: "Gateway API",
    works_from: Minor(1, 1),
    tested: (Minor(1, 2), Minor(1, 5)),
    majors: false,
};

pub const CERT_MANAGER: VersionRange = VersionRange {
    name: "cert-manager",
    works_from: Minor(1, 15),
    tested: (Minor(1, 16), Minor(1, 21)),
    majors: false,
};

/// Host operating systems `kuben setup` supports.
pub const HOSTS: &[&str] = &[
    "Ubuntu 22.04, 24.04",
    "Debian 12, 13",
    "RHEL, Rocky and AlmaLinux 9, 10",
];

pub const ARCHITECTURES: &[&str] = &["amd64", "arm64"];

/// What the Supported MVP claims, and what it does not.
pub const LIMITS: &[(&str, &str)] = &[
    ("clusters", "one cluster per installation"),
    ("placements", "one placement (namespace) per environment"),
    (
        "workloads",
        "stateless web and worker processes; production data in an external PostgreSQL",
    ),
    (
        "availability",
        "single failure domain: no high-availability claim (M7)",
    ),
    (
        "nodes",
        "node lifecycle (add, drain, replace) is the operator's (M6)",
    ),
    (
        "previews",
        "one preview environment per GitHub pull request, with a lifetime; a fork's preview gets no secrets",
    ),
    (
        "registries",
        "OCI registries with basic or token auth; GitHub for Git sources",
    ),
    (
        "gateways",
        "one Gateway API implementation; Traefik (k3s) is the tested one",
    ),
];

/// The envelope as JSON, for bundles and the API.
#[must_use]
pub fn envelope() -> serde_json::Value {
    serde_json::json!({
        "profile": "supported-mvp",
        "kubernetes": KUBERNETES.json(),
        "postgresql": POSTGRESQL.json(),
        "gatewayApi": GATEWAY_API.json(),
        "certManager": CERT_MANAGER.json(),
        "hosts": HOSTS,
        "architectures": ARCHITECTURES,
        "limits": LIMITS.iter().map(|(k, v)| serde_json::json!({ "area": k, "claim": v })).collect::<Vec<_>>(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn versions_parse_to_major_and_minor() {
        assert_eq!(Minor::parse("v1.36.4+k3s1"), Some(Minor(1, 36)));
        assert_eq!(Minor::parse("1.31"), Some(Minor(1, 31)));
        assert_eq!(Minor::parse("17-alpine"), Some(Minor(17, 0)));
        assert_eq!(Minor::parse("v1.30.0-eks-abc"), Some(Minor(1, 30)));
        assert_eq!(Minor::parse("1.32+"), Some(Minor(1, 32)));
        assert_eq!(Minor::parse("x"), None);
    }

    #[test]
    fn versions_fit_the_envelope() {
        assert_eq!(KUBERNETES.fit(Minor(1, 33)), Fit::Supported);
        assert_eq!(KUBERNETES.fit(Minor(1, 30)), Fit::Untested);
        assert_eq!(KUBERNETES.fit(Minor(1, 40)), Fit::Untested);
        assert_eq!(KUBERNETES.fit(Minor(1, 28)), Fit::Unsupported);
        assert_eq!(POSTGRESQL.fit(Minor(17, 0)), Fit::Supported);
        assert_eq!(POSTGRESQL.fit(Minor(14, 0)), Fit::Unsupported);
        assert_eq!(POSTGRESQL.fit(Minor(19, 0)), Fit::Untested);
        let (fit, text) = KUBERNETES.describe(Minor(1, 28));
        assert_eq!(fit, Fit::Unsupported);
        assert!(text.contains("1.29 or newer"), "{text}");
        let (_, text) = POSTGRESQL.describe(Minor(14, 0));
        assert_eq!(
            text,
            "PostgreSQL 14 is not supported: use 15 or newer (tested 15 to 18)"
        );
    }

    #[test]
    fn the_envelope_is_complete() {
        let e = envelope();
        assert_eq!(e["kubernetes"]["tested"], serde_json::json!(["1.31", "1.36"]));
        assert_eq!(e["postgresql"]["tested"], serde_json::json!(["15", "18"]));
        assert_eq!(e["limits"].as_array().map(Vec::len), Some(LIMITS.len()));
        for range in [KUBERNETES, POSTGRESQL, GATEWAY_API, CERT_MANAGER] {
            assert!(range.works_from <= range.tested.0, "{}", range.name);
            assert!(range.tested.0 <= range.tested.1, "{}", range.name);
        }
    }
}
