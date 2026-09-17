//! The image update watcher (M5.4): apps that follow a tag pattern of their
//! repository get a new release when the pattern's tag moves.
//!
//! Every replica runs one. A pass:
//! 1. Leases the due policies (their next check moves past the pass), so
//!    replicas never check the same app twice.
//! 2. For each app, lists the repository's tags (unless the policy follows
//!    one fixed tag) with the environment's registry login, and resolves the
//!    chosen tag to a digest.
//! 3. When the digest is new, deploys it with the app's newest
//!    configuration. A protected environment's run waits for approval, like
//!    any other.
//! 4. Records the outcome. A failure backs off exponentially, up to a day,
//!    and a registry's `Retry-After` is honored.

use std::{sync::Arc, time::Duration};

use kuben_core::{
    ids::OrgId,
    image_policy::{Pattern, next_check},
    time::now_ms,
};
use kuben_platform::{health::Health, secrets::Keyring};
use kuben_store::{
    Store, StoreError,
    repo::{Checked, ImagePolicy, Started},
};
use tokio_util::sync::CancellationToken;

use crate::oci::{ImageResolver, ResolveError, parse};

const HEALTH: &str = "image-policies";
const TICK: Duration = Duration::from_secs(30);
const BATCH: i64 = 10;
/// How long a leased policy is hidden from other replicas.
const LEASE_MS: i64 = 10 * 60_000;

/// What checking one policy found.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Found {
    /// The app runs the pattern's digest already.
    Current {
        tag: String,
        digest: String,
    },
    /// A run of the new digest was started.
    Deployed {
        tag: String,
        digest: String,
        run: uuid::Uuid,
    },
    /// The run was not started, for this reason.
    Refused {
        tag: String,
        digest: String,
        why: String,
    },
    Failed {
        why: String,
        retry_after: Option<u64>,
    },
}

/// The watcher of one process.
#[derive(Clone)]
pub struct Watcher {
    store: Store,
    images: Arc<dyn ImageResolver>,
    keyring: Option<Arc<Keyring>>,
}

impl std::fmt::Debug for Watcher {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Watcher").finish_non_exhaustive()
    }
}

impl Watcher {
    #[must_use]
    pub fn new(store: Store, images: Arc<dyn ImageResolver>, keyring: Option<Arc<Keyring>>) -> Self {
        Self {
            store,
            images,
            keyring,
        }
    }

    /// Check every due policy once. The number checked.
    pub async fn pass(&self) -> Result<usize, StoreError> {
        let mut checked = 0;
        for org in self.store.image_policy_orgs().await? {
            let now = now_ms();
            let leased = {
                let mut tenant = self.store.tenant(org).await?;
                let due = tenant.due_image_policies(now, BATCH).await?;
                for p in &due {
                    let lease = Checked {
                        next_check_at: now + LEASE_MS,
                        failures: u32::try_from(p.failures).unwrap_or(0),
                        error: p.last_error.as_deref(),
                        ..Checked::default()
                    };
                    tenant.image_policy_checked(p.target(), &lease).await?;
                }
                tenant.commit().await?;
                due
            };
            for policy in leased {
                let found = self.check(org, &policy).await;
                self.record(org, &policy, &found).await?;
                checked += 1;
            }
        }
        Ok(checked)
    }

    async fn check(&self, org: OrgId, policy: &ImagePolicy) -> Found {
        let failed = |why: String| Found::Failed {
            why,
            retry_after: None,
        };
        let pattern = match Pattern::parse(&policy.pattern) {
            Ok(p) => p,
            Err(e) => return failed(e.to_string()),
        };
        let registry = match parse(&policy.repository) {
            Ok(r) => r.registry,
            Err(e) => return failed(e.to_string()),
        };
        let login = match (&self.keyring, self.store.tenant(org).await) {
            (Some(keyring), Ok(mut tenant)) => {
                match crate::routes::secrets::open_registry_login(
                    keyring,
                    &mut tenant,
                    org,
                    policy.environment(),
                    &registry,
                )
                .await
                {
                    Ok(login) => login,
                    Err(e) => return failed(e.to_string()),
                }
            }
            (_, Err(e)) => return failed(e.to_string()),
            (None, Ok(_)) => None,
        };
        let tags = if pattern.needs_listing() {
            match self.images.list_tags(&policy.repository, login.as_ref()).await {
                Ok(tags) => tags,
                Err(ResolveError::RateLimited { retry_after, .. }) => {
                    return Found::Failed {
                        why: "the registry limits requests".into(),
                        retry_after: Some(retry_after),
                    };
                }
                Err(e) => return failed(e.to_string()),
            }
        } else {
            Vec::new()
        };
        let Some(tag) = pattern.select(&tags) else {
            return failed(format!(
                "no tag of {} matches {}",
                policy.repository, policy.pattern
            ));
        };
        let image = format!("{}:{tag}", policy.repository);
        let resolved = match self.images.resolve_as(&image, login.as_ref()).await {
            Ok(r) => r,
            Err(ResolveError::RateLimited { retry_after, .. }) => {
                return Found::Failed {
                    why: "the registry limits requests".into(),
                    retry_after: Some(retry_after),
                };
            }
            Err(e) => return failed(e.to_string()),
        };
        let digest = resolved.digest.to_string();
        self.deploy(org, policy, tag, &resolved.digest, &image, digest)
            .await
    }

    async fn deploy(
        &self,
        org: OrgId,
        policy: &ImagePolicy,
        tag: String,
        digest: &kuben_core::artifact::Digest,
        image: &str,
        text: String,
    ) -> Found {
        let run = async {
            let mut tenant = self.store.tenant(org).await?;
            if tenant.current_digest(policy.target()).await?.as_deref() == Some(text.as_str()) {
                return Ok::<_, StoreError>(None);
            }
            let started = tenant.deploy_followed_image(policy, digest, image).await?;
            tenant.commit().await?;
            Ok(Some(started))
        };
        match run.await {
            Ok(None) => Found::Current { tag, digest: text },
            Ok(Some(Started::Accepted { run, .. })) => Found::Deployed {
                tag,
                digest: text,
                run: *run.as_uuid(),
            },
            Ok(Some(other)) => Found::Refused {
                tag,
                digest: text,
                why: refusal(other),
            },
            Err(e) => Found::Failed {
                why: e.to_string(),
                retry_after: None,
            },
        }
    }

    async fn record(&self, org: OrgId, policy: &ImagePolicy, found: &Found) -> Result<(), StoreError> {
        let now = now_ms();
        let interval = u32::try_from(policy.interval_secs).unwrap_or(300);
        let failures = u32::try_from(policy.failures).unwrap_or(0);
        let checked = match found {
            Found::Current { tag, digest } => Checked {
                next_check_at: next_check(now, interval, 0, None),
                tag: Some(tag),
                digest: Some(digest),
                ..Checked::default()
            },
            Found::Deployed { tag, digest, run } => {
                tracing::info!(target = %policy.target_id, %tag, %digest, %run, "a followed image was deployed");
                Checked {
                    next_check_at: next_check(now, interval, 0, None),
                    tag: Some(tag),
                    digest: Some(digest),
                    run: Some(kuben_core::ids::DeploymentRunId::from_uuid(*run)),
                    ..Checked::default()
                }
            }
            Found::Refused { tag, digest, why } => Checked {
                next_check_at: next_check(now, interval, failures + 1, None),
                failures: failures + 1,
                error: Some(why),
                tag: Some(tag),
                digest: Some(digest),
                ..Checked::default()
            },
            Found::Failed { why, retry_after } => Checked {
                next_check_at: next_check(now, interval, failures + 1, *retry_after),
                failures: failures + 1,
                error: Some(why),
                ..Checked::default()
            },
        };
        let mut tenant = self.store.tenant(org).await?;
        tenant.image_policy_checked(policy.target(), &checked).await?;
        tenant.commit().await
    }
}

fn refusal(started: Started) -> String {
    match started {
        Started::Rejected(reject) => reject.to_string(),
        Started::SecretRevoked => "a secret the app uses is revoked".into(),
        Started::VulnerabilityBlocked => "the environment's vulnerability gate refuses the image".into(),
        Started::Frozen => "the environment is frozen".into(),
        Started::Untrusted => "an untrusted preview cannot use secrets".into(),
        Started::NotFound => "the app has no configuration yet".into(),
        Started::Accepted { .. } | Started::Replayed(_) | Started::KeyReused(_) => {
            "the run was accepted before".into()
        }
    }
}

/// Run `watcher` until `token` is cancelled.
pub async fn run(watcher: Watcher, health: Health, token: CancellationToken) -> anyhow::Result<()> {
    health.ok(HEALTH);
    loop {
        tokio::select! {
            () = token.cancelled() => return Ok(()),
            () = tokio::time::sleep(TICK) => {}
        }
        watcher.pass().await?;
    }
}
