//! Permissions and built-in roles.

use std::{fmt, str::FromStr};

use serde::{Deserialize, Serialize};

/// Fine-grained permission. Every mutating or streaming API requires one.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum Perm {
    OrgRead,
    OrgAdmin,
    ProjectRead,
    ProjectWrite,
    EnvRead,
    EnvWrite,
    EnvDeleteProtected,
    AppRead,
    AppWrite,
    AppDeploy,
    AppLogsRead,
    AppExec,
    SecretRead,
    SecretWrite,
    ReleasePromote,
    ReleaseApprove,
    AuditRead,
    UserAdmin,
}

/// Built-in roles. Custom roles map to a set of [`Perm`]s (future work).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Role {
    Owner,
    Admin,
    Developer,
    Viewer,
}

impl Role {
    /// Permissions granted by this role.
    #[must_use]
    pub fn perms(self) -> &'static [Perm] {
        use Perm::{
            AppDeploy, AppExec, AppLogsRead, AppRead, AppWrite, AuditRead, EnvDeleteProtected, EnvRead,
            EnvWrite, OrgAdmin, OrgRead, ProjectRead, ProjectWrite, ReleaseApprove, ReleasePromote,
            SecretRead, SecretWrite, UserAdmin,
        };
        match self {
            Self::Owner => &[
                OrgRead,
                OrgAdmin,
                ProjectRead,
                ProjectWrite,
                EnvRead,
                EnvWrite,
                EnvDeleteProtected,
                AppRead,
                AppWrite,
                AppDeploy,
                AppLogsRead,
                AppExec,
                SecretRead,
                SecretWrite,
                ReleasePromote,
                ReleaseApprove,
                AuditRead,
                UserAdmin,
            ],
            Self::Admin => &[
                OrgRead,
                ProjectRead,
                ProjectWrite,
                EnvRead,
                EnvWrite,
                AppRead,
                AppWrite,
                AppDeploy,
                AppLogsRead,
                AppExec,
                SecretRead,
                SecretWrite,
                ReleasePromote,
                ReleaseApprove,
                AuditRead,
                UserAdmin,
            ],
            Self::Developer => &[
