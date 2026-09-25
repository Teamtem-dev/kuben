package setup

// This server's PostgreSQL (ADR-025) and the cluster Kuben manages.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/install/journal"
)

// ensureDatabase: the PostgreSQL this server keeps its data in, installed
// from the distribution's packages when missing, started, with the role and
// database `kuben`. The role logs in over the Unix socket by peer
// authentication as the `kuben` system user, so it has no password; it owns
// its database and is no superuser, so row-level security applies to it.
// True when the configuration of Kuben 1.x was moved aside.
func (m *machine) ensureDatabase(ctx context.Context, opts Opts, configured opt.Val[string], book *journal.Book) (bool, error) {
	switch dataHomeOf(configured) {
	case dataExternal:
		st := m.ui.Step("PostgreSQL")
		st.Done("the database in " + ConfigFile)
		return false, book.Done(false, "an external database")
	case dataSqlite:
		st := m.ui.Step("PostgreSQL")
		st.Fail("Kuben 1.x data in SQLite")
		reset := opts.Yes
		if !reset {
			yes, asked := m.ui.Confirm("Kuben 1.2+ uses PostgreSQL (SQLite is discontinued). Backup old /etc/kuben/config.toml and set up PostgreSQL?")
			reset = asked && yes
		}
		if reset {
			backup := ConfigFile + ".v1-sqlite.bak"
			if err := os.Rename(ConfigFile, backup); err != nil {
				return false, fmt.Errorf("backing up old config.toml: %w", err)
			}
			m.ui.Note("Old SQLite configuration backed up to " + backup)
			m.ui.Done("SQLite migration", "backed up old configuration, setting up PostgreSQL")
			return true, m.setupLocalPostgres(ctx, book)
		}
		return false, fmt.Errorf("%[1]s keeps the data in SQLite, as Kuben 1.x did; this version keeps it in "+
			"PostgreSQL and does not carry 1.x data over. Delete %[1]s (setup then "+
			"writes one for this server's PostgreSQL), or set [database] url to an empty "+
			"PostgreSQL of your own, then run kuben setup again", ConfigFile)
	case dataLocal:
	}
	return false, m.setupLocalPostgres(ctx, book)
}

func (m *machine) setupLocalPostgres(ctx context.Context, book *journal.Book) error {
	st := m.ui.Step("PostgreSQL")
	defer st.Close()
	installed := m.postgresInstalled(ctx)
	if !installed {
		pk, ok := packagesOf(readText("/etc/os-release").Or(""))
		if !ok {
			st.Fail("not installed")
			return fmt.Errorf("install PostgreSQL 14 or newer with its systemd service, then run kuben setup again; "+
				"or set [database] url in %s to a PostgreSQL of your own", ConfigFile)
		}
		st.Command(pk.describe())
		if err := m.installPackages(ctx, pk); err != nil {
			return err
		}
	}
	if _, err := book.Claim(journal.KindPackage, "postgresql", !installed); err != nil {
		return err
	}
	wasRunning := m.serviceIsActive(ctx, "postgresql")
	if _, err := m.run(ctx, "systemctl", "enable", "--now", "--quiet", "postgresql"); err != nil {
		return err
	}
	if err := m.waitForPostgres(ctx, time.Minute); err != nil {
		return err
	}
	role, err := m.createUnlessPresent(ctx, "SELECT 1 FROM pg_roles WHERE rolname = 'kuben'", "CREATE ROLE kuben LOGIN")
	if err != nil {
		return err
	}
	if _, err := book.Claim(journal.KindPostgresRole, User, role); err != nil {
		return err
	}
	database, err := m.createUnlessPresent(ctx, "SELECT 1 FROM pg_database WHERE datname = 'kuben'", "CREATE DATABASE kuben OWNER kuben")
	if err != nil {
		return err
	}
	if _, err := book.Claim(journal.KindPostgresDatabase, User, database); err != nil {
		return err
	}
	version, err := m.psqlAsPostgres(ctx, "SHOW server_version")
	if err != nil {
		return err
	}
	detail := fmt.Sprintf("PostgreSQL %s, role and database kuben", strings.TrimSpace(version))
	st.Done(detail)
	return book.Done(!installed || !wasRunning || role || database, detail)
}

// createUnlessPresent runs create when query does not answer 1; true when
// it did.
func (m *machine) createUnlessPresent(ctx context.Context, query, create string) (bool, error) {
	out, err := m.psqlAsPostgres(ctx, query)
	if err != nil {
		return false, err
	}
	missing := strings.TrimSpace(out) != "1"
	if missing {
		if _, err := m.psqlAsPostgres(ctx, create); err != nil {
			return false, err
		}
	}
	return missing, nil
}

func (m *machine) postgresInstalled(ctx context.Context) bool {
	out, err := m.runner.output(ctx, nil, "systemctl", "cat", "postgresql.service")
	return err == nil && out.ok
}

func (m *machine) installPackages(ctx context.Context, p packages) error {
	switch p {
	case packagesApt:
		if _, err := m.run(ctx, "apt-get", "update", "-qq"); err != nil {
			return err
		}
		_, err := m.runEnv(ctx, []string{"DEBIAN_FRONTEND=noninteractive"}, "apt-get", "install", "-y", "-qq", "postgresql")
		return err
	case packagesDnf:
		if _, err := m.run(ctx, "dnf", "install", "-y", "-q", "postgresql-server"); err != nil {
			return err
		}
		if !exists("/var/lib/pgsql/data/PG_VERSION") {
			_, err := m.run(ctx, "postgresql-setup", "--initdb")
			return err
		}
		return nil
	case packagesZypper:
		_, err := m.run(ctx, "zypper", "--non-interactive", "install", "postgresql-server")
		return err
	}
	return nil
}

// waitForPostgres waits until PostgreSQL answers on its Unix socket.
func (m *machine) waitForPostgres(ctx context.Context, timeout time.Duration) error {
	started := m.clock.NowMs()
	for m.elapsed(started) < timeout {
		if exists("/run/postgresql/.s.PGSQL.5432") {
			if _, err := m.psqlAsPostgres(ctx, "SELECT 1"); err == nil {
				return nil
			}
		}
		if err := m.sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("PostgreSQL did not answer on /run/postgresql within %ds (journalctl -u postgresql has why)",
		int64(timeout/time.Second))
}

// psqlAsPostgres runs one statement as the superuser `postgres` over the
// Unix socket: its unaligned output.
func (m *machine) psqlAsPostgres(ctx context.Context, sql string) (string, error) {
	return m.run(ctx, "runuser", "-u", "postgres", "--", "psql", "--no-psqlrc", "-v", "ON_ERROR_STOP=1", "-tAqc", sql)
}

// ensureCluster is the kubeconfig Kuben will use: a copy in the state
// directory, owned by the service user (k3s writes its own for root only).
func (m *machine) ensureCluster(ctx context.Context, opts Opts, owner ids, book *journal.Book) (string, error) {
	installed := false
	var source string
	existing := m.existingKubeconfig()
	switch path, given := opts.Kubeconfig.Get(); {
	case given:
		st := m.ui.Step("Checking the cluster")
		if _, err := os.Stat(path); err != nil {
			st.Close()
			return "", fmt.Errorf("cannot read %s: %w", path, err)
		}
		st.Done("kubeconfig " + path)
		source = path
	case exists(K3sKubeconfig):
		m.ui.Done("Installing k3s", "already installed")
		// Installs from before the journal left a marker when setup made k3s.
		if _, err := book.Claim(journal.KindCluster, "k3s", exists(K3sMarker)); err != nil {
			return "", err
		}
		source = K3sKubeconfig
	case existing.IsSome():
		source = existing.Or("")
		m.ui.Done("Installing k3s", "using the cluster in "+source)
	case opts.NoK3s:
		m.ui.Fail("Finding a cluster", "none")
		return "", errors.New("no cluster found and --no-k3s given: pass --kubeconfig <file>")
	default:
		if err := m.installK3s(ctx, opts.Datastore); err != nil {
			return "", err
		}
		if _, err := book.Claim(journal.KindCluster, "k3s", true); err != nil {
			return "", err
		}
		installed = true
		source = K3sKubeconfig
	}

	st := m.ui.Step("Waiting for the cluster")
	nodes, err := m.waitForNodes(ctx, source, 3*time.Minute)
	if err != nil {
		st.Close()
		return "", err
	}
	st.Done(nodes)

	kubeCopy := filepath.Join(StateDir, "kubeconfig")
	content, err := os.ReadFile(source) //nolint:gosec // the operator's kubeconfig
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", source, err)
	}
	current, err := os.ReadFile(kubeCopy) //nolint:gosec // our own copy
	copied := err != nil || !bytes.Equal(current, content)
	if copied {
		if err := os.WriteFile(kubeCopy, content, 0o600); err != nil { //nolint:gosec // setup's local file copy
			return "", err //nolint:wrapcheck // names the path
		}
	}
	if err := setMode(kubeCopy, 0o600); err != nil {
		return "", err
	}
	if err := chown(kubeCopy, owner); err != nil {
		return "", err
	}
	return kubeCopy, book.Done(installed || copied, "kubeconfig "+source)
}

// waitForNodes polls until a node reports Ready; a one-line summary.
func (m *machine) waitForNodes(ctx context.Context, kubeconfig string, timeout time.Duration) (string, error) {
	started := m.clock.NowMs()
	last := "no answer from the API server yet"
	for m.elapsed(started) < timeout {
		ready, total, version, err := m.nodes(ctx, kubeconfig)
		switch {
		case err != nil:
			last = err.Error()
		case ready > 0:
			summary := fmt.Sprintf("%d/%d nodes ready", ready, total)
			if total == 1 {
				summary = "1 node"
			}
			return summary + ", Kubernetes " + version, nil
		default:
			last = "the node is not Ready yet"
		}
		if err := m.sleep(ctx, 2*time.Second); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("the cluster did not become ready within %ds: %s", int64(timeout/time.Second), last)
}

// nodes counts the Ready nodes of the cluster, and the kubelet version of
// the first.
func (m *machine) nodes(ctx context.Context, kubeconfig string) (int, int, string, error) {
	c, err := m.connect(kubeconfig)
	if err != nil {
		return 0, 0, "", err
	}
	items, err := c.list(ctx, "v1", "Node", "", "")
	if err != nil {
		return 0, 0, "", err
	}
	ready := 0
	for _, n := range items {
		if conditionTrue(n.Object, "Ready", "status", "conditions") {
			ready++
		}
	}
	version := ""
	if len(items) > 0 {
		version = stringAt(items[0].Object, "status", "nodeInfo", "kubeletVersion")
	}
	return ready, len(items), version, nil
}
