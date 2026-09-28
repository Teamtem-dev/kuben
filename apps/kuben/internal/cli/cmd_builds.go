package cli

// `kuben builds` (2.1): an app's builds from its Git source, a build's log
// (followed with -f), a build of the source head now, and stopping one.
// Like the other client commands they use the context `kuben login` wrote,
// or KUBEN_URL and KUBEN_TOKEN.

import (
	"context"
	"fmt"
	"io"
	"iter"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/apiclient"
)

// defaultBuildsLimit is how many builds `kuben builds` lists.
const defaultBuildsLimit = 20

func buildsCmd(g *globals) *cobra.Command {
	var opts buildsOpts
	cmd := &cobra.Command{
		Use:   "builds APP",
		Short: "An app's newest builds; logs, trigger and cancel act on one",
		Long: "An app's newest builds; logs, trigger and cancel act on one.\n\n" +
			"APP is the app: project/environment/app, or app with --project and --environment",
		Args: cobra.ExactArgs(1),
	}
	flags := cmd.Flags()
	flags.BoolVar(&opts.json, "json", false, "Print JSON")
	flags.Int64Var(&opts.limit, "limit", defaultBuildsLimit, "How many builds, newest first (at most 100)")
	readTarget := targetFlags(cmd)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		opts.app, opts.target = args[0], readTarget()
		return runBuilds(c.Context(), g, opts, os.LookupEnv)
	}
	cmd.AddCommand(buildLogsCmd(g), buildTriggerCmd(g), buildCancelCmd(g))
	return cmd
}

func buildLogsCmd(g *globals) *cobra.Command {
	var opts buildLogsOpts
	cmd := &cobra.Command{
		Use:   "logs APP BUILD",
		Short: "A build's log; -f follows it while the build runs",
		Args:  cobra.ExactArgs(2),
	}
	cmd.Flags().BoolVarP(&opts.follow, "follow", "f", false, "Keep printing new lines until the build's log ends")
	readTarget := targetFlags(cmd)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		opts.app, opts.build, opts.target = args[0], args[1], readTarget()
		return runBuildLogs(c.Context(), g, opts, os.LookupEnv)
	}
	return cmd
}

func buildTriggerCmd(g *globals) *cobra.Command {
	var opts buildOpts
	cmd := &cobra.Command{
		Use:   "trigger APP",
		Short: "Build the head of the app's Git branch now (a source sync)",
		Args:  cobra.ExactArgs(1),
	}
	readTarget := targetFlags(cmd)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		opts.app, opts.target = args[0], readTarget()
		return runBuildTrigger(c.Context(), g, opts, os.LookupEnv)
	}
	return cmd
}

func buildCancelCmd(g *globals) *cobra.Command {
	var opts buildOpts
	cmd := &cobra.Command{
		Use:   "cancel APP BUILD",
		Short: "Stop a build",
		Args:  cobra.ExactArgs(2),
	}
	readTarget := targetFlags(cmd)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		opts.app, opts.build, opts.target = args[0], args[1], readTarget()
		return runBuildCancel(c.Context(), g, opts, os.LookupEnv)
	}
	return cmd
}

// buildsOpts are the options of `kuben builds`.
type buildsOpts struct {
	app    string
	json   bool
	limit  int64
	target clientTarget
}

// buildLogsOpts are the options of `kuben builds logs`.
type buildLogsOpts struct {
	app    string
	build  string
	follow bool
	target clientTarget
}

// buildOpts are the options of `kuben builds trigger` and `cancel`.
type buildOpts struct {
	app    string
	build  string
	target clientTarget
}

// buildID checks that text names a build (a UUID) before it goes into a
// path.
func buildID(text string) (string, error) {
	id, err := uuid.Parse(text)
	if err != nil {
		return "", fmt.Errorf("`%s` is not a build id; `kuben builds <app>` lists them", text)
	}
	return id.String(), nil
}

func runBuilds(ctx context.Context, g *globals, opts buildsOpts, env envLookup) error {
	s, err := openClientSession(opts.target, env)
	if err != nil {
		return err
	}
	app, err := s.app(opts.app)
	if err != nil {
		return err
	}
	builds, err := s.api.Builds(ctx, app, min(max(opts.limit, 1), 100))
	if err != nil {
		return err //nolint:wrapcheck // the client's message
	}
	return printBuilds(g.stdout, builds, opts.json)
}

// printBuilds is the table of builds, or their JSON.
func printBuilds(w io.Writer, builds []apiclient.BuildDto, asJSON bool) error {
	if asJSON {
		text, err := serdePretty(emptyIfNil(builds))
		if err != nil {
			return err
		}
		out := &lineWriter{w: w}
		out.line(text)
		return out.err
	}
	table := make([][]string, 0, len(builds))
	for _, b := range builds {
		table = append(table, []string{
			b.ID, buildState(b), buildStage(b), shortCommit(b.Commit), b.Branch, buildTime(b.CreatedAt),
		})
	}
	return printClientTable(w, []string{"BUILD", "PHASE", "STAGE", "COMMIT", "BRANCH", "CREATED"}, table)
}

// buildState is the phase, with the failure or block when there is one.
func buildState(b apiclient.BuildDto) string {
	if f, ok := b.Failure.Get(); ok {
		return b.Phase + " (" + f + ")"
	}
	if r, ok := b.BlockedReason.Get(); ok {
		return b.Phase + " (" + r + ")"
	}
	return b.Phase
}

// buildStage is the stage the build is at: the running one, else the last
// one that finished or failed; `-` before any was reported.
func buildStage(b apiclient.BuildDto) string {
	stage := "-"
	for _, s := range b.Stages {
		switch s.Status {
		case "running":
			return s.Name
		case "succeeded", "failed":
			stage = s.Name
			if s.Status == "failed" {
				stage += " (failed)"
			}
		}
	}
	return stage
}

// shortCommit is the first 12 characters of a commit.
func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// buildTime is a unix-ms time in UTC, to the second.
func buildTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func runBuildLogs(ctx context.Context, g *globals, opts buildLogsOpts, env envLookup) error {
	build, err := buildID(opts.build)
	if err != nil {
		return err
	}
	s, err := openClientSession(opts.target, env)
	if err != nil {
		return err
	}
	app, err := s.app(opts.app)
	if err != nil {
		return err
	}
	if !opts.follow {
		text, err := s.api.BuildLog(ctx, app, build)
		if err != nil {
			return err //nolint:wrapcheck // the client's message
		}
		out := &lineWriter{w: g.stdout}
		out.print(text)
		return out.err
	}
	events, err := s.api.FollowBuildLog(ctx, app, build)
	if err != nil {
		return err //nolint:wrapcheck // the client's message
	}
	return printFollowedBuild(g.stdout, g.stderr, events)
}

// printFollowedBuild prints a followed build log: each line under the
// stage it belongs to, until the build's log ends. An end with an error
// names it on stderr.
func printFollowedBuild(stdout, stderr io.Writer, events iter.Seq2[apiclient.FollowEvent, error]) error {
	out, errOut := &lineWriter{w: stdout}, &lineWriter{w: stderr}
	for event, err := range events {
		if err != nil {
			return err
		}
		switch e := event.(type) {
		case apiclient.LineEvent:
			text := e.Line
			if stage, ok := e.Process.Get(); ok {
				text = "[" + stage + "] " + text
			}
			out.line(text)
			if out.err != nil {
				return out.err
			}
		case apiclient.EndEvent:
			if reason, ok := e.Error.Get(); ok {
				errOut.line(reason)
			}
			return out.err
		}
	}
	return out.err
}

func runBuildTrigger(ctx context.Context, g *globals, opts buildOpts, env envLookup) error {
	s, err := openClientSession(opts.target, env)
	if err != nil {
		return err
	}
	app, err := s.app(opts.app)
	if err != nil {
		return err
	}
	triggered, err := s.api.TriggerBuild(ctx, app)
	if err != nil {
		return err //nolint:wrapcheck // the client's message
	}
	return printTriggered(g.stdout, app, triggered)
}

// printTriggered says that the build was asked for, and where the newest
// build stands.
func printTriggered(w io.Writer, app apiclient.AppPath, t apiclient.TriggeredBuildDto) error {
	out := &lineWriter{w: w}
	out.line(fmt.Sprintf("%s: building the source head (sync %s)", app, t.SyncOperation))
	if b, ok := t.Build.Get(); ok {
		out.line(fmt.Sprintf("newest build  %s  %s  %s", b.ID, buildState(b), shortCommit(b.Commit)))
	}
	out.line(fmt.Sprintf("follow it with: kuben builds %s", app))
	return out.err
}

func runBuildCancel(ctx context.Context, g *globals, opts buildOpts, env envLookup) error {
	build, err := buildID(opts.build)
	if err != nil {
		return err
	}
	s, err := openClientSession(opts.target, env)
	if err != nil {
		return err
	}
	app, err := s.app(opts.app)
	if err != nil {
		return err
	}
	b, err := s.api.CancelBuild(ctx, app, build)
	if err != nil {
		return err //nolint:wrapcheck // the client's message
	}
	return printCancelled(g.stdout, app, b)
}

// printCancelled says whether the build stopped or is stopping.
func printCancelled(w io.Writer, app apiclient.AppPath, b apiclient.BuildDto) error {
	out := &lineWriter{w: w}
	switch b.Phase {
	case "cancelled":
		out.line(fmt.Sprintf("%s: build %s cancelled", app, b.ID))
	default:
		out.line(fmt.Sprintf("%s: build %s is stopping (%s)", app, b.ID, buildState(b)))
	}
	return out.err
}
