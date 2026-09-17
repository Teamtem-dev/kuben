//! The vulnerability state of an app's current release (M4.6): the newest
//! scan of each image, what the environment's gate says about it, and the
//! images' SBOMs.

use axum::{
    Json,
    extract::{Path, State},
    http::header,
    response::{IntoResponse, Response},
};
use kuben_core::{
    Error,
    perm::Perm,
    scan::{GateVerdict, ScanSummary},
    time::now_ms,
};
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    authz::Authz,
    error::ApiResult,
    routes::{request, scope},
    state::ApiState,
};

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct ScanDto {
    /// `ok`, or `unavailable` when the scanner, its feed or the image was
    /// not reachable (never "clean").
    pub status: String,
    pub scanner: String,
    /// When the vulnerability database was built.
    pub database_updated_at: Option<String>,
    pub scanned_at: String,
    pub critical: u32,
    pub high: u32,
    pub medium: u32,
    pub low: u32,
    pub unknown: u32,
    /// `severity:id` of the most severe findings.
    pub findings: Vec<String>,
}

impl From<ScanSummary> for ScanDto {
    fn from(s: ScanSummary) -> Self {
        Self {
            status: s.status.as_str().to_owned(),
            scanner: s.scanner,
            database_updated_at: s.db_updated_at.map(request::timestamp),
            scanned_at: request::timestamp(s.scanned_at),
            critical: s.counts.critical,
            high: s.counts.high,
            medium: s.counts.medium,
            low: s.counts.low,
            unknown: s.counts.unknown,
            findings: s
                .findings
                .iter()
                .map(|(severity, id)| format!("{}:{id}", severity.as_str()))
                .collect(),
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct ImageScanDto {
    pub digest: String,
    /// The newest scan; `None` when the image was never scanned.
    pub scan: Option<ScanDto>,
    /// An SBOM can be downloaded.
    pub sbom: bool,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct AppScansDto {
    pub release: Option<Uuid>,
    /// `pass`, `warn` or `block`: what the environment's gate says now.
    pub gate: String,
    pub reasons: Vec<String>,
    pub images: Vec<ImageScanDto>,
}

/// The scans of the app's current release and the gate's verdict.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/scans", operation_id = "getAppScans",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 200, body = AppScansDto), (status = 404, body = crate::error::Problem))
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<Json<AppScansDto>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let Some(release) = a.app.release else {
        return Ok(Json(AppScansDto {
            release: None,
            gate: "pass".into(),
            reasons: Vec::new(),
            images: Vec::new(),
        }));
    };
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    let digests = tenant.release_digests(release).await?.unwrap_or_default();
    let mut latest = tenant.latest_scans(&digests).await?;
    let sboms = tenant.sboms_kept(&digests).await?;
    let verdict = tenant
        .scan_verdict(a.app.target, release, now_ms())
        .await?
        .unwrap_or(GateVerdict::Pass);
    let (gate, reasons) = match verdict {
        GateVerdict::Pass => ("pass", Vec::new()),
        GateVerdict::Warn(r) => ("warn", r),
        GateVerdict::Block(r) => ("block", r),
    };
    let images = digests
        .into_iter()
        .map(|digest| ImageScanDto {
            scan: latest.remove(&digest).map(ScanDto::from),
            sbom: sboms.contains(&digest),
            digest,
        })
        .collect();
    Ok(Json(AppScansDto {
        release: Some(*release.as_uuid()),
        gate: gate.into(),
        reasons,
        images,
    }))
}

/// The CycloneDX SBOM of one image of the app's current release
/// (gzip-encoded JSON).
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/sbom/{digest}", operation_id = "getAppSbom",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        ("digest" = String, Path, description = "Image digest (`sha256:…`)"),
    ),
    responses(
        (status = 200, description = "CycloneDX JSON, `Content-Encoding: gzip`", content_type = "application/vnd.cyclonedx+json"),
        (status = 404, body = crate::error::Problem),
    )
)]
pub async fn sbom(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app, digest)): Path<(String, String, String, String)>,
) -> ApiResult<Response> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let missing = || Error::NotFound(format!("an SBOM of `{digest}` for app `{app}`"));
    let release = a.app.release.ok_or_else(missing)?;
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    let ours = tenant
        .release_digests(release)
        .await?
        .is_some_and(|d| d.contains(&digest));
    if !ours {
        return Err(missing().into());
    }
    let content = tenant.sbom(&digest).await?.ok_or_else(missing)?;
    let file = format!(
        "attachment; filename=\"{app}-{}.cdx.json\"",
        digest.replace(':', "-")
    );
    Ok((
        [
            (header::CONTENT_TYPE, "application/vnd.cyclonedx+json".to_owned()),
            (header::CONTENT_ENCODING, "gzip".to_owned()),
            (header::CONTENT_DISPOSITION, file),
        ],
        content,
    )
        .into_response())
}
