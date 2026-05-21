//! API tokens (scenario 3). Only `sha256(secret)` is stored; the token id is
//! public and used for the lookup, the secret is compared in constant time by
//! the API layer.

use kuben_core::{
    ids::{OrgId, TokenId, UserId},
    model::{ApiToken, TokenScope},
    time::now_ms,
};

use crate::{
    Store, StoreError,
    db::{with_reader, with_writer},
};

/// Input for a new token.
#[derive(Debug)]
pub struct NewToken {
    pub id: TokenId,
    pub org_id: OrgId,
    pub owner: UserId,
    pub name: String,
    pub prefix: String,
    pub secret_hash: Vec<u8>,
    pub scope: TokenScope,
    pub expires_at: Option<i64>,
