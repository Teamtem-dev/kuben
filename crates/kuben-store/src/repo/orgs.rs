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
