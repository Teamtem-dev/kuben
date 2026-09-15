//! AgentLink records (ADR-027, migration 0011): enrollment tokens and the
//! agent linked to each cluster.
//!
//! An enrolling agent is anonymous until its token is redeemed, so the
//! [`Store`] functions read these tables by token hash and cluster id; the
//! tenant functions ([`Tenant::create_agent_token`],
//! [`Tenant::revoke_cluster_agent`]) only touch clusters of the caller's
//! organization.
//!
//! * A token is kept as its SHA-256, bound to one cluster, expires, and is
//!   redeemed once; the device that redeemed it may resume it for a grace
//!   period after expiry (a lost answer never strands it).
//! * A cluster has one current agent device. Only that device, unrevoked,
//!   may link or renew; a fresh enrollment with another device replaces a
//!   revoked one, and the same device stays revoked (a trigger holds it).

use std::time::Duration;

use kuben_core::ids::{ClusterId, OrgId, TargetId};
use uuid::Uuid;

use super::Tenant;
use crate::{Store, StoreError};

const CLUSTER_OF_ORG: &str = "SELECT EXISTS (SELECT 1 FROM clusters WHERE id = $1 AND org_id = $2)";
const INSERT_TOKEN: &str = "INSERT INTO agent_tokens \
     (token_hash, org_id, cluster_id, expires_at, created_by, created_at) \
     VALUES ($1, $2, $3, kuben_now_ms() + $4, $5, kuben_now_ms())";
const TOKEN_FOR_UPDATE: &str = "SELECT org_id, cluster_id, expires_at, device_id, kuben_now_ms() AS now \
     FROM agent_tokens WHERE token_hash = $1 FOR UPDATE";
const REDEEM_TOKEN: &str = "UPDATE agent_tokens SET device_id = $2, redeemed_at = kuben_now_ms() \
     WHERE token_hash = $1 AND device_id IS NULL";
const RECORD_CERTIFICATE: &str = "INSERT INTO cluster_agents \
     (cluster_id, org_id, device_id, certificate_not_after, updated_at) \
     VALUES ($1, $2, $3, $4, kuben_now_ms()) \
     ON CONFLICT (cluster_id) DO UPDATE SET \
       revoked_at = CASE WHEN cluster_agents.device_id = EXCLUDED.device_id \
                         THEN cluster_agents.revoked_at END, \
       device_id = EXCLUDED.device_id, \
       certificate_not_after = EXCLUDED.certificate_not_after, \
       updated_at = EXCLUDED.updated_at \
     WHERE cluster_agents.org_id = EXCLUDED.org_id";
const AGENT: &str = "SELECT cluster_id, org_id, device_id, certificate_not_after, protocol_version, \
     features::text AS features, agent_version, linked_at, last_seen_at, revoked_at \
     FROM cluster_agents WHERE cluster_id = $1";
const RECORD_LINK: &str = "UPDATE cluster_agents \
     SET protocol_version = $3, features = $4::jsonb, agent_version = $5, \
         linked_at = kuben_now_ms(), last_seen_at = kuben_now_ms(), updated_at = kuben_now_ms() \
     WHERE cluster_id = $1 AND device_id = $2 AND revoked_at IS NULL";
const TOUCH: &str = "UPDATE cluster_agents SET last_seen_at = kuben_now_ms() \
     WHERE cluster_id = $1 AND device_id = $2 AND revoked_at IS NULL";
const TOKEN_ORG: &str = "SELECT org_id FROM agent_tokens WHERE cluster_id = $1 AND device_id = $2 LIMIT 1";
const TARGET_DELIVERY: &str = "SELECT delivery FROM application_targets WHERE id = $1 AND org_id = $2";
const HAND_OVER: &str = "UPDATE application_targets t SET delivery = 'agent' \
     FROM environment_placements p \
     WHERE t.id = $1 AND t.org_id = $2 AND t.delivery = 'controller' AND NOT t.deleting \
       AND p.id = t.placement_id AND p.org_id = t.org_id \
       AND EXISTS (SELECT 1 FROM cluster_agents a WHERE a.cluster_id = p.cluster_id AND a.org_id = p.org_id \
                   AND a.revoked_at IS NULL AND a.features @> jsonb_build_array($3::text))";
const RECORD_OBSERVATION: &str = "INSERT INTO runtime_observations \
     (target_id, org_id, project_id, generation, phase, reason, message, observed_at) \
     SELECT t.id, t.org_id, t.project_id, $3, $4, $5, $6, kuben_now_ms() \
     FROM application_targets t \
     JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id \
     WHERE t.id = $1 AND p.cluster_id = $2 AND t.org_id = $7 \
     ON CONFLICT (target_id) DO UPDATE \
     SET generation = EXCLUDED.generation, phase = EXCLUDED.phase, reason = EXCLUDED.reason, \
         message = EXCLUDED.message, observed_at = EXCLUDED.observed_at \
     WHERE runtime_observations.generation <= EXCLUDED.generation";
const OBSERVATION: &str = "SELECT generation, phase, reason, message, observed_at \
     FROM runtime_observations WHERE target_id = $1 AND org_id = $2";
const REVOKE: &str = "UPDATE cluster_agents SET revoked_at = kuben_now_ms(), updated_at = kuben_now_ms() \
     WHERE cluster_id = $1 AND org_id = $2 AND revoked_at IS NULL";

/// The feature a linked agent must have negotiated for new targets of its
/// cluster to be delivered by it (the protocol's `applicationRuntime`).
pub const RUNTIME_FEATURE: &str = "applicationRuntime";

/// How a target's runs reach its cluster (migration 0012).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Delivery {
    /// The materializer writes the target's App; the App controller carries
    /// it out.
    Controller,
    /// The cluster's agent carries the target's execution envelopes out.
    Agent,
}

impl Delivery {
    #[must_use]
    pub fn parse(text: &str) -> Option<Self> {
        match text {
            "controller" => Some(Self::Controller),
            "agent" => Some(Self::Agent),
            _ => None,
        }
    }

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Controller => "controller",
            Self::Agent => "agent",
        }
    }
}

/// The latest observation an agent reported for a target.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct RuntimeObservation {
    pub generation: i64,
    /// `accepted`, `applying`, `ready`, `failed`, `rejected` or `unknown`.
    pub phase: String,
    pub reason: Option<String>,
    pub message: Option<String>,
    /// Unix milliseconds.
    pub observed_at: i64,
}

/// How a token was redeemed.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TokenRedemption {
    First,
    /// Again, by the device that redeemed it.
    Resumed,
}

/// Why a token was not redeemed. The agent hears one answer for all.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TokenRefusal {
    Unknown,
    Expired,
    OtherCluster,
    OtherDevice,
}

/// A redeemed token: the cluster's organization and how it was redeemed.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Redeemed {
    pub org: OrgId,
    pub cluster: ClusterId,
    pub kind: TokenRedemption,
}

/// The agent linked to a cluster.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ClusterAgent {
    pub org: OrgId,
    pub cluster: ClusterId,
    pub device_id: String,
    /// Unix milliseconds.
    pub certificate_not_after: i64,
    pub protocol_version: Option<u32>,
    pub features: Vec<String>,
    pub agent_version: Option<String>,
    pub linked_at: Option<i64>,
    pub last_seen_at: Option<i64>,
    pub revoked_at: Option<i64>,
}

impl ClusterAgent {
    /// Whether `device_id` may link or renew for this cluster.
    #[must_use]
    pub fn accepts(&self, device_id: &str) -> bool {
        self.revoked_at.is_none() && self.device_id == device_id
    }
}

#[derive(sqlx::FromRow)]
struct TokenRow {
    org_id: String,
    cluster_id: Uuid,
    expires_at: i64,
    device_id: Option<String>,
    now: i64,
}

#[derive(sqlx::FromRow)]
struct AgentRow {
    cluster_id: Uuid,
    org_id: String,
    device_id: String,
    certificate_not_after: i64,
    protocol_version: Option<i32>,
    features: String,
    agent_version: Option<String>,
    linked_at: Option<i64>,
    last_seen_at: Option<i64>,
    revoked_at: Option<i64>,
}

fn decode(e: impl Into<Box<dyn std::error::Error + Send + Sync>>) -> sqlx::Error {
    sqlx::Error::Decode(e.into())
}

fn org(text: &str) -> Result<OrgId, sqlx::Error> {
    text.parse().map_err(|e: uuid::Error| decode(e))
}

fn millis(d: Duration) -> i64 {
    i64::try_from(d.as_millis()).unwrap_or(i64::MAX)
}

impl AgentRow {
    fn into_agent(self) -> Result<ClusterAgent, sqlx::Error> {
        Ok(ClusterAgent {
            org: org(&self.org_id)?,
            cluster: ClusterId::from_uuid(self.cluster_id),
            device_id: self.device_id,
            certificate_not_after: self.certificate_not_after,
            protocol_version: self
                .protocol_version
                .map(u32::try_from)
                .transpose()
                .map_err(decode)?,
            features: serde_json::from_str(&self.features).map_err(decode)?,
            agent_version: self.agent_version,
            linked_at: self.linked_at,
            last_seen_at: self.last_seen_at,
            revoked_at: self.revoked_at,
        })
    }
}

impl Tenant {
    /// Store a bootstrap token, hashed as `hash`, for `cluster` of this
    /// organization, valid for `ttl`. False when the organization has no such
    /// cluster.
    pub async fn create_agent_token(
        &mut self,
        cluster: ClusterId,
        hash: &[u8; 32],
        ttl: Duration,
        created_by: &str,
    ) -> Result<bool, StoreError> {
        let org = self.org.to_string();
        let known: bool = sqlx::query_scalar(CLUSTER_OF_ORG)
            .bind(*cluster.as_uuid())
            .bind(&org)
            .fetch_one(&mut *self.tx)
            .await?;
        if !known {
            return Ok(false);
        }
        sqlx::query(INSERT_TOKEN)
            .bind(&hash[..])
            .bind(&org)
            .bind(*cluster.as_uuid())
            .bind(millis(ttl))
            .bind(created_by)
            .execute(&mut *self.tx)
            .await?;
        Ok(true)
    }

    /// How `target` of this organization is delivered, if it exists.
    pub async fn target_delivery(&mut self, target: TargetId) -> Result<Option<Delivery>, StoreError> {
        let found: Option<String> = sqlx::query_scalar(TARGET_DELIVERY)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        found
            .map(|d| Delivery::parse(&d).ok_or_else(|| decode(format!("unknown delivery {d:?}")).into()))
            .transpose()
    }

    /// Record what the agent of `cluster` observed of `target`: only for a
    /// target on that cluster, and never over a newer generation. False when
    /// nothing was recorded.
    pub async fn record_runtime_observation(
        &mut self,
        cluster: ClusterId,
        target: TargetId,
        generation: i64,
        phase: &str,
        reason: Option<&str>,
        message: Option<&str>,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(RECORD_OBSERVATION)
            .bind(*target.as_uuid())
            .bind(*cluster.as_uuid())
            .bind(generation)
            .bind(phase)
            .bind(reason)
            .bind(message)
            .bind(self.org.to_string())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// The latest observation of `target`, if its agent reported one.
    pub async fn runtime_observation(
        &mut self,
        target: TargetId,
    ) -> Result<Option<RuntimeObservation>, StoreError> {
        Ok(sqlx::query_as(OBSERVATION)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?)
    }

    /// Hand `target` over from the App controller to its cluster's agent: its
    /// runs go through the agent from now on, never back (migration 0012).
    /// Only a live target the App controller delivers, and only when its
    /// cluster has a linked, unrevoked agent that carries applications; false
    /// otherwise.
    pub async fn hand_over_to_agent(&mut self, target: TargetId) -> Result<bool, StoreError> {
        let rows = sqlx::query(HAND_OVER)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .bind(RUNTIME_FEATURE)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Revoke the agent of `cluster`: it may no longer link or renew until
    /// another device enrolls. False when nothing was revoked.
    pub async fn revoke_cluster_agent(&mut self, cluster: ClusterId) -> Result<bool, StoreError> {
        let rows = sqlx::query(REVOKE)
            .bind(*cluster.as_uuid())
            .bind(self.org.to_string())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }
}

impl Store {
    /// Redeem the token hashed as `hash` for `device_id` of `cluster`, by the
    /// database clock. The device that redeemed it may resume it until
    /// `resume_grace` after its expiry.
    pub async fn redeem_agent_token(
        &self,
        hash: &[u8; 32],
        cluster: ClusterId,
        device_id: &str,
        resume_grace: Duration,
    ) -> Result<Result<Redeemed, TokenRefusal>, StoreError> {
        let mut tx = self.pool().begin().await?;
        let row: Option<TokenRow> = sqlx::query_as(TOKEN_FOR_UPDATE)
            .bind(&hash[..])
            .fetch_optional(&mut *tx)
            .await?;
        let Some(token) = row else {
            return Ok(Err(TokenRefusal::Unknown));
        };
        if token.cluster_id != *cluster.as_uuid() {
            return Ok(Err(TokenRefusal::OtherCluster));
        }
        let kind = match token.device_id.as_deref() {
            Some(device) if device == device_id => {
                if token.now >= token.expires_at.saturating_add(millis(resume_grace)) {
                    return Ok(Err(TokenRefusal::Expired));
                }
                TokenRedemption::Resumed
            }
            Some(_) => return Ok(Err(TokenRefusal::OtherDevice)),
            None if token.now >= token.expires_at => return Ok(Err(TokenRefusal::Expired)),
            None => {
                sqlx::query(REDEEM_TOKEN)
                    .bind(&hash[..])
                    .bind(device_id)
                    .execute(&mut *tx)
                    .await?;
                TokenRedemption::First
            }
        };
        tx.commit().await?;
        Ok(Ok(Redeemed {
            org: org(&token.org_id)?,
            cluster,
            kind,
        }))
    }

    /// Record the certificate issued to `device_id` for `cluster`: it becomes
    /// the cluster's agent device. Another device than a revoked one clears
    /// the revocation; the same device stays revoked.
    pub async fn record_agent_certificate(
        &self,
        org: OrgId,
        cluster: ClusterId,
        device_id: &str,
        not_after_ms: i64,
    ) -> Result<(), StoreError> {
        sqlx::query(RECORD_CERTIFICATE)
            .bind(*cluster.as_uuid())
            .bind(org.to_string())
            .bind(device_id)
            .bind(not_after_ms)
            .execute(self.pool())
            .await?;
        Ok(())
    }

    /// The agent of `cluster`, if one ever enrolled.
    pub async fn cluster_agent(&self, cluster: ClusterId) -> Result<Option<ClusterAgent>, StoreError> {
        let row: Option<AgentRow> = sqlx::query_as(AGENT)
            .bind(*cluster.as_uuid())
            .fetch_optional(self.pool())
            .await?;
        Ok(row.map(AgentRow::into_agent).transpose()?)
    }

    /// Record a link of the cluster's current, unrevoked device. False when
    /// `device_id` is not that device (or it was revoked).
    pub async fn record_agent_link(
        &self,
        cluster: ClusterId,
        device_id: &str,
        protocol_version: u32,
        features: &[String],
        agent_version: &str,
    ) -> Result<bool, StoreError> {
        let features = serde_json::to_string(features).map_err(|e| StoreError::from(decode(e)))?;
        let rows = sqlx::query(RECORD_LINK)
            .bind(*cluster.as_uuid())
            .bind(device_id)
            .bind(i32::try_from(protocol_version).unwrap_or(i32::MAX))
            .bind(features)
            .bind(agent_version)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// The organization of `cluster`, as the token `device_id` redeemed
    /// names it: where the first certificate of a cluster belongs.
    pub async fn agent_token_org(
        &self,
        cluster: ClusterId,
        device_id: &str,
    ) -> Result<Option<OrgId>, StoreError> {
        let found: Option<String> = sqlx::query_scalar(TOKEN_ORG)
            .bind(*cluster.as_uuid())
            .bind(device_id)
            .fetch_optional(self.pool())
            .await?;
        Ok(found.as_deref().map(org).transpose()?)
    }

    /// Note that the cluster's agent was heard from.
    pub async fn touch_agent(&self, cluster: ClusterId, device_id: &str) -> Result<(), StoreError> {
        sqlx::query(TOUCH)
            .bind(*cluster.as_uuid())
            .bind(device_id)
            .execute(self.pool())
            .await?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testing::{pg_store, skip};

    const GRACE: Duration = Duration::from_hours(1);

    async fn cluster(store: &Store, slug: &str) -> (OrgId, ClusterId) {
        let org = store.create_org(slug, slug).await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let cluster = t.create_cluster("primary").await.expect("cluster");
        t.commit().await.expect("commit");
        (org, cluster)
    }

    async fn token(store: &Store, org: OrgId, cluster: ClusterId, n: u8, ttl: Duration) -> bool {
        let mut t = store.tenant(org).await.expect("tenant");
        let created = t
            .create_agent_token(cluster, &[n; 32], ttl, "user:alice")
            .await
            .expect("create");
        t.commit().await.expect("commit");
        created
    }

    #[tokio::test]
    async fn a_token_is_redeemed_once_and_resumed_by_its_device() {
        let Some(store) = pg_store().await else {
            skip("agent tokens");
            return;
        };
        let (org, cluster) = cluster(&store, "a").await;
        assert!(token(&store, org, cluster, 1, Duration::from_mins(30)).await);
        let first = store
            .redeem_agent_token(&[1; 32], cluster, "sha256:device-a", GRACE)
            .await
            .expect("redeem");
        assert_eq!(
            first,
            Ok(Redeemed {
                org,
                cluster,
                kind: TokenRedemption::First
            })
        );
        let again = store
            .redeem_agent_token(&[1; 32], cluster, "sha256:device-a", GRACE)
            .await
            .expect("redeem");
        assert_eq!(again.map(|r| r.kind), Ok(TokenRedemption::Resumed));
        assert_eq!(
            store
                .redeem_agent_token(&[1; 32], cluster, "sha256:device-b", GRACE)
                .await
                .expect("redeem"),
            Err(TokenRefusal::OtherDevice)
        );
        assert!(
            sqlx::query("UPDATE agent_tokens SET device_id = 'sha256:device-b'")
                .execute(store.pool())
                .await
                .is_err(),
            "a redeemed token keeps its device"
        );
    }

    #[tokio::test]
    async fn a_redeemed_token_names_the_organization_of_its_device() {
        let Some(store) = pg_store().await else {
            skip("agent tokens");
            return;
        };
        let (org, cluster) = cluster(&store, "a").await;
        assert!(token(&store, org, cluster, 4, Duration::from_mins(30)).await);
        assert_eq!(
            store
                .agent_token_org(cluster, "sha256:device-a")
                .await
                .expect("read"),
            None,
            "not redeemed yet"
        );
        store
            .redeem_agent_token(&[4; 32], cluster, "sha256:device-a", GRACE)
            .await
            .expect("redeem")
            .expect("redeemed");
        assert_eq!(
            store
                .agent_token_org(cluster, "sha256:device-a")
                .await
                .expect("read"),
            Some(org)
        );
    }

    /// A project with one environment on `cluster`: its id and placement.
    async fn placement_on(
        store: &Store,
        org: OrgId,
        cluster: ClusterId,
    ) -> (kuben_core::ids::ProjectId, kuben_core::ids::PlacementId) {
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let env = t
            .create_environment(project, "production", "Production", true)
            .await
            .expect("environment");
        let placement = t
            .create_placement(project, env, cluster, "kb-shop-production")
            .await
            .expect("placement");
        t.commit().await.expect("commit");
        (project, placement)
    }

    async fn target(
        store: &Store,
        org: OrgId,
        project: kuben_core::ids::ProjectId,
        placement: kuben_core::ids::PlacementId,
        slug: &str,
    ) -> TargetId {
        let mut t = store.tenant(org).await.expect("tenant");
        let application = t
            .create_application(project, slug, slug)
            .await
            .expect("application");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        t.commit().await.expect("commit");
        target
    }

    async fn delivery(store: &Store, org: OrgId, target: TargetId) -> Delivery {
        let mut t = store.tenant(org).await.expect("tenant");
        t.target_delivery(target).await.expect("read").expect("target")
    }

    #[tokio::test]
    async fn new_targets_of_a_cluster_with_a_linked_agent_are_delivered_by_it() {
        let Some(store) = pg_store().await else {
            skip("agent delivery");
            return;
        };
        let (org, cluster) = cluster(&store, "a").await;
        let (project, placement) = placement_on(&store, org, cluster).await;
        let before = target(&store, org, project, placement, "before").await;
        assert_eq!(
            delivery(&store, org, before).await,
            Delivery::Controller,
            "no agent yet"
        );

        store
            .record_agent_certificate(org, cluster, "sha256:device-a", 1)
            .await
            .expect("certificate");
        let without = target(&store, org, project, placement, "without").await;
        assert_eq!(
            delivery(&store, org, without).await,
            Delivery::Controller,
            "enrolled, never linked"
        );
        assert!(
            store
                .record_agent_link(
                    cluster,
                    "sha256:device-a",
                    1,
                    &[RUNTIME_FEATURE.to_owned()],
                    "1.1.2"
                )
                .await
                .expect("link")
        );
        let linked = target(&store, org, project, placement, "linked").await;
        assert_eq!(delivery(&store, org, linked).await, Delivery::Agent);
        assert_eq!(
            delivery(&store, org, before).await,
            Delivery::Controller,
            "existing targets stay"
        );

        let mut t = store.tenant(org).await.expect("tenant");
        assert!(
            sqlx::query("UPDATE application_targets SET delivery = 'controller' WHERE id = $1")
                .bind(*linked.as_uuid())
                .execute(&mut *t.tx)
                .await
                .is_err(),
            "a target delivered by its agent stays so"
        );
        drop(t);

        let mut t = store.tenant(org).await.expect("tenant");
        assert!(t.revoke_cluster_agent(cluster).await.expect("revoke"));
        t.commit().await.expect("commit");
        let revoked = target(&store, org, project, placement, "revoked").await;
        assert_eq!(
            delivery(&store, org, revoked).await,
            Delivery::Controller,
            "a revoked agent"
        );
    }

    #[tokio::test]
    async fn a_target_is_handed_over_only_to_a_linked_agent_that_carries_applications() {
        let Some(store) = pg_store().await else {
            skip("handover");
            return;
        };
        let (org, cluster) = cluster(&store, "a").await;
        let (project, placement) = placement_on(&store, org, cluster).await;
        let web = target(&store, org, project, placement, "web").await;
        let hand_over = |target: TargetId| {
            let store = store.clone();
            async move {
                let mut t = store.tenant(org).await.expect("tenant");
                let moved = t.hand_over_to_agent(target).await.expect("hand over");
                t.commit().await.expect("commit");
                moved
            }
        };
        assert!(!hand_over(web).await, "no agent");
        store
            .record_agent_certificate(org, cluster, "sha256:device-a", 1)
            .await
            .expect("certificate");
        assert!(
            store
                .record_agent_link(cluster, "sha256:device-a", 1, &[], "1.1.2")
                .await
                .expect("link")
        );
        assert!(!hand_over(web).await, "an agent without the runtime feature");
        assert!(
            store
                .record_agent_link(
                    cluster,
                    "sha256:device-a",
                    1,
                    &[RUNTIME_FEATURE.to_owned()],
                    "1.1.2"
                )
                .await
                .expect("link")
        );
        assert!(hand_over(web).await);
        assert_eq!(delivery(&store, org, web).await, Delivery::Agent);
        assert!(!hand_over(web).await, "handed over once");
        let (other, _) = self::cluster(&store, "b").await;
        let mut t = store.tenant(other).await.expect("tenant");
        assert!(
            !t.hand_over_to_agent(web).await.expect("hand over"),
            "another organization's target"
        );
    }

    #[tokio::test]
    async fn an_agent_reports_only_on_targets_of_its_cluster_and_never_backwards() {
        let Some(store) = pg_store().await else {
            skip("runtime observations");
            return;
        };
        let (org, cluster) = cluster(&store, "a").await;
        let mut t = store.tenant(org).await.expect("tenant");
        let elsewhere = t.create_cluster("secondary").await.expect("cluster");
        t.commit().await.expect("commit");
        let (project, placement) = placement_on(&store, org, cluster).await;
        let web = target(&store, org, project, placement, "web").await;

        let record = |cluster: ClusterId, generation: i64, phase: &'static str| {
            let store = store.clone();
            async move {
                let mut t = store.tenant(org).await.expect("tenant");
                let recorded = t
                    .record_runtime_observation(cluster, web, generation, phase, None, None)
                    .await
                    .expect("record");
                t.commit().await.expect("commit");
                recorded
            }
        };
        assert!(!record(elsewhere, 2, "ready").await, "another cluster's agent");
        assert!(record(cluster, 2, "applying").await);
        assert!(record(cluster, 2, "ready").await, "the same generation moves on");
        assert!(!record(cluster, 1, "failed").await, "an older generation");

        let mut t = store.tenant(org).await.expect("tenant");
        let seen = t.runtime_observation(web).await.expect("read").expect("observed");
        assert_eq!((seen.generation, seen.phase.as_str()), (2, "ready"));
        drop(t);
        let (other, _) = self::cluster(&store, "b").await;
        let mut t = store.tenant(other).await.expect("tenant");
        assert_eq!(
            t.runtime_observation(web).await.expect("read"),
            None,
            "another organization"
        );
    }

    #[tokio::test]
    async fn tokens_are_bound_to_their_cluster_and_expire() {
        let Some(store) = pg_store().await else {
            skip("agent tokens");
            return;
        };
        let (org, cluster) = cluster(&store, "a").await;
        let (_, other) = self::cluster(&store, "b").await;
        assert!(token(&store, org, cluster, 1, Duration::from_mins(30)).await);
        assert!(token(&store, org, cluster, 2, Duration::ZERO).await);
        assert_eq!(
            store
                .redeem_agent_token(&[1; 32], other, "sha256:d", GRACE)
                .await
                .expect("redeem"),
            Err(TokenRefusal::OtherCluster)
        );
        assert_eq!(
            store
                .redeem_agent_token(&[2; 32], cluster, "sha256:d", GRACE)
                .await
                .expect("redeem"),
            Err(TokenRefusal::Expired)
        );
        assert_eq!(
            store
                .redeem_agent_token(&[9; 32], cluster, "sha256:d", GRACE)
                .await
                .expect("redeem"),
            Err(TokenRefusal::Unknown)
        );
        // Another organization cannot mint a token for this cluster.
        let (other_org, _) = self::cluster(&store, "c").await;
        assert!(!token(&store, other_org, cluster, 3, Duration::from_mins(30)).await);
    }

    #[tokio::test]
    async fn a_revoked_device_stays_revoked_until_another_enrolls() {
        let Some(store) = pg_store().await else {
            skip("cluster agents");
            return;
        };
        let (org, cluster) = cluster(&store, "a").await;
        store
            .record_agent_certificate(org, cluster, "sha256:device-a", 1_000)
            .await
            .expect("record");
        let features = vec!["applicationRuntime".to_owned()];
        assert!(
            store
                .record_agent_link(cluster, "sha256:device-a", 1, &features, "1.1.2")
                .await
                .expect("link")
        );
        assert!(
            !store
                .record_agent_link(cluster, "sha256:device-x", 1, &features, "1.1.2")
                .await
                .expect("link"),
            "only the current device links"
        );
        let agent = store.cluster_agent(cluster).await.expect("read").expect("agent");
        assert!(agent.accepts("sha256:device-a") && !agent.accepts("sha256:device-x"));
        assert_eq!(
            (agent.protocol_version, agent.features.clone()),
            (Some(1), features.clone())
        );

        let (other_org, _) = self::cluster(&store, "b").await;
        let mut t = store.tenant(other_org).await.expect("tenant");
        assert!(
            !t.revoke_cluster_agent(cluster).await.expect("revoke"),
            "another organization"
        );
        drop(t);
        let mut t = store.tenant(org).await.expect("tenant");
        assert!(t.revoke_cluster_agent(cluster).await.expect("revoke"));
        t.commit().await.expect("commit");
        let revoked = store.cluster_agent(cluster).await.expect("read").expect("agent");
        assert!(!revoked.accepts("sha256:device-a"));

        // A renewal of the same device keeps the revocation.
        store
            .record_agent_certificate(org, cluster, "sha256:device-a", 2_000)
            .await
            .expect("record");
        assert!(
            store
                .cluster_agent(cluster)
                .await
                .expect("read")
                .expect("agent")
                .revoked_at
                .is_some()
        );
        assert!(
            sqlx::query("UPDATE cluster_agents SET revoked_at = NULL")
                .execute(store.pool())
                .await
                .is_err(),
            "the same device stays revoked"
        );
        // A fresh enrollment with another device replaces it.
        store
            .record_agent_certificate(org, cluster, "sha256:device-b", 3_000)
            .await
            .expect("record");
        let replaced = store.cluster_agent(cluster).await.expect("read").expect("agent");
        assert!(replaced.accepts("sha256:device-b") && !replaced.accepts("sha256:device-a"));
    }
}
