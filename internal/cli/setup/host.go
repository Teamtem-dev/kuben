package setup

// What setup reads and runs on this machine (the helpers of mod.rs):
// commands, the host's facts, local HTTP, ports. Commands and ports go
// through the machine's runner and portFree, so tests run without root,
// systemd or a network.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// commandOutput is what a finished command printed, and whether it exited 0.
type commandOutput struct {
	stdout []byte
	stderr []byte
	ok     bool
}

// runner starts the programs setup uses (systemctl, useradd, psql, ufw, …).
type runner interface {
	// output runs argv with env added to this process's environment and
	// captures what it prints; the error says only that it could not start.
	output(ctx context.Context, env []string, argv ...string) (commandOutput, error)
	// status runs argv with this process's output streams (as Rust's
	// Command::status did); true when it started and exited 0.
	status(ctx context.Context, argv ...string) bool
}

// execRunner runs real processes.
type execRunner struct{}

func (execRunner) output(ctx context.Context, env []string, argv ...string) (commandOutput, error) {
	if len(argv) == 0 {
		return commandOutput{}, errors.New("no program to run")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // setup's own commands
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return commandOutput{}, err //nolint:wrapcheck // the caller names the program
	}
	return commandOutput{stdout: []byte(stdout.String()), stderr: []byte(stderr.String()), ok: err == nil}, nil
}

func (execRunner) status(ctx context.Context, argv ...string) bool {
	if len(argv) == 0 {
		return false
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // setup's own commands
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run() == nil
}

// run runs a command; on failure, its last lines are the error.
func (m *machine) run(ctx context.Context, argv ...string) (string, error) {
	return m.runEnv(ctx, nil, argv...)
}

// runEnv is run with extra environment variables (`NAME=value`).
func (m *machine) runEnv(ctx context.Context, env []string, argv ...string) (string, error) {
	out, err := m.runner.output(ctx, env, argv...)
	if err != nil {
		program := ""
		if len(argv) > 0 {
			program = argv[0]
		}
		return "", fmt.Errorf("running %s: %w", program, err)
	}
	if !out.ok {
		return "", fmt.Errorf("%s failed: %s", strings.Join(argv, " "), strings.TrimSpace(tail(out.stdout, out.stderr, 6)))
	}
	return string(out.stdout), nil
}

// stdoutOf is what argv printed on stdout; empty when it could not start.
func (m *machine) stdoutOf(ctx context.Context, argv ...string) string {
	out, err := m.runner.output(ctx, nil, argv...)
	if err != nil {
		return ""
	}
	return string(out.stdout)
}

// tail is the last non-empty lines of stdout followed by stderr.
func tail(stdout, stderr []byte, n int) string {
	text := strings.ToValidUTF8(string(stdout), "�") + strings.ToValidUTF8(string(stderr), "�")
	all := []string{}
	for _, l := range lines(text) {
		if strings.TrimSpace(l) != "" {
			all = append(all, l)
		}
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return strings.Join(all, "\n")
}

// isRoot reports whether this process runs as root on Linux (Rust read the
// owner of /proc/self, which exists only there).
func isRoot() bool {
	if _, err := os.Stat("/proc/self"); err != nil {
		return false
	}
	return os.Geteuid() == 0
}

func osName() opt.Val[string] {
	release, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return opt.None[string]()
	}
	return parseOSRelease(string(release))
}

func parseOSRelease(release string) opt.Val[string] {
	for _, line := range lines(release) {
		if name, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return opt.Some(strings.Trim(name, `"`))
		}
	}
	return opt.None[string]()
}

func memTotalGB() opt.Val[float64] {
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return opt.None[float64]()
	}
	return parseMemTotalGB(string(meminfo))
}

func parseMemTotalGB(meminfo string) opt.Val[float64] {
	for _, line := range lines(meminfo) {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return opt.None[float64]()
		}
		kb, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return opt.None[float64]()
		}
		return opt.Some(kb / 1024 / 1024)
	}
	return opt.None[float64]()
}

func (m *machine) inContainer() bool {
	if exists("/.dockerenv") {
		return true
	}
	if _, set := m.lookupEnv("container"); set {
		return true
	}
	cgroup, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	c := string(cgroup)
	return strings.Contains(c, "docker") || strings.Contains(c, "lxc") || strings.Contains(c, "containerd")
}

// existingKubeconfig is the kubeconfig an admin on this machine already
// uses, if any.
func (m *machine) existingKubeconfig() opt.Val[string] {
	if paths, set := m.lookupEnv("KUBECONFIG"); set {
		for _, p := range filepath.SplitList(paths) {
			if isFile(p) {
				return opt.Some(p)
			}
		}
	}
	home, set := m.lookupEnv("HOME")
	if !set {
		return opt.None[string]()
	}
	def := filepath.Join(home, ".kube", "config")
	if isFile(def) {
		return opt.Some(def)
	}
	return opt.None[string]()
}

// ids is a user's uid and gid.
type ids struct{ uid, gid uint32 }

func (m *machine) userIDs(ctx context.Context, name string) opt.Val[ids] { //nolint:unparam // kept for potential other users
	out, err := m.runner.output(ctx, nil, "getent", "passwd", name)
	if err != nil || !out.ok {
		return opt.None[ids]()
	}
	return parsePasswdIDs(string(out.stdout))
}

func parsePasswdIDs(line string) opt.Val[ids] {
	fields := strings.Split(strings.TrimSpace(line), ":")
	if len(fields) < 4 {
		return opt.None[ids]()
	}
	uid, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return opt.None[ids]()
	}
	gid, err := strconv.ParseUint(fields[3], 10, 32)
	if err != nil {
		return opt.None[ids]()
	}
	return opt.Some(ids{uid: uint32(uid), gid: uint32(gid)})
}

func sameFile(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	absA, errA := filepath.Abs(ra)
	absB, errB := filepath.Abs(rb)
	return errA == nil && errB == nil && absA == absB
}

func setMode(path string, mode os.FileMode) error {
	if err := os.Chmod(path, mode); err != nil {
		return err //nolint:wrapcheck // names the path
	}
	return nil
}

func chown(path string, owner ids) error {
	if err := os.Chown(path, int(owner.uid), int(owner.gid)); err != nil {
		return err //nolint:wrapcheck // names the path
	}
	return nil
}

func (m *machine) which(program string) opt.Val[string] {
	paths, set := m.lookupEnv("PATH")
	if !set {
		return opt.None[string]()
	}
	for _, dir := range filepath.SplitList(paths) {
		if candidate := filepath.Join(dir, program); isFile(candidate) {
			return opt.Some(candidate)
		}
	}
	return opt.None[string]()
}

func (m *machine) serviceActive(ctx context.Context) bool { return m.serviceIsActive(ctx, "kuben") }

func (m *machine) serviceIsActive(ctx context.Context, unit string) bool {
	return m.runner.status(ctx, "systemctl", "is-active", "--quiet", unit)
}

func (m *machine) serviceLog(ctx context.Context, n int) string {
	return m.stdoutOf(ctx, "journalctl", "-u", "kuben", "--no-pager", "-n", strconv.Itoa(n))
}

// serviceProblems is the service's latest warnings and errors, for a step
// that did not finish.
func (m *machine) serviceProblems(ctx context.Context, n int) string {
	log := m.stdoutOf(ctx, "journalctl", "-u", "kuben", "--no-pager", "-o", "cat", "-n", "400")
	problems := []string{}
	for _, l := range lines(log) {
		if strings.Contains(l, " WARN ") || strings.Contains(l, " ERROR ") {
			problems = append(problems, l)
		}
	}
	if len(problems) == 0 {
		return "It carries on in the background; journalctl -u kuben -f shows how far it is."
	}
	recent := problems[max(len(problems)-n, 0):]
	return "Its latest warnings (journalctl -u kuben has more):\n" + strings.Join(recent, "\n")
}

// portOwner is what listens on port, from `ss`, when it can tell.
func (m *machine) portOwner(ctx context.Context, port uint16) opt.Val[string] {
	out, err := m.runner.output(ctx, nil, "ss", "-ltnp", fmt.Sprintf("sport = :%d", port))
	if err != nil {
		return opt.None[string]()
	}
	return parsePortOwner(string(out.stdout))
}

func parsePortOwner(text string) opt.Val[string] {
	ls := lines(text)
	if len(ls) < 2 {
		return opt.None[string]()
	}
	_, users, ok := strings.Cut(ls[1], "users:(")
	if !ok {
		return opt.None[string]()
	}
	first, _, _ := strings.Cut(users, ",")
	return opt.Some(strings.Trim(first, `("`))
}

func (m *machine) portOwnerOr(ctx context.Context, port uint16) string {
	return m.portOwner(ctx, port).Or("another process")
}

// portFreeOn tries to listen on the port on every address.
func portFreeOn(port uint16) bool {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", fmt.Sprintf("0.0.0.0:%d", port)) //nolint:noctx // synchronous port availability probe
	if err != nil {
		return false
	}
	_ = l.Close() //nolint:errcheck // only a probe
	return true
}

// httpGet is a plain HTTP/1.1 GET on localhost, read to the end, as Rust
// wrote it without an HTTP client.
func httpGet(ctx context.Context, port uint16, path string) (int, string, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return 0, "", err //nolint:wrapcheck // shown as is
	}
	defer conn.Close()                          //nolint:errcheck // read to the end already
	deadline := time.Now().Add(5 * time.Second) //nolint:forbidigo // tcp socket deadline on setup port probe
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, "", err //nolint:wrapcheck // shown as is
	}
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", path); err != nil {
		return 0, "", err //nolint:wrapcheck // shown as is
	}
	raw, err := io.ReadAll(bufio.NewReader(conn))
	if err != nil {
		return 0, "", err //nolint:wrapcheck // shown as is
	}
	code, body, ok := parseHTTPResponse(string(raw))
	if !ok {
		return 0, "", errors.New("malformed HTTP response")
	}
	return code, body, nil
}

func parseHTTPResponse(raw string) (int, string, bool) {
	ls := lines(raw)
	if len(ls) == 0 {
		return 0, "", false
	}
	fields := strings.Fields(ls[0])
	if len(fields) < 2 {
		return 0, "", false
	}
	code, err := strconv.ParseUint(fields[1], 10, 16)
	if err != nil {
		return 0, "", false
	}
	_, body, _ := strings.Cut(raw, "\r\n\r\n")
	return int(code), body, true
}

// waitForHTTP polls path until it answers 200.
func (m *machine) waitForHTTP(ctx context.Context, port uint16, path string, timeout time.Duration) error {
	started := m.clock.NowMs()
	last := ""
	for m.elapsed(started) < timeout {
		code, _, err := m.httpGet(ctx, port, path)
		switch {
		case err != nil:
			last = err.Error()
		case code == 200:
			return nil
		default:
			last = fmt.Sprintf("HTTP %d", code)
		}
		if err := m.sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("%s did not answer 200 within %ds (%s)", path, int64(timeout/time.Second), last)
}

// elapsed is the time since started (unix ms) on the machine's clock.
func (m *machine) elapsed(started int64) time.Duration {
	return time.Duration(m.clock.NowMs()-started) * time.Millisecond
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // cancelled
	case <-t.C:
		return nil
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info == nil {
		return false
	}
	return info.Mode().IsRegular()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info == nil {
		return false
	}
	return info.IsDir()
}

// readText is the file's content, when it can be read.
func readText(path string) opt.Val[string] {
	data, err := os.ReadFile(path) //nolint:gosec // setup's own paths
	if err != nil {
		return opt.None[string]()
	}
	return opt.Some(string(data))
}
