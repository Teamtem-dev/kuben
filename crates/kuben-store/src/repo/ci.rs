//! CI trust policies and token exchange (M4.2, migration 0020).
//!
//! An exchange records the provider token's id and issues the Kuben token in
//! one transaction that holds the policy row: a replayed provider token gets
//! nothing, and a revocation either precedes the exchange (nothing issued) or
//! follows it (the new token is revoked with the others).

use kuben_core::{
    ci::TrustPolicy,
    ids::{EnvironmentId, OrgId, ProjectId, UserId},
    model::ApiToken,
    perm::Role,
    time::now_ms,
};
use uuid::Uuid;

use super::{NewToken, product::counter};
use crate::{Store, StoreError};

const PROVIDER: &str = "github-actions";
const POLICY_COLUMNS: &str = "id, org_id, project_id, environment_id, name, repository, repository_id, \
     repository_owner_id, refs, environments, events, role, token_ttl_secs, created_by, created_at, revoked_at";
const INSERT_POLICY: &str = "INSERT INTO ci_trust_policies \
     (id, org_id, project_id, environment_id, name, provider, repository_id, repository_owner_id, repository, \
      refs, environments, events, role, token_ttl_secs, created_by, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)";
const REVOKE_POLICY: &str = "UPDATE ci_trust_policies SET revoked_at = $3 \
     WHERE id = $1 AND org_id = $2 AND revoked_at IS NULL";
const REVOKE_POLICY_TOKENS: &str = "UPDATE api_tokens SET revoked_at = $2 \
     WHERE ci_policy_id = $1 AND revoked_at IS NULL";
const HOLD_POLICY: &str = "SELECT revoked_at FROM ci_trust_policies WHERE id = $1 FOR SHARE";
const PURGE_USES: &str = "DELETE FROM ci_token_uses WHERE expires_at < $1";
const RECORD_USE: &str = "INSERT INTO ci_token_uses (issuer, jti, policy_id, expires_at, used_at) \
     VALUES ($1, $2, $3, $4, $5) ON CONFLICT (issuer, jti) DO NOTHING";
const INSERT_TOKEN: &str = "INSERT INTO api_tokens \
     (id, org_id, owner_user_id, name, prefix, secret_hash, scopes, expires_at, created_at, ci_policy_id) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)";

/// A new trust policy.
#[derive(Clone, Debug)]
pub struct NewCiPolicy {
    pub org: OrgId,
    pub project: ProjectId,
    pub environment: Option<EnvironmentId>,
    pub name: String,
    /// `owner/name` when the policy is made.
    pub repository: String,
    pub policy: TrustPolicy,
    /// Whose authority exchanged tokens act under.
    pub created_by: UserId,
}

/// A stored trust policy.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CiPolicy {
    pub id: Uuid,
    pub org: OrgId,
    pub project: ProjectId,
    pub environment: Option<EnvironmentId>,
    pub name: String,
    pub repository: String,
    pub policy: TrustPolicy,
    pub created_by: UserId,
    pub created_at: i64,
    pub revoked_at: Option<i64>,
}

/// One accepted provider token and the Kuben token it becomes.
#[derive(Clone, Debug)]
pub struct CiExchange {
    pub policy: Uuid,
    pub issuer: String,
    pub jti: String,
    /// When the provider token expires (Unix milliseconds).
    pub provider_expires_at: i64,
    pub token: NewToken,
}

/// The outcome of [`Store::exchange_ci_token`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Exchanged {
    Issued(Box<ApiToken>),
    /// This provider token was exchanged before.
    Replayed,
    /// The policy was revoked or is gone.
    PolicyRevoked,
}

#[derive(sqlx::FromRow)]
struct PolicyRow {
    id: Uuid,
    org_id: String,
    project_id: Uuid,
    environment_id: Option<Uuid>,
    name: String,
    repository: String,
    repository_id: i64,
    repository_owner_id: i64,
    refs: Vec<String>,
    environments: Vec<String>,
    events: Vec<String>,
    role: String,
    token_ttl_secs: i32,
    created_by: String,
    created_at: i64,
    revoked_at: Option<i64>,
}

fn decode<E: std::error::Error + Send + Sync + 'static>(e: E) -> sqlx::Error {
    sqlx::Error::Decode(Box::new(e))
}

impl TryFrom<PolicyRow> for CiPolicy {
    type Error = sqlx::Error;

    fn try_from(r: PolicyRow) -> Result<Self, Self::Error> {
        Ok(Self {
            id: r.id,
            org: r.org_id.parse().map_err(decode)?,
            project: ProjectId::from_uuid(r.project_id),
            environment: r.environment_id.map(EnvironmentId::from_uuid),
            name: r.name,
            repository: r.repository,
            policy: TrustPolicy {
                repository_id: counter(r.repository_id)?,
                repository_owner_id: counter(r.repository_owner_id)?,
                refs: r.refs,
                environments: r.environments,
                events: r.events,
                role: r
                    .role
                    .parse::<Role>()
                    .map_err(|e| sqlx::Error::Decode(e.to_string().into()))?,
                token_ttl_secs: u32::try_from(r.token_ttl_secs).map_err(decode)?,
            },
            created_by: r.created_by.parse().map_err(decode)?,
            created_at: r.created_at,
            revoked_at: r.revoked_at,
        })
    }
}

fn signed(value: u64) -> Result<i64, sqlx::Error> {
    i64::try_from(value).map_err(|e| sqlx::Error::Encode(e.into()))
}

impl Store {
    /// Record `new`. The caller validates the policy.
    pub async fn create_ci_policy(&self, new: &NewCiPolicy) -> Result<CiPolicy, StoreError> {
        let id = Uuid::now_v7();
        let created_at = now_ms();
        let p = &new.policy;
        sqlx::query(INSERT_POLICY)
            .bind(id)
            .bind(new.org.to_string())
            .bind(*new.project.as_uuid())
            .bind(new.environment.map(|e| *e.as_uuid()))
            .bind(&new.name)
            .bind(PROVIDER)
            .bind(signed(p.repository_id)?)
            .bind(signed(p.repository_owner_id)?)
            .bind(&new.repository)
            .bind(&p.refs)
            .bind(&p.environments)
            .bind(&p.events)
            .bind(p.role.to_string())
            .bind(i32::try_from(p.token_ttl_secs).map_err(|e| sqlx::Error::Encode(e.into()))?)
            .bind(new.created_by.to_string())
            .bind(created_at)
            .execute(self.pool())
            .await?;
        Ok(CiPolicy {
            id,
            org: new.org,
            project: new.project,
            environment: new.environment,
            name: new.name.clone(),
            repository: new.repository.clone(),
            policy: p.clone(),
            created_by: new.created_by,
            created_at,
            revoked_at: None,
        })
    }

    /// The trust policies of `org`, oldest first.
    pub async fn ci_policies(&self, org: OrgId) -> Result<Vec<CiPolicy>, StoreError> {
        let sql = format!(
            "SELECT {POLICY_COLUMNS} FROM ci_trust_policies WHERE org_id = $1 ORDER BY created_at, id"
        );
        let rows: Vec<PolicyRow> = sqlx::query_as(sqlx::AssertSqlSafe(sql))
            .bind(org.to_string())
            .fetch_all(self.pool())
            .await?;
        Ok(rows
            .into_iter()
            .map(CiPolicy::try_from)
            .collect::<Result<_, _>>()?)
    }

    /// A trust policy by id, whatever its organization (the exchange).
    pub async fn ci_policy(&self, id: Uuid) -> Result<Option<CiPolicy>, StoreError> {
        let sql = format!("SELECT {POLICY_COLUMNS} FROM ci_trust_policies WHERE id = $1");
        let row: Option<PolicyRow> = sqlx::query_as(sqlx::AssertSqlSafe(sql))
            .bind(id)
            .fetch_optional(self.pool())
            .await?;
        Ok(row.map(CiPolicy::try_from).transpose()?)
    }

    /// Revoke policy `id` of `org` and every token issued under it. False
    /// when there is no such unrevoked policy.
    pub async fn revoke_ci_policy(&self, org: OrgId, id: Uuid) -> Result<bool, StoreError> {
        let now = now_ms();
        let mut tx = self.pool().begin().await?;
        let revoked = sqlx::query(REVOKE_POLICY)
            .bind(id)
            .bind(org.to_string())
            .bind(now)
            .execute(&mut *tx)
            .await?
            .rows_affected();
        if revoked == 0 {
            return Ok(false);
        }
        sqlx::query(REVOKE_POLICY_TOKENS)
            .bind(id)
            .bind(now)
            .execute(&mut *tx)
            .await?;
        tx.commit().await?;
        Ok(true)
    }

    /// Accept provider token `e.jti` once and issue `e.token` under policy
    /// `e.policy`.
    pub async fn exchange_ci_token(&self, e: &CiExchange) -> Result<Exchanged, StoreError> {
        let now = now_ms();
        let mut tx = self.pool().begin().await?;
        let held: Option<Option<i64>> = sqlx::query_scalar(HOLD_POLICY)
            .bind(e.policy)
            .fetch_optional(&mut *tx)
            .await?;
        if !matches!(held, Some(None)) {
            return Ok(Exchanged::PolicyRevoked);
        }
        sqlx::query(PURGE_USES).bind(now).execute(&mut *tx).await?;
        let recorded = sqlx::query(RECORD_USE)
            .bind(&e.issuer)
            .bind(&e.jti)
            .bind(e.policy)
            .bind(e.provider_expires_at)
            .bind(now)
            .execute(&mut *tx)
            .await?
            .rows_affected();
        if recorded == 0 {
            return Ok(Exchanged::Replayed);
        }
        let t = &e.token;
        let scopes = serde_json::to_string(&t.scope).map_err(|err| sqlx::Error::Encode(err.into()))?;
        sqlx::query(INSERT_TOKEN)
            .bind(t.id.to_string())
            .bind(t.org_id.to_string())
            .bind(t.owner.to_string())
            .bind(&t.name)
            .bind(&t.prefix)
            .bind(&t.secret_hash)
            .bind(&scopes)
            .bind(t.expires_at)
            .bind(now)
            .bind(e.policy)
            .execute(&mut *tx)
            .await?;
        tx.commit().await?;
        Ok(Exchanged::Issued(Box::new(ApiToken {
            id: t.id,
            org_id: t.org_id,
            owner: Some(t.owner),
            name: t.name.clone(),
            prefix: t.prefix.clone(),
            secret_hash: t.secret_hash.clone(),
            scope: t.scope.clone(),
            expires_at: t.expires_at,
            last_used_at: None,
            revoked_at: None,
            created_at: now,
        })))
    }
}

#[cfg(test)]
mod tests {
    use kuben_core::{
        ci::{DEFAULT_CI_TOKEN_TTL_SECS, DEFAULT_EVENTS},
        ids::TokenId,
        model::TokenScope,
    };

    use super::*;
    use crate::testing::{pg_store, skip};

    struct Fixture {
        org: OrgId,
        project: ProjectId,
        owner: UserId,
    }

    async fn fixture(store: &Store) -> Fixture {
        let org = store.create_org("a", "A").await.expect("org").id;
        let owner = store
            .create_user("owner@example.com", None, None)
            .await
            .expect("user")
            .id;
        store.add_membership(org, owner).await.expect("member");
        store.bind_org_role(org, owner, Role::Owner).await.expect("role");
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        t.commit().await.expect("commit");
        Fixture { org, project, owner }
    }

    fn new_policy(f: &Fixture, name: &str) -> NewCiPolicy {
        NewCiPolicy {
            org: f.org,
            project: f.project,
            environment: None,
            name: name.into(),
            repository: "acme/shop".into(),
            policy: TrustPolicy {
                repository_id: 123_456,
                repository_owner_id: 42,
                refs: vec!["refs/heads/main".into()],
                environments: vec![],
                events: DEFAULT_EVENTS.iter().map(|e| (*e).to_owned()).collect(),
                role: Role::Developer,
                token_ttl_secs: DEFAULT_CI_TOKEN_TTL_SECS,
            },
            created_by: f.owner,
        }
    }

    fn exchange(f: &Fixture, policy: Uuid, jti: &str) -> CiExchange {
        CiExchange {
            policy,
            issuer: "https://token.actions.githubusercontent.com".into(),
            jti: jti.into(),
            provider_expires_at: now_ms() + 300_000,
            token: NewToken {
                id: TokenId::new(),
                org_id: f.org,
                owner: f.owner,
                name: format!("ci:{jti}"),
                prefix: "kbn_pat_ci".into(),
                secret_hash: jti.as_bytes().to_vec(),
                scope: TokenScope {
                    role: Role::Developer,
                    project: Some(*f.project.as_uuid()),
                    environment: None,
                },
                expires_at: Some(now_ms() + 900_000),
            },
        }
    }

    #[tokio::test]
    async fn policies_round_trip_and_names_are_unique() {
        let Some(store) = pg_store().await else {
            return skip("policies_round_trip_and_names_are_unique");
        };
        let f = fixture(&store).await;
        let made = store
            .create_ci_policy(&new_policy(&f, "deploy"))
            .await
            .expect("create");
        assert_eq!(store.ci_policy(made.id).await.expect("read"), Some(made.clone()));
        assert_eq!(store.ci_policies(f.org).await.expect("list"), vec![made]);
        let twice = store.create_ci_policy(&new_policy(&f, "deploy")).await;
        assert!(twice.is_err_and(|e| e.is_unique_violation()));
    }

    #[tokio::test]
    async fn a_provider_token_is_exchanged_once() {
        let Some(store) = pg_store().await else {
            return skip("a_provider_token_is_exchanged_once");
        };
        let f = fixture(&store).await;
        let policy = store
            .create_ci_policy(&new_policy(&f, "deploy"))
            .await
            .expect("create");
        let first = store
            .exchange_ci_token(&exchange(&f, policy.id, "j-1"))
            .await
            .expect("exchange");
        let Exchanged::Issued(token) = first else {
            panic!("not issued: {first:?}");
        };
        assert_eq!(
            store
                .exchange_ci_token(&exchange(&f, policy.id, "j-1"))
                .await
                .expect("replay"),
            Exchanged::Replayed
        );
        assert_eq!(
            store
                .exchange_ci_token(&exchange(&f, Uuid::now_v7(), "j-2"))
                .await
                .expect("unknown"),
            Exchanged::PolicyRevoked
        );
        assert!(
            store
                .list_tokens(f.owner)
                .await
                .expect("list")
                .iter()
                .all(|t| t.id != token.id),
            "CI tokens are listed under their policy, not their owner"
        );
        assert!(store.find_token(token.id).await.expect("read").is_some());
    }

    #[tokio::test]
    async fn revoking_a_policy_revokes_its_tokens_and_stops_exchanges() {
        let Some(store) = pg_store().await else {
            return skip("revoking_a_policy_revokes_its_tokens_and_stops_exchanges");
        };
        let f = fixture(&store).await;
        let policy = store
            .create_ci_policy(&new_policy(&f, "deploy"))
            .await
            .expect("create");
        let Exchanged::Issued(token) = store
            .exchange_ci_token(&exchange(&f, policy.id, "j-1"))
            .await
            .expect("x")
        else {
            panic!("not issued");
        };
        let other = store.create_org("b", "B").await.expect("org").id;
        assert!(
            !store.revoke_ci_policy(other, policy.id).await.expect("foreign"),
            "another org's policy"
        );
        assert!(store.revoke_ci_policy(f.org, policy.id).await.expect("revoke"));
        assert!(!store.revoke_ci_policy(f.org, policy.id).await.expect("again"));
        let revoked = store.find_token(token.id).await.expect("read").expect("token");
        assert!(revoked.revoked_at.is_some());
        assert_eq!(
            store
                .exchange_ci_token(&exchange(&f, policy.id, "j-2"))
                .await
                .expect("exchange"),
            Exchanged::PolicyRevoked
        );
        let mut conn = store.pool().acquire().await.expect("conn");
        let changed = sqlx::query("UPDATE ci_trust_policies SET role = 'admin' WHERE id = $1")
            .bind(policy.id)
            .execute(&mut *conn)
            .await;
        assert!(changed.is_err(), "a policy is only ever revoked");
        let deleted = sqlx::query("DELETE FROM ci_trust_policies WHERE id = $1")
            .bind(policy.id)
            .execute(&mut *conn)
            .await;
        assert!(deleted.is_err());
    }
}
