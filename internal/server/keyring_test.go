package serve_test

import (
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	serve "github.com/Teamtem-dev/kuben/internal/server"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// serve.rs secret_keyring had no test of its own: the keyring lives in the
// state directory unless named, is created once, and a keyring that is
// not the installation's stops the server.
func TestTheKeyringIsCreatedOnceAndMustBeTheInstallations(t *testing.T) {
	st := pgtest.Store(t)
	ctx := t.Context()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.Server.StateDir = opt.Some(t.TempDir())
	first, err := serve.SecretKeyring(ctx, cfg, st, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Server.StateDir.Or(""), "secrets.keyring")); err != nil {
		t.Fatalf("the default file: %v", err)
	}
	again, err := serve.SecretKeyring(ctx, cfg, st, logger)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(first.Fingerprints(), again.Fingerprints()); diff != "" {
		t.Fatalf("a second start reads the same key: %s", diff)
	}

	other := filepath.Join(t.TempDir(), "other.keyring")
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	if err := os.WriteFile(other, []byte("1:"+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Secrets.KeyringFile = opt.Some(other)
	if _, err := serve.SecretKeyring(ctx, cfg, st, logger); err == nil ||
		!strings.Contains(err.Error(), "every replica must read the same keyring") {
		t.Fatalf("another installation's keyring: %v", err)
	}
}
