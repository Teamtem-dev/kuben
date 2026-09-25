package dns

import (
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/outbound"
)

// NewPlainResolver is a resolver that also speaks plain http, for tests.
func NewPlainResolver(url string) Resolver {
	return Resolver{client: outbound.New(true, timeout), url: strings.TrimRight(url, "/")}
}
