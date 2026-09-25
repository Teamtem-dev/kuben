package cli

// The client commands' work and output (cli/client.rs login, apps, deploy,
// status <app>, logs, rollback). Their output is the Rust CLI's, byte for
// byte: scripts read it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/term"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/client"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/oci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
)

// lineWriter writes lines and keeps the first write error, so a command's
// output reads like Rust's println! and the error is reported once.
type lineWriter struct {
	w   io.Writer
	err error
}

func (p *lineWriter) print(s string) {
	if p.err == nil {
		_, p.err = io.WriteString(p.w, s)
	}
}

func (p *lineWriter) line(s string) { p.print(s + "\n") }

// loginOpts are the options of `kuben login`.
type loginOpts struct {
	url    string
	token  opt.Val[string]
	name   opt.Val[string]
	target clientTarget
}

// readToken is a token from standard input, not echoed on a terminal.
func readToken(stdin io.Reader, stderr io.Writer) (string, error) {
	f, isFile := stdin.(*os.File)
	terminal := isFile && term.IsTerminal(int(f.Fd())) //nolint:gosec // a file descriptor fits an int
	errOut := &lineWriter{w: stderr}
	var line string
	if terminal {
		errOut.print("API token (Account → API tokens): ")
		typed, err := term.ReadPassword(int(f.Fd())) //nolint:gosec // a file descriptor fits an int
		errOut.line("")
		if err != nil {
			return "", fmt.Errorf("reading the token: %w", err)
		}
		line = string(typed)
	} else {
		read, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("reading the token: %w", err)
		}
		line = read
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return "", errors.New("no token given")
	}
	return token, nil
}

func runLogin(ctx context.Context, g *globals, opts loginOpts, env envLookup) error {
	url := strings.TrimRight(opts.url, "/")
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return fmt.Errorf("give the server's address with its scheme, e.g. https://%s", url)
	}
	token, ok := opts.token.Get()
	if !ok {
		read, err := readToken(g.stdin, g.stderr)
		if err != nil {
			return err
		}
		token = read
	}
	me, err := client.New(url, token).Me(ctx)
	if err != nil {
		return fmt.Errorf("signing in to %s: %w", url, err)
	}
	name := opts.name.Or(hostOf(url))
	file, err := contextsFile(env)
	if err != nil {
		return err
	}
	doc, err := loadContexts(file)
	if err != nil {
		return err
	}
	doc.Contexts[name] = serverContext{URL: url, Token: token, Project: opts.target.project, Environment: opts.target.environment}
	doc.Current = opt.Some(name)
	if err := doc.save(file); err != nil {
		return err
	}
	if h := hostOf(url); strings.HasPrefix(url, "http://") && h != "localhost" && h != "127.0.0.1" && h != "::1" {
		errOut := &lineWriter{w: g.stderr}
		errOut.line("warning: " + url + " is plain HTTP; the token crosses the network unencrypted")
	}
	out := &lineWriter{w: g.stdout}
	out.line(fmt.Sprintf("Logged in to %s as %s (context %s, saved in %s)", url, emailOf(me), name, file))
	return out.err
}

// emailOf is the `email` of a /me document, `?` without one.
func emailOf(me any) string {
	if doc, ok := me.(map[string]any); ok {
		if email, ok := doc["email"].(string); ok {
			return email
		}
	}
	return "?"
}

// appsOpts are the options of `kuben apps`.
type appsOpts struct {
	target clientTarget
	json   bool
}

func runApps(ctx context.Context, g *globals, opts appsOpts, env envLookup) error {
	s, err := openClientSession(opts.target, env)
	if err != nil {
		return err
	}
	onlyProject, onlyEnvironment := s.project(), s.environment()
	rows := []client.AppDto{}
	projects, err := s.api.Projects(ctx)
	if err != nil {
		return err //nolint:wrapcheck // the client's message
	}
	for _, project := range projects {
		if p, ok := onlyProject.Get(); ok && p != project.Name {
			continue
		}
		environments, err := s.api.Environments(ctx, project.Name)
		if err != nil {
			return err //nolint:wrapcheck // the client's message
		}
		for _, environment := range environments {
			if e, ok := onlyEnvironment.Get(); ok && e != environment.Name {
				continue
			}
			apps, err := s.api.Apps(ctx, project.Name, environment.Name)
			if err != nil {
				return err //nolint:wrapcheck // the client's message
			}
			rows = append(rows, apps...)
		}
	}
	return printApps(g.stdout, rows, opts.json)
}

// printApps is the table of apps, or their JSON.
func printApps(w io.Writer, rows []client.AppDto, asJSON bool) error {
	if asJSON {
		text, err := serdePretty(rows)
		if err != nil {
			return err
		}
		out := &lineWriter{w: w}
		out.line(text)
		return out.err
	}
	table := make([][]string, 0, len(rows))
	for _, a := range rows {
		state := "ready"
		if !a.Ready {
			state = a.Reason.Or("not ready")
		}
		table = append(table, []string{
			a.Project + "/" + a.Environment + "/" + a.Name,
			state,
			a.Image.Or(""),
			a.URL.Or(""),
			strconv.Itoa(len(a.Processes)),
		})
	}
	return printClientTable(w, []string{"APP", "STATE", "IMAGE", "URL", "PROCESSES"}, table)
}

// serdePretty is v as serde_json::to_string_pretty wrote it (fields in
// declaration order, two-space indent, no HTML escaping), without a final
// newline.
func serdePretty(v any) (string, error) {
	var text bytes.Buffer
	enc := json.NewEncoder(&text)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("writing JSON: %w", err)
	}
	return strings.TrimSuffix(text.String(), "\n"), nil
}

// printClientTable pads every column to its widest cell (in characters), two
// spaces apart, without trailing blanks.
func printClientTable(w io.Writer, header []string, rows [][]string) error {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i := range min(len(row), len(widths)) {
			widths[i] = max(widths[i], utf8.RuneCountInString(row[i]))
		}
	}
	out := &lineWriter{w: w}
	line := func(cells []string) {
		text := make([]string, 0, len(cells))
		for i := range min(len(cells), len(widths)) {
			pad := max(widths[i]-utf8.RuneCountInString(cells[i]), 0)
			text = append(text, cells[i]+strings.Repeat(" ", pad))
		}
		out.line(strings.TrimRightFunc(strings.Join(text, "  "), unicode.IsSpace))
	}
	line(header)
	for _, row := range rows {
		line(row)
	}
	return out.err
}

// currentRevision is the revision the app is on now (0 before its first
// run).
func currentRevision(releases []client.ReleaseDto) int64 {
	for _, r := range releases {
		if r.Current {
			return r.Revision
		}
	}
	if len(releases) > 0 {
		return releases[0].Revision
	}
	return 0
}

// previousRevision is the revision before the current one in a
// newest-first history.
func previousRevision(releases []client.ReleaseDto) opt.Val[int64] {
	current := currentRevision(releases)
	found := opt.None[int64]()
	for _, r := range releases {
		if r.Revision < current && r.Revision > found.Or(math.MinInt64) {
			found = opt.Some(r.Revision)
		}
	}
	return found
}

// deployOpts are the options of `kuben deploy`.
type deployOpts struct {
	app            string
	image          string
	idempotencyKey opt.Val[string]
	noWait         bool
	// timeout is how long to wait for the deployment, in seconds.
	timeout uint64
	target  clientTarget
}

func runDeploy(ctx context.Context, g *globals, opts deployOpts, env envLookup) error {
	s, err := openClientSession(opts.target, env)
	if err != nil {
		return err
	}
	app, err := s.app(opts.app)
	if err != nil {
		return err
	}
	image := opts.image
	if !strings.Contains(image, "@sha256:") {
		resolved, err := oci.Registry{}.ResolveAs(ctx, opts.image, opt.None[oci.Login]())
		if err != nil {
			return fmt.Errorf("resolving %s: %w", opts.image, err)
		}
		image = resolved.Pinned()
		errOut := &lineWriter{w: g.stderr}
		errOut.line(opts.image + " → " + image)
	}
	releases, err := s.api.Releases(ctx, app)
	if err != nil {
		return err //nolint:wrapcheck // the client's message
	}
	request := client.StartDeploymentRequest{
		Image:              opt.Some(image),
		Reason:             client.ReasonDeploy,
		ExpectedGeneration: uint64(max(currentRevision(releases), 0)), //nolint:gosec // not negative
	}
	key, ok := opts.idempotencyKey.Get()
	if !ok {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("making an idempotency key: %w", err)
		}
		key = "kuben-cli-" + id.String()
	}
	started, err := s.api.Deploy(ctx, app, request, key)
	if err != nil {
		return err //nolint:wrapcheck // the client's message
	}
	out := &lineWriter{w: g.stdout}
	out.line(fmt.Sprintf("%s: revision %d accepted (%s)", app, started.Generation, started.Phase))
	if out.err != nil || opts.noWait {
		return out.err
	}
	poll := func(ctx context.Context) (string, error) {
		d, err := s.api.Deployment(ctx, app, started.Run)
		return d.Phase, err //nolint:wrapcheck // the client's message
	}
	return waitFor(ctx, g.stdout, app, started.Phase, durationSeconds(opts.timeout), poll, realPacer{})
}

// durationSeconds is n seconds, saturating instead of overflowing.
func durationSeconds(n uint64) time.Duration {
	if n > uint64(math.MaxInt64/int64(time.Second)) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(n) * time.Second //nolint:gosec // bounded above
}

// pacer is the clock of waitFor.
type pacer interface {
	now() time.Time
	sleep(ctx context.Context, d time.Duration) error
}

type realPacer struct{}

func (realPacer) now() time.Time { return time.Now() } //nolint:forbidigo // production fallback when no mock clock is supplied

func (realPacer) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // cancelled
	case <-t.C:
		return nil
	}
}

// pollEvery is how often a deployment is asked where it stands.
const pollEvery = 2 * time.Second

// waitFor prints each new phase of the run until it ends or timeout passes.
func waitFor(ctx context.Context, w io.Writer, app client.AppPath, phase string, timeout time.Duration,
	poll func(context.Context) (string, error), clock pacer,
) error {
	out := &lineWriter{w: w}
	deadline := clock.now().Add(timeout)
	for {
		if p, err := run.ParsePhase(phase); err == nil {
			if p == run.Succeeded {
				out.line(app.String() + ": deployed")
				return out.err
			}
			if p.IsFinal() {
				return fmt.Errorf("%s: the deployment ended %s; kuben status %s says why", app, phase, app)
			}
		}
		if !clock.now().Before(deadline) {
			return fmt.Errorf("%s: still %s after %ds; it goes on on the server", app, phase, int64(timeout/time.Second))
		}
		if err := clock.sleep(ctx, pollEvery); err != nil {
			return err
		}
		now, err := poll(ctx)
		if err != nil {
			return err
		}
		if now != phase {
			out.line(app.String() + ": " + now)
			if out.err != nil {
				return out.err
			}
			phase = now
		}
	}
}

// statusOpts are the options of `kuben status`.
type statusOpts struct {
	json   bool
	target clientTarget
}

func runAppStatus(ctx context.Context, g *globals, opts statusOpts, text string, env envLookup) error {
	s, err := openClientSession(opts.target, env)
	if err != nil {
		return err
	}
	path, err := s.app(text)
	if err != nil {
		return err
	}
	detail, err := s.api.App(ctx, path)
	if err != nil {
		return err //nolint:wrapcheck // the client's message
	}
	releases, err := s.api.Releases(ctx, path)
	if err != nil {
		return err //nolint:wrapcheck // the client's message
	}
	doctor, doctorErr := s.api.Doctor(ctx, path)
	if opts.json {
		report := opt.None[client.DoctorReport]()
		if doctorErr == nil {
			report = opt.Some(doctor)
		}
		return printStatusJSON(g.stdout, detail, releases, report)
	}
	return printStatus(g.stdout, path, detail, releases, doctor, doctorErr)
}

// printStatusJSON is `{"app", "pods", "releases", "doctor"}` as the Rust
// CLI printed its serde_json::json! value: object keys sorted at every
// level (serde_json without preserve_order), two-space indent.
func printStatusJSON(w io.Writer, detail client.AppDetail, releases []client.ReleaseDto, doctor opt.Val[client.DoctorReport]) error {
	raw, err := json.Marshal(struct {
		App      client.AppDto                `json:"app"`
		Pods     []client.PodDto              `json:"pods"`
		Releases []client.ReleaseDto          `json:"releases"`
		Doctor   opt.Val[client.DoctorReport] `json:"doctor"`
	}{detail.App, emptyIfNil(detail.Pods), emptyIfNil(releases), doctor})
	if err != nil {
		return fmt.Errorf("writing JSON: %w", err)
	}
	sorted, err := wire.Canonical(raw)
	if err != nil {
		return fmt.Errorf("writing JSON: %w", err)
	}
	var text bytes.Buffer
	if err := json.Indent(&text, []byte(sorted), "", "  "); err != nil {
		return fmt.Errorf("writing JSON: %w", err)
	}
	out := &lineWriter{w: w}
	out.line(text.String())
	return out.err
}

// emptyIfNil is s, or an empty slice for nil: serde wrote `[]`, never null.
func emptyIfNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// inParens is ` (text)`, or nothing without text.
func inParens(v opt.Val[string]) string {
	if text, ok := v.Get(); ok {
		return " (" + text + ")"
	}
	return ""
}

func printStatus(w io.Writer, path client.AppPath, detail client.AppDetail, releases []client.ReleaseDto,
	doctor client.DoctorReport, doctorErr error,
) error {
	out := &lineWriter{w: w}
	a := detail.App
	state := "not ready"
	if a.Ready {
		state = "ready"
	}
	out.line(path.String() + "  " + state + inParens(a.Reason))
	if message, ok := a.Message.Get(); ok && message != "" {
		out.line("  " + message)
	}
	out.line("image     " + a.Image.Or("-"))
	out.line("url       " + a.URL.Or("-"))
	for _, r := range releases {
		if r.Current {
			by := ""
			if actor, ok := r.Actor.Get(); ok {
				by = " by " + actor
			}
			out.line(fmt.Sprintf("revision  %d (%s%s)", r.Revision, r.Reason, by))
			break
		}
	}
	ready := 0
	for _, p := range detail.Pods {
		if p.Ready {
			ready++
		}
	}
	out.line(fmt.Sprintf("pods      %d/%d ready", ready, len(detail.Pods)))
	for _, pod := range detail.Pods {
		out.line(fmt.Sprintf("  %s  %s%s  restarts %d", pod.Name, pod.Phase, inParens(pod.Reason), pod.Restarts))
	}
	if doctorErr != nil {
		out.line("doctor    unavailable: " + doctorErr.Error())
		return out.err
	}
	printDoctor(out, doctor)
	return out.err
}

func printDoctor(out *lineWriter, report client.DoctorReport) {
	out.line("doctor    " + report.Status)
	for _, check := range report.Checks {
		tag := "????"
		switch check.Status {
		case "ok":
			tag = "OK  "
		case "warn":
			tag = "WARN"
		case "fail":
			tag = "FAIL"
		}
		subject := ""
		if check.Subject != "" {
			subject = " " + check.Subject
		}
		out.line("  [" + tag + "] " + check.ID + subject + ": " + check.Detail)
		if hint, ok := check.Hint.Get(); ok {
			out.line("         " + hint)
		}
	}
}

// logsOpts are the options of `kuben logs`.
type logsOpts struct {
	app      string
	follow   bool
	previous bool
	tail     opt.Val[int64]
	process  opt.Val[string]
	target   clientTarget
}

func runLogs(ctx context.Context, g *globals, opts logsOpts, env envLookup) error {
	s, err := openClientSession(opts.target, env)
	if err != nil {
		return err
	}
	app, err := s.app(opts.app)
	if err != nil {
		return err
	}
	options := client.LogOptions{Tail: opts.tail, Process: opts.process, Previous: opts.previous}
	if !opts.follow {
		pods, err := s.api.Logs(ctx, app, options)
		if err != nil {
			return err //nolint:wrapcheck // the client's message
		}
		return printLogs(g.stdout, g.stderr, pods)
	}
	events, err := s.api.FollowLogs(ctx, app, options)
	if err != nil {
		return err //nolint:wrapcheck // the client's message
	}
	return printFollowed(g.stdout, g.stderr, events)
}

// printLogs prints each pod's lines, prefixed with the pod when there are
// several; a pod whose log could not be read is named on stderr.
func printLogs(stdout, stderr io.Writer, pods []client.PodLogs) error {
	out, errOut := &lineWriter{w: stdout}, &lineWriter{w: stderr}
	prefix := len(pods) > 1
	for _, pod := range pods {
		if e, ok := pod.Error.Get(); ok {
			errOut.line("[" + pod.Pod + "] " + e)
		}
		for _, line := range pod.Lines {
			text := withoutTimestamp(line)
			if prefix {
				text = "[" + pod.Pod + "] " + text
			}
			out.line(text)
		}
	}
	return out.err
}

// printFollowed prints followed lines as they come until the stream ends.
func printFollowed(stdout, stderr io.Writer, events iter.Seq2[client.FollowEvent, error]) error {
	out, errOut := &lineWriter{w: stdout}, &lineWriter{w: stderr}
	for event, err := range events {
		if err != nil {
			return err
		}
		switch e := event.(type) {
		case client.LineEvent:
			out.line("[" + e.Pod + "] " + e.Line)
			if out.err != nil {
				return out.err
			}
		case client.EndEvent:
			pod, hasPod := e.Pod.Get()
			reason, hasReason := e.Error.Get()
			switch {
			case hasPod && hasReason:
				errOut.line("[" + pod + "] " + reason)
			case hasPod:
				errOut.line("[" + pod + "] log ended")
			default:
				errOut.line(e.Error.Or("the stream ended"))
				return nil
			}
		}
	}
	return nil
}

// withoutTimestamp is `2026-…Z text` without its timestamp.
func withoutTimestamp(line string) string {
	stamp, rest, found := strings.Cut(line, " ")
	if found && len(stamp) >= 20 && stamp[4] == '-' {
		return rest
	}
	return line
}

// rollbackOpts are the options of `kuben rollback`.
type rollbackOpts struct {
	app string
	// to is the revision to return to (default: the one before the
	// current).
	to     opt.Val[int64]
	target clientTarget
}

func runRollback(ctx context.Context, g *globals, opts rollbackOpts, env envLookup) error {
	s, err := openClientSession(opts.target, env)
	if err != nil {
		return err
	}
	app, err := s.app(opts.app)
	if err != nil {
		return err
	}
	revision, ok := opts.to.Get()
	if !ok {
		releases, err := s.api.Releases(ctx, app)
		if err != nil {
			return err //nolint:wrapcheck // the client's message
		}
		if revision, ok = previousRevision(releases).Get(); !ok {
			return fmt.Errorf("%s has no earlier revision", app)
		}
	}
	dto, err := s.api.Rollback(ctx, app, revision)
	if err != nil {
		return err //nolint:wrapcheck // the client's message
	}
	out := &lineWriter{w: g.stdout}
	out.line(fmt.Sprintf("%s: back to revision %d (%s)", app, revision, dto.Image.Or("image unknown")))
	return out.err
}
