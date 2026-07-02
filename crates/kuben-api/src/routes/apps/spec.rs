//! Request bodies for creating and updating apps, their validation, and the
//! translation into an `App` spec.

use std::collections::{BTreeMap, BTreeSet};

use kuben_core::Error;
use kuben_crd::{
    App, AppSpec, Domain, EnvVar, HealthCheck, KeyRef, Process, Replicas, Runtime, Source, Volume,
};
use kuben_platform::controller::resources::{self, BuildError};
use serde::Deserialize;
use utoipa::ToSchema;

use super::{EnvVarDto, JOB, ProtocolDto, SecretRef, VolumeDto, WEB};
use crate::{routes::validate, state::ApiState};

const MAX_REPLICAS: u32 = 50;
const MAX_VOLUMES: usize = 5;

fn one() -> u32 {
    1
}

fn default_size() -> String {
    "small".into()
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct CreateApp {
    #[schema(example = "api")]
    pub name: String,
    #[schema(example = "ghcr.io/acme/api:1.4.2")]
    pub image: String,
    /// Port the process listens on; omit for workers and scheduled jobs.
    #[schema(example = 8080)]
    pub port: Option<u16>,
    #[serde(default)]
    pub command: Vec<String>,
    #[serde(default = "one")]
    pub replicas: u32,
    /// CPU autoscaling up to this many replicas when greater than `replicas`.
    pub max_replicas: Option<u32>,
    /// Size preset: `nano`, `small`, `medium`, `large` (or custom).
    #[serde(default = "default_size")]
    #[schema(example = "small")]
    pub size: String,
    #[serde(default)]
    pub env: Vec<EnvVarDto>,
    #[serde(default)]
    pub domains: Vec<String>,
    #[schema(example = "/healthz")]
    pub health_check_path: Option<String>,
    /// Cron expression: the app runs as a scheduled job (no port).
    #[schema(example = "0 3 * * *")]
    pub schedule: Option<String>,
    /// IANA time zone for `schedule` (default UTC).
    #[schema(example = "Europe/Berlin")]
    pub time_zone: Option<String>,
    #[serde(default)]
    pub protocol: ProtocolDto,
    #[serde(default)]
    pub volumes: Vec<VolumeDto>,
    /// Group id owning the volumes, for images running as a non-root user.
    #[schema(example = 1000)]
    pub fs_group: Option<i64>,
}

/// Partial update; omitted fields are left unchanged, lists are replaced.
#[derive(Debug, Default, Deserialize, ToSchema)]
pub struct UpdateApp {
    pub image: Option<String>,
    pub port: Option<u16>,
    pub command: Option<Vec<String>>,
    pub replicas: Option<u32>,
    pub max_replicas: Option<u32>,
    pub size: Option<String>,
    pub env: Option<Vec<EnvVarDto>>,
    pub domains: Option<Vec<String>>,
    /// Empty string removes the health check.
    pub health_check_path: Option<String>,
    /// Empty string removes the schedule.
    pub schedule: Option<String>,
    pub time_zone: Option<String>,
    pub protocol: Option<ProtocolDto>,
    /// Replaces the list. Removed volumes are unmounted, never deleted.
    pub volumes: Option<Vec<VolumeDto>>,
    /// `0` removes the fsGroup.
    pub fs_group: Option<i64>,
}

fn validate_env(env: &[EnvVarDto]) -> Result<(), Error> {
    let mut seen = BTreeSet::new();
    for e in env {
        validate::env_var_name(&e.name)?;
        if !seen.insert(e.name.as_str()) {
            return Err(Error::Validation(format!(
                "environment variable `{}` is set twice",
                e.name
            )));
        }
        match (&e.value, &e.secret) {
            (Some(_), Some(_)) => {
                return Err(Error::Validation(format!(
                    "`{}`: set either value or secret, not both",
                    e.name
                )));
            }
            (_, Some(s)) => {
                validate::dns_label("secret name", &s.name, 63)?;
                validate::secret_key(&s.key)?;
            }
            _ => {}
        }
    }
    Ok(())
}

fn validate_domains(domains: &[String]) -> Result<(), Error> {
    domains.iter().try_for_each(|d| validate::hostname(d))
}

fn validate_replicas(min: u32, max: u32) -> Result<(), Error> {
    if max > MAX_REPLICAS || min > max {
        return Err(Error::Validation(format!(
            "replicas must satisfy 0 ≤ replicas ≤ max_replicas ≤ {MAX_REPLICAS}"
        )));
    }
    Ok(())
}

fn validate_health_path(path: &str) -> Result<(), Error> {
    if path.is_empty()
        || (path.starts_with('/') && path.len() <= 256 && path.chars().all(|c| c.is_ascii_graphic()))
    {
        Ok(())
    } else {
        Err(Error::Validation(
            "health_check_path must be an absolute path such as /healthz".into(),
        ))
    }
}

/// Bytes of a Kubernetes storage quantity (`5Gi`, `500Mi`, `10G`, `1024`).
#[must_use]
pub fn quantity_bytes(q: &str) -> Option<u128> {
    let q = q.trim();
    let split = q.find(|c: char| !c.is_ascii_digit()).unwrap_or(q.len());
    let (number, unit) = q.split_at(split);
    let n: u128 = number.parse().ok()?;
    let multiplier: u128 = match unit {
        "" => 1,
        "Ki" => 1 << 10,
        "Mi" => 1 << 20,
        "Gi" => 1 << 30,
        "Ti" => 1 << 40,
        "k" => 1_000,
        "M" => 1_000_000,
        "G" => 1_000_000_000,
        "T" => 1_000_000_000_000,
        _ => return None,
    };
    n.checked_mul(multiplier)
}

fn validate_volumes(volumes: &[VolumeDto]) -> Result<(), Error> {
    if volumes.len() > MAX_VOLUMES {
        return Err(Error::Validation(format!(
            "at most {MAX_VOLUMES} volumes per app"
        )));
    }
    let (mut names, mut paths) = (BTreeSet::new(), BTreeSet::new());
    for v in volumes {
        validate::dns_label("volume name", &v.name, 30)?;
        let path_ok = v.mount_path.starts_with('/')
            && v.mount_path != "/"
            && v.mount_path.len() <= 256
            && !v.mount_path.contains("..")
            && !v.mount_path.chars().any(char::is_whitespace);
        if !path_ok {
            return Err(Error::Validation(format!(
                "volume `{}`: mount_path must be an absolute path other than /",
                v.name
            )));
        }
        if quantity_bytes(&v.size).is_none_or(|b| b == 0) {
            return Err(Error::Validation(format!(
                "volume `{}`: size must be a quantity such as 1Gi or 500Mi",
                v.name
            )));
        }
        if !names.insert(v.name.as_str()) || !paths.insert(v.mount_path.as_str()) {
            return Err(Error::Validation(
                "volume names and mount paths must be unique".into(),
            ));
        }
    }
    Ok(())
}

/// PersistentVolumeClaims can grow but never shrink.
fn check_no_shrink(current: &[Volume], next: &[VolumeDto]) -> Result<(), Error> {
    for n in next {
        if let Some(c) = current.iter().find(|c| c.name == n.name)
            && quantity_bytes(&n.size) < quantity_bytes(&c.size)
        {
            return Err(Error::Validation(format!(
                "volume `{}` cannot shrink from {} to {}",
                n.name, c.size, n.size
            )));
        }
    }
    Ok(())
}

fn validate_schedule(schedule: Option<&str>, time_zone: Option<&str>) -> Result<(), Error> {
    if let Some(s) = schedule.filter(|s| !s.trim().is_empty())
        && !resources::valid_schedule(s)
    {
        return Err(Error::Validation(format!(
            "`{s}` is not a cron expression (five fields, or @hourly/@daily/…)"
        )));
    }
    if let Some(tz) = time_zone.filter(|t| !t.is_empty()) {
        validate::time_zone(tz)?;
    }
    Ok(())
}

fn validate_fs_group(group: Option<i64>) -> Result<(), Error> {
    match group {
        Some(g) if !(0..=i64::from(i32::MAX)).contains(&g) => Err(Error::Validation(
            "fs_group must be a group id between 1 and 2147483647".into(),
        )),
        _ => Ok(()),
    }
}

/// The controller's cross-field rules, applied before anything is written.
pub(crate) fn validate_spec(spec: &AppSpec) -> Result<(), Error> {
    let probe = App::new("validation", spec.clone());
    match resources::validate(&probe) {
        Ok(()) | Err(BuildError::AwaitingBuild) => Ok(()),
        Err(e) => Err(Error::Validation(e.to_string())),
    }
}

pub(super) fn to_crd_env(e: &EnvVarDto) -> EnvVar {
    EnvVar {
        name: e.name.clone(),
        value: if e.secret.is_some() {
            None
        } else {
            Some(e.value.clone().unwrap_or_default())
        },
        from_secret: e.secret.as_ref().map(|s| KeyRef {
            name: s.name.clone(),
            key: s.key.clone(),
        }),
        from_service: None,
    }
}

pub(super) fn from_crd_env(e: &EnvVar, with_values: bool) -> EnvVarDto {
    let secret = e.from_secret.as_ref().or(e.from_service.as_ref());
    EnvVarDto {
        name: e.name.clone(),
        value: if with_values && secret.is_none() {
            e.value.clone()
        } else {
            None
        },
        secret: secret.map(|r| SecretRef {
            name: r.name.clone(),
            key: r.key.clone(),
        }),
    }
}

pub(super) fn to_domains(hosts: &[String]) -> Vec<Domain> {
    hosts
        .iter()
        .map(|h| Domain {
            host: h.trim().to_ascii_lowercase(),
            tls: "auto".into(),
        })
        .collect()
}

fn to_crd_volumes(volumes: &[VolumeDto]) -> Vec<Volume> {
    volumes
        .iter()
        .map(|v| Volume {
            name: v.name.clone(),
            mount_path: v.mount_path.clone(),
            size: v.size.trim().to_owned(),
            storage_class: None,
        })
        .collect()
}

fn non_empty(s: Option<&str>) -> Option<String> {
    s.map(str::trim).filter(|s| !s.is_empty()).map(str::to_owned)
}

pub(super) fn spec_from_create(body: &CreateApp) -> Result<AppSpec, Error> {
    validate::image(&body.image)?;
    validate::dns_label("size", &body.size, 30)?;
    let min = body.replicas;
    let max = body.max_replicas.unwrap_or(min).max(min);
    validate_replicas(min, max)?;
    validate_env(&body.env)?;
    validate_domains(&body.domains)?;
    validate_volumes(&body.volumes)?;
    validate_schedule(body.schedule.as_deref(), body.time_zone.as_deref())?;
    validate_fs_group(body.fs_group)?;
    if let Some(path) = &body.health_check_path {
        validate_health_path(path)?;
    }
    let schedule = non_empty(body.schedule.as_deref());
    let process = if schedule.is_some() { JOB } else { WEB };
    Ok(AppSpec {
        source: Source::from_image(body.image.trim()),
        runtime: Runtime {
            processes: BTreeMap::from([(
                process.to_owned(),
                Process {
                    command: body.command.clone(),
                    port: body.port,
                    size: body.size.clone(),
                    replicas: Replicas { min, max },
                    idle: None,
                    schedule,
                    time_zone: non_empty(body.time_zone.as_deref()),
                    protocol: body.protocol.into(),
                },
            )]),
            health_check: body
                .health_check_path
                .clone()
                .filter(|p| !p.is_empty())
                .map(|path| HealthCheck { path, port: None }),
            fs_group: body.fs_group.filter(|g| *g > 0),
        },
        env: body.env.iter().map(to_crd_env).collect(),
        domains: to_domains(&body.domains),
        volumes: to_crd_volumes(&body.volumes),
    })
}

/// Apply a partial update to an App spec (validated field by field; the
/// caller then runs [`validate_spec`] on the result).
pub(super) fn apply_update(spec: &mut AppSpec, u: UpdateApp) -> Result<(), Error> {
    if let Some(image) = u.image {
        validate::image(&image)?;
        spec.source = Source::from_image(image.trim());
    }
    let key = if spec.runtime.processes.contains_key(WEB) {
        WEB.to_owned()
    } else {
        spec.runtime
            .processes
            .keys()
            .next()
            .cloned()
            .ok_or_else(|| Error::Validation("app has no processes".into()))?
    };
    validate_schedule(u.schedule.as_deref(), u.time_zone.as_deref())?;
    if let Some(p) = spec.runtime.processes.get_mut(&key) {
        if let Some(r) = u.replicas {
            p.replicas.min = r;
            p.replicas.max = p.replicas.max.max(r);
        }
        if let Some(max) = u.max_replicas {
            p.replicas.max = max.max(p.replicas.min);
        }
        validate_replicas(p.replicas.min, p.replicas.max)?;
        if let Some(size) = u.size {
            validate::dns_label("size", &size, 30)?;
            p.size = size;
        }
        if let Some(command) = u.command {
            p.command = command;
        }
        if let Some(port) = u.port {
            p.port = Some(port);
        }
        if let Some(schedule) = u.schedule {
            p.schedule = non_empty(Some(&schedule));
        }
        if let Some(tz) = u.time_zone {
            p.time_zone = non_empty(Some(&tz));
        }
        if let Some(protocol) = u.protocol {
            p.protocol = protocol.into();
        }
    }
    if let Some(env) = u.env {
        validate_env(&env)?;
        spec.env = env.iter().map(to_crd_env).collect();
    }
    if let Some(hosts) = u.domains {
        validate_domains(&hosts)?;
        spec.domains = to_domains(&hosts);
    }
    if let Some(path) = u.health_check_path {
        validate_health_path(&path)?;
        spec.runtime.health_check = (!path.is_empty()).then_some(HealthCheck { path, port: None });
    }
    if let Some(volumes) = u.volumes {
        validate_volumes(&volumes)?;
        check_no_shrink(&spec.volumes, &volumes)?;
        spec.volumes = to_crd_volumes(&volumes);
    }
    if let Some(group) = u.fs_group {
        validate_fs_group(Some(group))?;
        spec.runtime.fs_group = (group > 0).then_some(group);
    }
    Ok(())
}

/// A hostname belongs to exactly one app (first come, first served), so one
/// tenant cannot route another tenant's domain. The owner is not revealed.
pub(super) fn ensure_domains_free(
    state: &ApiState,
    namespace: &str,
    app: &str,
    spec: &AppSpec,
) -> Result<(), Error> {
    for other in state.projections.apps() {
        if other.namespace == namespace && other.name == app {
            continue;
        }
        if let Some(d) = spec
            .domains
            .iter()
            .find(|d| other.domains.iter().any(|o| o.eq_ignore_ascii_case(&d.host)))
        {
            return Err(Error::Conflict(format!(
                "domain `{}` is already used by another app",
                d.host
            )));
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{super::sample_spec, *};
    use crate::routes::apps::ProtocolDto;

    #[test]
    fn update_changes_only_given_fields() {
        let mut s = sample_spec();
        let u = UpdateApp {
            image: Some(" nginx:1.28 ".into()),
            replicas: Some(3),
            ..UpdateApp::default()
        };
        apply_update(&mut s, u).expect("update");
        assert_eq!(s.source.image.as_deref(), Some("nginx:1.28"));
        let web = &s.runtime.processes[WEB];
        assert_eq!((web.replicas.min, web.replicas.max), (3, 3));
        assert_eq!(web.port, Some(80));
    }

    #[test]
    fn update_validates() {
        let mut s = sample_spec();
        let bad = [
            UpdateApp {
                replicas: Some(500),
                ..UpdateApp::default()
            },
            UpdateApp {
                env: Some(vec![
                    EnvVarDto {
                        name: "A".into(),
                        value: Some("1".into()),
                        secret: None,
                    },
                    EnvVarDto {
                        name: "A".into(),
                        value: Some("2".into()),
                        secret: None,
                    },
                ]),
                ..UpdateApp::default()
            },
            UpdateApp {
                env: Some(vec![EnvVarDto {
                    name: "B".into(),
                    value: Some("x".into()),
                    secret: Some(SecretRef {
                        name: "s".into(),
                        key: "k".into(),
                    }),
                }]),
                ..UpdateApp::default()
            },
            UpdateApp {
                health_check_path: Some("healthz".into()),
                ..UpdateApp::default()
            },
            UpdateApp {
                schedule: Some("every night".into()),
                ..UpdateApp::default()
            },
        ];
        for u in bad {
            assert!(apply_update(&mut s, u).is_err());
        }
    }

    #[test]
    fn env_values_are_hidden_without_permission() {
        let plain = EnvVar {
            name: "MODE".into(),
            value: Some("prod".into()),
            from_secret: None,
            from_service: None,
        };
        assert_eq!(from_crd_env(&plain, true).value.as_deref(), Some("prod"));
        assert!(from_crd_env(&plain, false).value.is_none());
        let secret = to_crd_env(&EnvVarDto {
            name: "TOKEN".into(),
            value: Some("ignored".into()),
            secret: Some(SecretRef {
                name: "api".into(),
                key: "token".into(),
            }),
        });
        assert!(secret.value.is_none(), "secret-backed vars never carry a value");
    }

    #[test]
    fn quantities_and_volume_rules() {
        assert_eq!(quantity_bytes("5Gi"), Some(5 << 30));
        assert_eq!(quantity_bytes("500Mi"), Some(500 << 20));
        assert_eq!(quantity_bytes("10G"), Some(10_000_000_000));
        assert_eq!(quantity_bytes("1.5Gi"), None);
        let vol = |name: &str, path: &str, size: &str| VolumeDto {
            name: name.into(),
            mount_path: path.into(),
            size: size.into(),
        };
        assert!(validate_volumes(&[vol("data", "/data", "5Gi")]).is_ok());
        for bad in [
            vol("data", "/", "1Gi"),
            vol("data", "data", "1Gi"),
            vol("data", "/d", "lots"),
            vol("Data", "/d", "1Gi"),
        ] {
            assert!(validate_volumes(&[bad]).is_err());
        }
        assert!(
            validate_volumes(&[vol("a", "/x", "1Gi"), vol("b", "/x", "1Gi")]).is_err(),
            "duplicate path"
        );
        let current = to_crd_volumes(&[vol("data", "/data", "5Gi")]);
        assert!(check_no_shrink(&current, &[vol("data", "/data", "10Gi")]).is_ok());
        assert!(check_no_shrink(&current, &[vol("data", "/data", "1Gi")]).is_err());
    }

    #[test]
    fn create_spec_applies_controller_rules() {
        let body =
            |schedule: Option<&str>, port: Option<u16>, replicas: u32, volumes: Vec<VolumeDto>| CreateApp {
                name: "api".into(),
                image: "nginx:1.27".into(),
                port,
                command: vec![],
                replicas,
                max_replicas: None,
                size: "small".into(),
                env: vec![],
                domains: vec![],
                health_check_path: None,
                schedule: schedule.map(str::to_owned),
                time_zone: None,
                protocol: ProtocolDto::Http,
                volumes,
                fs_group: None,
            };
        let job = spec_from_create(&body(Some("0 3 * * *"), None, 1, vec![])).expect("job");
        assert!(job.runtime.processes.contains_key(JOB));
        validate_spec(&job).expect("valid");
        let bad = spec_from_create(&body(Some("@daily"), Some(80), 1, vec![])).expect("fields ok");
        assert!(validate_spec(&bad).is_err(), "scheduled with port");
        let vol = VolumeDto {
            name: "data".into(),
            mount_path: "/data".into(),
            size: "1Gi".into(),
        };
        let scaled = spec_from_create(&body(None, Some(80), 2, vec![vol])).expect("fields ok");
        assert!(validate_spec(&scaled).is_err(), "volume needs one replica");
    }
}
