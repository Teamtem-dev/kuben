//! Repositories. Each function is a thin, explicit SQL statement — no ORM
//! magic (Drizzle philosophy) — with PostgreSQL's `$n` placeholders.

mod audit;
mod catalog;
mod deployments;
mod lifecycle;
mod materialize;
mod operations;
mod orgs;
mod product;
mod releases;
mod resolve;
mod sessions;
mod throttle;
mod tokens;
mod users;

pub use audit::NewAudit;
pub use catalog::{AppRecord, EnvironmentRecord, RunRecord};
pub use deployments::{Advance, PortableRelease, RUN_KIND, RunReason, RunSummary, StartDeployment, Started};
pub use lifecycle::{
    ENVIRONMENT_APPLY, ENVIRONMENT_DELETE, LIFECYCLE_KINDS, PROJECT_APPLY, PROJECT_DELETE, Subject,
    TARGET_DELETE,
};
pub use materialize::{Materialization, Materialized, RunPlan};
pub use operations::{Accepted, Claim, IdempotencyKey, NewOperation, OutboxMessage, Received};
pub use product::{EnvironmentKind, Project, Tenant, placement_id};
pub use releases::NewRelease;
pub use resolve::{Named, SqlScope};
pub use sessions::NewSession;
pub use throttle::ThrottleWindow;
pub use tokens::NewToken;
