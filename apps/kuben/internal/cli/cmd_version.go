package cli

// `kuben version` (VersionOpts) and the hidden `kuben copy-self`.

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/bundlelock"
)

func versionCmd(g *globals) *cobra.Command {
	var withBundle, asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information; `--bundle` adds what a release installs besides Kuben, with its digests",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if asJSON && !withBundle {
				return errors.New("the argument '--json' requires '--bundle'")
			}
			return printVersion(g.stdout, withBundle, asJSON)
		},
	}
	cmd.Flags().BoolVar(&withBundle, "bundle", false, "Also the pinned k3s, Gateway API, cert-manager and PostgreSQL")
	cmd.Flags().BoolVar(&asJSON, "json", false, "With --bundle: the lock file itself (JSON)")
	return cmd
}

func printVersion(w io.Writer, withBundle, asJSON bool) error {
	if asJSON {
		_, err := io.WriteString(w, bundlelock.Lock)
		return err //nolint:wrapcheck // stdout
	}
	if _, err := fmt.Fprintln(w, versionString()); err != nil || !withBundle {
		return err //nolint:wrapcheck // stdout
	}
	b, err := bundlelock.Get()
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	_, err = fmt.Fprintln(w, b.Summary())
	return err //nolint:wrapcheck // stdout
}

func copySelfCmd(*globals) *cobra.Command {
	return &cobra.Command{
		Use:    "copy-self TO",
		Short:  "Copy this binary to a path (the chart's backup job runs it next to PostgreSQL's client tools)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return copySelf(args[0])
		},
	}
}

// copySelf copies the running executable to `to`, made executable.
func copySelf(to string) error {
	me, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find this binary: %w", err)
	}
	src, err := os.Open(me) //nolint:gosec // our own executable
	if err != nil {
		return fmt.Errorf("open %s: %w", me, err)
	}
	defer src.Close()                                                      //nolint:errcheck // read only
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // an executable, by design
	if err != nil {
		return fmt.Errorf("create %s: %w", to, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		return errors.Join(fmt.Errorf("copy to %s: %w", to, err), dst.Close())
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("write %s: %w", to, err)
	}
	return os.Chmod(to, 0o755) //nolint:wrapcheck,gosec // names the path; an executable
}
