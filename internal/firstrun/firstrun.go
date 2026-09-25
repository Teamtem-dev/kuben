// Package firstrun is the first boot (crates/kuben/src/bootstrap.rs): the
// default organization and the admin user (Invariant I-2: nothing is ever
// seeded with a fixed secret; passwords are configured or generated), the
// hand-over of a generated password, and the setup banner.
package firstrun

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/host"
	"github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/auth"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// InitialAdminSecret is the Secret that receives a generated admin password
// when Kuben runs in a pod.
const InitialAdminSecret = "kuben-initial-admin" //nolint:gosec // not a credential; the name of the Kubernetes Secret

// InitialAdminFile receives a generated admin password when a binary runs
// without a terminal (a systemd unit), in the installation's state
// directory.
const InitialAdminFile = "initial-admin-password"

// EnsureAdmin makes sure the default organization and an admin user exist.
// It returns the generated password when it created one, so `serve` can
// hand it over exactly once.
//
// Replicas that start together against an empty PostgreSQL race here; the
// loser sees the unique constraint fail, finds the winner's rows and moves
// on.
func EnsureAdmin(ctx context.Context, cfg config.Config, st *store.Store, hasher *auth.Hasher, logger *slog.Logger) (opt.Val[string], error) {
	none := opt.None[string]()
	n, err := st.CountUsers(ctx)
	if err != nil || n > 0 {
		return none, err //nolint:wrapcheck // a store error, explained by the store
	}
	slug := cfg.Bootstrap.OrgSlug
	org, found, err := st.FindOrgBySlug(ctx, slug)
	if err != nil {
		return none, err //nolint:wrapcheck // a store error
	}
	if !found {
		created, createErr := st.CreateOrg(ctx, slug, cfg.Bootstrap.OrgName)
		if createErr != nil {
			org, found, err = st.FindOrgBySlug(ctx, slug)
			if err != nil {
				return none, err //nolint:wrapcheck // a store error
			}
			if !found {
				return none, createErr //nolint:wrapcheck // a store error
			}
		} else {
			org = created
		}
	}
	password, generated := cfg.Bootstrap.AdminPassword.Expose(), none
	if password == "" {
		p, err := RandomPassword()
		if err != nil {
			return none, err
		}
		password, generated = p, opt.Some(p)
	}
	hash, err := hasher.Hash(password)
	if err != nil {
		return none, fmt.Errorf("hash: %w", err)
	}
	user, err := st.CreateUser(ctx, cfg.Bootstrap.AdminEmail, opt.Some("Administrator"), opt.Some(hash))
	if err != nil {
		if n, countErr := st.CountUsers(ctx); countErr == nil && n > 0 {
			logger.Info("another replica bootstrapped the admin user")
			return none, nil
		}
		return none, err //nolint:wrapcheck // a store error
	}
	if err := st.AddMembership(ctx, org.ID, user.ID); err != nil {
		return none, err //nolint:wrapcheck // a store error
	}
	if err := st.BindOrgRole(ctx, org.ID, user.ID, perm.Owner); err != nil {
		return none, err //nolint:wrapcheck // a store error
	}
	logger.Info("bootstrapped admin user", "email", user.Email, "org", org.Slug)
	return generated, nil
}

// RandomPassword is 128 bits of entropy, URL-safe.
func RandomPassword() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HandOverPassword delivers a generated admin password, never through the
// logger: logs are shipped to log stores and kept there. In a pod it goes
// into the [InitialAdminSecret] Secret; a binary outside the cluster prints
// it to its own terminal or, without one, writes [InitialAdminFile].
func HandOverPassword(ctx context.Context, cfg config.Config, cluster opt.Val[*registry.Registry], password string, stderr Stderr, logger *slog.Logger) {
	email := cfg.Bootstrap.AdminEmail
	r, hasCluster := cluster.Get()
	namespace, hasNamespace := registry.OwnNamespace(cfg.Kube.Namespace).Get()
	if !hasCluster || !hasNamespace {
		handOverLocally(cfg, email, password, stderr, logger)
		return
	}
	if err := storePassword(ctx, r.Primary(), namespace, email, password); err != nil {
		logger.Error("generated an initial admin password but could not store it in a Secret; set a new one with `kuben reset-admin`",
			"error", err, "email", email)
		return
	}
	logger.Warn("generated initial admin password (stored in a Secret, not logged)",
		"email", email,
		"read_with", fmt.Sprintf("kubectl -n %s get secret %s -o jsonpath='{.data.password}' | base64 -d", namespace, InitialAdminSecret))
}

// Stderr is where the operator's terminal output goes, and whether it is a
// terminal.
type Stderr struct {
	Write      func(string)
	IsTerminal bool
}

func handOverLocally(cfg config.Config, email, password string, stderr Stderr, logger *slog.Logger) {
	if stderr.IsTerminal {
		stderr.Write(fmt.Sprintf("\n  Initial admin: %s\n  Password:      %s\n\n  Shown only now. Sign in and change it under Account.\n\n", email, password))
		logger.Warn("generated initial admin password (printed to the terminal, not logged)", "email", email)
		return
	}
	file := passwordFile(cfg)
	if err := host.WriteOwnerOnly(file, email+"\n"+password+"\n"); err != nil {
		logger.Error("generated an initial admin password but could not write it; set a new one with `kuben reset-admin`",
			"error", err, "email", email, "file", file)
		return
	}
	logger.Warn("generated initial admin password (written to the file, not logged); delete the file after signing in",
		"email", email, "file", file)
}

// passwordFile is [InitialAdminFile] in the installation's state directory.
func passwordFile(cfg config.Config) string {
	return filepath.Join(cfg.StateDir(), InitialAdminFile)
}

// storePassword server-side applies the Secret, as Rust's
// `Patch::Apply(..).force()` with Kuben's field manager.
func storePassword(ctx context.Context, c registry.Cluster, namespace, email, password string) error {
	secret := corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      InitialAdminSecret,
			Namespace: namespace,
			Labels:    map[string]string{"app.kubernetes.io/part-of": "kuben"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"email": []byte(email), "password": []byte(password)},
	}
	body, err := json.Marshal(secret)
	if err != nil {
		return fmt.Errorf("encode the Secret: %w", err)
	}
	force := true
	_, err = c.Typed.CoreV1().Secrets(namespace).Patch(ctx, InitialAdminSecret, types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: v1alpha1.FieldManager, Force: &force})
	if err != nil {
		return fmt.Errorf("apply the Secret: %s", registry.RedactCredentials(err.Error()))
	}
	return nil
}

// AnnounceSetup says, when no admin exists yet and no password is
// configured, that the account is created from the console: the setup link
// (with the token, on a public address) on the terminal; without one, only
// where the token is, since logs are kept. `kuben setup-token` prints the
// link again.
func AnnounceSetup(cfg config.Config, stderr Stderr, advertise opt.Val[string], now time.Time, logger *slog.Logger) {
	token := opt.None[string]()
	if httpapi.SetupTokenRequired(cfg) {
		t, err := httpapi.CurrentOrNewSetupToken(cfg, now)
		if err != nil {
			logger.Error("cannot write the setup token", "error", err, "file", httpapi.SetupTokenPath(cfg))
			return
		}
		token = opt.Some(t)
	}
	if stderr.IsTerminal {
		stderr.Write(SetupBanner(cfg, token, advertise) + "\n")
		return
	}
	logger.Warn("no admin account yet: finish the setup in the browser; `kuben setup-token` prints the link",
		"url", httpapi.SetupURL(cfg, opt.None[string](), advertise))
}

// SetupBanner is the text that sends the operator to the setup page.
func SetupBanner(cfg config.Config, token, advertise opt.Val[string]) string {
	url, notes := httpapi.SetupGuide(cfg, token, advertise)
	lines := []string{"", "  No admin account yet. Finish the setup in your browser:", "", "    " + url, ""}
	for _, n := range notes {
		lines = append(lines, "  "+n)
	}
	if token.IsSome() {
		lines = append(lines, "  The link is valid for 30 minutes; print a new one with `kuben setup-token`.")
	}
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}
