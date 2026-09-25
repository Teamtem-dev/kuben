// Package upgrade is upgrades (crates/kuben/src/cli/upgrade.rs, M4.8): the
// preflight `kuben upgrade-check` prints, run with the *new* binary before
// it replaces the old one, and the journaled migration every start and
// `kuben migrate` go through, with a backup first when migrations are
// pending. It imports nothing from internal/cli: the server calls it too.
package upgrade

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	coreupgrade "github.com/Teamtem-dev/kuben/go/hub/internal/core/upgrade"
	"github.com/Teamtem-dev/kuben/go/hub/internal/ops/backup"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/version"
	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

// FreeBytes is the free space of the file system holding dir (its nearest
// existing ancestor), from `df`.
func FreeBytes(ctx context.Context, dir string) opt.Val[uint64] {
	existing := filepath.Clean(dir)
	for {
		if _, err := os.Stat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return opt.None[uint64]()
		}
		existing = parent
	}
	out, err := exec.CommandContext(ctx, "df", "-Pk", existing).Output() //nolint:gosec // a fixed program on a path
	if err != nil {
		return opt.None[uint64]()
	}
	return ParseDF(string(out))
}

// ParseDF is the available KiB of the last line of `df -Pk`, in bytes.
func ParseDF(output string) opt.Val[uint64] {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return opt.None[uint64]()
	}
	kib, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil || kib > (1<<54)-1 {
		return opt.None[uint64]()
	}
	return opt.Some(kib << 10)
}

// facts is what the preflight of this binary against st sees.
func facts(ctx context.Context, cfg config.Config, st *store.Store, major bool) (coreupgrade.Facts, error) {
	schema, err := st.SchemaState(ctx)
	if err != nil {
		return coreupgrade.Facts{}, err //nolint:wrapcheck // a store error
	}
	versions, err := st.ServerVersions(ctx)
	if err != nil {
		return coreupgrade.Facts{}, err //nolint:wrapcheck // a store error
	}
	f := coreupgrade.Facts{
		Target:          version.Version,
		Major:           major,
		Schema:          schema.Applied,
		TargetSchema:    store.LatestMigration(),
		DirtySchema:     schema.Dirty,
		Now:             clock.System{}.NowMs(),
		TargetProtocols: coreupgrade.ProtocolRange{Min: protocol.SupportedVersions.Min, Max: protocol.SupportedVersions.Max},
		FreeBytes:       FreeBytes(ctx, cfg.BackupDir()),
	}
	if len(versions) > 0 {
		f.Running = opt.Some(versions[0])
	}
	if schema.Applied >= 25 {
		last, found, err := st.LastBackup(ctx)
		if err != nil {
			return coreupgrade.Facts{}, err //nolint:wrapcheck // a store error
		}
		if found {
			f.LastBackup = opt.Some(last.FinishedAt)
		}
	}
	if schema.Applied > 0 {
		if f.ActiveOperations, err = st.ActiveOperations(ctx); err != nil {
			return coreupgrade.Facts{}, err //nolint:wrapcheck // a store error
		}
		agents, err := st.AgentProtocols(ctx)
		if err != nil {
			return coreupgrade.Facts{}, err //nolint:wrapcheck // a store error
		}
		for _, a := range agents {
			f.Agents = append(f.Agents, coreupgrade.Agent{Cluster: a.Cluster, Protocol: a.Protocol})
		}
	}
	return f, nil
}

// Preflight is every finding of the preflight of this binary against the
// database of cfg, which it connects to without migrating.
func Preflight(ctx context.Context, cfg config.Config, major bool) ([]coreupgrade.Finding, error) {
	st, err := store.ConnectUnmigrated(ctx, cfg.Database)
	if err != nil {
		return nil, err //nolint:wrapcheck // explains itself
	}
	defer st.Close()
	f, err := facts(ctx, cfg, st, major)
	if err != nil {
		return nil, err
	}
	return coreupgrade.Preflight(f), nil
}

// Migrate migrates st as this version, journaled; when migrations are
// pending on a database with data, it takes a backup first where
// PostgreSQL's client tools are installed. A failed backup stops the
// migration.
func Migrate(ctx context.Context, cfg config.Config, st *store.Store, logger *slog.Logger) (store.Migrated, error) {
	state, err := st.GuardSchema(ctx)
	if err != nil {
		return store.Migrated{}, err //nolint:wrapcheck // a store error
	}
	pending := store.LatestMigration() - state.Applied
	if pending > 0 && state.Applied > 0 && cfg.Backup.BeforeUpgrade {
		if backup.ClientToolsInstalled() {
			logger.Info("taking a backup before the migrations", "pending", pending)
			if err := backup.Run(ctx, cfg, backup.Options{Scheduled: true}); err != nil {
				return store.Migrated{}, err //nolint:wrapcheck // explains itself
			}
		} else {
			logger.Warn("migrations are pending and pg_dump is not installed here: no backup is taken first "+
				"(the Helm chart takes one in its pre-upgrade hook)", "pending", pending)
		}
	}
	migrated, err := st.MigrateJournaled(ctx, version.Version)
	if err != nil {
		return store.Migrated{}, err //nolint:wrapcheck // a store error
	}
	if migrated.FromSchema != migrated.ToSchema || migrated.Resumed {
		logger.Info("database upgraded", "from", migrated.FromSchema, "to", migrated.ToSchema,
			"resumed", migrated.Resumed, "version", version.Version)
	}
	return migrated, nil
}
