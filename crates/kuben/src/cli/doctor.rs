//! `kuben doctor` — preflight checks. Each check prints OK / WARN / FAIL and
//! the command exits non-zero on any FAIL. Every error message is passed
//! through [`redact_credentials`]: URLs in errors may carry passwords.

use kuben_core::config::Config;
use kuben_platform::registry::{ClusterRegistry, redact_credentials};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Level {
    Ok,
    Warn,
    Fail,
}

struct Report {
    failed: bool,
}

impl Report {
    fn line(&mut self, level: Level, name: &str, detail: impl std::fmt::Display) {
        let tag = match level {
            Level::Ok => "OK  ",
            Level::Warn => "WARN",
            Level::Fail => {
                self.failed = true;
                "FAIL"
            }
        };
        println!("[{tag}] {name}: {}", redact_credentials(&detail.to_string()));
