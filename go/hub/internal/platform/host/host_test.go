package host_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/host"
)

func TestOwnerOnlyFileIsReplacedAndPrivate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret")
	if err := host.WriteOwnerOnly(file, "old\n"); err != nil {
		t.Fatal(err)
	}
	// A leftover with a wider mode must not keep it.
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := host.WriteOwnerOnly(file, "new\n"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new\n" {
		t.Errorf("content %q", got)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %o", mode)
	}
}

func TestConsoleURLFallsBackToThisMachine(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Bind = "0.0.0.0:3000"
	url := host.ConsoleURL(t.Context(), cfg)
	if !strings.HasPrefix(url, "http://") || !strings.HasSuffix(url, ":3000") {
		t.Errorf("url %s", url)
	}
}
