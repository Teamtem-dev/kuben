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

const INSERT_USER: &str = "INSERT INTO users \
     (id, email, display_name, password_hash, is_active, must_change_password, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7)";
const SELECT_USER_COLS: &str =
    "SELECT id, email, display_name, password_hash, is_active, must_change_password, created_at FROM users";
const SELECT_USER_BY_EMAIL: &str = "SELECT id, email, display_name, password_hash, is_active, must_change_password, created_at FROM users WHERE email = $1";
const SELECT_USER_BY_ID: &str = "SELECT id, email, display_name, password_hash, is_active, must_change_password, created_at FROM users WHERE id = $1";
const COUNT_USERS: &str = "SELECT COUNT(*) FROM users";
const UPDATE_PASSWORD: &str =
    "UPDATE users SET password_hash = $2, must_change_password = FALSE WHERE id = $1";

impl Store {
    /// Create a user. `password_hash` is a PHC string produced by the API
