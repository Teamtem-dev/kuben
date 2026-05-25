//! Login-throttle windows (scenario 1), shared by every replica. Buckets are
//! opaque keys chosen by the caller, which hashes them: no email address or
//! client IP is stored.

use crate::{
    Store, StoreError,
    db::{with_reader, with_writer},
};

/// A fixed window: `failures` counted since `started_at` (unix ms).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ThrottleWindow {
    pub failures: i64,
    pub started_at: i64,
