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
    /// layer (Argon2id) — the store never sees plaintext.
    pub async fn create_user(
        &self,
        email: &str,
        display_name: Option<&str>,
        password_hash: Option<&str>,
    ) -> Result<User, StoreError> {
        self.insert_user(email, display_name, password_hash, false).await
    }

    /// Create a user who must replace the (temporary) password on first login.
    pub async fn create_invited_user(
        &self,
        email: &str,
        display_name: Option<&str>,
        password_hash: Option<&str>,
    ) -> Result<User, StoreError> {
        self.insert_user(email, display_name, password_hash, true).await
    }

    async fn insert_user(
        &self,
        email: &str,
        display_name: Option<&str>,
        password_hash: Option<&str>,
        must_change_password: bool,
    ) -> Result<User, StoreError> {
        let user = User {
            id: UserId::new(),
            email: email.trim().to_ascii_lowercase(),
            display_name: display_name.map(str::to_owned),
            is_active: true,
            must_change_password,
            created_at: now_ms(),
        };
        with_writer!(self, |pool| {
            sqlx::query(INSERT_USER)
                .bind(user.id.to_string())
                .bind(&user.email)
                .bind(&user.display_name)
                .bind(password_hash)
                .bind(user.is_active)
                .bind(user.must_change_password)
                .bind(user.created_at)
                .execute(pool)
                .await?;
        });
        Ok(user)
    }

    pub async fn find_user_by_email(&self, email: &str) -> Result<Option<UserCredentials>, StoreError> {
        let email = email.trim().to_ascii_lowercase();
        let row: Option<UserRow> = with_reader!(self, |pool| {
            sqlx::query_as(SELECT_USER_BY_EMAIL)
                .bind(&email)
                .fetch_optional(pool)
