use kuben_core::{
    ids::UserId,
    model::{User, UserCredentials},
    time::now_ms,
};

use crate::{
    Store, StoreError,
    db::{with_reader, with_writer},
};

#[derive(Debug, sqlx::FromRow)]
struct UserRow {
    id: String,
    email: String,
    display_name: Option<String>,
    password_hash: Option<String>,
    is_active: bool,
    must_change_password: bool,
    created_at: i64,
}

impl TryFrom<UserRow> for UserCredentials {
    type Error = StoreError;
    fn try_from(r: UserRow) -> Result<Self, Self::Error> {
        Ok(Self {
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
            password_hash: r.password_hash,
        })
    }
}
