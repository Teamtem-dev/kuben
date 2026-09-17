//! Domain claims, DNS provider accounts and the records Kuben wrote through
//! them (M5.2, migration 0031).

use kuben_core::{
    domain::{ancestors, lock_key, overlaps},
    ids::{OrgId, TargetId},
    time::now_ms,
};
use uuid::Uuid;

use super::{SealedBytes, Tenant};
use crate::{Store, StoreError};

/// The claims, with a filter.
macro_rules! claims {
    ($tail:literal) => {
        concat!(
            "SELECT id, domain, token, status, method, provider_id, created_by, created_at, verified_at, \
             revoked_at, revoked_by, last_checked_at, last_error FROM domain_claims ",
            $tail
        )
    };
}
const CLAIMS: &str =
    claims!("WHERE org_id = $1 AND ($2 OR status <> 'revoked') ORDER BY domain, created_at DESC");
const CLAIM: &str = claims!("WHERE org_id = $1 AND id = $2");
const LOCK_CLAIM: &str = claims!("WHERE org_id = $1 AND id = $2 FOR UPDATE");
const INSERT_CLAIM: &str = "INSERT INTO domain_claims (id, org_id, domain, token, created_by, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6)";
const CHECKED: &str = "UPDATE domain_claims SET last_checked_at = $3, last_error = $4 \
     WHERE org_id = $1 AND id = $2";
const VERIFY: &str = "UPDATE domain_claims SET status = 'verified', method = $3, provider_id = $4, \
     verified_at = $5, last_checked_at = $5, last_error = NULL \
     WHERE org_id = $1 AND id = $2 AND status = 'pending'";
const REVOKE: &str = "UPDATE domain_claims SET status = 'revoked', revoked_at = $3, revoked_by = $4, \
     verified_at = NULL WHERE org_id = $1 AND id = $2 AND status <> 'revoked'";
const LOCK_SUFFIX: &str = "SELECT pg_advisory_xact_lock(hashtextextended($1, 5))";
const OVERLAPPING: &str = "SELECT domain, org_id FROM verified_domains \
     WHERE domain = $1 OR domain LIKE '%.' || $1 OR $1 LIKE '%.' || domain";
const REGISTER: &str =
    "INSERT INTO verified_domains (domain, org_id, claim_id, since) VALUES ($1, $2, $3, $4)";
const UNREGISTER: &str = "DELETE FROM verified_domains WHERE claim_id = $1 AND org_id = $2";
const OWNER: &str = "SELECT domain, org_id FROM verified_domains WHERE domain = ANY($1) \
     ORDER BY char_length(domain) DESC LIMIT 1";
const PROVIDERS: &str = "SELECT id, name, kind, created_by, created_at FROM dns_providers \
     WHERE org_id = $1 AND deleted_at IS NULL ORDER BY name";
const INSERT_PROVIDER: &str = "INSERT INTO dns_providers \
     (id, org_id, name, kind, secret, wrapped_key, key_version, created_by, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)";
const PROVIDER_SECRET: &str = "SELECT kind, secret, wrapped_key, key_version FROM dns_providers \
     WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL";
const DELETE_PROVIDER: &str = "UPDATE dns_providers SET deleted_at = $3 \
     WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL";
const RECORDS_OF: &str = "SELECT id, provider_id, target_id, name, record_type, content, zone_id, provider_ref \
     FROM dns_records WHERE org_id = $1 AND target_id = $2 ORDER BY name, record_type";
const UPSERT_RECORD: &str = "INSERT INTO dns_records \
     (id, org_id, provider_id, target_id, name, record_type, content, zone_id, provider_ref, created_at, updated_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10) \
     ON CONFLICT (provider_id, name, record_type, content) DO UPDATE \
     SET target_id = EXCLUDED.target_id, zone_id = EXCLUDED.zone_id, provider_ref = EXCLUDED.provider_ref, \
         updated_at = EXCLUDED.updated_at \
     RETURNING id";
const FORGET_RECORD: &str = "DELETE FROM dns_records WHERE org_id = $1 AND id = $2";

/// A domain claim.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct DomainClaim {
    pub id: Uuid,
    pub domain: String,
    /// The TXT value that proves the claim.
    pub token: String,
    /// `pending`, `verified` or `revoked`.
    pub status: String,
    /// `txt` or the provider kind that proved it.
    pub method: Option<String>,
    pub provider_id: Option<Uuid>,
    pub created_by: String,
    pub created_at: i64,
    pub verified_at: Option<i64>,
    pub revoked_at: Option<i64>,
    pub revoked_by: Option<String>,
    pub last_checked_at: Option<i64>,
    pub last_error: Option<String>,
}

/// The outcome of [`Tenant::verify_claim`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Verified {
    Verified,
    /// The claim is not pending (verified or revoked already), or missing.
    NotPending,
    /// Another organization verified an overlapping domain first.
    Taken(String),
}

/// A DNS provider account, without its token.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct DnsProvider {
    pub id: Uuid,
    pub name: String,
    pub kind: String,
    pub created_by: String,
    pub created_at: i64,
}

/// A DNS record Kuben wrote.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct DnsRecord {
    pub id: Uuid,
    pub provider_id: Uuid,
    pub target_id: Option<Uuid>,
    pub name: String,
    pub record_type: String,
    pub content: String,
    pub zone_id: String,
    pub provider_ref: Option<String>,
}

/// A record to remember.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NewDnsRecord<'a> {
    pub provider: Uuid,
    pub target: Option<TargetId>,
    pub name: &'a str,
    pub record_type: &'a str,
    pub content: &'a str,
    pub zone_id: &'a str,
    pub provider_ref: &'a str,
}

fn version(key_version: u32) -> Result<i32, sqlx::Error> {
    i32::try_from(key_version).map_err(|e| sqlx::Error::Encode(e.into()))
}

impl Tenant {
    /// Claim `domain` (canonical) with the TXT value `token`.
    pub async fn create_claim(
        &mut self,
        id: Uuid,
        domain: &str,
        token: &str,
        by: &str,
    ) -> Result<(), StoreError> {
        sqlx::query(INSERT_CLAIM)
            .bind(id)
            .bind(self.org.to_string())
            .bind(domain)
            .bind(token)
            .bind(by)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// The organization's claims; revoked ones only with `all`.
    pub async fn claims(&mut self, all: bool) -> Result<Vec<DomainClaim>, StoreError> {
        Ok(sqlx::query_as(CLAIMS)
            .bind(self.org.to_string())
            .bind(all)
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// Claim `id`.
    pub async fn claim(&mut self, id: Uuid) -> Result<Option<DomainClaim>, StoreError> {
        Ok(sqlx::query_as(CLAIM)
            .bind(self.org.to_string())
            .bind(id)
            .fetch_optional(&mut *self.tx)
            .await?)
    }

    /// Record a verification attempt of claim `id` that failed with `error`.
    pub async fn claim_checked(&mut self, id: Uuid, error: Option<&str>) -> Result<(), StoreError> {
        let error = error.map(|e| e.chars().take(1024).collect::<String>());
        sqlx::query(CHECKED)
            .bind(self.org.to_string())
            .bind(id)
            .bind(now_ms())
            .bind(error)
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// Mark claim `id` verified by `method` (and `provider`), unless another
    /// organization holds an overlapping verified domain. Claims that could
    /// overlap are serialized by a lock on the domain's last two labels.
    pub async fn verify_claim(
        &mut self,
        id: Uuid,
        method: &str,
        provider: Option<Uuid>,
    ) -> Result<Verified, StoreError> {
        let org = self.org.to_string();
        let Some(claim) = sqlx::query_as::<_, DomainClaim>(LOCK_CLAIM)
            .bind(&org)
            .bind(id)
            .fetch_optional(&mut *self.tx)
            .await?
            .filter(|c| c.status == "pending")
        else {
            return Ok(Verified::NotPending);
        };
        sqlx::query(LOCK_SUFFIX)
            .bind(lock_key(&claim.domain))
            .execute(&mut *self.tx)
            .await?;
        let held: Vec<(String, String)> = sqlx::query_as(OVERLAPPING)
            .bind(&claim.domain)
            .fetch_all(&mut *self.tx)
            .await?;
        if let Some((domain, _)) = held
            .iter()
            .find(|(domain, owner)| *owner != org && overlaps(domain, &claim.domain))
        {
            return Ok(Verified::Taken(domain.clone()));
        }
        let now = now_ms();
        sqlx::query(VERIFY)
            .bind(&org)
            .bind(id)
            .bind(method)
            .bind(provider)
            .bind(now)
            .execute(&mut *self.tx)
            .await?;
        if !held.iter().any(|(domain, _)| *domain == claim.domain) {
            sqlx::query(REGISTER)
                .bind(&claim.domain)
                .bind(&org)
                .bind(id)
                .bind(now)
                .execute(&mut *self.tx)
                .await?;
        }
        Ok(Verified::Verified)
    }

    /// Revoke claim `id`. False when it was revoked already or is missing.
    pub async fn revoke_claim(&mut self, id: Uuid, by: &str) -> Result<bool, StoreError> {
        let org = self.org.to_string();
        let rows = sqlx::query(REVOKE)
            .bind(&org)
            .bind(id)
            .bind(now_ms())
            .bind(by)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        sqlx::query(UNREGISTER)
            .bind(id)
            .bind(&org)
            .execute(&mut *self.tx)
            .await?;
        Ok(rows == 1)
    }

    /// Add a DNS provider account whose API token is `secret` (sealed for
    /// `id`).
    pub async fn create_dns_provider(
        &mut self,
        id: Uuid,
        name: &str,
        kind: &str,
        secret: &SealedBytes,
        by: &str,
    ) -> Result<(), StoreError> {
        sqlx::query(INSERT_PROVIDER)
            .bind(id)
            .bind(self.org.to_string())
            .bind(name)
            .bind(kind)
            .bind(&secret.ciphertext)
            .bind(&secret.wrapped_key)
            .bind(version(secret.key_version)?)
            .bind(by)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// The organization's DNS provider accounts.
    pub async fn dns_providers(&mut self) -> Result<Vec<DnsProvider>, StoreError> {
        Ok(sqlx::query_as(PROVIDERS)
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// The kind and sealed token of provider `id`.
    pub async fn dns_provider_secret(
        &mut self,
        id: Uuid,
    ) -> Result<Option<(String, SealedBytes)>, StoreError> {
        let row: Option<(String, Vec<u8>, Vec<u8>, i32)> = sqlx::query_as(PROVIDER_SECRET)
            .bind(self.org.to_string())
            .bind(id)
            .fetch_optional(&mut *self.tx)
            .await?;
        row.map(|(kind, ciphertext, wrapped_key, key_version)| {
            Ok((
                kind,
                SealedBytes {
                    ciphertext,
                    wrapped_key,
                    key_version: u32::try_from(key_version).map_err(|e| sqlx::Error::Decode(e.into()))?,
                },
            ))
        })
        .transpose()
    }

    /// Remove provider `id`. False when there is none.
    pub async fn delete_dns_provider(&mut self, id: Uuid) -> Result<bool, StoreError> {
        let rows = sqlx::query(DELETE_PROVIDER)
            .bind(self.org.to_string())
            .bind(id)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// The records Kuben wrote for `target`.
    pub async fn dns_records(&mut self, target: TargetId) -> Result<Vec<DnsRecord>, StoreError> {
        Ok(sqlx::query_as(RECORDS_OF)
            .bind(self.org.to_string())
            .bind(*target.as_uuid())
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// Remember a record written through a provider; its id.
    pub async fn record_dns(&mut self, r: &NewDnsRecord<'_>) -> Result<Uuid, StoreError> {
        let now = now_ms();
        Ok(sqlx::query_scalar(UPSERT_RECORD)
            .bind(Uuid::now_v7())
            .bind(self.org.to_string())
            .bind(r.provider)
            .bind(r.target.map(|t| *t.as_uuid()))
            .bind(r.name)
            .bind(r.record_type)
            .bind(r.content)
            .bind(r.zone_id)
            .bind(r.provider_ref)
            .bind(now)
            .fetch_one(&mut *self.tx)
            .await?)
    }

    /// Forget record `id` (it was deleted at the provider).
    pub async fn forget_dns_record(&mut self, id: Uuid) -> Result<(), StoreError> {
        sqlx::query(FORGET_RECORD)
            .bind(self.org.to_string())
            .bind(id)
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }
}

impl Store {
    /// The organization holding a verified claim that covers `host`
    /// (canonical), with the claimed domain; the most specific claim wins.
    pub async fn domain_owner(&self, host: &str) -> Result<Option<(String, OrgId)>, StoreError> {
        let candidates: Vec<&str> = ancestors(host);
        let row: Option<(String, String)> = sqlx::query_as(OWNER)
            .bind(&candidates)
            .fetch_optional(self.pool())
            .await?;
        row.map(|(domain, org)| {
            Ok((
                domain,
                org.parse()
                    .map_err(|e: uuid::Error| sqlx::Error::Decode(e.into()))?,
            ))
        })
        .transpose()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testing::{pg_store, skip};

    const TOKEN: &str = "0123456789abcdef0123456789abcdef";

    #[tokio::test]
    async fn verified_domains_are_unique_across_organizations() {
        let Some(store) = pg_store().await else {
            return skip("verified_domains_are_unique_across_organizations");
        };
        let a = store.create_org("a", "A").await.expect("org").id;
        let b = store.create_org("b", "B").await.expect("org").id;
        let (claim_a, claim_b, claim_b2) = (Uuid::now_v7(), Uuid::now_v7(), Uuid::now_v7());
        let mut t = store.tenant(a).await.expect("tenant");
        t.create_claim(claim_a, "example.com", TOKEN, "u")
            .await
            .expect("claim");
        assert_eq!(
            t.verify_claim(claim_a, "txt", None).await.expect("verify"),
            Verified::Verified
        );
        assert_eq!(
            t.verify_claim(claim_a, "txt", None).await.expect("verify"),
            Verified::NotPending
        );
        t.commit().await.expect("commit");

        let mut t = store.tenant(a).await.expect("tenant");
        let twice = t.create_claim(Uuid::now_v7(), "example.com", TOKEN, "u").await;
        assert!(
            twice.is_err_and(|e| e.is_unique_violation()),
            "one open claim per domain"
        );

        let mut t = store.tenant(b).await.expect("tenant");
        t.create_claim(claim_b, "shop.example.com", TOKEN, "u")
            .await
            .expect("claim");
        t.create_claim(claim_b2, "other.org", TOKEN, "u")
            .await
            .expect("claim");
        assert_eq!(
            t.verify_claim(claim_b, "txt", None).await.expect("verify"),
            Verified::Taken("example.com".into()),
            "a subdomain of another organization's domain"
        );
        assert_eq!(
            t.verify_claim(claim_b2, "txt", None).await.expect("verify"),
            Verified::Verified
        );
        assert_eq!(t.claims(false).await.expect("list").len(), 2, "isolated");
        t.commit().await.expect("commit");

        assert_eq!(
            store.domain_owner("a.shop.example.com").await.expect("owner"),
            Some(("example.com".into(), a))
        );
        assert_eq!(store.domain_owner("example.net").await.expect("owner"), None);

        let mut t = store.tenant(a).await.expect("tenant");
        assert!(t.revoke_claim(claim_a, "u").await.expect("revoke"));
        assert!(!t.revoke_claim(claim_a, "u").await.expect("revoke"), "once");
        t.commit().await.expect("commit");
        assert_eq!(store.domain_owner("example.com").await.expect("owner"), None);
        let mut t = store.tenant(b).await.expect("tenant");
        assert_eq!(
            t.verify_claim(claim_b, "txt", None).await.expect("verify"),
            Verified::Verified
        );
    }

    #[tokio::test]
    async fn providers_and_records_are_kept() {
        let Some(store) = pg_store().await else {
            return skip("providers_and_records_are_kept");
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let id = Uuid::now_v7();
        let sealed = SealedBytes {
            ciphertext: vec![1],
            wrapped_key: vec![2],
            key_version: 1,
        };
        let mut t = store.tenant(org).await.expect("tenant");
        t.create_dns_provider(id, "cf", "cloudflare", &sealed, "u")
            .await
            .expect("create");
        assert_eq!(t.dns_providers().await.expect("list")[0].name, "cf");
        assert_eq!(
            t.dns_provider_secret(id).await.expect("read"),
            Some(("cloudflare".into(), sealed.clone()))
        );
        let record = NewDnsRecord {
            provider: id,
            target: None,
            name: "shop.example.com",
            record_type: "A",
            content: "203.0.113.10",
            zone_id: "z1",
            provider_ref: "r1",
        };
        let first = t.record_dns(&record).await.expect("record");
        let again = t
            .record_dns(&NewDnsRecord {
                provider_ref: "r2",
                ..record.clone()
            })
            .await
            .expect("record");
        assert_eq!(first, again, "one row per record");
        t.forget_dns_record(first).await.expect("forget");
        assert!(t.delete_dns_provider(id).await.expect("delete"));
        assert!(t.dns_providers().await.expect("list").is_empty());
        t.commit().await.expect("commit");
    }
}
