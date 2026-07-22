//! First-boot bootstrap: default org + admin user (Invariant I-2: nothing is
//! ever seeded with a fixed secret; passwords are configured or generated).

use std::collections::BTreeMap;

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use k8s_openapi::{ByteString, api::core::v1::Secret};
use kube::{
    Api,
    api::{ObjectMeta, Patch, PatchParams},
};
use kuben_api::auth::password::Hasher;
use kuben_core::{config::Config, perm::Role};
use kuben_platform::registry::{ClusterRegistry, own_namespace};
