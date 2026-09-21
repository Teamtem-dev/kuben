// Package agent is the cluster agent: it receives execution envelopes from
// the hub and applies them with server-side apply, and nothing else
// (ADR-027). It is a module of its own so that it stays small: it never
// imports the hub, the store or controller-runtime.
package agent
