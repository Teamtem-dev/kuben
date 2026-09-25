// Package ui is the terminal output of `kuben setup`, `status` and
// `uninstall` (crates/kuben/src/cli/ui.rs), in the style of today's CLIs:
// one line per step, an orange spinner while it runs (with the elapsed time
// once it takes a while), then `✔`, `⚠` or `✖` with a short detail;
// commands the tool runs are echoed after an orange `❯`, questions after an
// orange `?`. Without a terminal (CI, a log) the same lines are printed
// once, without spinner or colour, so the output reads well in both.
//
// Everything goes to stderr, like the installer's messages. Questions are
// asked on /dev/tty, because under `curl … | sh` stdin is the script.
package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/Teamtem-dev/kuben/internal/core/ascii"
)

const (
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	red    = "\x1b[31m"
	// orange24 is Kuben orange (#f68b12) on terminals that announce 24-bit
	// colour …
	orange24 = "\x1b[38;2;246;139;18m"
	// orange256 is … its closest 256-colour neighbour everywhere else.
	orange256 = "\x1b[38;5;208m"
	dim       = "\x1b[2m"
	bold      = "\x1b[1m"
	reset     = "\x1b[0m"
	// clearLine returns to the start of the line and erases it.
	clearLine = "\r\x1b[2K"
	// showElapsedAfter: steps shorter than this show no timing.
	showElapsedAfter = 3 * time.Second
	// frameEvery is how often the spinner turns.
	frameEvery = 80 * time.Millisecond
)

// frames are the spinner's frames.
func frames() [10]string {
	return [10]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
}

// logo is the wordmark `kuben setup` opens with.
func logo() [6]string {
	return [6]string{
		"██╗  ██╗██╗   ██╗██████╗ ███████╗███╗   ██╗",
		"██║ ██╔╝██║   ██║██╔══██╗██╔════╝████╗  ██║",
		"█████╔╝ ██║   ██║██████╔╝█████╗  ██╔██╗ ██║",
		"██╔═██╗ ██║   ██║██╔══██╗██╔══╝  ██║╚██╗██║",
		"██║  ██╗╚██████╔╝██████╔╝███████╗██║ ╚████║",
		"╚═╝  ╚═╝ ╚═════╝ ╚═════╝ ╚══════╝╚═╝  ╚═══╝",
	}
}

// UI writes a command's progress. The zero UI is not usable; make one with
// [New] or [With].
type UI struct {
	w      io.Writer
	tty    bool
	accent string
}

// New writes to stderr, with colour and spinners when stderr is a terminal
// and NO_COLOR is not set.
func New() UI {
	colorterm := os.Getenv("COLORTERM")
	_, noColor := os.LookupEnv("NO_COLOR")
	return UI{
		w:      os.Stderr,
		tty:    term.IsTerminal(int(os.Stderr.Fd())) && !noColor, //nolint:gosec // a file descriptor fits an int
		accent: accentFor(colorterm == "truecolor" || colorterm == "24bit"),
	}
}

// With writes to w, as a terminal (colour, spinners) when tty is set, with
// the 256-colour orange. Tests and callers that capture the output use it.
func With(w io.Writer, tty bool) UI {
	return UI{w: w, tty: tty, accent: accentFor(false)}
}

func accentFor(truecolor bool) string {
	if truecolor {
		return orange24
	}
	return orange256
}

// emit writes s. Progress output is best effort: Rust's eprint! did not
// report a failed write either (it panicked, which nobody wants).
func (u UI) emit(s string) {
	_, _ = io.WriteString(u.w, s) //nolint:errcheck // best effort, see above
}

func (u UI) paint(colour, text string) string {
	if u.tty {
		return colour + text + reset
	}
	return text
}

// Banner is the orange KUBEN wordmark with the version under it.
func (u UI) Banner(version string) {
	var b strings.Builder
	b.WriteString("\n")
	for _, line := range logo() {
		b.WriteString("  " + u.paint(u.accent, line) + "\n")
	}
	b.WriteString("  " + u.paint(dim, "v"+version+" · a Kubernetes PaaS in a single binary · kuben.teamtem.com") + "\n\n")
	u.emit(b.String())
}

// Step starts a step; it spins until one of its finishers is called.
// Callers `defer step.Close()` so that a step left by an early return only
// clears its spinner line.
func (u UI) Step(label string) *Step {
	return startStep(u, label, time.Now()) //nolint:forbidigo // UI terminal spinner start timestamp
}

// Done is a step that is done, or that needed nothing because it already
// was.
func (u UI) Done(label, detail string) { u.line(glyphDone, label, detail) }

// Warn is a step that ended with a warning.
func (u UI) Warn(label, detail string) { u.line(glyphWarn, label, detail) }

// Fail is a step that failed.
func (u UI) Fail(label, detail string) { u.line(glyphFail, label, detail) }

// Note is an indented explanation under the previous line.
func (u UI) Note(text string) {
	var b strings.Builder
	for _, line := range lines(text) {
		b.WriteString("  " + u.paint(dim, line) + "\n")
	}
	u.emit(b.String())
}

// Command echoes a command this tool is about to run outside a step.
func (u UI) Command(cmd string) {
	prefix := ""
	if u.tty {
		prefix = clearLine
	}
	u.emit(prefix + u.paint(u.accent, "❯") + " " + u.paint(dim, cmd) + "\n")
}

// Heading is a bold heading after an empty line.
func (u UI) Heading(text string) {
	u.emit("\n" + u.paint(bold, text) + "\n")
}

// Blank is an empty line.
func (u UI) Blank() { u.emit("\n") }

// Ask asks on the terminal; Enter takes def. False when there is no
// terminal to ask (CI, cron, a pipe without a controlling tty).
func (u UI) Ask(question, def string) (string, bool) {
	if u.tty {
		u.emit(clearLine)
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", false
	}
	defer tty.Close() //nolint:errcheck // read from only
	return u.askOn(tty, question, def)
}

// askOn asks on tty, a terminal opened for reading and writing.
func (u UI) askOn(tty io.ReadWriter, question, def string) (string, bool) {
	prompt := u.paint(u.accent, "?") + " " + question + " " + u.paint(dim, "› "+def) + " "
	if _, err := io.WriteString(tty, prompt); err != nil {
		return "", false
	}
	answer, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return def, true
	}
	return answer, true
}

// Confirm is a yes/no question, No by default; ok is false without a
// terminal.
func (u UI) Confirm(question string) (yes, ok bool) {
	answer, ok := u.Ask(question+" (y/N)", "N")
	return ok && isYes(answer), ok
}

// ConfirmDefaultYes is a yes/no question, Yes by default; ok is false
// without a terminal.
func (u UI) ConfirmDefaultYes(question string) (yes, ok bool) {
	answer, ok := u.Ask(question+" (Y/n)", "Y")
	return ok && !isNo(answer), ok
}

func isYes(answer string) bool {
	a := ascii.Lower(answer)
	return a == "y" || a == "yes"
}

func isNo(answer string) bool {
	a := ascii.Lower(answer)
	return a == "n" || a == "no"
}

// glyph is how a line ends a step.
type glyph int

const (
	glyphDone glyph = iota
	glyphWarn
	glyphFail
)

func (u UI) line(g glyph, label, detail string) {
	var colour, mark string
	switch g {
	case glyphDone:
		colour, mark = green, "✔"
	case glyphWarn:
		colour, mark = yellow, "⚠"
	case glyphFail:
		colour, mark = red, "✖"
	}
	prefix := ""
	if u.tty {
		prefix = clearLine
	}
	label = strings.TrimRight(label, ".")
	if detail == "" {
		u.emit(fmt.Sprintf("%s%s %s.\n", prefix, u.paint(colour, mark), label))
		return
	}
	u.emit(fmt.Sprintf("%s%s %s. %s\n", prefix, u.paint(colour, mark), label, u.paint(dim, detail)))
}

// lines splits text as Rust's str::lines does: at `\n`, a `\r` before it
// dropped, and no empty line after a final newline.
func lines(text string) []string {
	if text == "" {
		return nil
	}
	parts := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, part := range parts {
		parts[i] = strings.TrimSuffix(part, "\r")
	}
	return parts
}

// Step is a running step. Finish it with [Step.Done], [Step.Warn] or
// [Step.Fail]; closed unfinished (an error returned early), it only clears
// its spinner line.
type Step struct {
	ui      UI
	label   string
	started time.Time

	// mu guards command, which the spinner shows after the label.
	mu      sync.Mutex
	command string

	// stop ends the spinner and spun is closed once it has; both are nil
	// without a terminal. halt makes stop close once.
	stop chan struct{}
	spun chan struct{}
	halt sync.Once
}

func startStep(u UI, label string, started time.Time) *Step {
	s := &Step{ui: u, label: label, started: started}
	if !u.tty {
		u.emit("… " + label + "\n")
		return s
	}
	s.stop, s.spun = make(chan struct{}), make(chan struct{})
	// The spinner's goroutine is owned by the step: halt stops it and
	// waits for it.
	go s.spin()
	return s
}

// spin redraws the spinner line until stop is closed.
func (s *Step) spin() {
	defer close(s.spun)
	ticker := time.NewTicker(frameEvery)
	defer ticker.Stop()
	all := frames()
	for i := 0; ; i++ {
		select {
		case <-s.stop:
			return
		default:
		}
		s.ui.emit(s.frame(all[i%len(all)], time.Since(s.started)))
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}

// frame is one drawing of the spinner line.
func (s *Step) frame(frame string, elapsed time.Duration) string {
	accent := s.ui.accent
	clock := ""
	if elapsed >= showElapsedAfter {
		clock = fmt.Sprintf(" %s%ds%s", dim, int64(elapsed/time.Second), reset)
	}
	s.mu.Lock()
	cmd := s.command
	s.mu.Unlock()
	suffix := ""
	if cmd != "" {
		suffix = " " + accent + "❯" + reset + " " + dim + cmd + reset
	}
	return clearLine + accent + frame + reset + " " + s.label + suffix + clock
}

// Command shows a command being run under this step without breaking the
// spinner line.
func (s *Step) Command(cmd string) {
	s.mu.Lock()
	s.command = cmd
	s.mu.Unlock()
	if !s.ui.tty {
		s.ui.emit("  " + s.ui.paint(s.ui.accent, "❯") + " " + s.ui.paint(dim, cmd) + "\n")
	}
}

// Done finishes the step with `✔`.
func (s *Step) Done(detail string) { s.finish(glyphDone, detail) }

// Warn finishes the step with `⚠`.
func (s *Step) Warn(detail string) { s.finish(glyphWarn, detail) }

// Fail finishes the step with `✖`.
func (s *Step) Fail(detail string) { s.finish(glyphFail, detail) }

// Close stops the spinner and clears its line; a finished step is already
// closed.
func (s *Step) Close() {
	s.halt.Do(func() {
		if s.stop == nil {
			return
		}
		close(s.stop)
		<-s.spun
		s.ui.emit(clearLine)
	})
}

func (s *Step) finish(g glyph, detail string) {
	s.Close()
	s.ui.line(g, s.label, withElapsed(detail, time.Since(s.started)))
}

// withElapsed is `detail · 41s` for a step that took a while, detail
// otherwise.
func withElapsed(detail string, elapsed time.Duration) string {
	if elapsed < showElapsedAfter {
		return detail
	}
	secs := int64(elapsed / time.Second)
	clock := fmt.Sprintf("%ds", secs)
	if secs >= 60 {
		clock = fmt.Sprintf("%dm%02ds", secs/60, secs%60)
	}
	if detail == "" {
		return clock
	}
	return detail + " · " + clock
}
