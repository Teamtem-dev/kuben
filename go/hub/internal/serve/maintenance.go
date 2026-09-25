package serve

// The housekeeping serve.rs starts once the database is ready: retention
// budgets, the backup watch and the install journal.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/install/journal"
	"github.com/Teamtem-dev/kuben/go/hub/internal/ops/backup"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// BackupsSubsystem is the health entry of the backup watch.
const BackupsSubsystem = "backups"

// backupStaleKey dedupes the incident of stale backups.
const backupStaleKey = "backup:stale"

// startMaintenance records the install journal and starts the retention
// budgets and the backup watch (database_ready). The channels close when
// each loop has ended.
func startMaintenance(ctx context.Context, cfg config.Config, st *store.Store, h *health.Health, logger *slog.Logger) []<-chan struct{} {
	recordInstallJournal(ctx, cfg, st, logger)
	budgets := make(chan struct{})
	go func() { //nolint:forbidigo // owned: ends with ctx
		defer close(budgets)
		keepBudgets(ctx, cfg, st, logger)
	}()
	backups := make(chan struct{})
	go func() { //nolint:forbidigo // owned: ends with ctx
		defer close(backups)
		watchBackups(ctx, cfg, st, h, logger)
	}()
	return []<-chan struct{}{budgets, backups}
}

// keepBudgets removes rows older than their retention budget (M4.12),
// hourly, and more often while a pass still finds a full batch.
func keepBudgets(ctx context.Context, cfg config.Config, st *store.Store, logger *slog.Logger) {
	wait := time.Minute
	for {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		done, err := st.ApplyRetentionNow(ctx, cfg.Retention)
		switch {
		case err != nil:
			logger.Warn("retention pass failed", "error", err)
			wait = 10 * time.Minute
		case done.More:
			wait = 5 * time.Second
		default:
			wait = time.Hour
		}
		if err == nil && done.Total() > 0 {
			logger.Info("old rows removed", "done", done)
		}
	}
}

// watchBackups reports `backups` degraded while the newest good backup is
// older than `backup.max_age_hours` (M4.7), the alert an operator acts on,
// and keeps an incident open for it.
func watchBackups(ctx context.Context, cfg config.Config, st *store.Store, h *health.Health, logger *slog.Logger) {
	if cfg.Backup.MaxAgeHours == 0 {
		return
	}
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for first := true; ; first = false {
		// Like tokio's interval, the first tick is immediate.
		if !first {
			select {
			case <-tick.C:
			case <-ctx.Done():
				return
			}
		}
		problem := backupProblem(ctx, cfg, st)
		if p, stale := problem.Get(); stale {
			h.Degrade(BackupsSubsystem, p)
		} else {
			h.OK(BackupsSubsystem)
		}
		if err := backupIncident(ctx, cfg, st, problem); err != nil {
			logger.Warn("the backup incident was not updated", "error", err)
		}
	}
}

// backupProblem is why the backups are not fresh, if they are not.
func backupProblem(ctx context.Context, cfg config.Config, st *store.Store) opt.Val[string] {
	last, found, err := st.LastBackup(ctx)
	if err != nil {
		return opt.Some("cannot read the backups: " + err.Error())
	}
	finished := opt.None[int64]()
	if found {
		finished = opt.Some(last.FinishedAt)
	}
	if _, err := backup.Freshness(finished, clock.System{}.NowMs(), cfg.Backup.MaxAgeHours); err != nil {
		return opt.Some(err.Error())
	}
	return opt.None[string]()
}

// backupIncident opens an incident of the installation's organization
// while backups are stale, and resolves it once they are not.
func backupIncident(ctx context.Context, cfg config.Config, st *store.Store, problem opt.Val[string]) error {
	org, found, err := st.InstallationOrg(ctx, cfg.Bootstrap.OrgSlug)
	if err != nil || !found {
		return err //nolint:wrapcheck // a store error, logged by the caller
	}
	tenant, err := st.Tenant(ctx, org)
	if err != nil {
		return err //nolint:wrapcheck // a store error
	}
	defer tenant.Rollback(ctx) //nolint:errcheck // after a commit it does nothing
	if detail, stale := problem.Get(); stale {
		_, _, err = tenant.OpenIncident(ctx, store.NewIncident{
			Kind:      "backup.stale",
			Severity:  "critical",
			DedupeKey: backupStaleKey,
			Title:     "Database backups are stale",
			Detail:    opt.Some(detail),
		})
	} else {
		_, _, err = tenant.ResolveIncidentKey(ctx, backupStaleKey, "system:backups")
	}
	if err != nil {
		return err //nolint:wrapcheck // a store error
	}
	return tenant.Commit(ctx) //nolint:wrapcheck // a store error
}

// recordInstallJournal copies the installer's journal into SQL (M2.6),
// when `kuben setup` left one on this host. Best effort: the host file
// stays the installer's record.
func recordInstallJournal(ctx context.Context, cfg config.Config, st *store.Store, logger *slog.Logger) {
	j, ok := journal.Peek(filepath.Join(cfg.StateDir(), journal.File))
	if !ok {
		return
	}
	host := hostName()
	recorded, err := st.RecordInstallJournal(ctx, host, j)
	switch {
	case err != nil:
		logger.Warn("cannot record the install journal", "error", err)
	case recorded:
		logger.Info("install journal recorded", "host", host, "runs", len(j.Runs))
	}
}

// hostName is the kernel's host name, else $HOSTNAME, else `kuben`.
func hostName() string {
	if b, err := os.ReadFile("/proc/sys/kernel/hostname"); err == nil {
		if h := strings.TrimSpace(string(b)); h != "" {
			return h
		}
	}
	if h := os.Getenv("HOSTNAME"); h != "" {
		return h
	}
	return "kuben"
}

// warnActivator says that `--roles=activator` does nothing: release builds
// carried no activator (the Rust `activator` feature was off), so the role
// is ignored with the same warning.
func warnActivator(cfg config.Config, logger *slog.Logger) {
	for _, r := range cfg.Server.Roles {
		if r == config.RoleActivator {
			logger.Warn("the activator role is not part of this build (scale-to-zero lands in phase 2); ignoring it")
			return
		}
	}
}
