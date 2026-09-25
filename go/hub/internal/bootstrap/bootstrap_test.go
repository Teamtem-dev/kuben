package bootstrap_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/auth"
	"github.com/Teamtem-dev/kuben/go/hub/internal/bootstrap"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBootstrapRunsOnceEvenWhenReplicasRace(t *testing.T) {
	st := pgtest.Store(t)
	hasher := auth.InsecureForTests()
	cfg := config.Default()
	generated := make([]opt.Val[string], 2)
	var g errgroup.Group
	for i := range generated {
		g.Go(func() error {
			p, err := bootstrap.EnsureAdmin(t.Context(), cfg, st, hasher, quiet())
			generated[i] = p
			return err
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if n := boolCount(generated[0].IsSome(), generated[1].IsSome()); n != 1 {
		t.Errorf("exactly one replica generates the password: %d did", n)
	}
	if n, err := st.CountUsers(t.Context()); err != nil || n != 1 {
		t.Errorf("users %d, %v", n, err)
	}
	again, err := bootstrap.EnsureAdmin(t.Context(), cfg, st, hasher, quiet())
	if err != nil || again.IsSome() {
		t.Errorf("again: %v %v", again, err)
	}
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

func TestPasswordFileSitsInTheStateDirectory(t *testing.T) {
	cfg := config.Default()
	dir := t.TempDir()
	cfg.Server.StateDir = opt.Some(dir)
	var printed string
	bootstrap.HandOverPassword(t.Context(), cfg, opt.None[*registry.Registry](), "pw",
		bootstrap.Stderr{Write: func(s string) { printed += s }}, quiet())
	if printed != "" {
		t.Errorf("printed without a terminal: %q", printed)
	}
	if got := bootstrap.PasswordFile(cfg); got != filepath.Join(dir, bootstrap.InitialAdminFile) {
		t.Errorf("file %s", got)
	}
	if got := readFile(t, bootstrap.PasswordFile(cfg)); got != "admin@kuben.local\npw\n" {
		t.Errorf("content %q", got)
	}
}

func TestSetupBannerCarriesTheTokenOnlyOverASecurePath(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Bind = "0.0.0.0:3000"
	cfg.Server.PublicURL = opt.Some("http://203.0.113.7:3000")
	here := opt.Some("203.0.113.7")
	abc := opt.Some("abc")
	contains := func(banner, want string, yes bool) {
		t.Helper()
		if strings.Contains(banner, want) != yes {
			t.Errorf("contains %q = %v:\n%s", want, !yes, banner)
		}
	}
	tunnel := bootstrap.SetupBanner(cfg, abc, here)
	contains(tunnel, "    http://localhost:3000/setup#token=abc\n", true)
	contains(tunnel, "ssh -L 3000:127.0.0.1:3000", true)
	contains(tunnel, "203.0.113.7:3000/setup", false)
	contains(tunnel, "30 minutes", true)
	without := bootstrap.SetupBanner(cfg, opt.None[string](), here)
	contains(without, "http://localhost:3000/setup\n", true)
	contains(without, "token", false)

	cfg.Security.InsecureSetup = true
	direct := bootstrap.SetupBanner(cfg, abc, here)
	contains(direct, "http://203.0.113.7:3000/setup#token=abc", true)
	contains(direct, "ssh", false)

	cfg.Security.InsecureSetup = false
	cfg.Server.PublicURL = opt.Some("https://kuben.apps.example.com")
	https := bootstrap.SetupBanner(cfg, abc, here)
	contains(https, "    https://kuben.apps.example.com/setup#token=abc\n", true)
	contains(https, "http://localhost:3000/setup#token=abc", true) // the tunnel until DNS is ready
}

func TestRandomPasswordsAre128Bits(t *testing.T) {
	a, err := bootstrap.RandomPassword()
	if err != nil {
		t.Fatal(err)
	}
	b, err := bootstrap.RandomPassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 22 || a == b || strings.ContainsAny(a, "+/=") {
		t.Errorf("%q %q", a, b)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
