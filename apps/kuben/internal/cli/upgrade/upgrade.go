// Package upgrade is `kuben upgrade-check` (crates/kuben/src/cli/upgrade.rs,
// M4.8), run with the *new* binary before it replaces the old one: it prints
// the findings of internal/maintenance/upgrade's preflight and fails when any does.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	coreupgrade "github.com/Teamtem-dev/kuben/apps/kuben/internal/core/upgrade"
	opsupgrade "github.com/Teamtem-dev/kuben/apps/kuben/internal/maintenance/upgrade"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/version"
)

// ErrNotSafe is the failure of an upgrade check with failing findings.
var ErrNotSafe = errors.New("this upgrade is not safe yet: fix the failures above, nothing has changed") //nolint:gochecknoglobals // sentinel

// Check is `kuben upgrade-check`: every finding; fails when any does.
func Check(ctx context.Context, cfg config.Config, major bool, out io.Writer) error {
	findings, err := opsupgrade.Preflight(ctx, cfg, major)
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	failed := false
	for _, finding := range findings {
		if _, err := fmt.Fprintf(out, "%s  %-11s %s\n", mark(finding.Level), finding.Check, finding.Detail); err != nil {
			return err //nolint:wrapcheck // stdout
		}
		failed = failed || finding.Level == coreupgrade.LevelFail
	}
	if failed {
		return ErrNotSafe
	}
	_, err = fmt.Fprintf(out, "ready to upgrade to %s\n", version.Version)
	return err //nolint:wrapcheck // stdout
}

func mark(l coreupgrade.Level) string {
	switch l {
	case coreupgrade.LevelOk:
		return "OK  "
	case coreupgrade.LevelWarn:
		return "WARN"
	case coreupgrade.LevelFail:
		return "FAIL"
	}
	return "????"
}
