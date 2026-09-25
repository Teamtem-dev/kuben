package agentlink

// The agent inside Kuben's own cluster (M2.8, local_agent.rs; plan §11:
// the agent is a separate process from the first real install).
//
//   - [SyncCA]: in a pod, the AgentLink CA lives in a Secret, so a new pod
//     keeps the CA its agents pin (ADR-030: never in the database).
//   - [RunLocalAgent]: the hub publishes the local agent's enrollment in a
//     Secret the agent's pod mounts: its own address, the CA, the
//     installation's `primary` cluster and, while that cluster has no agent
//     with a valid certificate, a short-lived bootstrap token, renewed
//     before it expires. Once the agent enrolled, the token is taken out
//     again.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

const (
	// EnrollmentSecret is the Secret the local agent's pod mounts.
	EnrollmentSecret = "kuben-agent-enrollment" //nolint:gosec // G101: a Secret name, not a credential
	// CASecret keeps the AgentLink CA of a hub that runs in a pod.
	CASecret          = "kuben-agentlink-ca" //nolint:gosec // G101: a Secret name, not a credential
	expiresAnnotation = "kuben.dev/token-expires-at"
	localManager      = "kuben-hub"
	localTokenTTL     = time.Hour
	// renewBefore: a token closer than this to its expiry is replaced.
	renewBefore   = 10 * time.Minute
	localInterval = 30 * time.Second
	managedBy     = "app.kubernetes.io/managed-by"
)

// LocalNamespace is the namespace of the local agent and its Secrets.
func LocalNamespace(cfg config.AgentCfg, kubeNamespace opt.Val[string]) string {
	if ns := cfg.Namespace.Or(""); ns != "" {
		return ns
	}
	return registry.OwnNamespace(kubeNamespace).Or("kuben-system")
}

// SyncCA keeps the AgentLink CA of stateDir and the Secret [CASecret] the
// same: the Secret wins when it exists; a CA made here is stored in it.
func SyncCA(ctx context.Context, client kubernetes.Interface, namespace, stateDir string, logger *slog.Logger) error {
	secrets := client.CoreV1().Secrets(namespace)
	dir := Directory(stateDir)
	restore := func(s *corev1.Secret) (bool, error) {
		cert, hasCert := s.Data[CACertificate]
		key, hasKey := s.Data[CAKey]
		if !hasCert || !hasKey {
			return false, nil
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, fmt.Errorf("%s: %w", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, CACertificate), cert, 0o644); err != nil { //nolint:gosec // the certificate agents pin is public
			return false, fmt.Errorf("%s: %w", CACertificate, err)
		}
		return true, writeOwnerOnly(filepath.Join(dir, CAKey), key)
	}
	live, err := secrets.Get(ctx, CASecret, metav1.GetOptions{})
	switch {
	case err == nil:
		if restored, err := restore(live); err != nil || restored {
			return err
		}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("read %s: %w", CASecret, err)
	}
	ca, err := ClusterCAIn(dir, logger)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CASecret, Labels: map[string]string{managedBy: "kuben"}},
		Data: map[string][]byte{
			CACertificate: []byte(ca.CertificatePEM()),
			CAKey:         []byte(ca.KeyPEM()),
		},
		Type: corev1.SecretTypeOpaque,
	}
	_, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
	switch {
	case err == nil:
		logger.Info("AgentLink CA stored", "namespace", namespace, "secret", CASecret)
		return nil
	case apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err):
		// Another replica stored its CA first: use that one.
		live, err := secrets.Get(ctx, CASecret, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read %s: %w", CASecret, err)
		}
		restored, err := restore(live)
		if err == nil && !restored {
			err = fmt.Errorf("%s holds no CA", CASecret)
		}
		return err
	}
	return fmt.Errorf("store %s: %w", CASecret, err)
}

// publishedToken is a bootstrap token and when it expires (unix ms).
type publishedToken struct {
	Token     string
	ExpiresAt int64
}

// published is what the enrollment Secret should hold.
type published struct {
	Hub, CA, Cluster string
	// Token is there while the agent is not enrolled.
	Token opt.Val[publishedToken]
}

// tokenToPublish is the token to publish: the live one while it is fresh,
// a new one when the agent still needs one, none once it enrolled. issue
// makes a token.
func tokenToPublish(enrolled bool, live opt.Val[publishedToken], now int64,
	issue func() (publishedToken, error),
) (opt.Val[publishedToken], error) {
	if enrolled {
		return opt.None[publishedToken](), nil
	}
	if t, ok := live.Get(); ok && t.ExpiresAt-renewBefore.Milliseconds() > now {
		return live, nil
	}
	t, err := issue()
	if err != nil {
		return opt.None[publishedToken](), err
	}
	return opt.Some(t), nil
}

// secretBody is the enrollment Secret as the hub applies it.
func secretBody(p published) *applycorev1.SecretApplyConfiguration {
	data := map[string][]byte{
		"hub":        []byte(p.Hub),
		"hub-ca.crt": []byte(p.CA),
		"cluster":    []byte(p.Cluster),
	}
	annotations := map[string]string{}
	if t, ok := p.Token.Get(); ok {
		data["token"] = []byte(t.Token)
		annotations[expiresAnnotation] = strconv.FormatInt(t.ExpiresAt, 10)
	}
	return applycorev1.Secret(EnrollmentSecret, "").
		WithLabels(map[string]string{managedBy: "kuben"}).
		WithAnnotations(annotations).
		WithType(corev1.SecretTypeOpaque).
		WithData(data)
}

// livePublished is what a live enrollment Secret holds.
func livePublished(s *corev1.Secret) (published, bool) {
	text := func(key string) (string, bool) {
		b, ok := s.Data[key]
		return string(b), ok && utf8.Valid(b)
	}
	hub, hasHub := text("hub")
	ca, hasCA := text("hub-ca.crt")
	cluster, hasCluster := text("cluster")
	if !hasHub || !hasCA || !hasCluster {
		return published{}, false
	}
	p := published{Hub: hub, CA: ca, Cluster: cluster}
	if token, ok := text("token"); ok {
		if expires, err := strconv.ParseInt(s.Annotations[expiresAnnotation], 10, 64); err == nil {
			p.Token = opt.Some(publishedToken{Token: token, ExpiresAt: expires})
		}
	}
	return p, true
}

// LocalAgentDeps is what the local agent's publisher works with.
type LocalAgentDeps struct {
	Store     *store.Store
	Client    kubernetes.Interface
	Config    config.AgentCfg
	OrgSlug   string
	StateDir  string
	Namespace string
	Clock     clock.Clock
	Logger    *slog.Logger
}

type publisher struct {
	d       LocalAgentDeps
	hub, ca string
}

// primaryCluster is the installation's organization and primary cluster.
type primaryCluster struct {
	org     ids.OrgID
	cluster ids.ClusterID
}

// ensurePrimary is the installation's organization and its primary
// cluster; false while there is no organization (the setup page makes
// one).
func (p publisher) ensurePrimary(ctx context.Context) (primaryCluster, bool, error) {
	org, found, err := p.d.Store.InstallationOrg(ctx, p.d.OrgSlug)
	if err != nil || !found {
		return primaryCluster{}, false, err //nolint:wrapcheck // a store error says what failed
	}
	t, err := p.d.Store.Tenant(ctx, org)
	if err != nil {
		return primaryCluster{}, false, err //nolint:wrapcheck // a store error says what failed
	}
	defer t.Rollback(ctx) //nolint:errcheck // after the commit it does nothing
	cluster, err := t.EnsureCluster(ctx, registry.Primary)
	if err != nil {
		return primaryCluster{}, false, err //nolint:wrapcheck // a store error says what failed
	}
	return primaryCluster{org: org, cluster: cluster}, true, t.Commit(ctx) //nolint:wrapcheck // a store error says what failed
}

func (p publisher) once(ctx context.Context) error {
	primary, found, err := p.ensurePrimary(ctx)
	if err != nil || !found {
		return err
	}
	now := p.d.Clock.NowMs()
	agent, hasAgent, err := p.d.Store.ClusterAgent(ctx, primary.cluster)
	if err != nil {
		return err //nolint:wrapcheck // a store error says what failed
	}
	enrolled := hasAgent && agent.RevokedAt.IsNone() && agent.CertificateNotAfter > now
	secrets := p.d.Client.CoreV1().Secrets(p.d.Namespace)
	live := opt.None[published]()
	switch s, err := secrets.Get(ctx, EnrollmentSecret, metav1.GetOptions{}); {
	case err == nil:
		if lp, ok := livePublished(s); ok {
			live = opt.Some(lp)
		}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("read %s: %w", EnrollmentSecret, err)
	}
	clusterText := primary.cluster.String()
	liveToken := opt.None[publishedToken]()
	if lp, ok := live.Get(); ok && lp.Cluster == clusterText {
		liveToken = lp.Token
	}
	token, err := tokenToPublish(enrolled, liveToken, now, func() (publishedToken, error) {
		return p.issue(ctx, primary, now)
	})
	if err != nil {
		return err
	}
	desired := published{Hub: p.hub, CA: p.ca, Cluster: clusterText, Token: token}
	if lp, ok := live.Get(); ok && lp == desired {
		return nil
	}
	_, err = secrets.Apply(ctx, secretBody(desired), metav1.ApplyOptions{FieldManager: localManager, Force: true})
	if err != nil {
		return fmt.Errorf("apply %s: %w", EnrollmentSecret, err)
	}
	p.d.Logger.Info("local agent enrollment published", "cluster", clusterText, "token", token.IsSome())
	return nil
}

// issue makes a bootstrap token for the primary cluster and stores its
// hash.
func (p publisher) issue(ctx context.Context, primary primaryCluster, now int64) (_ publishedToken, err error) {
	token, err := NewToken()
	if err != nil {
		return publishedToken{}, err
	}
	t, err := p.d.Store.Tenant(ctx, primary.org)
	if err != nil {
		return publishedToken{}, err //nolint:wrapcheck // a store error says what failed
	}
	defer func() { err = errors.Join(err, t.Rollback(ctx)) }()
	created, err := t.CreateAgentToken(ctx, primary.cluster, TokenHash(token), localTokenTTL, "hub:local-agent")
	switch {
	case err != nil:
		return publishedToken{}, err //nolint:wrapcheck // a store error says what failed
	case !created:
		return publishedToken{}, errors.New("the primary cluster is not the installation's")
	}
	if err := t.Commit(ctx); err != nil {
		return publishedToken{}, err //nolint:wrapcheck // a store error says what failed
	}
	return publishedToken{Token: token, ExpiresAt: clock.SaturatingAdd(now, localTokenTTL.Milliseconds())}, nil
}

// RunLocalAgent publishes the local agent's enrollment until ctx ends.
func RunLocalAgent(ctx context.Context, d LocalAgentDeps) error {
	hub := d.Config.Advertise.Or("")
	if hub == "" {
		return errors.New("agent.local needs agent.advertise, the address agents dial")
	}
	ca, err := os.ReadFile(filepath.Join(Directory(d.StateDir), CACertificate))
	if err != nil {
		return fmt.Errorf("the AgentLink CA is not there yet: %w", err)
	}
	p := publisher{d: d, hub: hub, ca: string(ca)}
	tick := time.NewTimer(0)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		if err := p.once(ctx); err != nil {
			d.Logger.Warn("cannot publish the local agent's enrollment; retrying", "error", err)
		}
		tick.Reset(localInterval)
	}
}
