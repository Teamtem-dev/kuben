//! Promotion of an app to another environment of the same project
//! (scenario 10), with a dry-run diff.

use std::collections::{BTreeMap, BTreeSet};

use axum::{
    Json,
    extract::{Path, State},
};
use k8s_openapi::api::core::v1::Secret;
use kube::{
    Api, ResourceExt,
    api::{ListParams, PostParams},
};
use kuben_core::{Error, perm::Perm};
use kuben_crd::{App, AppSpec, labels};
use kuben_platform::projection::AppView;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{AppDto, new_app_object, record_release, spec::validate_spec};
use crate::{
    authz::Authz,
    error::ApiResult,
    routes::{scope, validate},
    state::ApiState,
};

#[derive(Debug, Deserialize, ToSchema)]
pub struct Promote {
    /// Target environment (short name) in the same project.
    #[schema(example = "production")]
    pub to_environment: String,
    /// Only report what would change.
    #[serde(default)]
    pub dry_run: bool,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct PromoteResult {
    pub dry_run: bool,
    /// The app did not exist in the target and was (or would be) created.
    pub created: bool,
    /// Human-readable diff; values of environment variables are never shown.
    pub changes: Vec<String>,
    /// E.g. secrets referenced by the app that are missing in the target.
    pub warnings: Vec<String>,
    pub app: Option<AppDto>,
}

/// The spec an app gets in the target environment: image, processes,
/// health check and env come from the source; the target keeps its domains,
/// its scaling (replicas and size per process) and its volumes.
#[must_use]
pub fn promote_spec(source: &AppSpec, target: Option<&AppSpec>) -> AppSpec {
    let mut runtime = source.runtime.clone();
    if let Some(t) = target {
        for (name, p) in &mut runtime.processes {
            if let Some(existing) = t.runtime.processes.get(name) {
                p.replicas = existing.replicas;
                p.size.clone_from(&existing.size);
            }
        }
    }
    AppSpec {
        source: source.source.clone(),
        runtime,
        env: source.env.clone(),
        domains: target.map(|t| t.domains.clone()).unwrap_or_default(),
        volumes: match target {
            Some(t) if !t.volumes.is_empty() => t.volumes.clone(),
            _ => source.volumes.clone(),
        },
    }
}

fn fmt_port(port: Option<u16>) -> String {
    port.map_or_else(|| "none".to_owned(), |p| p.to_string())
}

/// Human-readable differences between two specs (no env values).
#[must_use]
pub fn spec_changes(old: Option<&AppSpec>, new: &AppSpec) -> Vec<String> {
    let image = |s: &AppSpec| s.source.image.clone().unwrap_or_else(|| "(git build)".into());
    let Some(old) = old else {
        return vec![format!("create the app with image {}", image(new))];
    };
    let mut out = Vec::new();
    if old.source != new.source {
        out.push(format!("image: {} → {}", image(old), image(new)));
    }
    let env = |s: &AppSpec| {
        s.env
            .iter()
            .map(|e| (e.name.clone(), e.clone()))
            .collect::<BTreeMap<_, _>>()
    };
    let (before, after) = (env(old), env(new));
    for name in after.keys().filter(|k| !before.contains_key(*k)) {
        out.push(format!("env: add {name}"));
    }
    for name in before.keys().filter(|k| !after.contains_key(*k)) {
        out.push(format!("env: remove {name}"));
    }
    for (name, value) in &after {
        if before.get(name).is_some_and(|b| b != value) {
            out.push(format!("env: change {name}"));
        }
    }
    for (name, p) in &new.runtime.processes {
        match old.runtime.processes.get(name) {
            None => out.push(format!("process {name}: add")),
            Some(o) => {
                if o.command != p.command {
                    out.push(format!("process {name}: command"));
                }
                if o.port != p.port {
                    out.push(format!(
                        "process {name}: port {} → {}",
                        fmt_port(o.port),
                        fmt_port(p.port)
                    ));
                }
                if o.schedule != p.schedule {
                    out.push(format!("process {name}: schedule"));
                }
            }
        }
    }
    for name in old
        .runtime
        .processes
        .keys()
        .filter(|n| !new.runtime.processes.contains_key(*n))
    {
        out.push(format!("process {name}: remove"));
    }
    if old.runtime.health_check != new.runtime.health_check {
        out.push("health check".into());
    }
    if old.volumes != new.volumes {
        out.push("volumes".into());
    }
    out
}

/// Secret references that the target environment cannot satisfy.
#[must_use]
pub fn missing_secrets(spec: &AppSpec, available: &BTreeMap<String, BTreeSet<String>>) -> Vec<String> {
    spec.env
        .iter()
        .filter_map(|e| e.from_secret.as_ref().map(|r| (e, r)))
        .filter(|(_, r)| !available.get(&r.name).is_some_and(|keys| keys.contains(&r.key)))
        .map(|(e, r)| {
            format!(
                "{} references secret `{}/{}`, which does not exist in the target environment",
                e.name, r.name, r.key
            )
        })
        .collect()
}

async fn managed_secret_keys(
    client: &kube::Client,
    namespace: &str,
) -> ApiResult<BTreeMap<String, BTreeSet<String>>> {
    let list = Api::<Secret>::namespaced(client.clone(), namespace)
        .list(&ListParams::default().labels(labels::MANAGED_SELECTOR))
        .await
        .map_err(|e| scope::kube_error(e, "secrets"))?;
    Ok(list
        .items
        .iter()
        .map(|s| {
            let keys = s
                .data
                .as_ref()
                .map(|d| d.keys().cloned().collect())
                .unwrap_or_default();
            (s.name_any(), keys)
        })
        .collect())
}

/// Promote the app to another environment of the same project.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/promote", operation_id = "promoteApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name (source)"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = Promote,
    responses(
        (status = 200, body = PromoteResult),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn promote(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<Promote>,
) -> ApiResult<Json<PromoteResult>> {
    let a = scope::app(&state, &authz, &project, &environment, &app)?;
    let _read = authz.require(&state, Perm::AppRead, &a.chain())?;
    validate::dns_label("to_environment", &body.to_environment, 63)?;
    let target = scope::environment(&state, &authz, &project, &body.to_environment)?;
    if target.view.name == a.env.view.name {
        return Err(Error::Validation("choose a different target environment".into()).into());
    }
    let _promote = authz.require(&state, Perm::ReleasePromote, &target.chain())?;
    if target.view.deleting {
        return Err(
            Error::Conflict(format!("environment `{}` is being deleted", target.short_name())).into(),
        );
    }
    let client = scope::cluster(&state)?;
    let source = Api::<App>::namespaced(client.clone(), &a.view.namespace)
        .get(&a.view.name)
        .await
        .map_err(|e| scope::kube_error(e, &app))?;
    let target_api = Api::<App>::namespaced(client.clone(), &target.view.namespace);
    let existing = target_api
        .get_opt(&a.view.name)
        .await
        .map_err(|e| scope::kube_error(e, &app))?;
    let spec = promote_spec(&source.spec, existing.as_ref().map(|e| &e.spec));
    validate_spec(&spec)?;
    let changes = spec_changes(existing.as_ref().map(|e| &e.spec), &spec);
    let warnings = missing_secrets(
        &spec,
        &managed_secret_keys(&client, &target.view.namespace).await?,
    );
    if body.dry_run || (existing.is_some() && changes.is_empty()) {
        return Ok(Json(PromoteResult {
            dry_run: body.dry_run,
            created: existing.is_none(),
            changes,
            warnings,
            app: None,
        }));
    }
    let (saved, created) = if let Some(mut live) = existing {
        live.spec = spec;
        let saved = target_api
            .replace(&a.view.name, &PostParams::default(), &live)
            .await
            .map_err(|e| scope::kube_error(e, &app))?;
        (saved, false)
    } else {
        let saved = target_api
            .create(
                &PostParams::default(),
                &new_app_object(&target, &a.view.name, spec),
            )
            .await
            .map_err(|e| scope::kube_error(e, &app))?;
        (saved, true)
    };
    record_release(
        &state,
        &authz,
        target.project.org,
        &target.view.namespace,
        &a.view.name,
        &saved.spec,
        "promote",
        Some(format!("from {}", a.env.short_name())),
    )
    .await;
    Ok(Json(PromoteResult {
        dry_run: false,
        created,
        changes,
        warnings,
        app: Some(AppDto::from_view(
            &AppView::from(&saved),
            &target.project.view.name,
            target.short_name(),
        )),
    }))
}

#[cfg(test)]
mod tests {
    use kuben_crd::{Replicas, Source};

    use super::{super::sample_spec, *};
    use crate::routes::apps::{
        EnvVarDto, SecretRef, WEB,
        spec::{to_crd_env, to_domains},
    };

    #[test]
    fn promotion_keeps_target_domains_and_scaling_and_reports_changes() {
        let mut source = sample_spec();
        source.source = Source::from_image("nginx:1.28");
        source.env = vec![to_crd_env(&EnvVarDto {
            name: "DATABASE_URL".into(),
            value: None,
            secret: Some(SecretRef {
                name: "db".into(),
                key: "url".into(),
            }),
        })];
        let mut target = sample_spec();
        target.domains = to_domains(&["shop.acme.com".to_owned()]);
        if let Some(web) = target.runtime.processes.get_mut(WEB) {
            web.replicas = Replicas { min: 3, max: 6 };
            web.size = "large".into();
        }
        let promoted = promote_spec(&source, Some(&target));
        assert_eq!(promoted.source.image.as_deref(), Some("nginx:1.28"));
        assert_eq!(promoted.domains[0].host, "shop.acme.com");
        assert_eq!(
            promoted.runtime.processes[WEB].replicas,
            Replicas { min: 3, max: 6 }
        );
        assert_eq!(promoted.runtime.processes[WEB].size, "large");

        let changes = spec_changes(Some(&target), &promoted);
        assert!(
            changes.contains(&"image: nginx:1.27 → nginx:1.28".to_owned()),
            "{changes:?}"
        );
        assert!(
            changes.contains(&"env: add DATABASE_URL".to_owned()),
            "{changes:?}"
        );
        assert!(spec_changes(None, &promoted)[0].starts_with("create the app"));
        assert!(spec_changes(Some(&promoted), &promoted).is_empty());

        let none = BTreeMap::new();
        assert_eq!(missing_secrets(&promoted, &none).len(), 1);
        let have = BTreeMap::from([("db".to_owned(), BTreeSet::from(["url".to_owned()]))]);
        assert!(missing_secrets(&promoted, &have).is_empty());
    }
}
