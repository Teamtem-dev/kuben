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

