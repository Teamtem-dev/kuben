//! Git sources (M3, ADR-028): validated repository, branch, commit and
//! build-recipe values, and the parts of a GitHub webhook Kuben acts on.
//!
//! A webhook is a hint, never the source of truth: the worker reads the
//! branch head from the provider before it records a new source epoch, so a
//! duplicate, reordered or forged-but-signed delivery cannot move a target
//! backwards (plan §14.1, `TargetState::observe_source_head`).

use std::{fmt, str::FromStr};

use serde::{Deserialize, Serialize};

/// A rejected source value.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum InvalidSource {
    #[error("not a full 40-character commit SHA: {0:?}")]
    Commit(String),
    #[error("not an `owner/name` repository: {0:?}")]
    Repository(String),
    #[error("not a valid branch name: {0:?}")]
    Branch(String),
    #[error("not a relative path inside the repository: {0:?}")]
    Path(String),
    #[error("the webhook payload is malformed: {0}")]
    Payload(String),
}

/// A full, lowercase commit SHA-1. Abbreviated SHAs and ref names are refused:
/// a build always names exactly one tree.
#[derive(Clone, Debug, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct CommitSha(String);

impl CommitSha {
    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }

    /// The first twelve digits, for names and labels.
    #[must_use]
    pub fn short(&self) -> &str {
        &self.0[..12]
    }

    /// The all-zero SHA GitHub sends for a created or deleted branch.
    #[must_use]
    pub fn is_zero(&self) -> bool {
        self.0.bytes().all(|b| b == b'0')
    }
}

impl FromStr for CommitSha {
    type Err = InvalidSource;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let ok = s.len() == 40
            && s.bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b));
        if ok {
            Ok(Self(s.to_owned()))
        } else {
            Err(InvalidSource::Commit(s.to_owned()))
        }
    }
}

impl TryFrom<String> for CommitSha {
    type Error = InvalidSource;

    fn try_from(s: String) -> Result<Self, Self::Error> {
        s.parse()
    }
}

impl From<CommitSha> for String {
    fn from(c: CommitSha) -> Self {
        c.0
    }
}

impl fmt::Display for CommitSha {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

/// `owner/name` of a hosted repository, compared case-insensitively by the
/// provider and stored lowercase here.
#[derive(Clone, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct RepoName(String);

impl RepoName {
    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }

    #[must_use]
    pub fn owner(&self) -> &str {
        self.0.split_once('/').map_or("", |(o, _)| o)
    }

    #[must_use]
    pub fn name(&self) -> &str {
        self.0.split_once('/').map_or("", |(_, n)| n)
    }
}

fn repo_part_ok(part: &str) -> bool {
    !part.is_empty()
        && part.len() <= 100
        && part != "."
        && part != ".."
        && part
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.'))
}

impl FromStr for RepoName {
    type Err = InvalidSource;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s.split_once('/') {
            Some((owner, name)) if repo_part_ok(owner) && repo_part_ok(name) => {
                Ok(Self(s.to_ascii_lowercase()))
            }
            _ => Err(InvalidSource::Repository(s.to_owned())),
        }
    }
}

impl TryFrom<String> for RepoName {
    type Error = InvalidSource;

    fn try_from(s: String) -> Result<Self, Self::Error> {
        s.parse()
    }
}

impl From<RepoName> for String {
    fn from(r: RepoName) -> Self {
        r.0
    }
}

impl fmt::Display for RepoName {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

/// A branch name under `refs/heads/`, following git's ref-format rules.
#[derive(Clone, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct BranchName(String);

impl BranchName {
    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }

    /// The branch of a full ref, if it is one.
    #[must_use]
    pub fn from_ref(git_ref: &str) -> Option<Self> {
        git_ref.strip_prefix("refs/heads/")?.parse().ok()
    }
}

impl FromStr for BranchName {
    type Err = InvalidSource;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let bad_char = s
            .chars()
            .any(|c| c.is_control() || c.is_whitespace() || "~^:?*[\\".contains(c));
        let ok = !s.is_empty()
            && s.len() <= 255
            && !bad_char
            && !s.starts_with(['-', '/', '.'])
            && !s.ends_with(['/', '.'])
            && s.strip_suffix(".lock").is_none()
            && !s.contains("..")
            && !s.contains("//")
            && !s.contains("@{")
            && !s.split('/').any(|part| part.starts_with('.'));
        if ok {
            Ok(Self(s.to_owned()))
        } else {
            Err(InvalidSource::Branch(s.to_owned()))
        }
    }
}

impl TryFrom<String> for BranchName {
    type Error = InvalidSource;

    fn try_from(s: String) -> Result<Self, Self::Error> {
        s.parse()
    }
}

impl From<BranchName> for String {
    fn from(b: BranchName) -> Self {
        b.0
    }
}

impl fmt::Display for BranchName {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

/// A normalized path inside the repository: relative, `/`-separated, no `.`
/// or `..` segments. The empty path is the repository root.
#[derive(Clone, Debug, Default, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct RepoPath(String);

impl RepoPath {
    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }

    #[must_use]
    pub fn is_root(&self) -> bool {
        self.0.is_empty()
    }

    /// `self/child`, or `child` at the root.
    #[must_use]
    pub fn join(&self, child: &Self) -> Self {
        match (self.is_root(), child.is_root()) {
            (true, _) => child.clone(),
            (false, true) => self.clone(),
            (false, false) => Self(format!("{}/{}", self.0, child.0)),
        }
    }
}

impl FromStr for RepoPath {
    type Err = InvalidSource;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let trimmed = s.trim_start_matches("./").trim_end_matches('/');
        if trimmed.is_empty() || trimmed == "." {
            return Ok(Self::default());
        }
        let ok = trimmed.len() <= 512
            && !trimmed.starts_with('/')
            && trimmed.split('/').all(|part| {
                !part.is_empty()
                    && part != "."
                    && part != ".."
                    && !part.chars().any(|c| c.is_control() || c == '\\')
            });
        if ok {
            Ok(Self(trimmed.to_owned()))
        } else {
            Err(InvalidSource::Path(s.to_owned()))
        }
    }
}

impl TryFrom<String> for RepoPath {
    type Error = InvalidSource;

    fn try_from(s: String) -> Result<Self, Self::Error> {
        s.parse()
    }
}

impl From<RepoPath> for String {
    fn from(p: RepoPath) -> Self {
        p.0
    }
}

/// How an image is built from the source.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum BuildStrategy {
    /// The build pod uses the Dockerfile if there is one, Railpack otherwise.
    /// Detection runs inside the build pod, never on the Kuben server.
    #[default]
    Auto,
    Dockerfile,
    Railpack,
}

impl BuildStrategy {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Auto => "auto",
            Self::Dockerfile => "dockerfile",
            Self::Railpack => "railpack",
        }
    }
}

impl FromStr for BuildStrategy {
    type Err = InvalidSource;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "auto" => Ok(Self::Auto),
            "dockerfile" => Ok(Self::Dockerfile),
            "railpack" => Ok(Self::Railpack),
            other => Err(InvalidSource::Payload(format!(
                "unknown build strategy {other:?}"
            ))),
        }
    }
}

/// The editable build plan of a source binding. Any change raises the
/// target's build configuration revision, so builds of the old plan no longer
/// deploy automatically.
#[derive(Clone, Debug, Default, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct BuildRecipe {
    #[serde(default)]
    pub strategy: BuildStrategy,
    /// Build context inside the repository (monorepo sub-directory).
    #[serde(default)]
    pub context: RepoPath,
    /// Dockerfile relative to `context`; `Dockerfile` when unset.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub dockerfile: Option<RepoPath>,
}

impl BuildRecipe {
    /// The Dockerfile path relative to the context.
    #[must_use]
    pub fn dockerfile_path(&self) -> RepoPath {
        self.dockerfile
            .clone()
            .unwrap_or_else(|| RepoPath("Dockerfile".to_owned()))
    }
}

/// A push to a branch, as far as Kuben reads it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PushEvent {
    pub installation_id: u64,
    pub repository_id: u64,
    pub repository: RepoName,
    pub branch: BranchName,
    /// The head the provider claims. A hint; the worker reads the real head.
    pub after: CommitSha,
    pub forced: bool,
    pub deleted: bool,
}

/// What happened to a pull request, as far as previews care (M5.1).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PullAction {
    /// Opened, reopened or marked ready: a preview should exist.
    Opened,
    /// New commits: the preview follows its head.
    Updated,
    /// Closed or merged: the preview goes.
    Closed,
}

/// A pull request event.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PullEvent {
    pub action: PullAction,
    pub installation_id: u64,
    /// The base repository the pull request targets.
    pub repository: RepoName,
    pub repository_id: u64,
    pub number: u64,
    /// The repository the head lives in; another one for a fork.
    pub head_repository: String,
    pub head_branch: String,
    pub head: CommitSha,
    /// The pull request is still open, as the event reports it.
    pub open: bool,
    pub draft: bool,
    /// The provider's `updated_at`, in Unix milliseconds: events older than
    /// what a preview has seen are ignored.
    pub updated_at: i64,
}

impl PullEvent {
    /// The head is in another repository than the base: the change comes
    /// from outside the repository's writers.
    #[must_use]
    pub fn from_fork(&self) -> bool {
        !self
            .head_repository
            .eq_ignore_ascii_case(self.repository.as_str())
    }
}

/// The installation lifecycle events Kuben tracks.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum InstallationAction {
    Created,
    Deleted,
    Suspended,
    Unsuspended,
}

/// A parsed webhook delivery.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum WebhookEvent {
    Ping,
    Push(PushEvent),
    Installation {
        action: InstallationAction,
        installation_id: u64,
        account: String,
    },
    PullRequest(PullEvent),
    /// Tag pushes, other events and actions Kuben does not act on.
    Ignored,
}

#[derive(Deserialize)]
struct RawRepository {
    id: u64,
    full_name: String,
}

#[derive(Deserialize)]
struct RawInstallation {
    id: u64,
    #[serde(default)]
    account: Option<RawAccount>,
}

#[derive(Deserialize)]
struct RawAccount {
    login: String,
}

#[derive(Deserialize)]
struct RawPush {
    #[serde(rename = "ref")]
    git_ref: String,
    after: String,
    #[serde(default)]
    forced: bool,
    #[serde(default)]
    deleted: bool,
    repository: RawRepository,
    installation: Option<RawInstallation>,
}

#[derive(Deserialize)]
struct RawPullRepo {
    full_name: String,
}

#[derive(Deserialize)]
struct RawPullSide {
    sha: String,
    #[serde(rename = "ref")]
    git_ref: String,
    /// `null` when the fork was deleted.
    repo: Option<RawPullRepo>,
}

#[derive(Deserialize)]
struct RawPull {
    number: u64,
    state: String,
    #[serde(default)]
    draft: bool,
    updated_at: String,
    head: RawPullSide,
}

#[derive(Deserialize)]
struct RawPullEvent {
    action: String,
    pull_request: RawPull,
    repository: RawRepository,
    installation: Option<RawInstallation>,
}

#[derive(Deserialize)]
struct RawInstallationEvent {
    action: String,
    installation: RawInstallation,
}

fn payload<T: for<'de> Deserialize<'de>>(body: &[u8]) -> Result<T, InvalidSource> {
    serde_json::from_slice(body).map_err(|e| InvalidSource::Payload(e.to_string()))
}

impl WebhookEvent {
    /// Parse a GitHub delivery of type `event` (the `X-GitHub-Event` header).
    /// Call only after the signature was verified.
    pub fn parse_github(event: &str, body: &[u8]) -> Result<Self, InvalidSource> {
        match event {
            "ping" => Ok(Self::Ping),
            "push" => {
                let raw: RawPush = payload(body)?;
                let Some(branch) = BranchName::from_ref(&raw.git_ref) else {
                    return Ok(Self::Ignored);
                };
                let installation = raw
                    .installation
                    .ok_or_else(|| InvalidSource::Payload("a push without an installation".into()))?;
                Ok(Self::Push(PushEvent {
                    installation_id: installation.id,
                    repository_id: raw.repository.id,
                    repository: raw.repository.full_name.parse()?,
                    branch,
                    after: raw.after.parse()?,
                    forced: raw.forced,
                    deleted: raw.deleted,
                }))
            }
            "pull_request" => Self::parse_pull(body),
            "installation" => {
                let raw: RawInstallationEvent = payload(body)?;
                let action = match raw.action.as_str() {
                    "created" => InstallationAction::Created,
                    "deleted" => InstallationAction::Deleted,
                    "suspend" => InstallationAction::Suspended,
                    "unsuspend" => InstallationAction::Unsuspended,
                    _ => return Ok(Self::Ignored),
                };
                Ok(Self::Installation {
                    action,
                    installation_id: raw.installation.id,
                    account: raw.installation.account.map(|a| a.login).unwrap_or_default(),
                })
            }
            _ => Ok(Self::Ignored),
        }
    }

    fn parse_pull(body: &[u8]) -> Result<Self, InvalidSource> {
        let raw: RawPullEvent = payload(body)?;
        let action = match raw.action.as_str() {
            "opened" | "reopened" | "ready_for_review" => PullAction::Opened,
            "synchronize" => PullAction::Updated,
            "closed" => PullAction::Closed,
            _ => return Ok(Self::Ignored),
        };
        let installation = raw
            .installation
            .ok_or_else(|| InvalidSource::Payload("a pull request without an installation".into()))?;
        let pull = raw.pull_request;
        let updated_at = jiff::Timestamp::from_str(&pull.updated_at)
            .map_err(|e| InvalidSource::Payload(format!("updated_at: {e}")))?
            .as_millisecond();
        Ok(Self::PullRequest(PullEvent {
            action,
            installation_id: installation.id,
            repository: raw.repository.full_name.parse()?,
            repository_id: raw.repository.id,
            number: pull.number,
            // A deleted fork is still a fork.
            head_repository: pull
                .head
                .repo
                .map_or_else(|| "(deleted)".to_owned(), |r| r.full_name),
            head_branch: pull.head.git_ref,
            head: pull.head.sha.parse()?,
            open: pull.state == "open",
            draft: pull.draft,
            updated_at,
        }))
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    const SHA: &str = "0123456789abcdef0123456789abcdef01234567";

    #[test]
    fn commits_are_full_lowercase_shas() {
        assert!(SHA.parse::<CommitSha>().is_ok());
        assert!("0123456".parse::<CommitSha>().is_err());
        assert!(SHA.to_uppercase().parse::<CommitSha>().is_err());
        assert!("0".repeat(40).parse::<CommitSha>().expect("zero").is_zero());
        assert_eq!(SHA.parse::<CommitSha>().expect("sha").short(), "0123456789ab");
    }

    #[test]
    fn repositories_are_owner_slash_name() {
        let r: RepoName = "Acme/Shop.Web".parse().expect("repo");
        assert_eq!(r.as_str(), "acme/shop.web");
        assert_eq!((r.owner(), r.name()), ("acme", "shop.web"));
        for bad in ["acme", "/shop", "acme/", "acme/shop/x", "acme/..", "ac me/shop"] {
            assert!(bad.parse::<RepoName>().is_err(), "{bad}");
        }
    }

    #[test]
    fn branches_follow_ref_format() {
        for good in ["main", "release/1.2", "feat/x-y_z"] {
            assert!(good.parse::<BranchName>().is_ok(), "{good}");
        }
        for bad in [
            "",
            "-x",
            "a..b",
            "a b",
            "a~1",
            "x.lock",
            "a/",
            "/a",
            "a//b",
            "a/.hidden",
            "a@{1}",
            "a:b",
        ] {
            assert!(bad.parse::<BranchName>().is_err(), "{bad}");
        }
        assert_eq!(
            BranchName::from_ref("refs/heads/main").map(String::from),
            Some("main".to_owned())
        );
        assert_eq!(BranchName::from_ref("refs/tags/v1"), None);
    }

    #[test]
    fn repo_paths_never_escape_the_checkout() {
        assert!("".parse::<RepoPath>().expect("root").is_root());
        assert!("./".parse::<RepoPath>().expect("root").is_root());
        assert_eq!(
            "./apps/web/".parse::<RepoPath>().expect("path").as_str(),
            "apps/web"
        );
        for bad in ["../x", "a/../../b", "/etc", "a//b", "a\\b"] {
            assert!(bad.parse::<RepoPath>().is_err(), "{bad}");
        }
        let ctx: RepoPath = "apps/web".parse().expect("ctx");
        let recipe = BuildRecipe {
            context: ctx.clone(),
            ..BuildRecipe::default()
        };
        assert_eq!(
            ctx.join(&recipe.dockerfile_path()).as_str(),
            "apps/web/Dockerfile"
        );
        assert_eq!(RepoPath::default().join(&ctx), ctx);
    }

    #[test]
    fn recipes_round_trip_as_json() {
        let recipe: BuildRecipe = serde_json::from_value(json!({
            "strategy": "dockerfile", "context": "svc", "dockerfile": "build/Dockerfile"
        }))
        .expect("recipe");
        assert_eq!(recipe.strategy, BuildStrategy::Dockerfile);
        let back: BuildRecipe =
            serde_json::from_value(serde_json::to_value(&recipe).expect("json")).expect("back");
        assert_eq!(back, recipe);
        assert!(serde_json::from_value::<BuildRecipe>(json!({ "context": "../x" })).is_err());
    }

    fn push_body(git_ref: &str) -> Vec<u8> {
        json!({
            "ref": git_ref, "before": "0".repeat(40), "after": SHA, "forced": true, "deleted": false,
            "repository": { "id": 42, "full_name": "Acme/Shop" },
            "installation": { "id": 7 },
        })
        .to_string()
        .into_bytes()
    }

    #[test]
    fn pushes_to_branches_are_parsed() {
        let event = WebhookEvent::parse_github("push", &push_body("refs/heads/main")).expect("push");
        let WebhookEvent::Push(push) = event else {
            panic!("not a push: {event:?}");
        };
        assert_eq!(push.installation_id, 7);
        assert_eq!(push.repository_id, 42);
        assert_eq!(push.repository.as_str(), "acme/shop");
        assert_eq!(push.branch.as_str(), "main");
        assert!(push.forced);
    }

    #[test]
    fn tags_and_unknown_events_are_ignored() {
        let tag = WebhookEvent::parse_github("push", &push_body("refs/tags/v1")).expect("tag");
        assert_eq!(tag, WebhookEvent::Ignored);
        assert_eq!(
            WebhookEvent::parse_github("issues", b"{}").expect("other"),
            WebhookEvent::Ignored
        );
        assert_eq!(
            WebhookEvent::parse_github("ping", b"{}").expect("ping"),
            WebhookEvent::Ping
        );
        assert!(WebhookEvent::parse_github("push", b"not json").is_err());
    }

    #[test]
    fn installation_events_carry_the_account() {
        let body =
            json!({ "action": "created", "installation": { "id": 9, "account": { "login": "acme" } } });
        let event = WebhookEvent::parse_github("installation", body.to_string().as_bytes()).expect("event");
        assert_eq!(
            event,
            WebhookEvent::Installation {
                action: InstallationAction::Created,
                installation_id: 9,
                account: "acme".into(),
            }
        );
    }

    fn pull_body(action: &str, head_repo: &serde_json::Value) -> Vec<u8> {
        json!({
            "action": action,
            "number": 12,
            "pull_request": {
                "number": 12, "state": if action == "closed" { "closed" } else { "open" },
                "draft": false, "updated_at": "2026-09-17T10:00:00Z",
                "head": { "sha": SHA, "ref": "feature/x", "repo": head_repo },
                "base": { "ref": "main" }
            },
            "repository": { "id": 42, "full_name": "Acme/Shop" },
            "installation": { "id": 7 }
        })
        .to_string()
        .into_bytes()
    }

    #[test]
    fn pull_requests_become_preview_events() {
        let body = pull_body("synchronize", &json!({ "full_name": "acme/shop" }));
        let WebhookEvent::PullRequest(pull) =
            WebhookEvent::parse_github("pull_request", &body).expect("pull")
        else {
            panic!("not a pull request");
        };
        assert_eq!(
            (pull.action, pull.number, pull.installation_id),
            (PullAction::Updated, 12, 7)
        );
        assert_eq!(pull.repository.as_str(), "acme/shop");
        assert_eq!((pull.head_branch.as_str(), pull.open), ("feature/x", true));
        assert_eq!(pull.updated_at, 1_789_639_200_000);
        assert!(!pull.from_fork());

        let fork = pull_body("opened", &json!({ "full_name": "mallory/shop" }));
        let WebhookEvent::PullRequest(pull) =
            WebhookEvent::parse_github("pull_request", &fork).expect("pull")
        else {
            panic!("not a pull request");
        };
        assert_eq!(pull.action, PullAction::Opened);
        assert!(pull.from_fork());

        let gone = pull_body("closed", &serde_json::Value::Null);
        let WebhookEvent::PullRequest(pull) =
            WebhookEvent::parse_github("pull_request", &gone).expect("pull")
        else {
            panic!("not a pull request");
        };
        assert!(pull.from_fork() && !pull.open && pull.action == PullAction::Closed);

        let labeled = pull_body("labeled", &json!({ "full_name": "acme/shop" }));
        assert_eq!(
            WebhookEvent::parse_github("pull_request", &labeled).expect("labeled"),
            WebhookEvent::Ignored
        );
    }
}
