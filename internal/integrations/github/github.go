// Package github is the GitHub App (M3): webhook signatures, App JWTs,
// installation tokens scoped to one repository, and the branch heads a sync
// reads. It replaces crates/kuben-api/src/github.rs.
//
// Tokens are short-lived by construction (GitHub issues them for an hour)
// and narrowed further: a build's fetch token can read the contents of its
// one repository and nothing else, and is revoked when the attempt ends
// (ADR-028). The App's private key never leaves this process.
//
// Requests are built and sent by go-github over the outbound client; the
// JWT, the token cache and the reading of answers are this package's own,
// because ghinstallation's JWT claims, token cache and error texts differ
// from the Rust behaviour (go/SUBSTITUTIONS.md).
package github

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	gh "github.com/google/go-github/v92/github"
	"github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/Teamtem-dev/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/integrations/outbound"
)

const (
	// timeout bounds the wait for an answer's headers, and again for its body.
	timeout = 15 * time.Second
	// tokenMargin: a cached token is dropped this long before it expires.
	tokenMargin = 10 * time.Minute
	// tokenCacheTTL: GitHub's tokens live an hour; the cache keeps them for less.
	tokenCacheTTL  = 50 * time.Minute
	tokenCacheSize = 10_000
)

// AppError is why the App could not be set up.
//
//sumtype:decl
type AppError interface {
	error
	appError()
}

// NotConfigured says that the App's id, key file or webhook secret is not
// configured.
type NotConfigured struct{}

// KeyUnreadable says that the App's private key could not be read.
type KeyUnreadable struct {
	// Path is the configured key file.
	Path string
	// Reason is what went wrong.
	Reason string
}

func (NotConfigured) Error() string {
	return "the GitHub App is not configured (git.github_app_id, git.github_private_key_file, git.github_webhook_secret)"
}

func (e KeyUnreadable) Error() string {
	return fmt.Sprintf("cannot read the GitHub App key %s: %s", e.Path, e.Reason)
}

func (NotConfigured) appError() {}
func (KeyUnreadable) appError() {}

// CommitStatus is a commit status to report.
type CommitStatus struct {
	// State is `pending`, `success`, `failure` or `error`.
	State string
	// Context names the check, e.g. `kuben/build`.
	Context string
	// Description is cut to 140 characters.
	Description string
	// TargetURL is where the status links to, if anywhere.
	TargetURL opt.Val[string]
}

// Installation is an installation as GitHub describes it.
type Installation struct {
	// Account is the login of the user or organization it belongs to.
	Account string
	// Suspended: GitHub reports a suspension time.
	Suspended bool
}

// tokenKey names a cached metadata token: installation and `owner/name`.
type tokenKey struct {
	installation uint64
	repository   string
}

// App is the GitHub App Kuben acts as. It is safe for concurrent use.
type App struct {
	appID         uint64
	sign          signer
	webhookSecret []byte
	api           string
	cloneBase     string
	clock         clock.Clock
	client        *gh.Client
	// clientErr is why the API URL gives no client; every call fails with it.
	clientErr error
	// tokens are metadata tokens for head reads, per installation and
	// repository.
	tokens *expirable.LRU[tokenKey, build.FetchToken]
}

var _ build.SourceProvider = (*App)(nil)

// New is the App of cfg, with its key read from disk. The error is an
// [AppError].
func New(cfg config.GitCfg, clk clock.Clock) (*App, error) {
	if !cfg.GithubEnabled() {
		return nil, NotConfigured{}
	}
	appID, okID := cfg.GithubAppID.Get()
	path, okPath := cfg.GithubPrivateKeyFile.Get()
	if !okID || !okPath {
		return nil, NotConfigured{}
	}
	key, err := readKey(path)
	if err != nil {
		return nil, KeyUnreadable{Path: path, Reason: err.Error()}
	}
	return newApp(appID, rsaSigner(key), []byte(cfg.GithubWebhookSecret.Expose()), cfg, clk), nil
}

// WebhookOnly is an App that only verifies webhook deliveries signed with
// secret: every call to GitHub fails. For tests of the webhook paths.
func WebhookOnly(secret []byte) *App {
	return newApp(0, noKey, secret, config.DefaultGitCfg(), clock.System{})
}

func newApp(appID uint64, sign signer, secret []byte, cfg config.GitCfg, clk clock.Clock) *App {
	api := strings.TrimRight(cfg.GithubAPIURL, "/")
	out := outbound.New(strings.HasPrefix(cfg.GithubAPIURL, "http://"), timeout)
	client, err := newClient(out, api)
	return &App{
		appID:         appID,
		sign:          sign,
		webhookSecret: append([]byte(nil), secret...),
		api:           api,
		cloneBase:     strings.TrimRight(cfg.GithubCloneURL, "/"),
		clock:         clk,
		client:        client,
		clientErr:     err,
		tokens:        expirable.NewLRU[tokenKey, build.FetchToken](tokenCacheSize, nil, tokenCacheTTL),
	}
}

// readKey reads the RSA key in the PEM file at path.
func readKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the operator configures the key file
	if err != nil {
		if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
			return nil, pathErr.Err
		}
		return nil, fmt.Errorf("%w", err)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("stream did not contain valid UTF-8")
	}
	return keyFromPEM(string(data))
}

// String names the App and its API, and leaves its secrets out.
func (a *App) String() string {
	return fmt.Sprintf("GithubApp{AppID: %d, API: %q, ..}", a.appID, a.api)
}

// GoString is String: %#v must not print the secrets either.
func (a *App) GoString() string { return a.String() }

// Format prints String for every verb.
func (a *App) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, a.String()) //nolint:errcheck // fmt.Formatter cannot report a write error
}

// Verify reports whether body carries a valid signature from this App's
// webhook.
func (a *App) Verify(body []byte, signature string) bool {
	return VerifySignature(a.webhookSecret, body, signature)
}
