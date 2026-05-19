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
