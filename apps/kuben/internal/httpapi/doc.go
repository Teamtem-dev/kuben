// Package httpapi is Kuben's HTTP API: the server generated from the
// contract in packages/api-client/openapi.json (package gen), the handlers
// that implement it, authentication, the event stream and the embedded
// console. Only internal/server imports it.
package httpapi

//go:generate cp ../../../../packages/api-client/openapi.json apidocs/openapi.json
//go:generate bash ../../../../scripts/go-tool.sh genspec ../../../../packages/api-client/openapi.json gen/openapi.ogen.json
//go:generate bash ../../../../scripts/go-tool.sh ogen --config ogen.yml --target gen --package gen --clean gen/openapi.ogen.json
