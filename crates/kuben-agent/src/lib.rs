//! The Kuben cluster agent (ADR-027).
//!
//! The agent is a separate process inside every cluster. It dials the hub
//! (never the other way round) over TLS 1.3 with mutual authentication, the
//! AgentLink, and is the only holder of the cluster's credentials: the API
//! has no kubeconfig once the agent carries the execution envelopes.
//!
//! * [`protocol`]: the messages, their framing and the version and feature
//!   negotiation of the handshake;
//! * [`tls`]: the TLS configurations of both ends, with the pinned CA;
//! * [`enroll`]: how an agent gets its identity with a bootstrap token;
//! * [`link`]: the agent's end: dial, hello, heartbeats, dial again;
//! * [`hub`]: the hub's end, a stub `kuben serve` will host;
//! * [`state`]: the device key and certificate on disk, and enrolling
//!   when there is no valid certificate;
//! * [`runtime`]: the executor that carries envelopes out in the cluster;
//! * [`bootstrap`]: the agent inside Kuben's own cluster, enrolled from what
//!   the hub publishes and keeping its identity in a Secret.

pub mod bootstrap;
pub mod enroll;
pub mod hub;
pub mod link;
pub mod protocol;
pub mod runtime;
pub mod state;
pub mod tls;
