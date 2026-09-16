//! The agent inside the cluster Kuben runs on (M2.8): no one hands it a
//! token by hand.
//!
//! * [`Enrollment`]: the hub publishes where it listens, its CA, this
//!   cluster's id and, while the agent has no valid certificate, a bootstrap
//!   token, as the files of a Secret mounted into the pod. The agent waits for
//!   them.
//! * [`IdentitySecret`]: the device key and the certificate live in a Secret
//!   of the agent's own, so a restarted or rescheduled pod keeps its identity
//!   instead of needing a new token.

use std::{
    collections::BTreeMap,
    fs,
    path::{Path, PathBuf},
    time::Duration,
};

use k8s_openapi::{ByteString, api::core::v1::Secret};
use kube::{
    Api, Client,
    api::{Patch, PatchParams},
};
use tokio_util::sync::CancellationToken;

use crate::state::{CERTIFICATE, DEVICE_KEY};

/// File names in the enrollment directory.
pub const HUB_FILE: &str = "hub";
pub const HUB_CA_FILE: &str = "hub-ca.crt";
pub const CLUSTER_FILE: &str = "cluster";
pub const TOKEN_FILE: &str = "token";
/// Where a pod finds its own namespace.
const NAMESPACE_FILE: &str = "/var/run/secrets/kubernetes.io/serviceaccount/namespace";
const POLL: Duration = Duration::from_secs(5);
const FIELD_MANAGER: &str = "kuben-agent";

/// What the hub published for this cluster's agent.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Enrollment {
    pub hub: String,
    pub hub_ca: PathBuf,
    pub cluster: String,
    /// The bootstrap token's file; it may be absent once the agent enrolled.
    pub token_file: PathBuf,
}

fn read_trimmed(path: &Path) -> Option<String> {
    let text = fs::read_to_string(path).ok()?;
    let text = text.trim();
    (!text.is_empty()).then(|| text.to_owned())
}

impl Enrollment {
    /// The enrollment in `dir`, once the hub published it.
    #[must_use]
    pub fn read(dir: &Path) -> Option<Self> {
        let hub_ca = dir.join(HUB_CA_FILE);
        read_trimmed(&hub_ca)?;
        Some(Self {
            hub: read_trimmed(&dir.join(HUB_FILE))?,
            cluster: read_trimmed(&dir.join(CLUSTER_FILE))?,
            hub_ca,
            token_file: dir.join(TOKEN_FILE),
        })
    }

    /// Wait until the hub has published the enrollment in `dir` (a mounted
    /// Secret appears in the pod a little after it is written). `None` when
    /// cancelled.
    pub async fn wait(dir: &Path, token: &CancellationToken) -> Option<Self> {
        let mut said = false;
        loop {
            if let Some(enrollment) = Self::read(dir) {
                return Some(enrollment);
            }
            if !said {
                tracing::info!(dir = %dir.display(), "waiting for the hub to publish this cluster's enrollment");
                said = true;
            }
            tokio::select! {
                () = token.cancelled() => return None,
                () = tokio::time::sleep(POLL) => {}
            }
        }
    }
}

/// The pod's own namespace.
#[must_use]
pub fn own_namespace() -> Option<String> {
    read_trimmed(Path::new(NAMESPACE_FILE))
}

/// The Secret holding the agent's device key and certificate.
#[derive(Clone)]
pub struct IdentitySecret {
    api: Api<Secret>,
    name: String,
}

impl std::fmt::Debug for IdentitySecret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("IdentitySecret")
            .field("name", &self.name)
            .finish_non_exhaustive()
    }
}

const FILES: [&str; 2] = [DEVICE_KEY, CERTIFICATE];

impl IdentitySecret {
    #[must_use]
    pub fn new(client: Client, namespace: &str, name: &str) -> Self {
        Self {
            api: Api::namespaced(client, namespace),
            name: name.to_owned(),
        }
    }

    /// Write the stored identity into the state directory `dir`, over what is
    /// there. `false` when the Secret holds none yet.
    pub async fn restore(&self, dir: &Path) -> Result<bool, kube::Error> {
        let Some(secret) = self.api.get_opt(&self.name).await? else {
            return Ok(false);
        };
        let data = secret.data.unwrap_or_default();
        let mut restored = false;
        for file in FILES {
            if let Some(ByteString(bytes)) = data.get(file) {
                write_private(&dir.join(file), bytes).map_err(|e| kube::Error::Service(e.into()))?;
                restored = true;
            }
        }
        Ok(restored)
    }

    /// Keep the identity files of `dir` in the Secret.
    pub async fn save(&self, dir: &Path) -> Result<(), kube::Error> {
        let data: BTreeMap<String, ByteString> = FILES
            .iter()
            .filter_map(|file| {
                fs::read(dir.join(file))
                    .ok()
                    .map(|b| ((*file).to_owned(), ByteString(b)))
            })
            .collect();
        let body = serde_json::json!({
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {
                "name": self.name,
                "labels": { "app.kubernetes.io/managed-by": "kuben" },
            },
            "type": "Opaque",
            "data": data,
        });
        self.api
            .patch(
                &self.name,
                &PatchParams::apply(FIELD_MANAGER).force(),
                &Patch::Apply(&body),
            )
            .await?;
        Ok(())
    }
}

fn write_private(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    use std::io::Write as _;
    fs::remove_file(path).ok();
    let mut options = fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    options.open(path)?.write_all(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn the_enrollment_is_waited_for_until_it_is_complete() {
        let dir = std::env::temp_dir().join(format!("kuben-enrollment-{}", std::process::id()));
        fs::create_dir_all(&dir).expect("dir");
        assert_eq!(Enrollment::read(&dir), None);
        fs::write(dir.join(HUB_FILE), "kuben.kuben-system.svc:7443\n").expect("hub");
        fs::write(dir.join(CLUSTER_FILE), "0190-cluster").expect("cluster");
        assert_eq!(Enrollment::read(&dir), None, "no CA yet");
        fs::write(dir.join(HUB_CA_FILE), " \n").expect("empty ca");
        assert_eq!(Enrollment::read(&dir), None, "an empty CA is none");

        let waiter = tokio::spawn({
            let dir = dir.clone();
            async move { Enrollment::wait(&dir, &CancellationToken::new()).await }
        });
        fs::write(dir.join(HUB_CA_FILE), "-----BEGIN CERTIFICATE-----").expect("ca");
        let got = tokio::time::timeout(Duration::from_secs(15), waiter)
            .await
            .expect("in time")
            .expect("join")
            .expect("enrollment");
        assert_eq!(got.hub, "kuben.kuben-system.svc:7443");
        assert_eq!(got.cluster, "0190-cluster");
        assert_eq!(got.token_file, dir.join(TOKEN_FILE));

        let cancelled = CancellationToken::new();
        cancelled.cancel();
        let empty = dir.join("empty");
        fs::create_dir_all(&empty).expect("empty dir");
        assert_eq!(Enrollment::wait(&empty, &cancelled).await, None);
        fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn private_files_replace_what_is_there() {
        let dir = std::env::temp_dir().join(format!("kuben-identity-{}", std::process::id()));
        fs::create_dir_all(&dir).expect("dir");
        let file = dir.join(DEVICE_KEY);
        write_private(&file, b"old").expect("first");
        write_private(&file, b"new").expect("second");
        assert_eq!(fs::read(&file).expect("read"), b"new");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            assert_eq!(
                fs::metadata(&file).expect("meta").permissions().mode() & 0o777,
                0o600
            );
        }
        fs::remove_dir_all(&dir).ok();
    }
}
