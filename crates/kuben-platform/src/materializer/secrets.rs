//! Managed secrets in the cluster (M4.4, ADR-030).
//!
//! Before a run writes its App or envelope, every secret revision it is
//! bound to is opened and written as an immutable Secret named after the
//! revision ([`crate::secrets::object_name`]); the rendered objects read from
//! those. A rotation is therefore a new object and a new pod template, never
//! a change under running pods. Once a run succeeds, the revision objects of
//! its environment that no run needs any more are removed.

use std::{collections::BTreeMap, time::Duration};

use k8s_openapi::{ByteString, api::core::v1::Secret, jiff::Timestamp};
use kube::{
    Api,
    api::{DeleteParams, ListParams, ObjectMeta, PostParams},
};
use kuben_crd::labels;
use kuben_store::repo::{BoundSecret, Materialization};

use super::{
    render::{FIELD_MANAGER, environment_name},
    worker::{Error, Stop, Worker, refused},
    write,
};
use crate::secrets::{Identity, Keyring, RegistryLogin, SECRET_ID, SECRET_REVISION, object_name};

/// Unused revision objects younger than this are kept: a run accepted a
/// moment ago may be about to use one.
const KEEP_UNUSED: Duration = Duration::from_mins(10);

/// The immutable Secret of the bound revision `b` of `m`'s run.
fn secret_object(m: &Materialization, b: &BoundSecret, keyring: &Keyring) -> Result<Secret, Stop> {
    let (org, secret) = (m.org.to_string(), b.binding.secret.to_string());
    let who = Identity {
        org: &org,
        secret: &secret,
        revision: b.binding.revision,
    };
    let values = keyring.open_values(who, &b.sealed).map_err(|e| {
        tracing::error!(run = %m.run, secret = %b.binding.name, revision = b.binding.revision, error = %e, "a secret revision does not open");
        refused("SecretUnreadable")
    })?;
    let (type_, data) = match &b.binding.registry {
        None => (
            "Opaque",
            values
                .into_iter()
                .map(|(k, v)| (k, ByteString(v.into_bytes())))
                .collect(),
        ),
        Some(registry) => {
            let login = RegistryLogin::from_values(&values).ok_or_else(|| refused("SecretUnreadable"))?;
            (
                "kubernetes.io/dockerconfigjson",
                BTreeMap::from([(
                    ".dockerconfigjson".to_owned(),
                    ByteString(login.docker_config(registry).into_bytes()),
                )]),
            )
        }
    };
    Ok(Secret {
        metadata: ObjectMeta {
            name: Some(object_name(&b.binding.name, b.binding.revision)),
            namespace: Some(m.namespace.clone()),
            labels: Some(BTreeMap::from([
                (labels::MANAGED_BY.to_owned(), labels::MANAGER.to_owned()),
                (labels::ORG.to_owned(), org),
                (labels::PROJECT.to_owned(), m.project_slug.clone()),
                (
                    labels::ENVIRONMENT.to_owned(),
                    environment_name(&m.project_slug, &m.environment_slug),
                ),
                (SECRET_ID.to_owned(), secret),
                (SECRET_REVISION.to_owned(), b.binding.revision.to_string()),
            ])),
            ..ObjectMeta::default()
        },
        immutable: Some(true),
        type_: Some(type_.into()),
        data: Some(data),
        ..Secret::default()
    })
}

/// The live object is the revision `b`: Kuben wrote it for this
/// organization, secret and revision.
fn is_revision(live: &Secret, m: &Materialization, b: &BoundSecret) -> bool {
    let l = live.metadata.labels.as_ref();
    let label = |key: &str| l.and_then(|l| l.get(key)).map(String::as_str);
    write::belongs_to(&live.metadata, m.org)
        && label(SECRET_ID) == Some(b.binding.secret.to_string().as_str())
        && label(SECRET_REVISION) == Some(b.binding.revision.to_string().as_str())
}

/// The `(secret, revision)` a revision object carries.
fn revision_of(s: &Secret) -> Option<(uuid::Uuid, u64)> {
    let l = s.metadata.labels.as_ref()?;
    Some((
        l.get(SECRET_ID)?.parse().ok()?,
        l.get(SECRET_REVISION)?.parse().ok()?,
    ))
}

impl Worker {
    /// Write the Secret of every revision `m`'s run is bound to. A revoked
    /// revision fails the run.
    pub(super) async fn write_secrets(&self, m: &Materialization) -> Result<(), Stop> {
        if m.secrets.is_empty() {
            return Ok(());
        }
        let Some(keyring) = self.keyring.as_deref() else {
            return Err(refused("SecretsUnavailable"));
        };
        let bound = {
            let mut tenant = self.store.tenant(m.org).await?;
            tenant.run_secrets(m.run).await?
        };
        let api = Api::<Secret>::namespaced(self.client.clone(), &m.namespace);
        let params = PostParams {
            field_manager: Some(FIELD_MANAGER.into()),
            ..PostParams::default()
        };
        for b in &bound {
            if b.revoked {
                return Err(refused("SecretRevoked"));
            }
            let name = object_name(&b.binding.name, b.binding.revision);
            match api.get_opt(&name).await? {
                Some(live) if is_revision(&live, m, b) => continue,
                Some(_) => return Err(refused("NameTaken")),
                None => {}
            }
            let desired = secret_object(m, b, keyring)?;
            match api.create(&params, &desired).await {
                Ok(_) => tracing::info!(run = %m.run, secret = %name, "secret revision written"),
                // Written meanwhile: the next attempt checks what is there.
                Err(e) if write::is_conflict(&e) => return Err(Error::Contended(name).into()),
                Err(e) => return Err(e.into()),
            }
        }
        Ok(())
    }

    /// Remove the revision objects of `m`'s environment that no run needs
    /// any more. The number removed.
    pub(super) async fn collect_secrets(&self, m: &Materialization) -> Result<usize, Error> {
        let in_use = {
            let mut tenant = self.store.tenant(m.org).await?;
            tenant.secret_revisions_in_use(m.environment).await?
        };
        let api = Api::<Secret>::namespaced(self.client.clone(), &m.namespace);
        let selector = format!(
            "{},{}={},{SECRET_ID}",
            labels::MANAGED_SELECTOR,
            labels::ORG,
            m.org
        );
        let listed = api.list(&ListParams::default().labels(&selector)).await?;
        let cutoff =
            Timestamp::now().as_millisecond() - i64::try_from(KEEP_UNUSED.as_millis()).unwrap_or(i64::MAX);
        let mut removed = 0;
        for s in &listed.items {
            let Some(revision) = revision_of(s) else {
                continue;
            };
            let young = s
                .metadata
                .creation_timestamp
                .as_ref()
                .is_none_or(|t| t.0.as_millisecond() > cutoff);
            if in_use.contains(&revision) || young {
                continue;
            }
            let name = s.metadata.name.clone().unwrap_or_default();
            match api.delete(&name, &DeleteParams::default()).await {
                Ok(_) => removed += 1,
                Err(kube::Error::Api(status)) if status.code == 404 => {}
                Err(e) => return Err(e.into()),
            }
        }
        if removed > 0 {
            tracing::info!(namespace = %m.namespace, removed, "unused secret revisions removed");
        }
        Ok(removed)
    }
}

#[cfg(test)]
mod tests {
    use kuben_core::{
        ids::{
            ApplicationId, ClusterId, ConfigRevisionId, DeploymentRunId, EnvironmentId, OperationId, OrgId,
            ProjectId, ReleaseId, TargetId,
        },
        ops::{Generation, RunPhase},
    };
    use kuben_store::repo::{Delivery, SecretBinding};
    use serde_json::json;
    use uuid::Uuid;

    use super::*;

    fn materialization() -> Materialization {
        Materialization {
            org: OrgId::new(),
            run: DeploymentRunId::new(),
            operation: OperationId::new(),
            phase: RunPhase::PendingDelivery,
            generation: Generation(1),
            lifecycle_uid: Uuid::now_v7(),
            project: ProjectId::new(),
            project_slug: "shop".into(),
            project_name: "Shop".into(),
            project_description: None,
            environment: EnvironmentId::new(),
            environment_slug: "prod".into(),
            environment_name: "Prod".into(),
            protected: false,
            env_type: "standard".into(),
            quota: None,
            namespace: "kb-shop-prod".into(),
            cluster: ClusterId::new(),
            delivery: Delivery::Controller,
            application: ApplicationId::new(),
            application_slug: "web".into(),
            application_name: "Web".into(),
            target: TargetId::new(),
            desired_generation: Generation(1),
            deleting: false,
            release: ReleaseId::new(),
            artifacts: BTreeMap::new(),
            image_repository: None,
            config_revision: ConfigRevisionId::new(),
            config_revision_number: 1,
            config: json!({}),
            render_plan: None,
            restarted_at: None,
            approval_expires_at: None,
            secrets: Vec::new(),
        }
    }

    fn bound(m: &Materialization, keyring: &Keyring, revision: u64) -> BoundSecret {
        let values = BTreeMap::from([("url".to_owned(), "postgres://db".to_owned())]);
        sealed_binding(m, keyring, revision, &values, None)
    }

    fn sealed_binding(
        m: &Materialization,
        keyring: &Keyring,
        revision: u64,
        values: &BTreeMap<String, String>,
        registry: Option<&str>,
    ) -> BoundSecret {
        let secret = Uuid::now_v7();
        let (org, id) = (m.org.to_string(), secret.to_string());
        let sealed = keyring
            .seal_values(
                Identity {
                    org: &org,
                    secret: &id,
                    revision,
                },
                values,
            )
            .expect("seal");
        BoundSecret {
            binding: SecretBinding {
                name: "db".into(),
                secret,
                revision,
                registry: registry.map(str::to_owned),
            },
            keys: vec!["url".into()],
            sealed,
            revoked: false,
        }
    }

    #[test]
    fn a_revision_becomes_an_immutable_labelled_secret() {
        let keyring = Keyring::from_keys([(1, [3; 32])]);
        let m = materialization();
        let b = bound(&m, &keyring, 2);
        let s = secret_object(&m, &b, &keyring).expect("object");
        assert_eq!(s.metadata.name.as_deref(), Some("db.r2"));
        assert_eq!(s.metadata.namespace.as_deref(), Some("kb-shop-prod"));
        assert_eq!(s.immutable, Some(true));
        assert_eq!(
            s.data.as_ref().and_then(|d| d.get("url")).map(|v| v.0.as_slice()),
            Some(b"postgres://db".as_slice())
        );
        assert!(is_revision(&s, &m, &b));
        assert_eq!(revision_of(&s), Some((b.binding.secret, 2)));
        let other = bound(&m, &keyring, 2);
        assert!(!is_revision(&s, &m, &other), "another secret of the same name");
        let foreign = Materialization {
            org: OrgId::new(),
            ..materialization()
        };
        assert!(!is_revision(&s, &foreign, &b), "another organization");
    }

    #[test]
    fn a_registry_login_becomes_a_pull_secret() {
        let keyring = Keyring::from_keys([(1, [3; 32])]);
        let m = materialization();
        let login = RegistryLogin {
            username: "bot".into(),
            password: "token".into(),
        };
        let b = sealed_binding(&m, &keyring, 1, &login.values(), Some("ghcr.io"));
        let s = secret_object(&m, &b, &keyring).expect("object");
        assert_eq!(s.type_.as_deref(), Some("kubernetes.io/dockerconfigjson"));
        let data = s.data.expect("data");
        let config: serde_json::Value = serde_json::from_slice(&data[".dockerconfigjson"].0).expect("json");
        assert_eq!(config["auths"]["ghcr.io"]["username"], "bot");
        let broken = sealed_binding(&m, &keyring, 1, &BTreeMap::new(), Some("ghcr.io"));
        assert!(secret_object(&m, &broken, &keyring).is_err(), "not a login");
    }

    #[test]
    fn a_revision_sealed_for_another_run_does_not_open() {
        let keyring = Keyring::from_keys([(1, [3; 32])]);
        let m = materialization();
        let mut b = bound(&m, &keyring, 2);
        b.binding.revision = 3;
        assert!(matches!(
            secret_object(&m, &b, &keyring),
            Err(Stop::Refused(code)) if code == "SecretUnreadable"
        ));
        let elsewhere = Keyring::from_keys([(1, [4; 32])]);
        let b = bound(&m, &keyring, 2);
        assert!(secret_object(&m, &b, &elsewhere).is_err());
    }
}
