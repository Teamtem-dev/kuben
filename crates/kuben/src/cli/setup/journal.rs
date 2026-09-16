//! The installer's journal (M2.6, plan §11.2): every `kuben setup` run, the
//! outcome of each of its steps, and who owns each resource the installer
//! touched — Kuben, when setup created it, or someone else, when it was
//! there first. A resource's owner is decided the first time setup sees it
//! and never changes, so `kuben uninstall --purge` removes only what setup
//! made (I12) and a repeated install never takes over what it did not create.
//!
//! The file lives at `/var/lib/kuben/install/journal.json`, written by root
//! atomically after every step and readable by the `kuben` service user;
//! `kuben serve` copies it into SQL once the database is up.

use std::{
    io::Write as _,
    path::{Path, PathBuf},
};

use anyhow::Context as _;
use serde::{Deserialize, Serialize};

/// The journal, relative to the state directory.
pub const JOURNAL: &str = "install/journal.json";
const FORMAT: u32 = 1;
/// Runs kept; older ones are dropped.
const MAX_RUNS: usize = 20;

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Journal {
    pub format: u32,
    #[serde(default)]
    pub runs: Vec<Run>,
    #[serde(default)]
    pub resources: Vec<Resource>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Run {
    /// The kuben version that ran.
    pub kuben: String,
    pub started_at: i64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub finished_at: Option<i64>,
    /// `None` while running, or when the run was interrupted.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub succeeded: Option<bool>,
    #[serde(default)]
    pub steps: Vec<Step>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Step {
    pub id: String,
    pub result: StepResult,
    pub detail: String,
    pub at: i64,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum StepResult {
    /// The step changed the machine.
    Changed,
    /// Everything was already as it should be.
    Unchanged,
    Failed,
}

/// Something the installer touched.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum Kind {
    File,
    Directory,
    SystemUser,
    SystemdUnit,
    Package,
    PostgresRole,
    PostgresDatabase,
    /// The Kubernetes distribution (k3s).
    Cluster,
    FirewallRule,
    /// A Kubernetes object, named `kind/namespace/name` or `kind/name`.
    KubernetesObject,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum Owner {
    /// Setup created it; uninstall may remove it.
    Kuben,
    /// It was there before setup first looked; never removed or taken over.
    Preexisting,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Resource {
    pub kind: Kind,
    pub name: String,
    pub owner: Owner,
    pub since: i64,
}

/// A journal bound to its file; every change is saved at once.
#[derive(Debug)]
pub struct Book {
    path: PathBuf,
    journal: Journal,
    /// The service user's group may read the file.
    group: Option<u32>,
    /// The step in progress, recorded as failed if the run stops in it.
    current: Option<String>,
}

impl Journal {
    /// Read `path`; a missing file is an empty journal. An unreadable one is
    /// moved aside (it is a record, never an input to a decision that could
    /// destroy data) and a fresh journal starts.
    pub fn read(path: &Path) -> anyhow::Result<Self> {
        let text = match std::fs::read_to_string(path) {
            Ok(text) => text,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Self::fresh()),
            Err(e) => return Err(e).with_context(|| format!("reading {}", path.display())),
        };
        match serde_json::from_str::<Self>(&text) {
            Ok(journal) => Ok(journal),
            Err(e) => {
                let aside = path.with_extension(format!("unreadable-{}", now()));
                std::fs::rename(path, &aside).ok();
                tracing::warn!(error = %e, kept = %aside.display(), "the install journal was unreadable; starting a new one");
                Ok(Self::fresh())
            }
        }
    }

    /// Read `path` without touching it: `None` when it is missing or
    /// unreadable (for `--plan` and `kuben serve`).
    #[must_use]
    pub fn peek(path: &Path) -> Option<Self> {
        serde_json::from_str(&std::fs::read_to_string(path).ok()?).ok()
    }

    fn fresh() -> Self {
        Self {
            format: FORMAT,
            ..Self::default()
        }
    }

    /// Who owns `name`, if setup has seen it.
    #[must_use]
    pub fn owner(&self, kind: Kind, name: &str) -> Option<Owner> {
        self.resources
            .iter()
            .find(|r| r.kind == kind && r.name == name)
            .map(|r| r.owner)
    }

    /// Setup created `name` (a journal-less install says nothing).
    #[must_use]
    pub fn owns(&self, kind: Kind, name: &str) -> bool {
        self.owner(kind, name) == Some(Owner::Kuben)
    }

    /// The last run, when it did not finish well.
    #[must_use]
    pub fn unfinished(&self) -> Option<&Run> {
        self.runs.last().filter(|r| r.succeeded != Some(true))
    }

    /// Record `name` as seen: `created` when setup made it just now. The
    /// first sighting decides the owner for good.
    pub fn claim(&mut self, kind: Kind, name: &str, created: bool) -> Owner {
        if let Some(owner) = self.owner(kind, name) {
            return owner;
        }
        let owner = if created { Owner::Kuben } else { Owner::Preexisting };
        self.resources.push(Resource {
            kind,
            name: name.to_owned(),
            owner,
            since: now(),
        });
        owner
    }

    /// Forget `name` after uninstall removed it.
    pub fn release(&mut self, kind: Kind, name: &str) {
        self.resources.retain(|r| !(r.kind == kind && r.name == name));
    }

    fn begin(&mut self, kuben: &str) {
        self.runs.push(Run {
            kuben: kuben.to_owned(),
            started_at: now(),
            finished_at: None,
            succeeded: None,
            steps: Vec::new(),
        });
        let excess = self.runs.len().saturating_sub(MAX_RUNS);
        self.runs.drain(..excess);
    }

    fn record(&mut self, id: &str, result: StepResult, detail: &str) {
        if let Some(run) = self.runs.last_mut() {
            run.steps.push(Step {
                id: id.to_owned(),
                result,
                detail: detail.to_owned(),
                at: now(),
            });
        }
    }

    fn finish(&mut self, succeeded: bool) {
        if let Some(run) = self.runs.last_mut() {
            run.finished_at = Some(now());
            run.succeeded = Some(succeeded);
        }
    }

    /// Whether the last run changed anything.
    #[must_use]
    pub fn last_run_changed(&self) -> bool {
        self.runs
            .last()
            .is_some_and(|r| r.steps.iter().any(|s| s.result != StepResult::Unchanged))
    }
}

impl Book {
    /// Open the journal under `state_dir`, readable by `group`.
    pub fn open(state_dir: &Path, group: Option<u32>) -> anyhow::Result<Self> {
        let path = state_dir.join(JOURNAL);
        Ok(Self {
            journal: Journal::read(&path)?,
            path,
            group,
            current: None,
        })
    }

    /// Let the service user's `group` read the journal from now on.
    pub fn set_group(&mut self, group: u32) {
        self.group = Some(group);
    }

    #[must_use]
    pub const fn journal(&self) -> &Journal {
        &self.journal
    }

    /// Start a run of `kuben` (a version).
    pub fn begin(&mut self, kuben: &str) -> anyhow::Result<()> {
        self.journal.begin(kuben);
        self.save()
    }

    /// A step begins; [`Book::done`] or [`Book::finish`] closes it.
    pub fn start(&mut self, id: &str) {
        self.current = Some(id.to_owned());
    }

    /// The step in progress ended, having `changed` the machine or not.
    pub fn done(&mut self, changed: bool, detail: impl AsRef<str>) -> anyhow::Result<()> {
        let id = self.current.take().unwrap_or_else(|| "step".to_owned());
        let result = if changed {
            StepResult::Changed
        } else {
            StepResult::Unchanged
        };
        self.journal.record(&id, result, detail.as_ref());
        self.save()
    }

    /// See [`Journal::claim`].
    pub fn claim(&mut self, kind: Kind, name: &str, created: bool) -> anyhow::Result<Owner> {
        let owner = self.journal.claim(kind, name, created);
        self.save()?;
        Ok(owner)
    }

    /// See [`Journal::release`].
    pub fn release(&mut self, kind: Kind, name: &str) -> anyhow::Result<()> {
        self.journal.release(kind, name);
        self.save()
    }

    /// End the run; `error` names why it stopped, recorded on the step in
    /// progress.
    pub fn finish(&mut self, error: Option<&anyhow::Error>) -> anyhow::Result<()> {
        if let (Some(id), Some(e)) = (self.current.take(), error) {
            self.journal.record(&id, StepResult::Failed, &format!("{e:#}"));
        }
        self.journal.finish(error.is_none());
        self.save()
    }

    /// Write-then-rename, so a crash leaves the old journal or the new one.
    fn save(&self) -> anyhow::Result<()> {
        let dir = self.path.parent().context("the journal has no directory")?;
        std::fs::create_dir_all(dir).with_context(|| format!("creating {}", dir.display()))?;
        restrict(dir, 0o750, self.group);
        let staged = self.path.with_extension("json.new");
        {
            let mut file =
                std::fs::File::create(&staged).with_context(|| format!("writing {}", staged.display()))?;
            serde_json::to_writer_pretty(&mut file, &self.journal)?;
            file.write_all(b"\n")?;
            file.sync_all()?;
        }
        restrict(&staged, 0o640, self.group);
        std::fs::rename(&staged, &self.path).with_context(|| format!("writing {}", self.path.display()))?;
        Ok(())
    }
}

/// Owner root, group `group` (the service user may read), `mode`.
fn restrict(path: &Path, mode: u32, group: Option<u32>) {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode)).ok();
        if let Some(gid) = group {
            std::os::unix::fs::chown(path, None, Some(gid)).ok();
        }
    }
    #[cfg(not(unix))]
    {
        let _ = (path, mode, group);
    }
}

fn now() -> i64 {
    kuben_core::time::now_ms()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_first_sighting_decides_the_owner_for_good() {
        let mut j = Journal::fresh();
        assert_eq!(
            j.claim(Kind::PostgresDatabase, "kuben", false),
            Owner::Preexisting
        );
        assert_eq!(
            j.claim(Kind::PostgresDatabase, "kuben", true),
            Owner::Preexisting,
            "a later run never takes a database over"
        );
        assert!(!j.owns(Kind::PostgresDatabase, "kuben"));
        assert_eq!(j.claim(Kind::Cluster, "k3s", true), Owner::Kuben);
        assert!(j.owns(Kind::Cluster, "k3s"));
        assert_eq!(j.owner(Kind::File, "/etc/kuben/config.toml"), None);
        j.release(Kind::Cluster, "k3s");
        assert_eq!(j.owner(Kind::Cluster, "k3s"), None);
    }

    #[test]
    fn runs_record_steps_and_stay_bounded() {
        let mut j = Journal::fresh();
        j.begin("2.0.0");
        j.record("binary", StepResult::Changed, "v2.0.0 → /usr/local/bin/kuben");
        assert!(j.unfinished().is_some(), "a run in progress is unfinished");
        j.finish(false);
        assert_eq!(j.unfinished().map(|r| r.steps.len()), Some(1));
        j.begin("2.0.0");
        j.record("binary", StepResult::Unchanged, "up to date");
        j.finish(true);
        assert!(j.unfinished().is_none());
        assert!(!j.last_run_changed());
        for _ in 0..30 {
            j.begin("2.0.0");
        }
        assert_eq!(j.runs.len(), MAX_RUNS);
    }

    #[test]
    fn the_book_saves_atomically_and_moves_an_unreadable_file_aside() {
        let dir = std::env::temp_dir().join(format!("kuben-journal-{}", uuid_like()));
        let mut book = Book::open(&dir, None).expect("open");
        book.begin("2.0.0").expect("begin");
        book.claim(Kind::SystemUser, "kuben", true).expect("claim");
        book.start("user");
        book.done(true, "kuben (uid 999)").expect("done");
        book.start("database");
        book.finish(Some(&anyhow::anyhow!("PostgreSQL did not answer")))
            .expect("finish");
        let steps = &book.journal().runs[0].steps;
        assert_eq!(
            steps
                .iter()
                .map(|s| (s.id.as_str(), s.result))
                .collect::<Vec<_>>(),
            [("user", StepResult::Changed), ("database", StepResult::Failed)]
        );
        assert_eq!(book.journal().runs[0].succeeded, Some(false));
        let again = Book::open(&dir, None).expect("reopen");
        assert_eq!(again.journal(), book.journal());
        assert!(!dir.join("install/journal.json.new").exists());

        std::fs::write(dir.join(JOURNAL), "{ not json").expect("corrupt");
        let fresh = Book::open(&dir, None).expect("reopen corrupt");
        assert!(fresh.journal().runs.is_empty());
        let kept = std::fs::read_dir(dir.join("install"))
            .expect("dir")
            .filter_map(Result::ok)
            .any(|e| e.file_name().to_string_lossy().contains("unreadable"));
        assert!(kept, "the unreadable journal is kept aside");
        std::fs::remove_dir_all(&dir).ok();
    }

    fn uuid_like() -> String {
        format!("{}-{}", std::process::id(), now())
    }

    #[test]
    fn the_format_reads_back() {
        let json = r#"{"format":1,"runs":[{"kuben":"2.0.0","startedAt":1,"steps":[{"id":"k3s","result":"changed","detail":"v1.33","at":2}]}],
            "resources":[{"kind":"cluster","name":"k3s","owner":"kuben","since":2}]}"#;
        let j: Journal = serde_json::from_str(json).expect("journal");
        assert!(j.owns(Kind::Cluster, "k3s"));
        assert_eq!(j.runs[0].steps[0].result, StepResult::Changed);
        assert!(j.unfinished().is_some(), "an interrupted run has no outcome");
    }
}
