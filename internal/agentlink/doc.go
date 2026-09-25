// Package agentlink is the hub's end of AgentLink (ADR-027): the port of
// crates/kuben-agent/src/hub.rs and the hub's half of enroll.rs (the
// cluster CA, bootstrap tokens, the enrollment service), and of
// crates/kuben-platform/src/agentlink.rs and local_agent.rs (the SQL
// registry, the listener `kuben serve` hosts, the materializer's dispatch
// and the local agent's enrollment). The wire types and the TLS
// configurations both ends share are in agentlink/protocol.
package agentlink
