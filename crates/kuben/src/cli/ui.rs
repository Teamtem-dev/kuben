//! Terminal output of `kuben setup`, `status` and `uninstall`, in the style
//! of today's CLIs: one line per step, a spinner while it runs, then `✔`,
//! `⚠`, `✖` or `○` with a short detail; commands the tool runs are echoed
//! after `❯`. Without a terminal (CI, a log) the same lines are printed once,
//! without spinner or colour, so the output stays readable in both places.
//!
//! Everything goes to stderr, like the installer's messages; the final link
//! is the one thing worth capturing and goes to stdout as well.

use std::{
    io::{IsTerminal as _, Write as _},
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    thread::JoinHandle,
    time::Duration,
};

const FRAMES: [&str; 10] = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
const GREEN: &str = "\x1b[32m";
const YELLOW: &str = "\x1b[33m";
const RED: &str = "\x1b[31m";
const CYAN: &str = "\x1b[36m";
const DIM: &str = "\x1b[2m";
const BOLD: &str = "\x1b[1m";
const RESET: &str = "\x1b[0m";

#[derive(Clone, Copy)]
pub struct Ui {
    tty: bool,
}

impl Ui {
    #[must_use]
    pub fn new() -> Self {
        Self {
            tty: std::io::stderr().is_terminal() && std::env::var_os("NO_COLOR").is_none(),
        }
    }

    fn paint(self, colour: &str, text: &str) -> String {
        if self.tty {
            format!("{colour}{text}{RESET}")
        } else {
            text.to_owned()
        }
    }

    /// Start a step; it spins until one of its finishers is called.
    #[must_use]
    pub fn step(self, label: impl Into<String>) -> Step {
        Step::start(self, label.into())
    }

    /// A step that is already done.
    pub fn done(self, label: &str, detail: impl AsRef<str>) {
        self.line(Glyph::Done, label, detail.as_ref());
    }

    /// A step that was not needed.
    pub fn skip(self, label: &str, detail: impl AsRef<str>) {
        self.line(Glyph::Skip, label, detail.as_ref());
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
        eprintln!("{} {}", self.paint(CYAN, "❯"), self.paint(DIM, cmd));
    }

    pub fn heading(self, text: &str) {
        eprintln!("\n{}", self.paint(BOLD, text));
    }

    pub fn blank() {
        eprintln!();
    }

    fn line(self, glyph: Glyph, label: &str, detail: &str) {
        let (colour, mark) = match glyph {
            Glyph::Done => (GREEN, "✔"),
            Glyph::Skip => (DIM, "○"),
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
    Skip,
    Warn,
    Fail,
}

/// A running step. Finish it with [`Step::done`], [`Step::skip`],
/// [`Step::warn`] or [`Step::fail`]; dropped unfinished (an error escaped
/// with `?`), it only clears its spinner line.
pub struct Step {
    ui: Ui,
    label: String,
    stop: Arc<AtomicBool>,
    spinner: Option<JoinHandle<()>>,
}

impl Step {
    fn start(ui: Ui, label: String) -> Self {
        let stop = Arc::new(AtomicBool::new(false));
        let spinner = ui.tty.then(|| {
            let stop = Arc::clone(&stop);
            let text = label.clone();
            std::thread::spawn(move || {
                let mut stderr = std::io::stderr();
                for frame in FRAMES.iter().cycle() {
                    if stop.load(Ordering::Relaxed) {
                        break;
                    }
                    let _ = write!(stderr, "\r\x1b[2K{CYAN}{frame}{RESET} {text}");
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
            stop,
            spinner,
        }
    }

    pub fn done(self, detail: impl AsRef<str>) {
        self.finish(Glyph::Done, detail.as_ref());
    }

    pub fn skip(self, detail: impl AsRef<str>) {
        self.finish(Glyph::Skip, detail.as_ref());
    }

    pub fn warn(self, detail: impl AsRef<str>) {
        self.finish(Glyph::Warn, detail.as_ref());
    }

    pub fn fail(self, detail: impl AsRef<str>) {
        self.finish(Glyph::Fail, detail.as_ref());
    }

    fn finish(mut self, glyph: Glyph, detail: &str) {
        self.halt();
        self.ui.line(glyph, &self.label, detail);
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn plain_output_has_no_escape_codes() {
        let ui = Ui { tty: false };
        assert_eq!(ui.paint(GREEN, "ok"), "ok");
        let tty = Ui { tty: true };
        assert_eq!(tty.paint(GREEN, "ok"), "\x1b[32mok\x1b[0m");
    }

    #[test]
    fn a_step_can_be_dropped_unfinished() {
        let step = Ui { tty: false }.step("Doing something");
        drop(step);
        Ui { tty: false }.step("Doing something else").done("fine");
    }
}
