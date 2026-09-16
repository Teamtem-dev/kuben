//! The bundle lock (M2.17): what a release installs besides Kuben, pinned by
//! version and digest. `bundle.lock.json` at the repository root is the one
//! place these pins live; the binary embeds it, `kuben setup` installs from
//! it, the chart's PostgreSQL follows it (a test holds them together), and
//! each release publishes it under its signed checksums.

use std::{collections::BTreeMap, sync::LazyLock};

use serde::Deserialize;

/// The lock file, as released.
pub const LOCK: &str = include_str!("../../../bundle.lock.json");

#[derive(Debug, Deserialize)]
pub struct Pinned {
    pub url: String,
    pub sha256: String,
}

#[derive(Debug, Deserialize)]
pub struct K3s {
    pub version: String,
    pub installer: Pinned,
}

#[derive(Debug, Deserialize)]
pub struct GatewayApi {
    pub version: String,
    pub url: String,
    pub sha256: String,
}

#[derive(Debug, Deserialize)]
pub struct CertManager {
    pub version: String,
    pub chart: Pinned,
    /// Image digest per component (`controller`, `webhook`, …).
    pub images: BTreeMap<String, String>,
}

#[derive(Debug, Deserialize)]
pub struct Postgresql {
    pub image: String,
    pub tag: String,
    pub digest: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Bundle {
    pub schema: u32,
    pub k3s: K3s,
    pub gateway_api: GatewayApi,
    pub cert_manager: CertManager,
    pub postgresql: Postgresql,
}

static BUNDLE: LazyLock<Bundle> = LazyLock::new(|| {
    // The file is part of the source; the tests below read it the same way.
    serde_json::from_str(LOCK).unwrap_or_else(|e| panic!("bundle.lock.json does not parse: {e}"))
});

/// One line per pin, for `kuben version --bundle`.
#[must_use]
pub fn summary() -> String {
    let b = bundle();
    let images = b
        .cert_manager
        .images
        .iter()
        .map(|(component, digest)| format!("    {component:<16} {digest}"))
        .collect::<Vec<_>>()
        .join("\n");
    format!(
        "bundle lock (schema {})\n\
         k3s              {} (installer sha256 {})\n\
         Gateway API      {} (sha256 {})\n\
         cert-manager     {} (chart sha256 {})\n{images}\n\
         PostgreSQL       {}:{}@{} (Helm chart)",
        b.schema,
        b.k3s.version,
        b.k3s.installer.sha256,
        b.gateway_api.version,
        b.gateway_api.sha256,
        b.cert_manager.version,
        b.cert_manager.chart.sha256,
        b.postgresql.image,
        b.postgresql.tag,
        b.postgresql.digest,
    )
}

/// The pins of this build.
#[must_use]
pub fn bundle() -> &'static Bundle {
    &BUNDLE
}

#[cfg(test)]
mod tests {
    use super::*;

    fn is_digest(value: &str) -> bool {
        value
            .strip_prefix("sha256:")
            .is_some_and(|hex| hex.len() == 64 && hex.bytes().all(|b| b.is_ascii_hexdigit()))
    }

    fn is_sha256(value: &str) -> bool {
        value.len() == 64 && value.bytes().all(|b| b.is_ascii_hexdigit())
    }

    #[test]
    fn every_pin_has_a_version_and_a_digest() {
        let b = bundle();
        assert_eq!(b.schema, 1);
        assert!(b.k3s.version.starts_with('v') && b.k3s.version.contains("+k3s"));
        assert!(b.k3s.installer.url.contains(&b.k3s.version.replace('+', "%2B")));
        assert!(
            b.gateway_api
                .url
                .contains(&format!("/{}/", b.gateway_api.version))
        );
        assert!(
            b.cert_manager
                .chart
                .url
                .ends_with(&format!("cert-manager-{}.tgz", b.cert_manager.version))
        );
        for sum in [
            &b.k3s.installer.sha256,
            &b.gateway_api.sha256,
            &b.cert_manager.chart.sha256,
        ] {
            assert!(is_sha256(sum), "{sum}");
        }
        let components: Vec<&str> = b.cert_manager.images.keys().map(String::as_str).collect();
        assert_eq!(
            components,
            [
                "acmesolver",
                "cainjector",
                "controller",
                "startupapicheck",
                "webhook"
            ]
        );
        assert!(b.cert_manager.images.values().all(|d| is_digest(d)));
        assert!(is_digest(&b.postgresql.digest));
        for url in [
            &b.k3s.installer.url,
            &b.gateway_api.url,
            &b.cert_manager.chart.url,
        ] {
            assert!(url.starts_with("https://"), "{url}");
        }
    }

    #[test]
    fn the_charts_postgresql_is_the_locked_one() {
        let values: serde_yaml_ng::Value =
            serde_yaml_ng::from_str(include_str!("../../../charts/kuben/values.yaml")).expect("values.yaml");
        let image = &values["postgresql"]["image"];
        let b = &bundle().postgresql;
        assert_eq!(image["repository"].as_str(), Some(b.image.as_str()));
        assert_eq!(image["tag"].as_str(), Some(b.tag.as_str()));
        assert_eq!(image["digest"].as_str(), Some(b.digest.as_str()));
    }
}
