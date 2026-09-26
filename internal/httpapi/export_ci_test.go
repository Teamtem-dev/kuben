package httpapi

import (
	"net/http"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/integrations/oidc"
)

// CI trust internals under test (routes/ci.rs).
var (
	TrustPolicy = trustPolicy
	Bearer      = bearer
	CITokenName = ciTokenName
)

// CIExchangePath is where GitHub Actions exchanges its OIDC token.
const CIExchangePath = ciExchangePath

// CIExchangeOn is the exchange endpoint on verifier: tests give it a
// verifier whose issuer they play.
func (s *Server) CIExchangeOn(verifier *oidc.Github) http.Handler {
	return s.ciExchangeOn(opt.Some(verifier))
}
