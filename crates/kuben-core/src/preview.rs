//! Preview environments (M5.1; plan §9 C14): naming, lifetimes and what a
//! preview may copy from its source environment.

use serde_json::Value;

use crate::policy::EnvironmentPolicy;

/// The longest lifetime a preview may be given, in hours (30 days).
pub const MAX_TTL_HOURS: u32 = 720;
const HOUR_MS: i64 = 3_600_000;

/// The environment name of pull request `number`'s preview in `epoch`:
/// `pr<number>-<epoch>`. A reopened pull request gets a new epoch, so a new
/// name and namespace. `None` when it would not fit an environment name.
#[must_use]
pub fn slug(number: u64, epoch: u64) -> Option<String> {
    let slug = format!("pr{number}-{epoch}");
    (slug.len() <= 20).then_some(slug)
}

/// When a preview touched at `now` with a lifetime of `ttl_hours` expires;
/// never earlier than `current`.
#[must_use]
pub fn expiry(now: i64, ttl_hours: u32, current: Option<i64>) -> i64 {
    let at = now.saturating_add(i64::from(ttl_hours.clamp(1, MAX_TTL_HOURS)) * HOUR_MS);
    current.map_or(at, |c| c.max(at))
}

/// `expires_at` pushed out by `hours`, but never more than
/// [`MAX_TTL_HOURS`] beyond `now`.
#[must_use]
pub fn extend(now: i64, expires_at: i64, hours: u32) -> i64 {
    let limit = now.saturating_add(i64::from(MAX_TTL_HOURS) * HOUR_MS);
    expires_at
        .max(now)
        .saturating_add(i64::from(hours) * HOUR_MS)
        .min(limit)
}

/// The configuration a preview's app gets from its source app: without
/// secret references (a preview binds only its own environment's secrets,
/// and an untrusted one none) and without custom domains (a preview serves
/// its generated hostname only). What was removed is listed.
#[must_use]
pub fn preview_config(source: &Value) -> (Value, Vec<String>) {
    let mut config = source.clone();
    let mut removed = Vec::new();
    if let Some(env) = config.get_mut("env").and_then(Value::as_array_mut) {
        env.retain(|var| {
            let secret = var.get("fromSecret").is_some_and(|s| !s.is_null());
            if secret {
                let name = var.get("name").and_then(Value::as_str).unwrap_or("?");
                removed.push(format!("env {name}"));
            }
            !secret
        });
    }
    if let Some(domains) = config.get_mut("domains").and_then(Value::as_array_mut) {
        for domain in domains.drain(..) {
            let host = domain.get("host").and_then(Value::as_str).unwrap_or("?");
            removed.push(format!("domain {host}"));
        }
    }
    if let Some(object) = config.as_object_mut()
        && object.remove("imagePullSecrets").is_some()
    {
        removed.push("image pull secrets".to_owned());
    }
    (config, removed)
}

impl EnvironmentPolicy {
    /// A preview's policy, inherited from its source environment's: the same
    /// roles and vulnerability gate, but no approvals, so every push of the
    /// pull request deploys.
    #[must_use]
    pub const fn for_preview(source: &Self) -> Self {
        Self {
            required_approvals: 0,
            ..*source
        }
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;
    use crate::scan::ScanGate;

    #[test]
    fn slugs_carry_the_number_and_epoch() {
        assert_eq!(slug(12, 1).as_deref(), Some("pr12-1"));
        assert_eq!(slug(12, 2).as_deref(), Some("pr12-2"));
        assert_eq!(slug(1_234_567_890, 123_456_789), None);
    }

    #[test]
    fn lifetimes_are_bounded() {
        assert_eq!(expiry(0, 2, None), 2 * HOUR_MS);
        assert_eq!(expiry(0, 2, Some(5 * HOUR_MS)), 5 * HOUR_MS, "never shortened");
        assert_eq!(expiry(0, 10_000, None), i64::from(MAX_TTL_HOURS) * HOUR_MS);
        assert_eq!(
            extend(10, 0, 1),
            10 + HOUR_MS,
            "an expired preview counts from now"
        );
        assert_eq!(extend(0, HOUR_MS, 10_000), i64::from(MAX_TTL_HOURS) * HOUR_MS);
    }

    #[test]
    fn previews_copy_no_secrets_or_domains() {
        let source = json!({
            "runtime": { "processes": { "web": { "port": 8080 } } },
            "env": [
                { "name": "MODE", "value": "preview" },
                { "name": "DB", "fromSecret": { "name": "db", "key": "url" } },
                { "name": "API", "fromService": { "name": "api", "key": "url" } }
            ],
            "domains": [{ "host": "shop.example.com" }],
            "imagePullSecrets": ["ghcr.r1"]
        });
        let (config, removed) = preview_config(&source);
        assert_eq!(
            config["env"],
            json!([{ "name": "MODE", "value": "preview" }, { "name": "API", "fromService": { "name": "api", "key": "url" } }])
        );
        assert_eq!(config["domains"], json!([]));
        assert!(config.get("imagePullSecrets").is_none());
        assert_eq!(config["runtime"], source["runtime"]);
        assert_eq!(
            removed,
            ["env DB", "domain shop.example.com", "image pull secrets"]
        );
    }

    #[test]
    fn preview_policies_keep_the_gate_but_not_the_approvals() {
        let production = EnvironmentPolicy::production();
        let preview = EnvironmentPolicy::for_preview(&production);
        assert_eq!(preview.required_approvals, 0);
        assert_eq!(preview.scan, ScanGate::production());
        assert_eq!(preview.deploy_role, production.deploy_role);
        preview.validate().expect("valid");
    }
}
