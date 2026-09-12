//! Terminal output of `kuben setup`, `status` and `uninstall`, in the style
//! of today's CLIs: one line per step, an orange spinner while it runs (with
//! the elapsed time once it takes a while), then `✔`, `⚠` or `✖` with a short
//! detail; commands the tool runs are echoed after an orange `❯`, questions
//! after an orange `?`. Without a terminal (CI, a log) the same lines are
//! printed once, without spinner or colour, so the output reads well in both.
//!
//! Everything goes to stderr, like the installer's messages; the final link
//! is the one thing worth capturing and goes to stdout as well. Questions are
//! asked on `/dev/tty`, because under `curl … | sh` stdin is the script.

use std::{
    io::{BufRead as _, BufReader, IsTerminal as _, Write as _},
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    thread::JoinHandle,
    time::{Duration, Instant},
};

const FRAMES: [&str; 10] = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
const GREEN: &str = "\x1b[32m";
const YELLOW: &str = "\x1b[33m";
const RED: &str = "\x1b[31m";
/// Kuben orange (#f68b12) on terminals that announce 24-bit colour …
const ORANGE_24: &str = "\x1b[38;2;246;139;18m";
/// … and its closest 256-colour neighbour everywhere else.
const ORANGE_256: &str = "\x1b[38;5;208m";
const DIM: &str = "\x1b[2m";
const BOLD: &str = "\x1b[1m";
const RESET: &str = "\x1b[0m";
/// Steps shorter than this show no timing.
const SHOW_ELAPSED_AFTER: Duration = Duration::from_secs(3);

/// The wordmark `kuben setup` opens with.
const LOGO: [&str; 6] = [
    "██╗  ██╗██╗   ██╗██████╗ ███████╗███╗   ██╗",
    "██║ ██╔╝██║   ██║██╔══██╗██╔════╝████╗  ██║",
    "█████╔╝ ██║   ██║██████╔╝█████╗  ██╔██╗ ██║",
    "██╔═██╗ ██║   ██║██╔══██╗██╔══╝  ██║╚██╗██║",
    "██║  ██╗╚██████╔╝██████╔╝███████╗██║ ╚████║",
    "╚═╝  ╚═╝ ╚═════╝ ╚═════╝ ╚══════╝╚═╝  ╚═══╝",
];

#[derive(Clone, Copy)]
pub struct Ui {
    tty: bool,
    accent: &'static str,
}

impl Ui {
    #[must_use]
    pub fn new() -> Self {
        let truecolor = std::env::var("COLORTERM").is_ok_and(|v| v == "truecolor" || v == "24bit");
        Self {
            tty: std::io::stderr().is_terminal() && std::env::var_os("NO_COLOR").is_none(),
            accent: if truecolor { ORANGE_24 } else { ORANGE_256 },
        }
    }

    fn paint(self, colour: &str, text: &str) -> String {
        if self.tty {
            format!("{colour}{text}{RESET}")
        } else {
            text.to_owned()
        }
    }

    /// The orange KUBEN wordmark with the version under it.
    pub fn banner(self, version: &str) {
        eprintln!();
        for line in LOGO {
            eprintln!("  {}", self.paint(self.accent, line));
        }
        eprintln!(
            "  {}\n",
            self.paint(
                DIM,
                &format!("v{version} · a Kubernetes PaaS in a single binary · kuben.teamtem.com")
            )
        );
    }

    /// Start a step; it spins until one of its finishers is called.
    #[must_use]
    pub fn step(self, label: impl Into<String>) -> Step {
        Step::start(self, label.into())
    }

    /// A step that is done, or that needed nothing because it already was.
    pub fn done(self, label: &str, detail: impl AsRef<str>) {
        self.line(Glyph::Done, label, detail.as_ref());
    }

    pub fn warn(self, label: &str, detail: impl AsRef<str>) {
        self.line(Glyph::Warn, label, detail.as_ref());
    }

    pub fn fail(self, label: &str, detail: impl AsRef<str>) {
        self.line(Glyph::Fail, label, detail.as_ref());
    }

    /// An indented explanation under the previous line.
    pub fn note(self, text: &str) {
        for line in text.lines() {
            eprintln!("  {}", self.paint(DIM, line));
        }
    }

    /// A command this tool is about to run.
    pub fn command(self, cmd: &str) {
        eprintln!("{} {}", self.paint(self.accent, "❯"), self.paint(DIM, cmd));
    }

    pub fn heading(self, text: &str) {
        eprintln!("\n{}", self.paint(BOLD, text));
    }

    pub fn blank() {
        eprintln!();
    }

    /// Ask on the terminal; Enter takes `default`. `None` when there is no
    /// terminal to ask (CI, cron, a pipe without a controlling tty).
    #[must_use]
    pub fn ask(self, question: &str, default: &str) -> Option<String> {
        let tty = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open("/dev/tty")
            .ok()?;
        let prompt = format!(
            "{} {question} {} ",
            self.paint(self.accent, "?"),
            self.paint(DIM, &format!("› {default}"))
        );
        (&tty).write_all(prompt.as_bytes()).ok()?;
        (&tty).flush().ok()?;
        let mut answer = String::new();
        BufReader::new(&tty).read_line(&mut answer).ok()?;
        let answer = answer.trim();
        Some(if answer.is_empty() {
            default.to_owned()
        } else {
            answer.to_owned()
        })
    }

    /// A yes/no question, `No` by default; `None` without a terminal.
    #[must_use]
    pub fn confirm(self, question: &str) -> Option<bool> {
        let answer = self.ask(&format!("{question} (y/N)"), "N")?;
        Some(matches!(answer.to_ascii_lowercase().as_str(), "y" | "yes"))
    }

    fn line(self, glyph: Glyph, label: &str, detail: &str) {
        let (colour, mark) = match glyph {
            Glyph::Done => (GREEN, "✔"),
            Glyph::Warn => (YELLOW, "⚠"),
            Glyph::Fail => (RED, "✖"),
        };
        let label = label.trim_end_matches('.');
        if detail.is_empty() {
            eprintln!("{} {label}.", self.paint(colour, mark));
        } else {
            eprintln!(
                "{} {label}. {}",
                self.paint(colour, mark),
                self.paint(DIM, detail)
            );
        }
    }
}

impl Default for Ui {
    fn default() -> Self {
        Self::new()
    }
}

#[derive(Clone, Copy)]
enum Glyph {
    Done,
    Warn,
    Fail,
}

/// A running step. Finish it with [`Step::done`], [`Step::warn`] or
/// [`Step::fail`]; dropped unfinished (an error escaped with `?`), it only
/// clears its spinner line.
pub struct Step {
    ui: Ui,
    label: String,
    started: Instant,
    stop: Arc<AtomicBool>,
    spinner: Option<JoinHandle<()>>,
}

impl Step {
    fn start(ui: Ui, label: String) -> Self {
        let started = Instant::now();
        let stop = Arc::new(AtomicBool::new(false));
        let spinner = ui.tty.then(|| {
            let stop = Arc::clone(&stop);
            let text = label.clone();
            let accent = ui.accent;
            std::thread::spawn(move || {
                let mut stderr = std::io::stderr();
                for frame in FRAMES.iter().cycle() {
                    if stop.load(Ordering::Relaxed) {
                        break;
                    }
                    let elapsed = started.elapsed();
                    let clock = if elapsed >= SHOW_ELAPSED_AFTER {
                        format!(" {DIM}{}s{RESET}", elapsed.as_secs())
                    } else {
                        String::new()
                    };
                    let _ = write!(stderr, "\r\x1b[2K{accent}{frame}{RESET} {text}{clock}");
                    let _ = stderr.flush();
                    std::thread::sleep(Duration::from_millis(80));
                }
            })
        });
        if !ui.tty {
            eprintln!("… {label}");
        }
        Self {
            ui,
            label,
            started,
            stop,
            spinner,
        }
    }

    pub fn done(self, detail: impl AsRef<str>) {
        self.finish(Glyph::Done, detail.as_ref());
    }

    pub fn warn(self, detail: impl AsRef<str>) {
        self.finish(Glyph::Warn, detail.as_ref());
    }

    pub fn fail(self, detail: impl AsRef<str>) {
        self.finish(Glyph::Fail, detail.as_ref());
    }

    fn finish(mut self, glyph: Glyph, detail: &str) {
        self.halt();
        let detail = with_elapsed(detail, self.started.elapsed());
        self.ui.line(glyph, &self.label, &detail);
    }

    fn halt(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        if let Some(handle) = self.spinner.take() {
            let _ = handle.join();
            eprint!("\r\x1b[2K");
        }
    }
}

impl Drop for Step {
    fn drop(&mut self) {
        self.halt();
    }
}

/// `detail · 41s` for a step that took a while; `detail` otherwise.
fn with_elapsed(detail: &str, elapsed: Duration) -> String {
    if elapsed < SHOW_ELAPSED_AFTER {
        return detail.to_owned();
    }
    let secs = elapsed.as_secs();
    let clock = if secs >= 60 {
        format!("{}m{:02}s", secs / 60, secs % 60)
    } else {
        format!("{secs}s")
    };
    if detail.is_empty() {
        clock
    } else {
        format!("{detail} · {clock}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const PLAIN: Ui = Ui {
        tty: false,
        accent: ORANGE_256,
    };

    #[test]
    fn plain_output_has_no_escape_codes() {
        assert_eq!(PLAIN.paint(GREEN, "ok"), "ok");
        let tty = Ui { tty: true, ..PLAIN };
        assert_eq!(tty.paint(GREEN, "ok"), "\x1b[32mok\x1b[0m");
        assert_eq!(
            tty.paint(tty.accent, "❯"),
            "\x1b[38;5;208m❯\x1b[0m",
            "orange, not blue"
        );
    }

    #[test]
    fn a_step_can_be_dropped_unfinished() {
        let step = PLAIN.step("Doing something");
        drop(step);
        PLAIN.step("Doing something else").done("fine");
    }

    #[test]
    fn long_steps_show_how_long_they_took() {
        assert_eq!(
            with_elapsed("v1.36.4+k3s1", Duration::from_secs(1)),
            "v1.36.4+k3s1"
        );
        assert_eq!(
            with_elapsed("v1.36.4+k3s1", Duration::from_secs(41)),
            "v1.36.4+k3s1 · 41s"
        );
        assert_eq!(with_elapsed("", Duration::from_secs(75)), "1m15s");
    }
}
