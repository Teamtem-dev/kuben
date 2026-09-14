//! Strongly typed identifiers. All IDs are UUIDv7 (time-ordered) so they
//! index well in PostgreSQL B-trees.

use std::{fmt, str::FromStr};

use serde::{Deserialize, Serialize};
use uuid::Uuid;

macro_rules! id_type {
    ($(#[$meta:meta])* $name:ident) => {
        $(#[$meta])*
        #[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
        #[serde(transparent)]
        pub struct $name(Uuid);

        impl $name {
            /// Generate a fresh time-ordered id.
            #[must_use]
            pub fn new() -> Self {
                Self(Uuid::now_v7())
            }

            #[must_use]
            pub const fn from_uuid(id: Uuid) -> Self {
                Self(id)
            }

            #[must_use]
            pub const fn as_uuid(&self) -> &Uuid {
                &self.0
            }
        }

        impl Default for $name {
            fn default() -> Self {
                Self::new()
            }
        }

        impl fmt::Display for $name {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                self.0.fmt(f)
            }
        }

        impl FromStr for $name {
            type Err = uuid::Error;
            fn from_str(s: &str) -> Result<Self, Self::Err> {
                Uuid::parse_str(s).map(Self)
            }
        }

        impl From<$name> for Uuid {
            fn from(id: $name) -> Uuid {
                id.0
            }
        }
    };
}

id_type!(
    /// A user account.
    UserId
);
id_type!(
    /// An organization (tenant).
    OrgId
);
id_type!(
    /// An audit event.
    AuditId
);
id_type!(
    /// An API token.
    TokenId
);
id_type!(
    /// One application on one environment placement (ADR-026).
    TargetId
);
id_type!(
    /// An immutable, portable release: artifact digests plus portable config.
    ReleaseId
);
id_type!(
    /// One attempt to make a release effective on one target.
    DeploymentRunId
);
id_type!(
    /// One build attempt; an infrastructure retry is a new attempt.
    BuildAttemptId
);
id_type!(
    /// A project: owns applications and environments.
    ProjectId
);
id_type!(
    /// A logical environment such as staging or production (ADR-026).
    EnvironmentId
);
id_type!(
    /// A Kubernetes cluster registered with Kuben.
    ClusterId
);
id_type!(
    /// An environment's binding to one cluster and namespace (ADR-026).
    PlacementId
);
id_type!(
    /// An application definition in a project.
    ApplicationId
);
id_type!(
    /// A durable operation: one accepted request and its execution (plan §9.2).
    OperationId
);

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ids_roundtrip_through_strings() {
        let id = UserId::new();
        let parsed: UserId = id.to_string().parse().expect("parse");
        assert_eq!(id, parsed);
    }

    #[test]
    fn ids_are_time_ordered() {
        let a = OrgId::new();
        let b = OrgId::new();
        assert!(a <= b);
    }
}
