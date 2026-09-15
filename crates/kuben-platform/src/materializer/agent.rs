//! Delivery through the cluster's agent (M1.9, ADR-027). For a target whose
//! `delivery` is `agent`, the run's frozen plan goes to the cluster's agent
//! as an execution envelope instead of an App object, and the run follows
//! what the agent observed.
//!
//! * The envelope carries the frozen plan as canonical JSON with its digest,
//!   so the agent checks it before writing anything.
//! * Delivery is durable without a queue of its own: while the cluster's
//!   agent is not linked the operation waits and is claimed again, and an
//!   envelope that brings no observation is sent again (the apiserver takes
//!   the same envelope again).
//! * The agent's observations, recorded in SQL by the hub, move the run:
//!   accepted → acceptedByCluster; applying → verifying; ready → succeeded;
//!   failed or rejected → failed with the agent's reason; a newer generation
//!   → superseded.
//! * A target handed over from the App controller loses its App object
//!   first: marked (the App controller then leaves it alone), then deleted
//!   with orphan propagation, so the agent adopts the workloads in place
//!   instead of making new ones.

use std::{convert::Infallible, fmt, pin::Pin, time::Duration};

use kube::{
    Api, Resource, ResourceExt,
    api::{DeleteParams, Patch, PatchParams, PropagationPolicy},
};
use kuben_agent::protocol::Apply;
use kuben_core::ops::{RunEvent, RunPhase};
use kuben_crd::{App, ApplicationRuntimeSpec, Environment, PlanEnvelope, Project};
use kuben_store::repo::{Claim, Materialization, RunPlan, RuntimeObservation};
use serde_json::json;
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;

use super::{
    render::{self, render},
    worker::{Error, LEASE, Stop, Worker, pause, refused},
    write,
};
use crate::render::{canonical, sha256};

/// Hands envelopes to the agent of a cluster: the hub in `kuben serve`.
pub trait AgentDispatch: Send + Sync + fmt::Debug {
    /// Send `apply` to the agent of `cluster` over its live link; false when
    /// that agent is not linked, or did not negotiate the runtime feature.
    fn send(&self, cluster: &str, apply: Apply) -> Pin<Box<dyn Future<Output = bool> + Send + '_>>;
}

/// How long an envelope may go without an observation before it is sent
/// again.
const RESEND: Duration = Duration::from_mins(1);
/// How long a run waits for its cluster's agent before it is claimed again.
const AGENT_WAIT: Duration = Duration::from_secs(10);
/// How long a handover waits for the garbage collector to orphan the App's
/// workloads before it looks again.
const ORPHAN_WAIT: Duration = Duration::from_secs(2);

/// The envelope of `m`'s run, carrying its frozen `plan`.
pub fn envelope(m: &Materialization, plan: &RunPlan) -> Result<Apply, String> {
    let resources = canonical(&plan.resources);
    let digest = sha256(&resources);
    let generation = i64::try_from(m.generation.0).map_err(|e| e.to_string())?;
    let spec = ApplicationRuntimeSpec {
        target_id: m.target.to_string(),
        lifecycle_uid: m.lifecycle_uid.to_string(),
        control_epoch: 0,
        generation,
        input_hash: sha256(&format!(
            "{}/{}/{generation}/{digest}",
            m.release, m.config_revision
        )),
        release_id: m.release.to_string(),
        plan: PlanEnvelope {
            id: plan.id.to_string(),
            renderer_version: plan.renderer_version.clone(),
            digest,
            resources,
        },
    };
    Ok(Apply {
        target: m.target.to_string(),
        namespace: m.namespace.clone(),
        name: m.application_slug.clone(),
        spec: serde_json::to_string(&spec).map_err(|e| e.to_string())?,
    })
}

/// What the agent's latest observation says about the run's generation.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Heard {
    /// Nothing about this generation yet.
    Nothing,
    Accepted,
    Applying,
    Ready,
    Failed(String),
    /// The agent already carries a newer generation.
    Newer,
}

/// Read `observation` for the run of `generation`.
#[must_use]
pub fn heard(observation: Option<&RuntimeObservation>, generation: i64) -> Heard {
    let Some(o) = observation else {
        return Heard::Nothing;
    };
    if o.generation > generation {
        return Heard::Newer;
    }
    if o.generation < generation {
        return Heard::Nothing;
    }
    let reason = |fallback: &str| o.reason.clone().unwrap_or_else(|| fallback.to_owned());
    match o.phase.as_str() {
        "accepted" => Heard::Accepted,
        "applying" => Heard::Applying,
        "ready" => Heard::Ready,
        "failed" => Heard::Failed(reason("AgentFailed")),
        "rejected" => Heard::Failed(reason("EnvelopeRejected")),
        _ => Heard::Nothing,
    }
}

impl Worker {
    /// Carry a run of an agent-delivered target.
    pub(super) async fn drive_agent(
        &self,
        claim: &Claim,
        m: &Materialization,
        token: &CancellationToken,
    ) -> Result<Infallible, Stop> {
        let mut phase = m.phase;
        if phase == RunPhase::Planned {
            phase = self.advance(claim, m, RunEvent::ReadyForDelivery).await?;
        }
        match phase {
            RunPhase::PendingDelivery
            | RunPhase::AcceptedByCluster
            | RunPhase::Preflight
            | RunPhase::Applying
            | RunPhase::Verifying => {}
            other => return Err(Error::Unsupported(other.as_str()).into()),
        }
        let apply = self.prepare_envelope(claim, m, token).await?;
        self.follow_agent(claim, m, phase, &apply, token).await
    }

    /// Everything the envelope needs: the project and environment objects,
    /// the namespace, the frozen plan; then the envelope itself.
    async fn prepare_envelope(
        &self,
        claim: &Claim,
        m: &Materialization,
        token: &CancellationToken,
    ) -> Result<Apply, Stop> {
        let rendered = render(m).map_err(|e| refused(e.code()))?;
        if m.render_plan.is_none() {
            self.freeze(claim, m, &rendered.app).await?;
        }
        let project = self
            .ensure(Api::<Project>::all(self.client.clone()), &rendered.project, m.org)
            .await?;
        let mut environment = rendered.environment;
        if !render::set_owner(&mut environment, &project) {
            return Err(Error::Incomplete(format!("Project/{}", m.project_slug)).into());
        }
        self.ensure(Api::<Environment>::all(self.client.clone()), &environment, m.org)
            .await?;
        self.wait_namespace(&m.namespace, token).await?;
        self.orphan_app(m).await?;
        let mut tenant = self.store.tenant(m.org).await?;
        let plan = tenant
            .run_render_plan(m.run)
            .await?
            .ok_or_else(|| refused("PlanMissing"))?;
        drop(tenant);
        envelope(m, &plan).map_err(|_| refused("RenderFailed"))
    }

    /// A target handed over from the App controller still has its App
    /// object: delete it, orphaning its workloads, and wait until it is
    /// gone. The agent then adopts them in place, without new pods; with
    /// the App left, they would have two controllers.
    async fn orphan_app(&self, m: &Materialization) -> Result<(), Stop> {
        let apps = Api::<App>::namespaced(self.client.clone(), &m.namespace);
        let Some(live) = apps.get_opt(&m.application_slug).await? else {
            return Ok(());
        };
        let ours = live
            .annotations()
            .get(render::annotations::ID)
            .is_none_or(|id| *id == m.target.to_string());
        if !write::belongs_to(live.meta(), m.org) || !ours {
            return Err(refused("NameTaken"));
        }
        if !live.annotations().contains_key(render::annotations::HANDOVER) {
            // First take the App from its controller, which leaves a marked
            // App alone; delete it only after a pause, once a write of that
            // controller already under way has landed. A write after the
            // orphaning would give the workloads back an owner that is going,
            // and the garbage collector would take them with it (CI run
            // 34985518950).
            let mark = json!({ "metadata": { "annotations": {
                render::annotations::HANDOVER: m.target.to_string(),
            } } });
            apps.patch(&m.application_slug, &PatchParams::default(), &Patch::Merge(&mark))
                .await?;
            return Err(Stop::Wait(ORPHAN_WAIT, "HandingOverApp"));
        }
        if live.metadata.deletion_timestamp.is_none() {
            let orphan = DeleteParams {
                propagation_policy: Some(PropagationPolicy::Orphan),
                ..DeleteParams::default()
            };
            match apps.delete(&m.application_slug, &orphan).await {
                Ok(_) => tracing::info!(target = %m.target, "handover: the App goes, its workloads stay"),
                Err(kube::Error::Api(s)) if s.code == 404 => return Ok(()),
                Err(e) => return Err(e.into()),
            }
        }
        // The garbage collector drops the App from its workloads' owners,
        // then removes it.
        Err(Stop::Wait(ORPHAN_WAIT, "OrphaningApp"))
    }

    /// Send the envelope and follow the agent's observations until the run
    /// settles.
    async fn follow_agent(
        &self,
        claim: &Claim,
        m: &Materialization,
        mut phase: RunPhase,
        apply: &Apply,
        token: &CancellationToken,
    ) -> Result<Infallible, Stop> {
        let Some(agents) = self.agents.as_ref() else {
            return Err(Stop::Wait(AGENT_WAIT, "NoAgentLink"));
        };
        let cluster = m.cluster.to_string();
        let generation = i64::try_from(m.generation.0).unwrap_or(i64::MAX);
        let deadline = Instant::now() + self.verify_deadline;
        let mut sent: Option<Instant> = None;
        loop {
            if !self.store.renew_lease(claim, LEASE).await? {
                return Err(Stop::Fenced);
            }
            let observation = {
                let mut tenant = self.store.tenant(m.org).await?;
                tenant.runtime_observation(m.target).await?
            };
            match heard(observation.as_ref(), generation) {
                Heard::Newer => return Err(Stop::Superseded),
                Heard::Failed(reason) => return Err(Stop::Refused(reason)),
                Heard::Ready => {
                    phase = self.accepted(claim, m, phase).await?;
                    self.to_verifying(claim, m, phase).await?;
                    let done = self.advance(claim, m, RunEvent::Verified).await?;
                    return Err(Stop::Settled(done, None));
                }
                Heard::Applying => {
                    phase = self.accepted(claim, m, phase).await?;
                    phase = self.to_verifying(claim, m, phase).await?;
                }
                Heard::Accepted => phase = self.accepted(claim, m, phase).await?,
                Heard::Nothing => {
                    if sent.is_none_or(|at| at.elapsed() >= RESEND) {
                        if !agents.send(&cluster, apply.clone()).await {
                            return Err(Stop::Wait(AGENT_WAIT, "AgentUnavailable"));
                        }
                        sent = Some(Instant::now());
                    }
                }
            }
            if Instant::now() >= deadline {
                return Err(refused("VerifyTimeout"));
            }
            pause(token).await?;
        }
    }

    /// The agent persisted the envelope: the run is accepted by the cluster.
    async fn accepted(&self, claim: &Claim, m: &Materialization, phase: RunPhase) -> Result<RunPhase, Stop> {
        if phase == RunPhase::PendingDelivery {
            self.advance(claim, m, RunEvent::AcceptedByCluster).await
        } else {
            Ok(phase)
        }
    }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use kuben_core::{
        ids::{
            ApplicationId, ClusterId, ConfigRevisionId, DeploymentRunId, EnvironmentId, OperationId, OrgId,
            ProjectId, ReleaseId, RenderPlanId, TargetId,
        },
        ops::Generation,
    };
    use kuben_store::repo::Delivery;
    use serde_json::json;
    use uuid::Uuid;

    use super::*;
    use crate::render::RENDERER_VERSION;

    fn materialization() -> Materialization {
        Materialization {
            org: OrgId::new(),
            run: DeploymentRunId::new(),
            operation: OperationId::new(),
            phase: RunPhase::PendingDelivery,
            generation: Generation(3),
            lifecycle_uid: Uuid::now_v7(),
            project: ProjectId::new(),
            project_slug: "shop".into(),
            project_name: "Shop".into(),
            project_description: None,
            environment: EnvironmentId::new(),
            environment_slug: "prod".into(),
            environment_name: "Prod".into(),
            protected: false,
            env_type: "standard".into(),
            quota: None,
            namespace: "kb-shop-prod".into(),
            cluster: ClusterId::new(),
            delivery: Delivery::Agent,
            application: ApplicationId::new(),
            application_slug: "web".into(),
            application_name: "Web".into(),
            target: TargetId::new(),
            desired_generation: Generation(3),
            deleting: false,
            release: ReleaseId::new(),
            artifacts: BTreeMap::new(),
            image_repository: None,
            config_revision: ConfigRevisionId::new(),
            config_revision_number: 1,
            config: json!({}),
            render_plan: None,
            restarted_at: None,
        }
    }

    #[test]
    fn the_envelope_passes_the_agents_checks() {
        let m = materialization();
        let plan = RunPlan {
            id: RenderPlanId::new(),
            renderer_version: RENDERER_VERSION.into(),
            capability_snapshot: json!({}),
            // Keys in another order than canonical JSON has them.
            resources: json!([{ "metadata": { "namespace": "kb-shop-prod", "name": "web-web" }, "kind": "Deployment", "apiVersion": "apps/v1" }]),
        };
        let apply = envelope(&m, &plan).expect("envelope");
        assert_eq!(
            (apply.namespace.as_str(), apply.name.as_str()),
            ("kb-shop-prod", "web")
        );
        assert_eq!(apply.target, m.target.to_string());
        let checked = kuben_agent::runtime::check(&apply).expect("the agent accepts it");
        assert_eq!(checked.spec.generation, 3);
        assert_eq!(checked.spec.release_id, m.release.to_string());
        assert_eq!(checked.spec.plan.id, plan.id.to_string());
        assert_eq!(checked.resources.len(), 1);
    }

    fn observed(generation: i64, phase: &str, reason: Option<&str>) -> RuntimeObservation {
        RuntimeObservation {
            generation,
            phase: phase.into(),
            reason: reason.map(str::to_owned),
            message: None,
            observed_at: 0,
        }
    }

    #[test]
    fn observations_move_only_the_run_of_their_generation() {
        assert_eq!(heard(None, 3), Heard::Nothing);
        assert_eq!(
            heard(Some(&observed(2, "ready", None)), 3),
            Heard::Nothing,
            "an older run"
        );
        assert_eq!(heard(Some(&observed(4, "accepted", None)), 3), Heard::Newer);
        assert_eq!(heard(Some(&observed(3, "accepted", None)), 3), Heard::Accepted);
        assert_eq!(heard(Some(&observed(3, "applying", None)), 3), Heard::Applying);
        assert_eq!(heard(Some(&observed(3, "ready", None)), 3), Heard::Ready);
        assert_eq!(
            heard(Some(&observed(3, "failed", Some("RolloutFailed"))), 3),
            Heard::Failed("RolloutFailed".into())
        );
        assert_eq!(
            heard(Some(&observed(3, "rejected", None)), 3),
            Heard::Failed("EnvelopeRejected".into())
        );
        assert_eq!(heard(Some(&observed(3, "unknown", None)), 3), Heard::Nothing);
    }
}
