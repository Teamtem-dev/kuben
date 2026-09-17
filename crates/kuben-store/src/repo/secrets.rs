//! Managed secrets (M4.4, ADR-030, migration 0022).
//!
//! The store never sees a value: callers seal a revision's values between
//! [`Tenant::reserve_secret_revision`], which fixes the identity the seal is
//! bound to, and [`Tenant::insert_secret_revision`], in one transaction.
//! Deployment runs bind the current revision of every secret their
//! configuration references when they are accepted; names without a
//! managed secret are left to the cluster (Secrets made before M4.4).

use std::collections::BTreeSet;

use kuben_core::{
    ids::{ConfigRevisionId, DeploymentRunId, EnvironmentId, ProjectId, TargetId},
    ops::RunPhase,
    time::now_ms,
};
use uuid::Uuid;

use super::{NewAudit, Tenant, product::counter};
use crate::{Store, StoreError};

macro_rules! summary {
    ($filter:literal) => {
        concat!(
            "SELECT s.id, s.name, s.current_revision, r.keys, r.revoked_at IS NOT NULL AS revoked, ",
            "s.created_at, s.updated_at FROM secrets s ",
            "JOIN secret_revisions r ON r.secret_id = s.id AND r.revision = s.current_revision ",
            "AND r.org_id = s.org_id ",
            "WHERE s.environment_id = $1 AND s.org_id = $2 AND s.deleted_at IS NULL ",
            $filter
        )
    };
}
const SECRETS: &str = summary!("ORDER BY s.name");
const SECRET: &str = summary!("AND s.name = $3");
const CREATE_SECRET: &str = "INSERT INTO secrets \
     (id, org_id, project_id, environment_id, name, created_by, created_at, updated_at) \
     SELECT $1, e.org_id, e.project_id, e.id, $5, $6, $7, $7 FROM environments e \
     WHERE e.id = $4 AND e.org_id = $2 AND e.project_id = $3 AND NOT e.deleting \
     ON CONFLICT (environment_id, name) WHERE deleted_at IS NULL DO NOTHING";
const NEXT_REVISION: &str = "UPDATE secrets s SET current_revision = current_revision + 1, updated_at = $4 \
     FROM environments e \
     WHERE s.environment_id = $1 AND s.name = $2 AND s.org_id = $3 AND s.deleted_at IS NULL \
       AND e.id = s.environment_id AND e.org_id = s.org_id AND NOT e.deleting \
     RETURNING s.id, s.current_revision";
const INSERT_REVISION: &str = "INSERT INTO secret_revisions \
     (secret_id, org_id, revision, keys, ciphertext, wrapped_key, key_version, created_by, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)";
const LIVE_SECRET: &str = "SELECT id FROM secrets \
     WHERE environment_id = $1 AND name = $2 AND org_id = $3 AND deleted_at IS NULL";
const REVISIONS: &str = "SELECT revision, keys, key_version, created_by, created_at, revoked_at, revoked_by \
     FROM secret_revisions WHERE secret_id = $1 AND org_id = $2 ORDER BY revision DESC";
const REVOKE: &str = "UPDATE secret_revisions SET revoked_at = $4, revoked_by = $5 \
     WHERE secret_id = $1 AND revision = $2 AND org_id = $3 AND revoked_at IS NULL";
const REVISION_EXISTS: &str =
    "SELECT EXISTS (SELECT 1 FROM secret_revisions WHERE secret_id = $1 AND revision = $2 AND org_id = $3)";
/// Live targets of an environment whose newest configuration references a
/// secret by name.
const USERS: &str = "SELECT t.id, a.slug FROM application_targets t \
     JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id \
     JOIN applications a ON a.id = t.application_id AND a.org_id = t.org_id \
     JOIN LATERAL (SELECT c.config FROM target_config_revisions c \
       WHERE c.target_id = t.id AND c.org_id = t.org_id ORDER BY c.revision DESC LIMIT 1) c ON TRUE \
     WHERE p.environment_id = $1 AND t.org_id = $2 AND NOT t.deleting \
       AND EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(c.config -> 'env') = 'array' \
         THEN c.config -> 'env' ELSE '[]'::jsonb END) AS v (value) \
         WHERE v.value -> 'fromSecret' ->> 'name' = $3) \
     ORDER BY a.slug";
const DELETE: &str = "UPDATE secrets SET deleted_at = $3, updated_at = $3 \
     WHERE id = $1 AND org_id = $2 AND deleted_at IS NULL";
/// The managed secret, if any, behind every name a configuration revision
/// references, in the environment of `target`.
const WANTED: &str = "WITH wanted AS ( \
       SELECT DISTINCT v.value -> 'fromSecret' ->> 'name' AS name FROM target_config_revisions c \
       CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(c.config -> 'env') = 'array' \
         THEN c.config -> 'env' ELSE '[]'::jsonb END) AS v (value) \
       WHERE c.id = $1 AND c.org_id = $2 AND jsonb_typeof(v.value -> 'fromSecret' -> 'name') = 'string') \
     SELECT w.name, s.id AS secret_id, s.current_revision, r.revoked_at IS NOT NULL AS revoked \
     FROM wanted w \
     JOIN application_targets t ON t.id = $3 AND t.org_id = $2 \
     JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id \
     LEFT JOIN secrets s ON s.environment_id = p.environment_id AND s.name = w.name \
       AND s.org_id = $2 AND s.deleted_at IS NULL \
     LEFT JOIN secret_revisions r ON r.secret_id = s.id AND r.revision = s.current_revision \
     ORDER BY w.name";
const BIND: &str = "INSERT INTO run_secret_bindings (run_id, org_id, secret_id, revision, name) \
     SELECT $1, $2, x.secret_id, x.revision, x.name \
     FROM unnest($3::uuid[], $4::bigint[], $5::text[]) AS x (secret_id, revision, name)";
const RUN_BINDINGS: &str = "SELECT name, secret_id, revision FROM run_secret_bindings \
     WHERE run_id = $1 AND org_id = $2 ORDER BY name";
const RUN_SECRETS: &str = "SELECT b.name, b.secret_id, b.revision, r.keys, r.ciphertext, r.wrapped_key, \
     r.key_version, r.revoked_at IS NOT NULL AS revoked FROM run_secret_bindings b \
     JOIN secret_revisions r ON r.secret_id = b.secret_id AND r.revision = b.revision AND r.org_id = b.org_id \
     WHERE b.run_id = $1 AND b.org_id = $2 ORDER BY b.name";
/// Revisions the cluster may still need in an environment: those of runs
/// that may still write, and of each target's newest and newest successful
/// run.
const IN_USE: &str = "SELECT DISTINCT b.secret_id, b.revision FROM run_secret_bindings b \
     JOIN deployment_runs r ON r.id = b.run_id AND r.org_id = b.org_id \
     JOIN application_targets t ON t.id = r.target_id AND t.org_id = r.org_id \
     JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id \
     WHERE p.environment_id = $1 AND b.org_id = $2 AND ( \
       r.phase <> ALL($3) \
       OR r.id = (SELECT d.id FROM deployment_runs d WHERE d.target_id = r.target_id \
         AND d.org_id = r.org_id ORDER BY d.generation DESC LIMIT 1) \
       OR r.id = (SELECT d.id FROM deployment_runs d WHERE d.target_id = r.target_id \
         AND d.org_id = r.org_id AND d.phase = 'succeeded' ORDER BY d.generation DESC LIMIT 1))";
const SEALED_BELOW: &str = "SELECT secret_id, revision, ciphertext, wrapped_key, key_version \
     FROM secret_revisions WHERE org_id = $1 AND key_version < $2 \
     ORDER BY secret_id, revision LIMIT $3";
const RESEAL: &str = "UPDATE secret_revisions SET wrapped_key = $4, key_version = $5 \
     WHERE secret_id = $1 AND revision = $2 AND org_id = $3 AND key_version = $6";

const RECORD_FINGERPRINT: &str = "INSERT INTO secret_key_fingerprints (version, fingerprint, first_seen) \
     VALUES ($1, $2, $3) ON CONFLICT (version) DO NOTHING";
const FINGERPRINTS: &str = "SELECT version, fingerprint FROM secret_key_fingerprints ORDER BY version";

/// A revision's values as sealed by the caller.
#[derive(Clone, PartialEq, Eq)]
pub struct SealedBytes {
    pub ciphertext: Vec<u8>,
    pub wrapped_key: Vec<u8>,
    pub key_version: u32,
}

impl std::fmt::Debug for SealedBytes {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("SealedBytes")
            .field("key_version", &self.key_version)
            .finish_non_exhaustive()
    }
}

/// A live secret and its current revision.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SecretSummary {
    pub id: Uuid,
    pub name: String,
    pub revision: u64,
    pub keys: Vec<String>,
    /// The current revision is revoked: nothing can be deployed with it.
    pub revoked: bool,
    pub created_at: i64,
    pub updated_at: i64,
}

/// One revision of a secret, without its values.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SecretRevisionInfo {
    pub revision: u64,
    pub keys: Vec<String>,
    pub key_version: u32,
    pub created_by: String,
    pub created_at: i64,
    pub revoked_at: Option<i64>,
    pub revoked_by: Option<String>,
}

/// The identity a new revision is sealed for.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Reserved {
    pub secret: Uuid,
    pub revision: u64,
}

/// The revision of a secret a run renders.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SecretBinding {
    /// The name the configuration referenced.
    pub name: String,
    pub secret: Uuid,
    pub revision: u64,
}

/// A bound revision with its sealed values, for the materializer.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BoundSecret {
    pub binding: SecretBinding,
    pub keys: Vec<String>,
    pub sealed: SealedBytes,
    pub revoked: bool,
}

/// A revision sealed under an older key.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct StaleSeal {
    pub secret: Uuid,
    pub revision: u64,
    pub sealed: SealedBytes,
}

/// The outcome of [`Tenant::revoke_secret_revision`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Revoked {
    Done,
    /// Revoked before; revocation is never undone.
    Already,
    NotFound,
}

/// The outcome of [`Tenant::delete_secret`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum SecretDeleted {
    Done,
    NotFound,
    /// These apps of the environment reference it.
    InUse(Vec<String>),
}

/// A name a configuration references, and the managed secret behind it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct Wanted {
    pub name: String,
    pub current: Option<(Uuid, u64)>,
    pub revoked: bool,
}

#[derive(sqlx::FromRow)]
struct SummaryRow {
    id: Uuid,
    name: String,
    current_revision: i64,
    keys: Vec<String>,
    revoked: bool,
    created_at: i64,
    updated_at: i64,
}

#[derive(sqlx::FromRow)]
struct RevisionRow {
    revision: i64,
    keys: Vec<String>,
    key_version: i32,
    created_by: String,
    created_at: i64,
    revoked_at: Option<i64>,
    revoked_by: Option<String>,
}

#[derive(sqlx::FromRow)]
struct WantedRow {
    name: String,
    secret_id: Option<Uuid>,
    current_revision: Option<i64>,
    revoked: bool,
}

#[derive(sqlx::FromRow)]
struct BoundRow {
    name: String,
    secret_id: Uuid,
    revision: i64,
    keys: Vec<String>,
    ciphertext: Vec<u8>,
    wrapped_key: Vec<u8>,
    key_version: i32,
    revoked: bool,
}

#[derive(sqlx::FromRow)]
struct StaleRow {
    secret_id: Uuid,
    revision: i64,
    ciphertext: Vec<u8>,
    wrapped_key: Vec<u8>,
    key_version: i32,
}

fn signed(value: u64) -> Result<i64, sqlx::Error> {
    i64::try_from(value).map_err(|e| sqlx::Error::Encode(e.into()))
}

fn version(value: i32) -> Result<u32, sqlx::Error> {
    u32::try_from(value).map_err(|e| sqlx::Error::Decode(e.into()))
}

impl TryFrom<SummaryRow> for SecretSummary {
    type Error = sqlx::Error;
    fn try_from(r: SummaryRow) -> Result<Self, Self::Error> {
        Ok(Self {
            id: r.id,
            name: r.name,
            revision: counter(r.current_revision)?,
            keys: r.keys,
            revoked: r.revoked,
            created_at: r.created_at,
            updated_at: r.updated_at,
        })
    }
}

impl TryFrom<RevisionRow> for SecretRevisionInfo {
    type Error = sqlx::Error;
    fn try_from(r: RevisionRow) -> Result<Self, Self::Error> {
        Ok(Self {
            revision: counter(r.revision)?,
            keys: r.keys,
            key_version: version(r.key_version)?,
            created_by: r.created_by,
            created_at: r.created_at,
            revoked_at: r.revoked_at,
            revoked_by: r.revoked_by,
        })
    }
}

impl TryFrom<BoundRow> for BoundSecret {
    type Error = sqlx::Error;
    fn try_from(r: BoundRow) -> Result<Self, Self::Error> {
        Ok(Self {
            binding: SecretBinding {
                name: r.name,
                secret: r.secret_id,
                revision: counter(r.revision)?,
            },
            keys: r.keys,
            sealed: SealedBytes {
                ciphertext: r.ciphertext,
                wrapped_key: r.wrapped_key,
                key_version: version(r.key_version)?,
            },
            revoked: r.revoked,
        })
    }
}

impl Tenant {
    /// The live secrets of `environment`, by name.
    pub async fn secrets(&mut self, environment: EnvironmentId) -> Result<Vec<SecretSummary>, StoreError> {
        let rows: Vec<SummaryRow> = sqlx::query_as(SECRETS)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(SecretSummary::try_from)
            .collect::<Result<_, _>>()?)
    }

    /// The live secret `name` of `environment`.
    pub async fn secret(
        &mut self,
        environment: EnvironmentId,
        name: &str,
    ) -> Result<Option<SecretSummary>, StoreError> {
        let row: Option<SummaryRow> = sqlx::query_as(SECRET)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .bind(name)
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row.map(SecretSummary::try_from).transpose()?)
    }

    /// Take the next revision number of secret `name`, creating the secret
    /// when it does not exist. `None` when the project has no such live
    /// environment. The revision must be inserted in the same transaction.
    pub async fn reserve_secret_revision(
        &mut self,
        project: ProjectId,
        environment: EnvironmentId,
        name: &str,
        created_by: &str,
    ) -> Result<Option<Reserved>, StoreError> {
        let org = self.org.to_string();
        let now = now_ms();
        sqlx::query(CREATE_SECRET)
            .bind(Uuid::now_v7())
            .bind(&org)
            .bind(*project.as_uuid())
            .bind(*environment.as_uuid())
            .bind(name)
            .bind(created_by)
            .bind(now)
            .execute(&mut *self.tx)
            .await?;
        let row: Option<(Uuid, i64)> = sqlx::query_as(NEXT_REVISION)
            .bind(*environment.as_uuid())
            .bind(name)
            .bind(&org)
            .bind(now)
            .fetch_optional(&mut *self.tx)
            .await?;
        row.map(|(secret, revision)| {
            Ok(Reserved {
                secret,
                revision: counter(revision)?,
            })
        })
        .transpose()
    }

    /// Record the `reserved` revision with `keys` and its sealed values, and
    /// its audit record (names only).
    pub async fn insert_secret_revision(
        &mut self,
        reserved: Reserved,
        keys: &[String],
        sealed: &SealedBytes,
        created_by: &str,
        mut audit: NewAudit,
    ) -> Result<(), StoreError> {
        let org = self.org.to_string();
        sqlx::query(INSERT_REVISION)
            .bind(reserved.secret)
            .bind(&org)
            .bind(signed(reserved.revision)?)
            .bind(keys)
            .bind(&sealed.ciphertext)
            .bind(&sealed.wrapped_key)
            .bind(i32::try_from(sealed.key_version).map_err(|e| sqlx::Error::Encode(e.into()))?)
            .bind(created_by)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        audit.data = Some(serde_json::json!({ "revision": reserved.revision, "keys": keys }));
        self.append_audit(audit).await
    }

    /// Append `audit` for this organization in this transaction.
    pub async fn append_audit(&mut self, mut audit: NewAudit) -> Result<(), StoreError> {
        audit.org_id = Some(self.org);
        audit.insert(&mut *self.tx).await?;
        Ok(())
    }

    async fn live_secret(
        &mut self,
        environment: EnvironmentId,
        name: &str,
    ) -> Result<Option<Uuid>, StoreError> {
        Ok(sqlx::query_scalar(LIVE_SECRET)
            .bind(*environment.as_uuid())
            .bind(name)
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?)
    }

    /// Every revision of the live secret `name`, newest first.
    pub async fn secret_revisions(
        &mut self,
        environment: EnvironmentId,
        name: &str,
    ) -> Result<Option<Vec<SecretRevisionInfo>>, StoreError> {
        let Some(secret) = self.live_secret(environment, name).await? else {
            return Ok(None);
        };
        let rows: Vec<RevisionRow> = sqlx::query_as(REVISIONS)
            .bind(secret)
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(Some(
            rows.into_iter()
                .map(SecretRevisionInfo::try_from)
                .collect::<Result<_, _>>()?,
        ))
    }

    /// Revoke `revision` of the live secret `name` for good. Runs bound to it
    /// fail instead of delivering it; the values stay unreadable to the API.
    pub async fn revoke_secret_revision(
        &mut self,
        environment: EnvironmentId,
        name: &str,
        revision: u64,
        revoked_by: &str,
        audit: NewAudit,
    ) -> Result<Revoked, StoreError> {
        let Some(secret) = self.live_secret(environment, name).await? else {
            return Ok(Revoked::NotFound);
        };
        let org = self.org.to_string();
        let rows = sqlx::query(REVOKE)
            .bind(secret)
            .bind(signed(revision)?)
            .bind(&org)
            .bind(now_ms())
            .bind(revoked_by)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        if rows == 1 {
            self.append_audit(audit).await?;
            return Ok(Revoked::Done);
        }
        let exists: bool = sqlx::query_scalar(REVISION_EXISTS)
            .bind(secret)
            .bind(signed(revision)?)
            .bind(&org)
            .fetch_one(&mut *self.tx)
            .await?;
        Ok(if exists {
            Revoked::Already
        } else {
            Revoked::NotFound
        })
    }

    /// The live targets of `environment` whose newest configuration
    /// references secret `name`, with their app names.
    pub async fn secret_users(
        &mut self,
        environment: EnvironmentId,
        name: &str,
    ) -> Result<Vec<(TargetId, String)>, StoreError> {
        let rows: Vec<(Uuid, String)> = sqlx::query_as(USERS)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .bind(name)
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(|(id, slug)| (TargetId::from_uuid(id), slug))
            .collect())
    }

    /// Delete the live secret `name` unless an app references it. Its
    /// revisions stay, for the runs bound to them.
    pub async fn delete_secret(
        &mut self,
        environment: EnvironmentId,
        name: &str,
        audit: NewAudit,
    ) -> Result<SecretDeleted, StoreError> {
        let Some(secret) = self.live_secret(environment, name).await? else {
            return Ok(SecretDeleted::NotFound);
        };
        let users = self.secret_users(environment, name).await?;
        if !users.is_empty() {
            return Ok(SecretDeleted::InUse(
                users.into_iter().map(|(_, slug)| slug).collect(),
            ));
        }
        sqlx::query(DELETE)
            .bind(secret)
            .bind(self.org.to_string())
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        self.append_audit(audit).await?;
        Ok(SecretDeleted::Done)
    }

    /// What a run of `config_revision` on `target` would bind.
    pub(super) async fn wanted_secrets(
        &mut self,
        config_revision: ConfigRevisionId,
        target: TargetId,
    ) -> Result<Vec<Wanted>, StoreError> {
        let rows: Vec<WantedRow> = sqlx::query_as(WANTED)
            .bind(*config_revision.as_uuid())
            .bind(self.org.to_string())
            .bind(*target.as_uuid())
            .fetch_all(&mut *self.tx)
            .await?;
        rows.into_iter()
            .map(|r| {
                let current = match (r.secret_id, r.current_revision) {
                    (Some(id), Some(revision)) => Some((id, counter(revision)?)),
                    _ => None,
                };
                Ok(Wanted {
                    name: r.name,
                    current,
                    revoked: r.revoked,
                })
            })
            .collect()
    }

    /// Bind `run` to the current revisions in `wanted`.
    pub(super) async fn bind_run_secrets(
        &mut self,
        run: DeploymentRunId,
        wanted: &[Wanted],
    ) -> Result<(), StoreError> {
        let (mut ids, mut revisions, mut names) = (Vec::new(), Vec::new(), Vec::new());
        for w in wanted {
            if let Some((id, revision)) = w.current {
                ids.push(id);
                revisions.push(signed(revision)?);
                names.push(w.name.clone());
            }
        }
        if ids.is_empty() {
            return Ok(());
        }
        sqlx::query(BIND)
            .bind(*run.as_uuid())
            .bind(self.org.to_string())
            .bind(ids)
            .bind(revisions)
            .bind(names)
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// The revisions `run` is bound to.
    pub async fn run_secret_bindings(
        &mut self,
        run: DeploymentRunId,
    ) -> Result<Vec<SecretBinding>, StoreError> {
        let rows: Vec<(String, Uuid, i64)> = sqlx::query_as(RUN_BINDINGS)
            .bind(*run.as_uuid())
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?;
        rows.into_iter()
            .map(|(name, secret, revision)| {
                Ok(SecretBinding {
                    name,
                    secret,
                    revision: counter(revision)?,
                })
            })
            .collect()
    }

    /// The revisions `run` is bound to, with their sealed values.
    pub async fn run_secrets(&mut self, run: DeploymentRunId) -> Result<Vec<BoundSecret>, StoreError> {
        let rows: Vec<BoundRow> = sqlx::query_as(RUN_SECRETS)
            .bind(*run.as_uuid())
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(BoundSecret::try_from)
            .collect::<Result<_, _>>()?)
    }

    /// The `(secret, revision)` pairs the workloads of `environment` may
    /// still read.
    pub async fn secret_revisions_in_use(
        &mut self,
        environment: EnvironmentId,
    ) -> Result<BTreeSet<(Uuid, u64)>, StoreError> {
        let settled: Vec<&str> = RunPhase::ALL
            .into_iter()
            .filter(|p| p.is_final() || *p == RunPhase::Failed)
            .map(RunPhase::as_str)
            .collect();
        let rows: Vec<(Uuid, i64)> = sqlx::query_as(IN_USE)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .bind(settled)
            .fetch_all(&mut *self.tx)
            .await?;
        rows.into_iter()
            .map(|(secret, revision)| Ok((secret, counter(revision)?)))
            .collect()
    }

    /// Up to `limit` revisions sealed under a key older than `current`.
    pub async fn stale_seals(&mut self, current: u32, limit: i64) -> Result<Vec<StaleSeal>, StoreError> {
        let rows: Vec<StaleRow> = sqlx::query_as(SEALED_BELOW)
            .bind(self.org.to_string())
            .bind(i32::try_from(current).map_err(|e| sqlx::Error::Encode(e.into()))?)
            .bind(limit)
            .fetch_all(&mut *self.tx)
            .await?;
        rows.into_iter()
            .map(|r| {
                Ok(StaleSeal {
                    secret: r.secret_id,
                    revision: counter(r.revision)?,
                    sealed: SealedBytes {
                        ciphertext: r.ciphertext,
                        wrapped_key: r.wrapped_key,
                        key_version: version(r.key_version)?,
                    },
                })
            })
            .collect()
    }

    /// Replace the sealed data key of `stale` with `resealed`, unless it
    /// changed meanwhile. The values are not touched.
    pub async fn reseal(&mut self, stale: &StaleSeal, resealed: &SealedBytes) -> Result<bool, StoreError> {
        let to_i32 = |v: u32| i32::try_from(v).map_err(|e| sqlx::Error::Encode(e.into()));
        if resealed.ciphertext != stale.sealed.ciphertext {
            return Err(sqlx::Error::Protocol("a reseal must keep the ciphertext".into()).into());
        }
        let rows = sqlx::query(RESEAL)
            .bind(stale.secret)
            .bind(signed(stale.revision)?)
            .bind(self.org.to_string())
            .bind(&resealed.wrapped_key)
            .bind(to_i32(resealed.key_version)?)
            .bind(to_i32(stale.sealed.key_version)?)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }
}

/// How a keyring compares with the keys this database has seen.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct KeyringCheck {
    /// Versions whose key differs from the one recorded: the keyring is not
    /// this installation's.
    pub mismatched: Vec<u32>,
    /// Recorded versions the keyring lacks: revisions sealed under them
    /// cannot be opened.
    pub missing: Vec<u32>,
}

impl Store {
    /// Record the fingerprints of a keyring's keys and compare them with
    /// those recorded before.
    pub async fn check_keyring(&self, fingerprints: &[(u32, [u8; 32])]) -> Result<KeyringCheck, StoreError> {
        let to_i32 = |v: u32| i32::try_from(v).map_err(|e| sqlx::Error::Encode(e.into()));
        let mut tx = self.pool().begin().await?;
        for (version, fingerprint) in fingerprints {
            sqlx::query(RECORD_FINGERPRINT)
                .bind(to_i32(*version)?)
                .bind(fingerprint.as_slice())
                .bind(now_ms())
                .execute(&mut *tx)
                .await?;
        }
        let known: Vec<(i32, Vec<u8>)> = sqlx::query_as(FINGERPRINTS).fetch_all(&mut *tx).await?;
        tx.commit().await?;
        let mut check = KeyringCheck::default();
        for (version, fingerprint) in known {
            let version = u32::try_from(version).map_err(|e| sqlx::Error::Decode(e.into()))?;
            match fingerprints.iter().find(|(v, _)| *v == version) {
                Some((_, ours)) if ours.as_slice() == fingerprint.as_slice() => {}
                Some(_) => check.mismatched.push(version),
                None => check.missing.push(version),
            }
        }
        Ok(check)
    }
}

#[cfg(test)]
mod tests {
    use kuben_core::{
        ids::{OrgId, ReleaseId},
        ops::Generation,
    };
    use serde_json::{Value, json};

    use super::*;
    use crate::{
        repo::{PortableRelease, RunReason, StartDeployment, Started},
        testing::{pg_store, skip},
    };

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
    const BY: &str = "user:alice";

    struct Fixture {
        org: OrgId,
        project: ProjectId,
        environment: EnvironmentId,
        target: TargetId,
        release: ReleaseId,
    }

    async fn fixture(store: &Store, slug: &str) -> Fixture {
        let org = store.create_org(slug, slug).await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let environment = t
            .create_environment(project, "prod", "Prod", false)
            .await
            .expect("environment");
        let cluster = t.create_cluster("eu-1").await.expect("cluster");
        let placement = t
            .create_placement(project, environment, cluster, &format!("{slug}-shop"))
            .await
            .expect("placement");
        let application = t.create_application(project, "web", "Web").await.expect("app");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        let (release, _) = t
            .create_release(
                project,
                &PortableRelease {
                    application,
                    artifacts: [("web".to_owned(), DIGEST.parse().expect("digest"))].into(),
                    process_contract: json!({}),
                    portable_config: json!({}),
                    renderer_schema: 1,
                    source: None,
                    created_by: BY.into(),
                },
            )
            .await
            .expect("release");
        t.commit().await.expect("commit");
        Fixture {
            org,
            project,
            environment,
            target,
            release,
        }
    }

    fn sealed(tag: u8) -> SealedBytes {
        SealedBytes {
            ciphertext: vec![tag; 40],
            wrapped_key: vec![tag; 60],
            key_version: 1,
        }
    }

    /// Set a new revision of `name` with key `url`.
    async fn put(store: &Store, f: &Fixture, name: &str, tag: u8) -> Reserved {
        let mut t = store.tenant(f.org).await.expect("tenant");
        let reserved = t
            .reserve_secret_revision(f.project, f.environment, name, BY)
            .await
            .expect("reserve")
            .expect("environment");
        t.insert_secret_revision(reserved, &["url".into()], &sealed(tag), BY, NewAudit::default())
            .await
            .expect("insert");
        t.commit().await.expect("commit");
        reserved
    }

    fn referencing(names: &[&str]) -> Value {
        let env: Vec<Value> = names
            .iter()
            .map(|n| json!({ "name": n.to_uppercase(), "fromSecret": { "name": n, "key": "url" } }))
            .chain([json!({ "name": "PLAIN", "value": "x" })])
            .collect();
        json!({ "env": env })
    }

    /// Start a deploy of a new configuration referencing `names`.
    async fn deploy(store: &Store, f: &Fixture, names: &[&str]) -> Started {
        let mut t = store.tenant(f.org).await.expect("tenant");
        let (revision, _) = t
            .create_config_revision(f.project, f.target, &referencing(names), BY)
            .await
            .expect("revision")
            .expect("target");
        let state = t.target_state(f.target).await.expect("read").expect("target");
        let req = StartDeployment {
            project: f.project,
            target: f.target,
            release: f.release,
            config_revision: revision,
            render_plan: None,
            expected_generation: state.desired_generation,
            lifecycle_uid: state.lifecycle_uid,
            reason: RunReason::Deploy,
            requested_by: BY.into(),
            input_hash: format!("{revision}").into_bytes(),
        };
        let started = t
            .start_deployment(&req, NewAudit::default(), None)
            .await
            .expect("start");
        t.commit().await.expect("commit");
        started
    }

    fn run_of(started: Started) -> DeploymentRunId {
        match started {
            Started::Accepted { run, .. } => run,
            other => panic!("not accepted: {other:?}"),
        }
    }

    #[tokio::test]
    async fn secrets_are_revisions_seen_only_by_their_organization() {
        let Some(store) = pg_store().await else {
            return skip("secrets_are_revisions_seen_only_by_their_organization");
        };
        let f = fixture(&store, "a").await;
        let other = fixture(&store, "b").await;
        let first = put(&store, &f, "db", 1).await;
        let second = put(&store, &f, "db", 2).await;
        assert_eq!(
            (first.secret, first.revision, second.revision),
            (second.secret, 1, 2)
        );

        let mut t = store.tenant(f.org).await.expect("tenant");
        let listed = t.secrets(f.environment).await.expect("list");
        assert_eq!(listed.len(), 1);
        assert_eq!((listed[0].name.as_str(), listed[0].revision), ("db", 2));
        assert_eq!(listed[0].keys, ["url"]);
        let revisions = t
            .secret_revisions(f.environment, "db")
            .await
            .expect("read")
            .expect("secret");
        assert_eq!(revisions.iter().map(|r| r.revision).collect::<Vec<_>>(), [2, 1]);
        assert_eq!(revisions[0].created_by, BY);
        assert_eq!(
            t.secret_revisions(f.environment, "nope").await.expect("read"),
            None
        );
        for (revision, expected) in [(2, Revoked::Done), (2, Revoked::Already), (9, Revoked::NotFound)] {
            let revoked = t
                .revoke_secret_revision(f.environment, "db", revision, BY, NewAudit::default())
                .await
                .expect("revoke");
            assert_eq!(revoked, expected, "revision {revision}");
        }
        let current = t
            .secret(f.environment, "db")
            .await
            .expect("read")
            .expect("secret");
        assert!(current.revoked);
        t.commit().await.expect("commit");

        let mut t = store.tenant(other.org).await.expect("tenant");
        assert!(t.secrets(f.environment).await.expect("list").is_empty());
        assert_eq!(t.secret(f.environment, "db").await.expect("read"), None);
        assert_eq!(
            t.revoke_secret_revision(f.environment, "db", 1, BY, NewAudit::default())
                .await
                .expect("revoke"),
            Revoked::NotFound
        );
        let foreign = t
            .reserve_secret_revision(other.project, f.environment, "db", BY)
            .await
            .expect("reserve");
        assert_eq!(foreign, None, "another organization's environment");
    }

    #[tokio::test]
    async fn runs_bind_the_current_revision_and_refuse_a_revoked_one() {
        let Some(store) = pg_store().await else {
            return skip("runs_bind_the_current_revision_and_refuse_a_revoked_one");
        };
        let f = fixture(&store, "a").await;
        let db = put(&store, &f, "db", 1).await;
        let first = run_of(deploy(&store, &f, &["db", "legacy"]).await);
        put(&store, &f, "db", 2).await;
        let second = run_of(deploy(&store, &f, &["db"]).await);

        let mut t = store.tenant(f.org).await.expect("tenant");
        let binding = |revision| {
            vec![SecretBinding {
                name: "db".into(),
                secret: db.secret,
                revision,
            }]
        };
        assert_eq!(t.run_secret_bindings(first).await.expect("read"), binding(1));
        assert_eq!(t.run_secret_bindings(second).await.expect("read"), binding(2));
        let bound = t.run_secrets(second).await.expect("read");
        assert_eq!((bound[0].sealed.clone(), bound[0].revoked), (sealed(2), false));
        assert_eq!(
            t.secret_revisions_in_use(f.environment).await.expect("read"),
            BTreeSet::from([(db.secret, 2)]),
            "the superseded run needs nothing"
        );
        let users = t.secret_users(f.environment, "db").await.expect("read");
        assert_eq!(users, [(f.target, "web".to_owned())]);
        t.revoke_secret_revision(f.environment, "db", 2, BY, NewAudit::default())
            .await
            .expect("revoke");
        t.commit().await.expect("commit");

        assert_eq!(deploy(&store, &f, &["db"]).await, Started::SecretRevoked);
        let mut t = store.tenant(f.org).await.expect("tenant");
        let state = t.target_state(f.target).await.expect("read").expect("target");
        assert_eq!(state.desired_generation, Generation(2), "nothing was accepted");
        let bound = t.run_secrets(second).await.expect("read");
        assert!(bound[0].revoked, "the materializer sees the revocation");
        drop(t);
        run_of(deploy(&store, &f, &["legacy"]).await);
    }

    #[tokio::test]
    async fn referenced_secrets_are_not_deleted() {
        let Some(store) = pg_store().await else {
            return skip("referenced_secrets_are_not_deleted");
        };
        let f = fixture(&store, "a").await;
        let old = put(&store, &f, "db", 1).await;
        run_of(deploy(&store, &f, &["db"]).await);
        let mut t = store.tenant(f.org).await.expect("tenant");
        assert_eq!(
            t.delete_secret(f.environment, "db", NewAudit::default())
                .await
                .expect("delete"),
            SecretDeleted::InUse(vec!["web".into()])
        );
        t.commit().await.expect("commit");
        run_of(deploy(&store, &f, &[]).await);
        let mut t = store.tenant(f.org).await.expect("tenant");
        for expected in [SecretDeleted::Done, SecretDeleted::NotFound] {
            let deleted = t
                .delete_secret(f.environment, "db", NewAudit::default())
                .await
                .expect("delete");
            assert_eq!(deleted, expected);
        }
        assert!(t.secrets(f.environment).await.expect("list").is_empty());
        t.commit().await.expect("commit");
        let new = put(&store, &f, "db", 3).await;
        assert_ne!(new.secret, old.secret, "a new secret of the same name");
        assert_eq!(new.revision, 1);
    }

    #[tokio::test]
    async fn keyrings_are_checked_and_stale_seals_resealed() {
        let Some(store) = pg_store().await else {
            return skip("keyrings_are_checked_and_stale_seals_resealed");
        };
        let (ours, theirs) = ([1u8; 32], [2u8; 32]);
        assert_eq!(
            store.check_keyring(&[(1, ours)]).await.expect("check"),
            KeyringCheck::default()
        );
        assert_eq!(
            store
                .check_keyring(&[(1, theirs)])
                .await
                .expect("check")
                .mismatched,
            [1]
        );
        let rotated = store.check_keyring(&[(2, theirs)]).await.expect("check");
        assert_eq!((rotated.mismatched, rotated.missing), (vec![], vec![1]));

        let f = fixture(&store, "a").await;
        let reserved = put(&store, &f, "db", 1).await;
        let mut t = store.tenant(f.org).await.expect("tenant");
        let stale = t.stale_seals(2, 10).await.expect("read");
        assert_eq!(stale.len(), 1);
        assert_eq!(
            (stale[0].secret, stale[0].revision),
            (reserved.secret, reserved.revision)
        );
        let resealed = SealedBytes {
            wrapped_key: vec![9; 60],
            key_version: 2,
            ..stale[0].sealed.clone()
        };
        assert!(t.reseal(&stale[0], &resealed).await.expect("reseal"));
        assert!(
            !t.reseal(&stale[0], &resealed).await.expect("reseal"),
            "changed meanwhile"
        );
        let tampered = SealedBytes {
            ciphertext: vec![0; 40],
            ..resealed.clone()
        };
        assert!(t.reseal(&stale[0], &tampered).await.is_err());
        assert!(t.stale_seals(2, 10).await.expect("read").is_empty());
        t.commit().await.expect("commit");
    }
}
