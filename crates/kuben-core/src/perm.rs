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
