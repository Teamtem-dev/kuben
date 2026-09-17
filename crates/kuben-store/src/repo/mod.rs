//! Repositories. Each function is a thin, explicit SQL statement — no ORM
//! magic (Drizzle philosophy) — with PostgreSQL's `$n` placeholders.

#[cfg(test)]
mod acceptance;
mod agents;
mod audit;
mod backups;
mod builds;
mod capabilities;
mod catalog;
mod ci;
mod deployments;
mod installs;
mod lifecycle;
mod materialize;
mod operations;
mod orgs;
mod policies;
mod product;
mod releases;
mod resolve;
mod scans;
mod secrets;
mod sessions;
mod sso;
mod throttle;
mod tokens;
mod usage;
mod users;

pub use agents::{
    ClusterAgent, Delivery, RUNTIME_FEATURE, Redeemed, RuntimeObservation, TokenRedemption, TokenRefusal,
};
pub use audit::NewAudit;
pub use backups::{BackupRecord, DatabaseFacts, LastBackup, RESTORE_GENERATION_JUMP, Restored};
pub use builds::{
    BUILD_KIND, BUILD_PROCESS, Bound, BuildAdvance, BuildAttempt, BuildProgress, Completed, GITHUB,
    HeadObserved, MAX_BUILD_ATTEMPTS, NewBinding, SOURCE_SYNC_KIND, SlotLimits, SourceBinding, reject_code,
};
pub use capabilities::CapabilityRecord;
pub use catalog::{AppRecord, EnvironmentRecord, RunRecord, RuntimeStatus};
pub use ci::{CiExchange, CiPolicy, Exchanged, NewCiPolicy};
pub use deployments::{Advance, PortableRelease, RUN_KIND, RunReason, RunSummary, StartDeployment, Started};
pub use lifecycle::{
    ENVIRONMENT_APPLY, ENVIRONMENT_DELETE, LIFECYCLE_KINDS, PROJECT_APPLY, PROJECT_DELETE, Subject,
    TARGET_DELETE,
};
pub use materialize::{Materialization, Materialized, RunPlan};
pub use operations::{Accepted, Claim, IdempotencyKey, NewOperation, OutboxMessage, Received};
pub use policies::{ApprovalRecord, Decided, PolicyRevision, RunApproval};
pub use product::{EnvironmentKind, Project, Tenant, placement_id};
pub use releases::NewRelease;
pub use resolve::{Named, SqlScope};
pub use scans::{NewException, NewScan, VulnException};
pub use secrets::{
    BoundSecret, KeyringCheck, Reservation, Reserved, Revoked, SealedBytes, SealedRevision, SecretBinding,
    SecretDeleted, SecretKind, SecretRevisionInfo, SecretSummary,
};
pub use sessions::NewSession;
pub use sso::{PendingSso, SsoSignIn};
pub use throttle::ThrottleWindow;
pub use tokens::NewToken;
pub use usage::LiveConfig;
