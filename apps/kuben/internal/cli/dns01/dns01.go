// Package dns01 is `kuben dns01-issuer` (M5.2), the port of
// crates/kuben/src/cli/dns01.rs: a cert-manager ClusterIssuer that answers
// ACME DNS-01 challenges through Cloudflare, for wildcard and apex
// certificates, and for hosts behind a proxy or without port 80.
//
// The API token goes into a Secret next to cert-manager; the issuer
// references it. Both are applied server-side under Kuben's field manager,
// so running the command again updates them.
package dns01

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/registry"
)

// Manager is the field manager both objects are applied under.
const Manager = "kuben"

// The ACME directories of Let's Encrypt.
const (
	LetsEncrypt        = "https://acme-v02.api.letsencrypt.org/directory"
	LetsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// TokenKey is the key of the token in its Secret.
const TokenKey = "api-token"

// Redacted stands for the token in a dry run.
const Redacted = "<redacted>"

// Options are the options of `kuben dns01-issuer` (Dns01Opts).
type Options struct {
	// Name is the ClusterIssuer to create or update.
	Name string
	// Email is the ACME account's email address.
	Email string
	// TokenFile holds a Cloudflare API token with Zone:DNS:Edit on the
	// zones.
	TokenFile string
	// Namespace is the namespace cert-manager runs in (the token Secret goes
	// there).
	Namespace string
	// Staging uses Let's Encrypt's staging server (untrusted certificates).
	Staging bool
	// DryRun prints the objects instead of applying them (the token is
	// redacted).
	DryRun bool
}

// secretName is the token Secret's name.
func (o Options) secretName() string { return o.Name + "-cloudflare" }

// Manifests is the token Secret and the ClusterIssuer of opts.
func Manifests(opts Options, token string) (secret, issuer map[string]any) {
	labels := func() map[string]any { return map[string]any{"app.kubernetes.io/managed-by": Manager} }
	secret = map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": opts.secretName(), "namespace": opts.Namespace, "labels": labels()},
		"type":       "Opaque",
		"stringData": map[string]any{TokenKey: token},
	}
	server := LetsEncrypt
	if opts.Staging {
		server = LetsEncryptStaging
	}
	issuer = map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "ClusterIssuer",
		"metadata":   map[string]any{"name": opts.Name, "labels": labels()},
		"spec": map[string]any{"acme": map[string]any{
			"email":               opts.Email,
			"server":              server,
			"privateKeySecretRef": map[string]any{"name": opts.Name + "-account"},
			"solvers": []any{map[string]any{"dns01": map[string]any{"cloudflare": map[string]any{
				"apiTokenSecretRef": map[string]any{"name": opts.secretName(), "key": TokenKey},
			}}}},
		}},
	}
	return secret, issuer
}

// Check refuses an address that is not one and reads the token.
func Check(opts Options) (string, error) {
	if !strings.Contains(opts.Email, "@") || strings.ContainsFunc(opts.Email, unicode.IsSpace) {
		return "", errors.New("--email must be an email address")
	}
	text, err := os.ReadFile(opts.TokenFile)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", opts.TokenFile, err)
	}
	token := strings.TrimSpace(string(text))
	clear(text)
	if token == "" {
		return "", fmt.Errorf("%s is empty", opts.TokenFile)
	}
	return token, nil
}

// DryRun is what `--dry-run` prints: both objects as YAML, the token
// redacted.
func DryRun(opts Options) (string, error) {
	secret, issuer := Manifests(opts, Redacted)
	s, err := yaml.Marshal(secret)
	if err != nil {
		return "", fmt.Errorf("encoding the Secret: %w", err)
	}
	i, err := yaml.Marshal(issuer)
	if err != nil {
		return "", fmt.Errorf("encoding the ClusterIssuer: %w", err)
	}
	return string(s) + "\n---\n" + string(i) + "\n", nil
}

// issuerResource is cert-manager's ClusterIssuer.
func issuerResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}
}

// Run is `kuben dns01-issuer`.
func Run(ctx context.Context, cfg config.Config, opts Options, stdout io.Writer) error {
	token, err := Check(opts)
	if err != nil {
		return err
	}
	if opts.DryRun {
		text, err := DryRun(opts)
		if err != nil {
			return err
		}
		_, err = io.WriteString(stdout, text)
		return err //nolint:wrapcheck // stdout
	}
	r, err := registry.Connect(cfg.Kube)
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	c := r.Primary()
	secret, issuer := Manifests(opts, token)
	force := true
	patch := metav1.PatchOptions{FieldManager: Manager, Force: &force}
	body, err := json.Marshal(secret)
	if err != nil {
		return fmt.Errorf("encoding the Secret: %w", err)
	}
	if _, err := c.Typed.CoreV1().Secrets(opts.Namespace).Patch(ctx, opts.secretName(), types.ApplyPatchType, body, patch); err != nil {
		return fmt.Errorf("writing the token Secret in %s: %s", opts.Namespace, registry.RedactCredentials(err.Error()))
	}
	body, err = json.Marshal(issuer)
	if err != nil {
		return fmt.Errorf("encoding the ClusterIssuer: %w", err)
	}
	if _, err := c.Dynamic.Resource(issuerResource()).Patch(ctx, opts.Name, types.ApplyPatchType, body, patch); err != nil {
		return fmt.Errorf("writing the ClusterIssuer (is cert-manager installed?): %s", registry.RedactCredentials(err.Error()))
	}
	_, err = fmt.Fprintf(stdout, "ClusterIssuer %s is set up for DNS-01 through Cloudflare. Use it with "+
		"`kubectl patch kubenconfig kuben --type merge -p '{\"spec\":{\"clusterIssuer\":\"%s\"}}'`; "+
		"`kubectl get clusterissuer %s` shows when it is ready.\n", opts.Name, opts.Name, opts.Name)
	return err //nolint:wrapcheck // stdout
}
