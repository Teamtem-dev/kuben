package bootstrap_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/internal/agent/bootstrap"
	"github.com/Teamtem-dev/kuben/internal/agent/state"
)

// Ported from crates/kuben-agent/src/bootstrap.rs.

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTheEnrollmentIsWaitedForUntilItIsComplete(t *testing.T) {
	dir := t.TempDir()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, ok := bootstrap.ReadEnrollment(dir); ok {
		t.Fatal("nothing published yet")
	}
	write(t, filepath.Join(dir, bootstrap.HubFile), "kuben.kuben-system.svc:7443\n")
	write(t, filepath.Join(dir, bootstrap.ClusterFile), "0190-cluster")
	if _, ok := bootstrap.ReadEnrollment(dir); ok {
		t.Fatal("no CA yet")
	}
	write(t, filepath.Join(dir, bootstrap.HubCAFile), " \n")
	if _, ok := bootstrap.ReadEnrollment(dir); ok {
		t.Fatal("an empty CA is none")
	}

	type waited struct {
		e  bootstrap.Enrollment
		ok bool
	}
	got := make(chan waited, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	go func() {
		e, ok := bootstrap.WaitEnrollment(ctx, dir, 10*time.Millisecond, quiet)
		got <- waited{e, ok}
	}()
	write(t, filepath.Join(dir, bootstrap.HubCAFile), "-----BEGIN CERTIFICATE-----")
	w := <-got
	if !w.ok {
		t.Fatal("not in time")
	}
	if w.e.Hub != "kuben.kuben-system.svc:7443" || w.e.Cluster != "0190-cluster" || w.e.TokenFile != filepath.Join(dir, bootstrap.TokenFile) {
		t.Fatalf("%+v", w.e)
	}

	cancelled, stop := context.WithCancel(t.Context())
	stop()
	if _, ok := bootstrap.WaitEnrollment(cancelled, t.TempDir(), bootstrap.Poll, quiet); ok {
		t.Fatal("cancelled")
	}
}

func TestPrivateFilesReplaceWhatIsThere(t *testing.T) {
	file := filepath.Join(t.TempDir(), state.DeviceKey)
	if err := state.WritePrivate(file, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.WritePrivate(file, []byte("new")); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(file)
	if err != nil || string(content) != "new" {
		t.Fatal(string(content), err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode(), err)
	}
}
