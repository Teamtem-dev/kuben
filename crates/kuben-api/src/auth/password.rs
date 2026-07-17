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
        let argon = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
        let salt = SaltString::generate(&mut OsRng);
        let dummy = argon
            .hash_password(b"kuben-dummy-password-for-constant-time", &salt)
            .map(|h| h.to_string())
            .unwrap_or_default();
        Self { argon, dummy }
    }

    /// Fast parameters for tests.
    #[must_use]
    pub fn insecure_for_tests() -> Self {
        Self::with_params(Params::new(8, 1, 1, None).unwrap_or_else(|_| Params::default()))
    }

    /// Hash a password into a PHC string.
    pub fn hash(&self, password: &str) -> Result<String, argon2::password_hash::Error> {
        let salt = SaltString::generate(&mut OsRng);
        Ok(self.argon.hash_password(password.as_bytes(), &salt)?.to_string())
    }

    /// Verify a password against a PHC string. Any parse error is a mismatch.
    #[must_use]
    pub fn verify(&self, password: &str, phc: &str) -> bool {
        PasswordHash::new(phc)
            .is_ok_and(|parsed| self.argon.verify_password(password.as_bytes(), &parsed).is_ok())
    }

    /// A valid hash of an unknown password, used to equalize timing when the
    /// account does not exist.
    #[must_use]
    pub fn dummy_hash(&self) -> String {
        self.dummy.clone()
    }
}

#[cfg(test)]
