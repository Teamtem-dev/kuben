//! Process composition: runtime, shared state, supervised subsystems, ordered
//! shutdown (Invariant I-15).

use std::{future::Future, net::SocketAddr, sync::Arc, time::Duration};

use anyhow::Context as _;

use kuben_core::{
    config::{Config, Role, RuntimeCfg},
    traits::StaticPolicy,
};
use kuben_platform::{
    health::Health,
    leader::{self, Election},
    projection::Projections,
    registry::{ClusterRegistry, own_namespace},
    supervise::supervise,
};
