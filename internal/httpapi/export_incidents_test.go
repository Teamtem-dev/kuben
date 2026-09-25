package httpapi

import "github.com/Teamtem-dev/kuben/internal/httpapi/gen"

// CheckEndpoint exposes checkEndpoint.
func CheckEndpoint(body *gen.CreateEndpoint) ([]string, error) { return checkEndpoint(body) }
