// Package tools pins the versions of the developer tools the Go code is
// built and checked with, in a module of its own so that their
// dependencies never reach the hub, the agent or the API module; it is not
// a member of go.work. genspec, which prepares the API contract for ogen,
// is Kuben's own and lives here too. Run any of them with
// scripts/go-tool.sh <name>.
package tools
