//! Single sign-on state and accounts (M4.3, migration 0021).
//!
//! A sign-in links the provider subject to one user (created, or found by
//! the verified email), makes them a member of the organization and sets
//! their organization role to the one the provider's groups earn, all in
//! one transaction.

use kuben_core::{
    ids::{OrgId, UserId},
    model::User,
    sso::SsoPerson,
    time::now_ms,
};

use crate::{Store, StoreError};

const PURGE: &str = "DELETE FROM sso_logins WHERE expires_at < $1";
const BEGIN: &str = "INSERT INTO sso_logins (state_hash, nonce, verifier, return_to, created_at, expires_at) \
     VALUES ($1, $2, $3, $4, $5, $6)";
const TAKE: &str = "DELETE FROM sso_logins WHERE state_hash = $1 AND expires_at > $2 \
     RETURNING nonce, verifier, return_to";
const IDENTITY: &str = "SELECT user_id FROM identities WHERE provider = $1 AND subject = $2 FOR UPDATE";
const USER_BY_EMAIL: &str = "SELECT id FROM users WHERE email = $1 FOR UPDATE";
const INSERT_USER: &str = "INSERT INTO users \
     (id, email, display_name, password_hash, is_active, must_change_password, created_at) \
     VALUES ($1, $2, $3, NULL, TRUE, FALSE, $4)";
const LINK: &str = "INSERT INTO identities (id, user_id, provider, subject, email, created_at, last_login_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $6)";
const TOUCH: &str =
    "UPDATE identities SET email = $3, last_login_at = $4 WHERE provider = $1 AND subject = $2";
const USER: &str = "SELECT id, email, display_name, is_active, must_change_password, created_at \
     FROM users WHERE id = $1";
const JOIN: &str = "INSERT INTO memberships (org_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING";
const SET_ROLE: &str = "INSERT INTO role_bindings \
     (id, org_id, subject_kind, subject_id, role, scope_kind, scope_uid, created_at) \
     VALUES ($1, $2, 'user', $3, $4, 'org', NULL, $5) \
     ON CONFLICT (org_id, subject_kind, subject_id, scope_kind, (COALESCE(scope_uid, ''))) \
     DO UPDATE SET role = EXCLUDED.role";
const HAS_IDENTITY: &str = "SELECT EXISTS (SELECT 1 FROM identities WHERE user_id = $1)";

/// What a started sign-in remembers.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PendingSso {
    pub nonce: String,
    pub verifier: String,
    pub return_to: String,
}

/// The outcome of [`Store::sso_sign_in`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum SsoSignIn {
    SignedIn(User),
    /// The account is deactivated: nothing changed.
    Inactive,
}

#[derive(sqlx::FromRow)]
struct UserRow {
    id: String,
    email: String,
    display_name: Option<String>,
    is_active: bool,
    must_change_password: bool,
    created_at: i64,
}

impl Store {
    /// Remember a started sign-in until `expires_at`.
    pub async fn begin_sso(
        &self,
        state_hash: &[u8],
        pending: &PendingSso,
        expires_at: i64,
    ) -> Result<(), StoreError> {
        let now = now_ms();
        let mut tx = self.pool().begin().await?;
        sqlx::query(PURGE).bind(now).execute(&mut *tx).await?;
        sqlx::query(BEGIN)
            .bind(state_hash)
            .bind(&pending.nonce)
            .bind(&pending.verifier)
            .bind(&pending.return_to)
            .bind(now)
            .bind(expires_at)
            .execute(&mut *tx)
            .await?;
        tx.commit().await?;
        Ok(())
    }

    /// The started sign-in of `state_hash`, once, while it is valid.
    pub async fn take_sso(&self, state_hash: &[u8]) -> Result<Option<PendingSso>, StoreError> {
        let row: Option<(String, String, String)> = sqlx::query_as(TAKE)
            .bind(state_hash)
            .bind(now_ms())
            .fetch_optional(self.pool())
            .await?;
        Ok(row.map(|(nonce, verifier, return_to)| PendingSso {
            nonce,
            verifier,
            return_to,
        }))
    }

    /// Sign `person` in through `provider` into `org`.
    pub async fn sso_sign_in(
        &self,
        org: OrgId,
        provider: &str,
        person: &SsoPerson,
    ) -> Result<SsoSignIn, StoreError> {
        let now = now_ms();
        let mut tx = self.pool().begin().await?;
        let linked: Option<String> = sqlx::query_scalar(IDENTITY)
            .bind(provider)
            .bind(&person.subject)
            .fetch_optional(&mut *tx)
            .await?;
        let user_id = if let Some(id) = linked {
            sqlx::query(TOUCH)
                .bind(provider)
                .bind(&person.subject)
                .bind(&person.email)
                .bind(now)
                .execute(&mut *tx)
                .await?;
            id
        } else {
            let existing: Option<String> = sqlx::query_scalar(USER_BY_EMAIL)
                .bind(&person.email)
                .fetch_optional(&mut *tx)
                .await?;
            let id = if let Some(id) = existing {
                id
            } else {
                let id = UserId::new().to_string();
                sqlx::query(INSERT_USER)
                    .bind(&id)
                    .bind(&person.email)
                    .bind(person.name.as_deref())
                    .bind(now)
                    .execute(&mut *tx)
                    .await?;
                id
            };
            sqlx::query(LINK)
                .bind(uuid::Uuid::now_v7().to_string())
                .bind(&id)
                .bind(provider)
                .bind(&person.subject)
                .bind(&person.email)
                .bind(now)
                .execute(&mut *tx)
                .await?;
            id
        };
        let row: UserRow = sqlx::query_as(USER).bind(&user_id).fetch_one(&mut *tx).await?;
        if !row.is_active {
            return Ok(SsoSignIn::Inactive);
        }
        sqlx::query(JOIN)
            .bind(org.to_string())
            .bind(&user_id)
            .execute(&mut *tx)
            .await?;
        sqlx::query(SET_ROLE)
            .bind(uuid::Uuid::now_v7().to_string())
            .bind(org.to_string())
            .bind(&user_id)
            .bind(person.role.to_string())
            .bind(now)
            .execute(&mut *tx)
            .await?;
        tx.commit().await?;
        let decode = |e: uuid::Error| sqlx::Error::Decode(e.into());
        Ok(SsoSignIn::SignedIn(User {
            id: row.id.parse().map_err(decode)?,
            email: row.email,
            display_name: row.display_name,
            is_active: row.is_active,
            must_change_password: row.must_change_password,
            created_at: row.created_at,
        }))
    }

    /// Whether `user` is linked to an identity provider.
    pub async fn has_identity(&self, user: UserId) -> Result<bool, StoreError> {
        Ok(sqlx::query_scalar(HAS_IDENTITY)
            .bind(user.to_string())
            .fetch_one(self.pool())
            .await?)
    }
}

#[cfg(test)]
mod tests {
    use kuben_core::perm::Role;

    use super::*;
    use crate::testing::{pg_store, skip};

    const PROVIDER: &str = "https://idp.example.com";

    fn person(subject: &str, email: &str, role: Role) -> SsoPerson {
        SsoPerson {
            subject: subject.into(),
            email: email.into(),
            name: Some("Carol".into()),
            role,
        }
    }

    fn pending() -> PendingSso {
        PendingSso {
            nonce: "n".repeat(32),
            verifier: "v".repeat(43),
            return_to: "/projects".into(),
        }
    }

    #[tokio::test]
    async fn a_started_sign_in_is_taken_once_and_expires() {
        let Some(store) = pg_store().await else {
            return skip("a_started_sign_in_is_taken_once_and_expires");
        };
        let state = [7u8; 32];
        store
            .begin_sso(&state, &pending(), now_ms() + 600_000)
            .await
            .expect("begin");
        assert_eq!(store.take_sso(&state).await.expect("take"), Some(pending()));
        assert_eq!(store.take_sso(&state).await.expect("again"), None, "once");
        let expired = [8u8; 32];
        store
            .begin_sso(&expired, &pending(), now_ms() + 1)
            .await
            .expect("begin");
        tokio::time::sleep(std::time::Duration::from_millis(5)).await;
        assert_eq!(store.take_sso(&expired).await.expect("take"), None, "expired");
        assert!(
            store
                .begin_sso(&[1u8; 3], &pending(), now_ms() + 1_000)
                .await
                .is_err()
        );
    }

    #[tokio::test]
    async fn sign_ins_create_link_and_resync_the_role() {
        let Some(store) = pg_store().await else {
            return skip("sign_ins_create_link_and_resync_the_role");
        };
        let org = store.create_org("acme", "ACME").await.expect("org").id;
        let SsoSignIn::SignedIn(carol) = store
            .sso_sign_in(org, PROVIDER, &person("s-1", "carol@example.com", Role::Admin))
            .await
            .expect("sign in")
        else {
            panic!("refused");
        };
        assert!(store.has_identity(carol.id).await.expect("identity"));
        let members = store.list_members(org).await.expect("members");
        assert_eq!((members.len(), members[0].role), (1, Role::Admin));

        let SsoSignIn::SignedIn(again) = store
            .sso_sign_in(org, PROVIDER, &person("s-1", "carol@example.com", Role::Viewer))
            .await
            .expect("sign in")
        else {
            panic!("refused");
        };
        assert_eq!(again.id, carol.id, "the same subject is the same user");
        let members = store.list_members(org).await.expect("members");
        assert_eq!(
            (members.len(), members[0].role),
            (1, Role::Viewer),
            "demoted by the provider"
        );

        let local = store
            .create_user("dave@example.com", None, Some("phc"))
            .await
            .expect("user");
        assert!(!store.has_identity(local.id).await.expect("identity"));
        let SsoSignIn::SignedIn(linked) = store
            .sso_sign_in(org, PROVIDER, &person("s-2", "dave@example.com", Role::Developer))
            .await
            .expect("sign in")
        else {
            panic!("refused");
        };
        assert_eq!(linked.id, local.id, "linked by the verified email");
        assert!(store.has_identity(local.id).await.expect("identity"));
    }

    #[tokio::test]
    async fn deactivated_accounts_stay_out() {
        let Some(store) = pg_store().await else {
            return skip("deactivated_accounts_stay_out");
        };
        let org = store.create_org("acme", "ACME").await.expect("org").id;
        let user = store
            .create_user("erin@example.com", None, None)
            .await
            .expect("user");
        let mut conn = store.pool().acquire().await.expect("conn");
        sqlx::query("UPDATE users SET is_active = FALSE WHERE id = $1")
            .bind(user.id.to_string())
            .execute(&mut *conn)
            .await
            .expect("deactivate");
        let outcome = store
            .sso_sign_in(org, PROVIDER, &person("s-3", "erin@example.com", Role::Admin))
            .await
            .expect("sign in");
        assert_eq!(outcome, SsoSignIn::Inactive);
        assert!(
            store.list_members(org).await.expect("members").is_empty(),
            "nothing changed"
        );
        assert!(
            !store.has_identity(user.id).await.expect("identity"),
            "rolled back"
        );
    }
}
