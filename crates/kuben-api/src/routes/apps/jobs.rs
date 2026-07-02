//! Scheduled runs: "run now" for a scheduled process (scenario 7).

use std::time::{SystemTime, UNIX_EPOCH};

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use k8s_openapi::api::batch::v1::{CronJob, Job};
use kube::{Api, api::PostParams};
use kuben_core::{Error, perm::Perm};
use kuben_platform::controller::resources;
use serde::{Deserialize, Serialize};
