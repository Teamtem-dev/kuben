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
                OrgRead,
                ProjectRead,
                EnvRead,
                AppRead,
                AppWrite,
                AppDeploy,
                AppLogsRead,
                AppExec,
                SecretRead,
            ],
            Self::Viewer => &[OrgRead, ProjectRead, EnvRead, AppRead, AppLogsRead],
        }
    }

    /// Position in the strict hierarchy `viewer < developer < admin < owner`;
    /// every role's permissions are a superset of the role below it.
    #[must_use]
    pub const fn rank(self) -> u8 {
        match self {
            Self::Viewer => 0,
            Self::Developer => 1,
            Self::Admin => 2,
            Self::Owner => 3,
        }
    }

    /// The weaker of two roles (e.g. the effective role of an API token is
    /// the weaker of its own cap and its owner's role).
    #[must_use]
    pub const fn weaker(self, other: Self) -> Self {
        if self.rank() <= other.rank() { self } else { other }
    }

    /// Whether this role grants `perm`.
    #[must_use]
    pub fn grants(self, perm: Perm) -> bool {
        self.perms().contains(&perm)
    }
}

impl fmt::Display for Role {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let s = match self {
            Self::Owner => "owner",
            Self::Admin => "admin",
            Self::Developer => "developer",
            Self::Viewer => "viewer",
        };
        f.write_str(s)
    }
}

impl FromStr for Role {
    type Err = crate::Error;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "owner" => Ok(Self::Owner),
            "admin" => Ok(Self::Admin),
            "developer" => Ok(Self::Developer),
            "viewer" => Ok(Self::Viewer),
            other => Err(crate::Error::Validation(format!("unknown role `{other}`"))),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn owner_is_superset_of_every_role() {
        for role in [Role::Admin, Role::Developer, Role::Viewer] {
            for perm in role.perms() {
                assert!(
                    Role::Owner.grants(*perm),
                    "owner must grant {perm:?} (from {role})"
                );
            }
        }
    }

    #[test]
    fn roles_form_a_strict_hierarchy() {
        let ordered = [Role::Viewer, Role::Developer, Role::Admin, Role::Owner];
        for pair in ordered.windows(2) {
            let (lower, higher) = (pair[0], pair[1]);
            assert!(lower.rank() < higher.rank());
            for perm in lower.perms() {
                assert!(
                    higher.grants(*perm),
                    "{higher} must grant {perm:?} (from {lower})"
                );
            }
        }
        assert_eq!(Role::Owner.weaker(Role::Developer), Role::Developer);
        assert_eq!(Role::Viewer.weaker(Role::Admin), Role::Viewer);
    }

    #[test]
    fn viewer_cannot_exec_or_write() {
        assert!(!Role::Viewer.grants(Perm::AppExec));
        assert!(!Role::Viewer.grants(Perm::AppWrite));
        assert!(Role::Viewer.grants(Perm::AppLogsRead));
    }

    #[test]
    fn role_parses_from_str() {
        assert_eq!("developer".parse::<Role>().expect("parse"), Role::Developer);
        assert!("root".parse::<Role>().is_err());
    }
}
