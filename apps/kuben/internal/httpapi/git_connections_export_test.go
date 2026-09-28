package httpapi

// Internals of the Git connection routes and their webhooks under test.

import (
	"net/http"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/oci"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

var (
	CheckGitToken    = checkGitToken
	TokenHint        = tokenHint
	CheckDtoOf       = checkDto
	GitProviderError = gitProviderError
)

// Where the connections' webhooks are served.
const (
	GitlabWebhookPrefix = gitlabWebhookPrefix
	GiteaWebhookPrefix  = giteaWebhookPrefix
	GithubWebhookPrefix = githubWebhookPrefix
)

// ReadNewConnection is readNewConnection's provider and base URL.
func ReadNewConnection(body *gen.CreateGitConnection, named bool) (store.GitProvider, string, error) {
	c, err := readNewConnection(body, named)
	return c.provider, c.base, err
}

// GitConnectionDtoOf is s's view of c.
func GitConnectionDtoOf(s *Server, c store.GitConnection) gen.GitConnectionDto {
	return s.gitConnectionDto(c)
}

// ReadConnectionDelivery verifies and reads a delivery to conn, whose
// webhook secret is secret; false when it answered a refusal.
func ReadConnectionDelivery(
	s *Server, w http.ResponseWriter, r *http.Request, body []byte, conn store.GitConnection, secret []byte,
) (string, opt.Val[gitprovider.Push], bool) {
	hook := hookedConnection{org: conn.Org, conn: conn, secret: secret}
	read := s.readGitlab
	switch conn.Provider {
	case store.GitProviderGitea:
		read = s.readGitea
	case store.GitProviderGitHub:
		read = s.readGithub
	case store.GitProviderGitLab:
	}
	d, ok := read(w, r, body, hook)
	return d.id, d.push, ok
}

// IntegrationTransports are how s reaches Git providers and checks
// organization registry logins.
func IntegrationTransports(s *Server) (http.RoundTripper, oci.LoginChecker) {
	return s.deps.GitTransport, s.deps.Registries
}
