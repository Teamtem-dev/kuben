use kuben_core::{
    ids::{OrgId, UserId},
    model::{Member, Organization, RoleBinding, ScopeKind, SubjectKind, User},
    perm::Role,
    time::now_ms,
};

use crate::{
    Store, StoreError,
    db::{with_reader, with_writer},
};

#[derive(Debug, sqlx::FromRow)]
struct OrgRow {
    id: String,
    slug: String,
    name: String,
    created_at: i64,
}

impl TryFrom<OrgRow> for Organization {
    type Error = StoreError;
    fn try_from(r: OrgRow) -> Result<Self, Self::Error> {
        Ok(Self {
            id: r
                .id
                .parse()
                .map_err(|e: uuid::Error| sqlx::Error::Decode(e.into()))?,
            slug: r.slug,
            name: r.name,
            created_at: r.created_at,
        })
    }
}

#[derive(Debug, sqlx::FromRow)]
struct BindingRow {
    org_id: String,
    subject_kind: String,
    subject_id: String,
    role: String,
    scope_kind: String,
    scope_uid: Option<String>,
}

#[derive(Debug, sqlx::FromRow)]
struct MemberRow {
    id: String,
    email: String,
    display_name: Option<String>,
    is_active: bool,
    must_change_password: bool,
    created_at: i64,
    role: String,
}

const SELECT_MEMBERS: &str = "SELECT u.id AS id, u.email AS email, u.display_name AS display_name, \
     u.is_active AS is_active, u.must_change_password AS must_change_password, u.created_at AS created_at, \
     rb.role AS role FROM role_bindings rb JOIN users u ON u.id = rb.subject_id \
     WHERE rb.org_id = $1 AND rb.subject_kind = 'user' AND rb.scope_kind = 'org' ORDER BY u.email";
const UPDATE_ORG_ROLE: &str = "UPDATE role_bindings SET role = $3 \
     WHERE org_id = $1 AND subject_kind = 'user' AND subject_id = $2 AND scope_kind = 'org'";
const DELETE_USER_BINDINGS: &str =
    "DELETE FROM role_bindings WHERE org_id = $1 AND subject_kind = 'user' AND subject_id = $2";
const DELETE_MEMBERSHIP: &str = "DELETE FROM memberships WHERE org_id = $1 AND user_id = $2";
const COUNT_OWNERS: &str = "SELECT COUNT(*) FROM role_bindings \
     WHERE org_id = $1 AND subject_kind = 'user' AND scope_kind = 'org' AND role = 'owner'";

fn parse_role(s: &str) -> Result<Role, sqlx::Error> {
    s.parse()
        .map_err(|e: kuben_core::Error| sqlx::Error::Decode(e.to_string().into()))
}

const INSERT_ORG: &str = "INSERT INTO organizations (id, slug, name, created_at) VALUES ($1, $2, $3, $4)";
const SELECT_ORG_BY_SLUG: &str = "SELECT id, slug, name, created_at FROM organizations WHERE slug = $1";
const INSERT_MEMBERSHIP: &str = "INSERT INTO memberships (org_id, user_id) VALUES ($1, $2)";
const INSERT_BINDING: &str = "INSERT INTO role_bindings (id, org_id, subject_kind, subject_id, role, scope_kind, scope_uid, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8)";
const SELECT_BINDINGS_FOR_USER: &str = "SELECT org_id, subject_kind, subject_id, role, scope_kind, scope_uid FROM role_bindings \
     WHERE subject_kind = 'user' AND subject_id = $1";

impl Store {
    pub async fn create_org(&self, slug: &str, name: &str) -> Result<Organization, StoreError> {
        let org = Organization {
            id: OrgId::new(),
            slug: slug.into(),
            name: name.into(),
            created_at: now_ms(),
        };
        with_writer!(self, |pool| {
            sqlx::query(INSERT_ORG)
                .bind(org.id.to_string())
                .bind(&org.slug)
                .bind(&org.name)
                .bind(org.created_at)
                .execute(pool)
                .await?;
        });
        Ok(org)
    }

    pub async fn find_org_by_slug(&self, slug: &str) -> Result<Option<Organization>, StoreError> {
        let row: Option<OrgRow> = with_reader!(self, |pool| {
            sqlx::query_as(SELECT_ORG_BY_SLUG)
                .bind(slug)
                .fetch_optional(pool)
                .await?
        });
        row.map(Organization::try_from).transpose()
    }

    pub async fn add_membership(&self, org: OrgId, user: UserId) -> Result<(), StoreError> {
        with_writer!(self, |pool| {
            sqlx::query(INSERT_MEMBERSHIP)
                .bind(org.to_string())
                .bind(user.to_string())
                .execute(pool)
                .await?;
        });
        Ok(())
    }

    /// Bind `role` for `user` at org scope.
    pub async fn bind_org_role(&self, org: OrgId, user: UserId, role: Role) -> Result<(), StoreError> {
        with_writer!(self, |pool| {
            sqlx::query(INSERT_BINDING)
                .bind(uuid::Uuid::now_v7().to_string())
                .bind(org.to_string())
                .bind(SubjectKind::User.as_str())
                .bind(user.to_string())
                .bind(role.to_string())
                .bind(ScopeKind::Org.as_str())
                .bind(Option::<String>::None)
                .bind(now_ms())
                .execute(pool)
                .await?;
        });
        Ok(())
    }

    pub async fn bindings_for_user(&self, user: UserId) -> Result<Vec<RoleBinding>, StoreError> {
        let rows: Vec<BindingRow> = with_reader!(self, |pool| {
            sqlx::query_as(SELECT_BINDINGS_FOR_USER)
                .bind(user.to_string())
                .fetch_all(pool)
                .await?
        });
        rows.into_iter()
            .map(|r| {
                Ok(RoleBinding {
                    org_id: r
                        .org_id
                        .parse()
                        .map_err(|e: uuid::Error| sqlx::Error::Decode(e.into()))?,
                    subject_kind: match r.subject_kind.as_str() {
                        "team" => SubjectKind::Team,
                        "token" => SubjectKind::Token,
                        _ => SubjectKind::User,
                    },
                    subject_id: r.subject_id,
                    role: r
                        .role
                        .parse()
                        .map_err(|e: kuben_core::Error| sqlx::Error::Decode(e.to_string().into()))?,
                    scope_kind: match r.scope_kind.as_str() {
                        "project" => ScopeKind::Project,
                        "environment" => ScopeKind::Environment,
                        "app" => ScopeKind::App,
                        _ => ScopeKind::Org,
                    },
                    scope_uid: r.scope_uid,
                })
            })
            .collect()
    }

    /// Members of an org with their org-level role, ordered by email.
    pub async fn list_members(&self, org: OrgId) -> Result<Vec<Member>, StoreError> {
        let rows: Vec<MemberRow> = with_reader!(self, |pool| sqlx::query_as(SELECT_MEMBERS)
            .bind(org.to_string())
            .fetch_all(pool)
            .await?);
        rows.into_iter()
            .map(|r| {
                Ok(Member {
                    role: parse_role(&r.role)?,
                    user: User {
                        id: r
                            .id
                            .parse()
                            .map_err(|e: uuid::Error| sqlx::Error::Decode(e.into()))?,
                        email: r.email,
                        display_name: r.display_name,
                        is_active: r.is_active,
                        must_change_password: r.must_change_password,
                        created_at: r.created_at,
                    },
                })
            })
            .collect()
    }

    /// Change a member's org-level role (creates the binding if missing).
    pub async fn set_org_role(&self, org: OrgId, user: UserId, role: Role) -> Result<(), StoreError> {
        let updated = with_writer!(self, |pool| sqlx::query(UPDATE_ORG_ROLE)
            .bind(org.to_string())
            .bind(user.to_string())
            .bind(role.to_string())
            .execute(pool)
            .await?
            .rows_affected());
        if updated == 0 {
            self.bind_org_role(org, user, role).await?;
        }
        Ok(())
    }

    /// Remove every binding and the membership of `user` in `org`, atomically.
    pub async fn remove_member(&self, org: OrgId, user: UserId) -> Result<(), StoreError> {
        with_writer!(self, |pool| {
            let mut tx = pool.begin().await?;
            sqlx::query(DELETE_USER_BINDINGS)
                .bind(org.to_string())
                .bind(user.to_string())
                .execute(&mut *tx)
                .await?;
            sqlx::query(DELETE_MEMBERSHIP)
                .bind(org.to_string())
                .bind(user.to_string())
                .execute(&mut *tx)
                .await?;
            tx.commit().await?;
        });
        Ok(())
    }

    pub async fn count_owners(&self, org: OrgId) -> Result<i64, StoreError> {
        let (n,): (i64,) = with_reader!(self, |pool| sqlx::query_as(COUNT_OWNERS)
            .bind(org.to_string())
            .fetch_one(pool)
            .await?);
        Ok(n)
    }
}
