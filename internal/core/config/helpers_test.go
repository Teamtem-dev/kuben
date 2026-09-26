package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
)

// kerrOf is the *kerrors.Error in err's chain, by value, and whether there is one.
func kerrOf(err error) (kerrors.Error, bool) {
	var e *kerrors.Error
	if errors.As(err, &e) && e != nil {
		return *e, true
	}
	return kerrors.Error{}, false
}

// jail is a configuration source that touches neither /etc nor the process
// environment: a temporary directory holding `kuben.toml` (when local is not
// empty) and `etc/config.toml` (when system is not empty), and env as the
// environment.
func jail(t *testing.T, system, local string, env map[string]string) config.Source {
	t.Helper()
	dir := t.TempDir()
	systemFile := filepath.Join(dir, "etc", "config.toml")
	write := func(path, content string) {
		if content == "" {
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(systemFile, system)
	write(filepath.Join(dir, config.LocalFile), local)
	return config.Source{
		Files:   []string{systemFile, config.LocalFile},
		Dir:     dir,
		Environ: environ(env),
	}
}

func environ(env map[string]string) func() []string {
	list := make([]string, 0, len(env))
	for name, value := range env {
		list = append(list, name+"="+value)
	}
	sort.Strings(list)
	return func() []string { return list }
}
