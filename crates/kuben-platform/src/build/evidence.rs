//! What a scan container left behind (M4.6): its report in the termination
//! log and the image's SBOM in its log, between markers. Both are read once
//! the pod is done and kept in SQL; the pod is deleted afterwards.

use base64::{Engine as _, engine::general_purpose::STANDARD};
use k8s_openapi::{api::core::v1::Pod, jiff::Timestamp};
use kube::{
    Api,
    api::{ListParams, LogParams},
};
use kuben_core::{
    ops::outcome::SCAN_CONTAINER,
    scan::{ScanReport, ScanStatus, ScanSummary},
};

/// Largest SBOM kept (gzip).
pub const MAX_SBOM_BYTES: usize = 8 << 20;
/// Most log read from a scan container.
const MAX_LOG_BYTES: i64 = 12 << 20;
const BEGIN: &str = "kuben-sbom-begin\n";
const END: &str = "\nkuben-sbom-end";

/// The gzip SBOM between the markers of `log`, if it is well formed.
#[must_use]
pub fn sbom_of(log: &str) -> Option<Vec<u8>> {
    let start = log.find(BEGIN)? + BEGIN.len();
    let end = start + log[start..].find(END)?;
    let bytes = STANDARD.decode(log[start..end].trim()).ok()?;
    (bytes.len() <= MAX_SBOM_BYTES && bytes.starts_with(&[0x1f, 0x8b])).then_some(bytes)
}

/// The report in `message`, or an unavailable one saying why not.
#[must_use]
pub fn report_of(message: Option<&str>) -> ScanReport {
    match message.map(str::trim) {
        None | Some("") => ScanReport::unavailable("the scan left no report"),
        Some(m) => serde_json::from_str(m)
            .unwrap_or_else(|_| ScanReport::unavailable("the scan report does not parse")),
    }
}

/// The summary of `report`, scanned at `now` (Unix milliseconds).
#[must_use]
pub fn summary_of(report: &ScanReport, now: i64) -> ScanSummary {
    let db_updated_at = report
        .db
        .as_deref()
        .and_then(|d| d.parse::<Timestamp>().ok())
        .map(Timestamp::as_millisecond);
    let ok = report.status == ScanStatus::Ok;
    ScanSummary {
        status: report.status,
        scanner: report.scanner.chars().take(128).collect(),
        db_updated_at,
        counts: if ok {
            report.counts
        } else {
            kuben_core::scan::Counts::default()
        },
        findings: if ok { report.listed() } else { Vec::new() },
        scanned_at: now,
    }
}

/// The report and SBOM of the newest pod `selector` matches in `pods`.
pub async fn collect(pods: &Api<Pod>, selector: &str) -> kube::Result<(ScanReport, Option<Vec<u8>>)> {
    let listed = pods.list(&ListParams::default().labels(selector)).await?;
    let Some(pod) = listed
        .items
        .iter()
        .max_by_key(|p| p.metadata.creation_timestamp.as_ref().map(|t| t.0))
    else {
        return Ok((ScanReport::unavailable("the scan pod is gone"), None));
    };
    let terminated = pod
        .status
        .as_ref()
        .and_then(|s| s.container_statuses.as_ref())
        .and_then(|all| all.iter().find(|c| c.name == SCAN_CONTAINER))
        .and_then(|c| c.state.as_ref()?.terminated.as_ref());
    let Some(terminated) = terminated else {
        return Ok((ScanReport::unavailable("the scan did not run"), None));
    };
    let report = report_of(terminated.message.as_deref());
    if report.status != ScanStatus::Ok {
        return Ok((report, None));
    }
    let name = pod.metadata.name.clone().unwrap_or_default();
    let params = LogParams {
        container: Some(SCAN_CONTAINER.to_owned()),
        limit_bytes: Some(MAX_LOG_BYTES),
        ..LogParams::default()
    };
    let sbom = match pods.logs(&name, &params).await {
        Ok(log) => sbom_of(&log),
        Err(e) => {
            tracing::warn!(pod = %name, error = %e, "the SBOM of a scan could not be read");
            None
        }
    };
    Ok((report, sbom))
}

#[cfg(test)]
mod tests {
    use kuben_core::scan::Severity;

    use super::*;

    #[test]
    fn sboms_are_taken_from_between_the_markers() {
        let gz = [0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3, 1, 2, 3];
        let log = format!(
            "Downloading DB...\n{BEGIN}{}\n{}\nafter",
            STANDARD.encode(gz),
            &END[1..]
        );
        assert_eq!(sbom_of(&log), Some(gz.to_vec()));
        assert_eq!(sbom_of("no markers"), None);
        let not_gzip = format!("{BEGIN}{}{END}", STANDARD.encode(b"plain json"));
        assert_eq!(sbom_of(&not_gzip), None);
        let broken = format!("{BEGIN}!!!{END}");
        assert_eq!(sbom_of(&broken), None);
        let unterminated = format!("{BEGIN}{}", STANDARD.encode(gz));
        assert_eq!(sbom_of(&unterminated), None, "a truncated log");
    }

    #[test]
    fn reports_become_summaries() {
        let ok = report_of(Some(
            r#"{"status":"ok","scanner":"trivy 0.74.0","db":"2026-09-17T06:00:00Z",
                "counts":{"critical":1,"high":0,"medium":3,"low":0,"unknown":0},
                "findings":["CRITICAL:CVE-2026-1"]}"#,
        ));
        let s = summary_of(&ok, 42);
        assert_eq!(s.status, ScanStatus::Ok);
        assert_eq!(s.db_updated_at, Some(1_789_624_800_000));
        assert_eq!(s.findings, [(Severity::Critical, "CVE-2026-1".to_owned())]);
        assert_eq!((s.counts.critical, s.counts.medium, s.scanned_at), (1, 3, 42));

        for message in [None, Some(""), Some("not json")] {
            let r = report_of(message);
            assert_eq!(r.status, ScanStatus::Unavailable, "{message:?}");
            assert_eq!(summary_of(&r, 1).counts.critical, 0);
        }
        let unavailable = report_of(Some(
            r#"{"status":"unavailable","scanner":"trivy","detail":"no feed"}"#,
        ));
        assert_eq!(unavailable.detail.as_deref(), Some("no feed"));
    }
}
