//! Argon2id password hashing (Invariant I-4). Parameters follow the OWASP
//! recommendation (m=19 MiB, t=2, p=1) and are configurable.

use argon2::{
    Algorithm, Argon2, Params, PasswordHash, PasswordHasher, PasswordVerifier, Version,
    password_hash::{SaltString, rand_core::OsRng},
};
use kuben_core::config::SecurityCfg;

#[derive(Debug)]
pub struct Hasher {
    argon: Argon2<'static>,
    dummy: String,
}

impl Hasher {
    #[must_use]
    pub fn from_config(cfg: &SecurityCfg) -> Self {
        let params = Params::new(cfg.argon2_m_kib, cfg.argon2_t, cfg.argon2_p, None)
            .unwrap_or_else(|_| Params::default());
        Self::with_params(params)
    }

    #[must_use]
    pub fn with_params(params: Params) -> Self {
