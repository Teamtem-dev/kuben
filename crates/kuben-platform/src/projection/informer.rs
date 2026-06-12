//! Informers: one shared watch per kind, with backoff, label selectors and
//! `managedFields` stripped, feeding [`Projections`].

use std::{fmt::Debug, sync::Arc};

use futures::{StreamExt, TryStreamExt};
use k8s_openapi::api::core::v1::Pod;
use kube::{
    Api, Client, Resource, ResourceExt,
    runtime::{WatchStreamExt, watcher},
};
use kuben_crd::{App, Environment, Project, labels};
use serde::de::DeserializeOwned;
use tokio_util::sync::CancellationToken;

