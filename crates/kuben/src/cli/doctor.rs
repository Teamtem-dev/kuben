//! `kuben doctor` — preflight checks. Each check prints OK / WARN / FAIL and
//! the command exits non-zero on any FAIL. Every error message is passed
//! through [`redact_credentials`]: URLs in errors may carry passwords.

use kuben_core::config::Config;
use kuben_platform::registry::{ClusterRegistry, redact_credentials};

