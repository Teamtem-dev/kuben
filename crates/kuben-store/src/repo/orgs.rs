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
