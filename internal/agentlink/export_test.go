package agentlink

import (
	corev1 "k8s.io/api/core/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// What the external tests of local.go reach.
type (
	PublishedToken = publishedToken
	Published      = published
)

// TokenToPublish exposes tokenToPublish.
func TokenToPublish(enrolled bool, live opt.Val[PublishedToken], now int64, issue func() (PublishedToken, error)) (opt.Val[PublishedToken], error) {
	return tokenToPublish(enrolled, live, now, issue)
}

// SecretBody exposes secretBody.
func SecretBody(p Published) *applycorev1.SecretApplyConfiguration { return secretBody(p) }

// LivePublished exposes livePublished.
func LivePublished(s *corev1.Secret) (Published, bool) { return livePublished(s) }
