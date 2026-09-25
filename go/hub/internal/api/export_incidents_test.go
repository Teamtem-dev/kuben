package api

import "github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"

// CheckEndpoint exposes checkEndpoint.
func CheckEndpoint(body *gen.CreateEndpoint) ([]string, error) { return checkEndpoint(body) }
