// Package api is Kuben's HTTP API: the server generated from the contract in
// packages/api-client/openapi.json (package gen), the handlers that
// implement it, authentication, the event stream and the embedded console.
package api

//go:generate go run ./genspec ../../../../packages/api-client/openapi.json gen/openapi.ogen.json
//go:generate go tool ogen --config ogen.yml --target gen --package gen --clean gen/openapi.ogen.json
