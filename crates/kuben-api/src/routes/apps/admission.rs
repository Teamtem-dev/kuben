//! Resource admission of app changes (M4.5; plan §11, §16).
//!
//! Before a change is accepted, the app's peak requests (every process at
//! its maximum replicas, plus the surge pod of a rolling update) are added
//! to those of the other live apps and must fit the environment's quota and
//! the installation's quota for the organization; a pod larger than the
//! largest schedulable node is refused as well. Nodes that are not known yet
//! only give a warning, never a false "fits". Nothing here can be overridden
//! from the API: quotas are raised by whoever owns them.

use kube::api::ListParams;
use kuben_core::{
    Error,
    capacity::{self, Estimate, Limits},
    ids::{EnvironmentId, TargetId},
};
use kuben_crd::{App, AppSpec, KubenConfig, Quota};
use kuben_platform::{
    controller::{platform_of, resources},
    discovery::ClusterFacts,
    registry::ClusterId,
};
use kuben_store::repo::Tenant;
use serde_json::Value;

use super::spec_of;
use crate::{error::ApiResult, routes::scope, state::ApiState};

/// Stands in for the image when an app's resources are computed: only its
/// processes matter.
pub(crate) const ANY_IMAGE: &str = "admission.invalid/any:latest";

/// Where an app change lands.
#[derive(Clone, Copy, Debug)]
pub(crate) struct Placement<'a> {
    pub environment: EnvironmentId,
    /// The environment's quota (`kuben_crd::Quota` as JSON).
    pub quota: Option<&'a Value>,
    pub target: TargetId,
}

/// The size presets of the cluster's `KubenConfig`, or the defaults without
/// a cluster.
async fn platform(state: &ApiState) -> ApiResult<resources::Platform> {
    let Ok(client) = scope::cluster(state) else {
        return Ok(resources::Platform::default());
    };
    let configs = kube::Api::<KubenConfig>::all(client)
        .list(&ListParams::default())
        .await
        .map_err(|e| scope::kube_error(e, "kubenconfigs"))?;
    Ok(platform_of(&configs.items))
}

/// The limits of an environment quota.
#[must_use]
pub(crate) fn environment_limits(quota: Option<&Value>) -> Limits {
    let Some(quota) = quota.and_then(|q| serde_json::from_value::<Quota>(q.clone()).ok()) else {
        return Limits::default();
    };
    Limits {
        cpu_millis: quota.cpu.as_deref().and_then(capacity::cpu_millis),
        memory_bytes: quota.memory.as_deref().and_then(capacity::bytes),
        pods: quota.pods.map(u64::from),
    }
}

/// The peak requests of an app with `spec`.
fn peak(
    spec: &AppSpec,
    platform: &resources::Platform,
) -> Result<resources::AppDemand, resources::BuildError> {
    resources::demand(&App::new("admission", spec.clone()), platform)
}

/// Admit `spec` at `at`, in `tenant`'s transaction. The warnings to show.
pub(crate) async fn admit(
    state: &ApiState,
    tenant: &mut Tenant,
    at: Placement<'_>,
    spec: &AppSpec,
) -> ApiResult<Vec<String>> {
    let platform = platform(state).await?;
    let quota = &state.cfg.quota;
    let org_limits = quota.org_limits().map_err(Error::Internal)?;
    let own = peak(spec, &platform).map_err(|e| Error::Validation(e.to_string()))?;
    let others: Vec<_> = tenant
        .live_configs()
        .await?
        .into_iter()
        .filter(|l| l.target != at.target)
        .collect();
    if let Some(max) = quota.org_apps
        && u64::try_from(others.len()).unwrap_or(u64::MAX) >= max
    {
        return Err(Error::Conflict(format!("the organization's quota allows {max} apps")).into());
    }
    let (mut in_environment, mut in_org) = (own.peak, own.peak);
    for other in others {
        let Some(other_spec) = spec_of(other.config, Some(ANY_IMAGE)) else {
            continue;
        };
        // A configuration that no longer renders cannot run either.
        let Ok(demand) = peak(&other_spec, &platform) else {
            continue;
        };
        in_org = in_org.plus(demand.peak);
        if other.environment == at.environment {
            in_environment = in_environment.plus(demand.peak);
        }
    }
    environment_limits(at.quota)
        .admit(in_environment)
        .map_err(|e| Error::Conflict(format!("the environment's quota is exceeded: {e}")))?;
    org_limits
        .admit(in_org)
        .map_err(|e| Error::Conflict(format!("the organization's quota is exceeded: {e}")))?;

    let facts: Option<ClusterFacts> = tenant
        .cluster_capabilities(ClusterId::PRIMARY)
        .await?
        .and_then(|r| serde_json::from_value(r.facts).ok());
    let (cpu, memory) = own.largest_pod;
    Ok(
        match capacity::estimate(facts.and_then(|f| f.node_capacity()), cpu, memory) {
            Estimate::FitsEstimate => Vec::new(),
            Estimate::UnlikelyToSchedule(why) => {
                return Err(Error::Conflict(format!("the app would not be scheduled: {why}")).into());
            }
            Estimate::UnknownConstraints(why) => vec![format!("scheduling is not estimated: {why}")],
        },
    )
}

/// Refuse a new environment beyond the organization's quota.
pub(crate) async fn admit_environment(state: &ApiState, tenant: &mut Tenant) -> ApiResult<()> {
    let Some(max) = state.cfg.quota.org_environments else {
        return Ok(());
    };
    if tenant.live_environment_count().await? >= max {
        return Err(Error::Conflict(format!("the organization's quota allows {max} environments")).into());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    #[test]
    fn environment_quotas_become_limits() {
        assert!(environment_limits(None).is_unlimited());
        let limits = environment_limits(Some(&json!({ "cpu": "4", "memory": "8Gi", "pods": 20 })));
        assert_eq!(
            (limits.cpu_millis, limits.memory_bytes, limits.pods),
            (Some(4000), Some(8 << 30), Some(20))
        );
        assert!(environment_limits(Some(&json!("broken"))).is_unlimited());
    }

    #[test]
    fn stored_configurations_are_measured_without_their_image() {
        let config = json!({ "runtime": { "processes": { "web": { "port": 80, "replicas": { "min": 2, "max": 2 } } } } });
        let spec = spec_of(Some(config), Some(ANY_IMAGE)).expect("spec");
        let d = peak(&spec, &resources::Platform::default()).expect("demand");
        assert_eq!(d.peak.pods, 3, "two replicas and a surge pod");
    }
}
