//! Git → isolated build (M3, ADR-028).
//!
//! [`worker`] claims `source.sync` and `build` operations. A sync reads the
//! branch head through a [`SourceProvider`]; a build runs as one rootless
//! BuildKit Job ([`job`]) with a repository-scoped, short-lived fetch token
//! and no service-account token, and succeeds only when an
//! [`OutputVerifier`] finds the reported digest in the registry. The traits
//! are the failure boundary to the outside world; their HTTP
//! implementations live with the API's transport.

pub mod job;
pub mod observe;
#[cfg(test)]
mod scenarios;
pub mod steps;
pub mod worker;

use std::{fmt, time::SystemTime};

use kuben_core::{
    artifact::Digest,
    source::{BranchName, CommitSha, RepoName},
};

pub use worker::{BuildWorker, run};

/// A branch head read from the provider.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Head {
    pub commit: CommitSha,
    /// The provider's immutable repository id.
    pub repository_id: u64,
}

/// A read-only credential for exactly one repository.
#[derive(Clone, PartialEq, Eq)]
pub struct FetchToken {
    pub token: String,
    pub expires_at: SystemTime,
}

impl fmt::Debug for FetchToken {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("FetchToken")
            .field("token", &"<redacted>")
            .field("expires_at", &self.expires_at)
            .finish()
    }
}

/// Why the provider did not answer.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum ProviderError {
    /// The installation, repository or branch does not exist (any more).
    #[error("not found: {0}")]
    NotFound(String),
    /// The provider refused the App's credentials or the installation is
    /// suspended.
    #[error("refused: {0}")]
    Refused(String),
    /// A transient failure; try again later.
    #[error("unavailable: {0}")]
    Unavailable(String),
}

/// A Git hosting provider, as far as builds need it.
#[async_trait::async_trait]
pub trait SourceProvider: Send + Sync + fmt::Debug {
    /// The current head of `branch`.
    async fn head(
        &self,
        installation: u64,
        repository: &RepoName,
        branch: &BranchName,
    ) -> Result<Head, ProviderError>;

    /// A token that can only read `repository`'s contents.
    async fn fetch_token(
        &self,
        installation: u64,
        repository: &RepoName,
    ) -> Result<FetchToken, ProviderError>;

    /// Revoke a token handed out by [`SourceProvider::fetch_token`].
    async fn revoke(&self, token: &FetchToken) -> Result<(), ProviderError>;

    /// The HTTPS URL build pods clone `repository` from.
    fn clone_url(&self, repository: &RepoName) -> String;
}

/// Why an output was not accepted.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum VerifyError {
    /// The registry answered and does not hold that manifest.
    #[error("the registry has no manifest {0}")]
    Missing(String),
    /// The registry could not be asked; try again later.
    #[error("the registry is unavailable: {0}")]
    Unavailable(String),
}

/// Checks a build's reported output against the registry (ADR-028).
#[async_trait::async_trait]
pub trait OutputVerifier: Send + Sync + fmt::Debug {
    /// `Ok` when `repository@digest` exists.
    async fn verify(&self, repository: &str, digest: &Digest) -> Result<(), VerifyError>;
}
