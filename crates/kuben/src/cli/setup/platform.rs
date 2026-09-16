//! The managed path (M2.7, ADR-031): a pinned k3s, installed with a verified
//! installer and with reservations, and on it what makes apps reachable over
//! HTTPS: k3s's Traefik as the Gateway API provider, the Gateway API CRDs
//! (standard channel), cert-manager with Gateway API support, a Let's Encrypt
//! ClusterIssuer when an email is given, and the `KubenConfig` that has Kuben
//! create and own its Gateway.
//!
//! Every object is created only when it is missing and recorded in the
//! journal; what is there already is used and never upgraded or taken over.
//! A cluster brought with `--kubeconfig` gets nothing: `kuben doctor` reports
//! what it lacks.

use std::{path::Path, process::Command, time::Duration};

use anyhow::{Context as _, bail};
use k8s_openapi::{
    api::apps::v1::Deployment,
    apiextensions_apiserver::pkg::apis::apiextensions::v1::CustomResourceDefinition,
};
use kube::{
    Api, Client, ResourceExt,
    api::{DeleteParams, DynamicObject, GroupVersionKind, Patch, PatchParams},
    config::KubeConfigOptions,
    discovery::{Scope, pinned_kind},
};
use serde::Deserialize as _;
use serde_json::{Value, json};
use sha2::{Digest as _, Sha256};

use super::{
    journal::{Book, Journal, Kind},
    run_env, tail, which,
};
use crate::cli::ui::Ui;

/// The k3s release the managed path installs.
pub const K3S_VERSION: &str = "v1.36.4+k3s1";
/// k3s's installer for exactly that release, and its SHA-256: the script
/// verifies the k3s binary against the release's checksums in turn.
const K3S_INSTALLER: &str = "https://raw.githubusercontent.com/k3s-io/k3s/v1.36.4%2Bk3s1/install.sh";
const K3S_INSTALLER_SHA256: &str = "46177d4c99440b4c0311b67233823a8e8a2fc09693f6c89af1a7161e152fbfad";
pub const GATEWAY_API_VERSION: &str = "v1.5.1";
const GATEWAY_API_URL: &str =
    "https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml";
const GATEWAY_API_SHA256: &str = "751002b3b91a87f7ae3bd2517c79a47a8d7ed6702901808a1cf9bd97d284f9b8";
pub const CERT_MANAGER_VERSION: &str = "v1.21.2";
/// The GatewayClass of k3s's Traefik, and the entry points its listeners use.
pub const GATEWAY_CLASS: &str = "traefik";
const TRAEFIK_PORTS: (u16, u16) = (8000, 8443);
pub const ISSUER: &str = "letsencrypt";
pub const GATEWAY_NAMESPACE: &str = "kuben-system";
/// Field manager and label value of what setup writes into the cluster.
const MANAGER: &str = "kuben-setup";
const MANAGED_BY: &str = "app.kubernetes.io/managed-by";

// Journal names of the cluster objects setup may create.
pub const CRDS: &str = "crd/gateway-api";
pub const TRAEFIK_CONFIG: &str = "helmchartconfig/kube-system/traefik";
pub const CERT_MANAGER: &str = "helmchart/kube-system/kuben-cert-manager";
pub const NAMESPACE: &str = "namespace/kuben-system";
pub const CLUSTER_ISSUER: &str = "clusterissuer/letsencrypt";
pub const KUBEN_CONFIG: &str = "kubenconfig/kuben";
pub const AGENT: &str = "agent/kuben-system/kuben-agent";
pub const CONSOLE: &str = "console/kuben-console/kuben-console";
/// Where the console's route lives: a namespace of Kuben's own label, which
/// the listeners of Kuben's Gateway accept routes from.
pub const CONSOLE_NAMESPACE: &str = "kuben-console";
/// The agent's manifest, shared with the Helm chart.
const AGENT_MANIFEST: &str = include_str!("../../../../../charts/kuben/files/agent.yaml");

/// How k3s keeps its own state.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, clap::ValueEnum)]
pub enum Datastore {
    /// SQLite: one server.
    #[default]
    Sqlite,
    /// Embedded etcd: a server that will grow to several.
    Etcd,
}

/// What the platform step is asked for.
#[derive(Clone, Debug, Default)]
pub struct Wanted<'a> {
    pub domain: Option<&'a str>,
    pub acme_email: Option<&'a str>,
    pub acme_staging: bool,
}

/// `INSTALL_K3S_EXEC` for `datastore`: reservations for the system and
/// Kubernetes, and eviction before the node runs out.
#[must_use]
pub fn k3s_exec(datastore: Datastore) -> String {
    let mut exec = vec![
        "server",
        "--kubelet-arg=system-reserved=cpu=100m,memory=256Mi",
        "--kubelet-arg=kube-reserved=cpu=100m,memory=256Mi",
        "--kubelet-arg=eviction-hard=memory.available<100Mi,nodefs.available<10%",
    ];
    if datastore == Datastore::Etcd {
        exec.push("--cluster-init");
    }
    exec.join(" ")
}

/// Download `url` to `to` and check its SHA-256.
fn download_verified(url: &str, sha256: &str, to: &Path) -> anyhow::Result<()> {
    if which("curl").is_none() {
        bail!("install curl first (apt-get install -y curl), then run kuben setup again");
    }
    let to_str = to.to_str().context("a download path is not UTF-8")?;
    run_env(
        &[
            "curl",
            "-fsSL",
            "--proto",
            "=https",
            "--tlsv1.2",
            "-o",
            to_str,
            url,
        ],
        &[],
    )?;
    let body = std::fs::read(to)?;
    let got = hex(&Sha256::digest(&body));
    if got != sha256 {
        std::fs::remove_file(to).ok();
        bail!("{url} does not match its pinned checksum (sha256 {got}, expected {sha256}); nothing was run");
    }
    Ok(())
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes.iter().fold(String::new(), |mut s, b| {
        let _ = write!(s, "{b:02x}");
        s
    })
}

/// Install [`K3S_VERSION`] with its verified installer.
pub fn install_k3s(ui: Ui, datastore: Datastore) -> anyhow::Result<()> {
    let step = ui.step(format!("Installing k3s {K3S_VERSION}"));
    let script = std::env::temp_dir().join(format!("kuben-k3s-install-{}.sh", std::process::id()));
    if let Err(e) = download_verified(K3S_INSTALLER, K3S_INSTALLER_SHA256, &script) {
        step.fail("the installer could not be verified");
        return Err(e);
    }
    let exec = k3s_exec(datastore);
    ui.command(&format!(
        "INSTALL_K3S_VERSION={K3S_VERSION} INSTALL_K3S_EXEC=\"{exec}\" sh install.sh"
    ));
    let output = Command::new("sh")
        .arg(&script)
        .env("INSTALL_K3S_VERSION", K3S_VERSION)
        .env("INSTALL_K3S_EXEC", &exec)
        .output()
        .context("running the k3s installer")?;
    std::fs::remove_file(&script).ok();
    if !output.status.success() {
        step.fail("the k3s installer failed");
        ui.note(&tail(&output.stdout, &output.stderr, 12));
        bail!("k3s did not install; the lines above are its last output (journalctl -u k3s has more)");
    }
    step.done(format!("{K3S_VERSION}, {datastore:?} datastore"));
    Ok(())
}

/// A client for `kubeconfig`, on a runtime of its own (setup is synchronous).
fn connect(runtime: &tokio::runtime::Runtime, kubeconfig: &Path) -> anyhow::Result<Client> {
    runtime.block_on(async {
        let raw = kube::config::Kubeconfig::read_from(kubeconfig)?;
        let config = kube::Config::from_custom_kubeconfig(raw, &KubeConfigOptions::default()).await?;
        Ok(Client::try_from(config)?)
    })
}

fn runtime() -> anyhow::Result<tokio::runtime::Runtime> {
    Ok(tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()?)
}

async fn dynamic_api(client: &Client, object: &DynamicObject) -> anyhow::Result<Api<DynamicObject>> {
    let types = object
        .types
        .as_ref()
        .context("an object without apiVersion and kind")?;
    let gvk = GroupVersionKind::try_from(types)?;
    let (resource, capabilities) = pinned_kind(client, &gvk).await?;
    Ok(match capabilities.scope {
        Scope::Cluster => Api::all_with(client.clone(), &resource),
        Scope::Namespaced => Api::namespaced_with(
            client.clone(),
            object.namespace().as_deref().unwrap_or("default"),
            &resource,
        ),
    })
}

fn object(value: Value) -> anyhow::Result<DynamicObject> {
    Ok(serde_json::from_value(value)?)
}

/// Whether `object` exists, and whether setup wrote it (its field manager).
async fn existing(client: &Client, object: &DynamicObject) -> anyhow::Result<Option<bool>> {
    let api = dynamic_api(client, object).await?;
    Ok(api.get_opt(&object.name_any()).await?.map(|live| {
        live.managed_fields()
            .iter()
            .any(|f| f.manager.as_deref() == Some(MANAGER))
    }))
}

/// Apply `object` as setup; `force` takes fields over from other managers
/// (only for objects setup created).
async fn apply(client: &Client, object: &DynamicObject, force: bool) -> anyhow::Result<()> {
    let api = dynamic_api(client, object).await?;
    let mut params = PatchParams::apply(MANAGER);
    params.force = force;
    api.patch(&object.name_any(), &params, &Patch::Apply(object))
        .await?;
    Ok(())
}

/// Poll `check` every two seconds until it says yes or `timeout` passes.
async fn eventually<F, Fut>(timeout: Duration, mut check: F) -> bool
where
    F: FnMut() -> Fut,
    Fut: Future<Output = bool>,
{
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        if check().await {
            return true;
        }
        if tokio::time::Instant::now() >= deadline {
            return false;
        }
        tokio::time::sleep(Duration::from_secs(2)).await;
    }
}

fn labelled(mut value: Value) -> Value {
    value["metadata"]["labels"] = json!({ MANAGED_BY: MANAGER });
    value
}

fn traefik_config() -> Value {
    labelled(json!({
        "apiVersion": "helm.cattle.io/v1",
        "kind": "HelmChartConfig",
        "metadata": { "name": "traefik", "namespace": "kube-system" },
        "spec": { "valuesContent": "providers:\n  kubernetesGateway:\n    enabled: true\ngateway:\n  enabled: false\n" },
    }))
}

fn cert_manager_chart() -> Value {
    labelled(json!({
        "apiVersion": "helm.cattle.io/v1",
        "kind": "HelmChart",
        "metadata": { "name": "kuben-cert-manager", "namespace": "kube-system" },
        "spec": {
            "repo": "https://charts.jetstack.io",
            "chart": "cert-manager",
            "version": CERT_MANAGER_VERSION,
            "targetNamespace": "cert-manager",
            "createNamespace": true,
            "valuesContent": "crds:\n  enabled: true\n  keep: true\nstartupapicheck:\n  enabled: false\nconfig:\n  apiVersion: controller.config.cert-manager.io/v1alpha1\n  kind: ControllerConfiguration\n  enableGatewayAPI: true\n",
        },
    }))
}

fn namespace() -> Value {
    labelled(json!({
        "apiVersion": "v1",
        "kind": "Namespace",
        "metadata": { "name": GATEWAY_NAMESPACE },
    }))
}

#[must_use]
pub fn cluster_issuer(email: &str, staging: bool) -> Value {
    let server = if staging {
        "https://acme-staging-v02.api.letsencrypt.org/directory"
    } else {
        "https://acme-v02.api.letsencrypt.org/directory"
    };
    labelled(json!({
        "apiVersion": "cert-manager.io/v1",
        "kind": "ClusterIssuer",
        "metadata": { "name": ISSUER },
        "spec": { "acme": {
            "server": server,
            "email": email,
            "privateKeySecretRef": { "name": format!("{ISSUER}-account") },
            // HTTP-01 through Kuben's own Gateway: the solver route attaches
            // to its plain-HTTP listener.
            "solvers": [{ "http01": { "gatewayHTTPRoute": { "parentRefs": [{
                "group": "gateway.networking.k8s.io",
                "kind": "Gateway",
                "name": "kuben",
                "namespace": GATEWAY_NAMESPACE,
                "sectionName": "http",
            }] } } }],
        } },
    }))
}

/// The `KubenConfig` fields setup owns.
#[must_use]
pub fn kuben_config(wanted: &Wanted<'_>, issuer: bool) -> Value {
    let mut spec = json!({
        "gatewayClassName": GATEWAY_CLASS,
        "gatewayPorts": { "http": TRAEFIK_PORTS.0, "https": TRAEFIK_PORTS.1 },
    });
    if issuer {
        spec["clusterIssuer"] = json!(ISSUER);
    }
    if let Some(domain) = wanted.domain {
        spec["baseDomain"] = json!(domain);
    }
    labelled(json!({
        "apiVersion": "kuben.dev/v1alpha1",
        "kind": "KubenConfig",
        "metadata": { "name": "kuben" },
        "spec": spec,
    }))
}

/// Create `value` when it is missing; record who owns it. `true` when setup
/// wrote it now.
async fn ensure_object(client: &Client, book: &mut Book, name: &str, value: Value) -> anyhow::Result<bool> {
    let object = object(value)?;
    let created = match existing(client, &object).await? {
        None => true,
        // Setup's own object: kept in step with this version.
        Some(true) => book.journal().owns(Kind::KubernetesObject, name),
        Some(false) => false,
    };
    book.claim(Kind::KubernetesObject, name, created)?;
    if book.journal().owns(Kind::KubernetesObject, name) {
        apply(client, &object, true).await?;
        return Ok(created);
    }
    Ok(false)
}

async fn crd_established(client: &Client, name: &str) -> bool {
    Api::<CustomResourceDefinition>::all(client.clone())
        .get_opt(name)
        .await
        .ok()
        .flatten()
        .and_then(|crd| crd.status)
        .and_then(|s| s.conditions)
        .is_some_and(|c| c.iter().any(|c| c.type_ == "Established" && c.status == "True"))
}

/// Install the Gateway API CRDs when the cluster has none; never upgrade them.
async fn ensure_gateway_api(client: &Client, book: &mut Book) -> anyhow::Result<String> {
    if crd_established(client, "gateways.gateway.networking.k8s.io").await {
        book.claim(Kind::KubernetesObject, CRDS, false)?;
        return Ok("Gateway API CRDs already installed (kept as they are)".to_owned());
    }
    let file = std::env::temp_dir().join(format!("kuben-gateway-api-{}.yaml", std::process::id()));
    download_verified(GATEWAY_API_URL, GATEWAY_API_SHA256, &file)?;
    let text = std::fs::read_to_string(&file)?;
    std::fs::remove_file(&file).ok();
    book.claim(Kind::KubernetesObject, CRDS, true)?;
    for document in serde_yaml_ng::Deserializer::from_str(&text) {
        let value = serde_json::to_value(serde_yaml_ng::Value::deserialize(document)?)?;
        if value.is_null() {
            continue;
        }
        apply(client, &object(value)?, true).await?;
    }
    Ok(format!("Gateway API {GATEWAY_API_VERSION} CRDs installed"))
}

/// Everything before the service starts: Gateway API CRDs, Traefik as the
/// Gateway provider, cert-manager, Kuben's namespace, and the issuer.
pub fn ensure_platform(
    ui: Ui,
    kubeconfig: &Path,
    wanted: &Wanted<'_>,
    agent: bool,
    book: &mut Book,
) -> anyhow::Result<()> {
    let step = ui.step("Gateway, TLS and platform components");
    let runtime = runtime()?;
    let client = connect(&runtime, kubeconfig)?;
    let result: anyhow::Result<(bool, Vec<String>)> = runtime.block_on(async {
        let mut changed = false;
        let mut notes = Vec::new();
        let crds = ensure_gateway_api(&client, book).await?;
        changed |= crds.ends_with("installed");
        notes.push(crds);
        changed |= ensure_object(&client, book, TRAEFIK_CONFIG, traefik_config()).await?;
        if !book.journal().owns(Kind::KubernetesObject, TRAEFIK_CONFIG) {
            notes.push(
                "Traefik's HelmChartConfig is someone else's: check providers.kubernetesGateway.enabled"
                    .into(),
            );
        }
        changed |= ensure_object(&client, book, NAMESPACE, namespace()).await?;
        if agent {
            changed |= ensure_set(&client, book, AGENT, "Deployment", agent_objects()?).await?;
        }
        let cert_manager_present = crd_established(&client, "clusterissuers.cert-manager.io").await;
        if cert_manager_present && !book.journal().owns(Kind::KubernetesObject, CERT_MANAGER) {
            book.claim(Kind::KubernetesObject, CERT_MANAGER, false)?;
            notes.push("cert-manager already installed (kept; it needs Gateway API support enabled)".into());
        } else {
            changed |= ensure_object(&client, book, CERT_MANAGER, cert_manager_chart()).await?;
        }
        let class_ready = eventually(Duration::from_mins(5), || gateway_class_accepted(&client)).await;
        if !class_ready {
            notes.push(format!("GatewayClass {GATEWAY_CLASS} is not accepted yet"));
        }
        let webhook = eventually(Duration::from_mins(5), || {
            deployment_available(&client, "cert-manager", "cert-manager-webhook")
        })
        .await;
        if let Some(email) = wanted.acme_email {
            if webhook {
                // The webhook answers a moment after it is Available.
                let issuer = cluster_issuer(email, wanted.acme_staging);
                let mut applied = Err(anyhow::anyhow!("not tried"));
                for _ in 0..30 {
                    applied = ensure_object(&client, book, CLUSTER_ISSUER, issuer.clone()).await;
                    if applied.is_ok() {
                        break;
                    }
                    tokio::time::sleep(Duration::from_secs(2)).await;
                }
                changed |= applied?;
            } else {
                notes.push(
                    "cert-manager is not ready yet: run kuben setup again to add the ClusterIssuer".into(),
                );
            }
        }
        anyhow::Ok((changed, notes))
    });
    match result {
        Ok((changed, notes)) => {
            let detail = if notes.is_empty() {
                format!(
                    "Traefik Gateway, Gateway API {GATEWAY_API_VERSION}, cert-manager {CERT_MANAGER_VERSION}"
                )
            } else {
                notes.join("; ")
            };
            if notes.iter().any(|n| n.contains("not ")) {
                step.warn(&detail);
            } else {
                step.done(&detail);
            }
            book.done(changed, detail)
        }
        Err(e) => {
            step.fail("could not be set up");
            Err(e)
        }
    }
}

/// After the service applied its CRDs: the `KubenConfig` setup owns fields
/// of. Fields someone else set are kept; setup says so.
pub fn ensure_kuben_config(
    ui: Ui,
    kubeconfig: &Path,
    wanted: &Wanted<'_>,
    book: &mut Book,
) -> anyhow::Result<()> {
    let step = ui.step("Kuben's Gateway (KubenConfig)");
    let runtime = runtime()?;
    let client = connect(&runtime, kubeconfig)?;
    let issuer = book
        .journal()
        .owner(Kind::KubernetesObject, CLUSTER_ISSUER)
        .is_some();
    let result = runtime.block_on(async {
        if !eventually(Duration::from_mins(2), || {
            crd_established(&client, "kubenconfigs.kuben.dev")
        })
        .await
        {
            bail!("the KubenConfig CRD is not established; is kuben.service running?");
        }
        let object = object(kuben_config(wanted, issuer))?;
        let before = existing(&client, &object).await?;
        book.claim(Kind::KubernetesObject, KUBEN_CONFIG, before.is_none())?;
        match apply(&client, &object, false).await {
            Ok(()) => Ok(before.is_none() || before == Some(false)),
            // Someone else set these fields (kubectl, the chart): theirs.
            Err(e)
                if e.downcast_ref::<kube::Error>()
                    .is_some_and(|k| matches!(k, kube::Error::Api(s) if s.code == 409)) =>
            {
                Err(anyhow::anyhow!(
                    "KubenConfig fields are set by someone else; kept: {e}"
                ))
            }
            Err(e) => Err(e),
        }
    });
    match result {
        Ok(changed) => {
            let detail = format!(
                "Gateway class {GATEWAY_CLASS}{}{}",
                if issuer {
                    ", HTTPS through Let's Encrypt"
                } else {
                    ", plain HTTP until --acme-email is given"
                },
                wanted
                    .domain
                    .map(|d| format!(", apps under {d}"))
                    .unwrap_or_default()
            );
            step.done(&detail);
            book.done(changed, detail)
        }
        Err(e) if e.to_string().contains("set by someone else") => {
            step.warn(e.to_string());
            book.done(false, e.to_string())
        }
        Err(e) => {
            step.fail("not written");
            Err(e)
        }
    }
}

/// The agent's objects, as the chart renders them, in [`GATEWAY_NAMESPACE`].
/// `KUBEN_AGENT_IMAGE` replaces the release image (tests, mirrors).
pub fn agent_objects() -> anyhow::Result<Vec<Value>> {
    let image = std::env::var("KUBEN_AGENT_IMAGE")
        .ok()
        .filter(|i| !i.is_empty())
        .unwrap_or_else(|| format!("ghcr.io/teamtem-dev/kuben:{}", crate::cli::VERSION));
    let manifest = AGENT_MANIFEST
        .replace("__NAME__", "kuben-agent")
        .replace("__NAMESPACE__", GATEWAY_NAMESPACE)
        .replace("__INSTANCE__", "kuben")
        .replace("__IMAGE__", &image)
        .replace("__PULL_POLICY__", "IfNotPresent")
        .replace("__MANAGED_BY__", MANAGER);
    let mut objects = Vec::new();
    for document in serde_yaml_ng::Deserializer::from_str(&manifest) {
        let mut value = serde_json::to_value(serde_yaml_ng::Value::deserialize(document)?)?;
        if value.is_null() {
            continue;
        }
        if !matches!(value["kind"].as_str(), Some("ClusterRole" | "ClusterRoleBinding")) {
            value["metadata"]["namespace"] = json!(GATEWAY_NAMESPACE);
        }
        objects.push(value);
    }
    Ok(objects)
}

/// Apply `objects`, recorded together as `name`, when this cluster has no
/// object of `anchor`'s kind and name yet or setup made it; one someone else
/// made is left alone. `true` when they were created now.
async fn ensure_set(
    client: &Client,
    book: &mut Book,
    name: &str,
    anchor: &str,
    objects: Vec<Value>,
) -> anyhow::Result<bool> {
    let first = objects
        .iter()
        .find(|o| o["kind"] == anchor)
        .cloned()
        .with_context(|| format!("{name} has no {anchor}"))?;
    let present = existing(client, &object(first)?).await?;
    let created = match present {
        None => true,
        Some(ours) => ours && book.journal().owns(Kind::KubernetesObject, name),
    };
    book.claim(Kind::KubernetesObject, name, created)?;
    if !book.journal().owns(Kind::KubernetesObject, name) {
        return Ok(false);
    }
    for value in objects {
        apply(client, &object(value)?, true).await?;
    }
    Ok(present.is_none())
}

/// The console behind Kuben's Gateway at `https://{host}`: a route in
/// [`CONSOLE_NAMESPACE`] to a Service whose one endpoint is this server
/// (`hub`, `port`), where `kuben serve` listens outside the cluster. The
/// Gateway gives `host` a listener and a certificate like any app's domain.
#[must_use]
pub fn console_objects(host: &str, hub: &str, port: u16) -> Vec<Value> {
    let kuben = json!({ MANAGED_BY: kuben_crd::labels::MANAGER });
    let gateway = kuben_platform::controller::resources::GatewayRef::owned_default();
    let family = if hub.contains(':') { "IPv6" } else { "IPv4" };
    let domains = json!([{ "host": host, "tls": "auto" }]).to_string();
    vec![
        json!({
            "apiVersion": "v1",
            "kind": "Namespace",
            "metadata": { "name": CONSOLE_NAMESPACE, "labels": kuben },
        }),
        json!({
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": { "name": "kuben-console", "namespace": CONSOLE_NAMESPACE, "labels": kuben },
            "spec": { "ports": [{ "name": "http", "port": 80, "protocol": "TCP" }] },
        }),
        json!({
            "apiVersion": "discovery.k8s.io/v1",
            "kind": "EndpointSlice",
            "metadata": {
                "name": "kuben-console-host",
                "namespace": CONSOLE_NAMESPACE,
                "labels": {
                    "kubernetes.io/service-name": "kuben-console",
                    "endpointslice.kubernetes.io/managed-by": MANAGER,
                    MANAGED_BY: kuben_crd::labels::MANAGER,
                },
            },
            "addressType": family,
            "endpoints": [{ "addresses": [hub], "conditions": { "ready": true } }],
            "ports": [{ "name": "http", "port": port, "protocol": "TCP" }],
        }),
        json!({
            "apiVersion": "gateway.networking.k8s.io/v1",
            "kind": "HTTPRoute",
            "metadata": {
                "name": "kuben-console",
                "namespace": CONSOLE_NAMESPACE,
                "labels": kuben,
                "annotations": { kuben_platform::controller::resources::DOMAINS_ANNOTATION: domains },
            },
            "spec": {
                "parentRefs": [{
                    "name": gateway.name,
                    "namespace": gateway.namespace,
                    "sectionName": kuben_platform::controller::gateway::host_listener_name(host),
                }],
                "hostnames": [host],
                "rules": [{
                    "matches": [{ "path": { "type": "PathPrefix", "value": "/" } }],
                    "backendRefs": [{ "name": "kuben-console", "port": 80 }],
                }],
            },
        }),
    ]
}

/// After the Gateway exists: the console's HTTPS address (M2.11).
pub fn ensure_console(
    ui: Ui,
    kubeconfig: &Path,
    host: &str,
    hub: &str,
    port: u16,
    book: &mut Book,
) -> anyhow::Result<()> {
    let step = ui.step(format!("The console at https://{host}"));
    let runtime = runtime()?;
    let client = connect(&runtime, kubeconfig)?;
    let result = runtime.block_on(ensure_set(
        &client,
        book,
        CONSOLE,
        "HTTPRoute",
        console_objects(host, hub, port),
    ));
    match result {
        Ok(changed) if book.journal().owns(Kind::KubernetesObject, CONSOLE) => {
            let detail = format!("point DNS for {host} at this server; the certificate follows");
            step.done(&detail);
            book.done(changed, detail)
        }
        Ok(_) => {
            let detail = format!("a route {CONSOLE_NAMESPACE}/kuben-console is someone else's; kept");
            step.warn(&detail);
            book.done(false, detail)
        }
        Err(e) => {
            step.fail("not written");
            Err(e)
        }
    }
}

async fn gateway_class_accepted(client: &Client) -> bool {
    let Ok(object) = object(json!({
        "apiVersion": "gateway.networking.k8s.io/v1",
        "kind": "GatewayClass",
        "metadata": { "name": GATEWAY_CLASS },
    })) else {
        return false;
    };
    let Ok(api) = dynamic_api(client, &object).await else {
        return false;
    };
    api.get_opt(GATEWAY_CLASS)
        .await
        .ok()
        .flatten()
        .and_then(|class| class.data.pointer("/status/conditions").cloned())
        .and_then(|c| c.as_array().cloned())
        .is_some_and(|c| c.iter().any(|c| c["type"] == "Accepted" && c["status"] == "True"))
}

async fn deployment_available(client: &Client, namespace: &str, name: &str) -> bool {
    Api::<Deployment>::namespaced(client.clone(), namespace)
        .get_opt(name)
        .await
        .ok()
        .flatten()
        .and_then(|d| d.status)
        .and_then(|s| s.conditions)
        .is_some_and(|c| c.iter().any(|c| c.type_ == "Available" && c.status == "True"))
}

/// `kuben uninstall --purge` on a cluster setup did not install: remove the
/// objects setup created that belong only to Kuben. The Gateway API CRDs and
/// cert-manager stay: other workloads may use them by now (I12).
pub fn purge_objects(ui: Ui, kubeconfig: &Path, journal: &Journal) -> Vec<&'static str> {
    let mut kept = Vec::new();
    if journal.owns(Kind::KubernetesObject, CRDS) {
        kept.push("the Gateway API CRDs");
    }
    if journal.owns(Kind::KubernetesObject, CERT_MANAGER) {
        kept.push("cert-manager (HelmChart kube-system/kuben-cert-manager)");
    }
    let mut removable: Vec<(&str, Value)> = Vec::new();
    if journal.owns(Kind::KubernetesObject, AGENT) {
        removable.extend(
            agent_objects()
                .unwrap_or_default()
                .into_iter()
                .map(|o| (AGENT, o)),
        );
    }
    removable.extend(
        [
            // The namespace takes the console's route and Service with it.
            (CONSOLE, console_objects("", "", 0).swap_remove(0)),
            (KUBEN_CONFIG, kuben_config(&Wanted::default(), false)),
            (CLUSTER_ISSUER, cluster_issuer("", false)),
            (TRAEFIK_CONFIG, traefik_config()),
            (NAMESPACE, namespace()),
        ]
        .into_iter()
        .filter(|(name, _)| journal.owns(Kind::KubernetesObject, name)),
    );
    if removable.is_empty() {
        return kept;
    }
    let step = ui.step("Removing the cluster objects kuben setup created");
    let outcome = runtime().and_then(|rt| {
        let client = connect(&rt, kubeconfig)?;
        rt.block_on(async {
            for (_, value) in &removable {
                let object = object(value.clone())?;
                let api = dynamic_api(&client, &object).await?;
                match api.delete(&object.name_any(), &DeleteParams::default()).await {
                    Ok(_) => {}
                    Err(kube::Error::Api(s)) if s.code == 404 => {}
                    Err(e) => return Err(e.into()),
                }
            }
            anyhow::Ok(())
        })
    });
    match outcome {
        Ok(()) => {
            let mut names: Vec<&str> = removable.iter().map(|(n, _)| *n).collect();
            names.dedup();
            step.done(names.join(", "));
        }
        Err(e) => step.warn(e.to_string()),
    }
    kept
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn k3s_runs_with_reservations_and_the_chosen_datastore() {
        let single = k3s_exec(Datastore::Sqlite);
        assert!(single.starts_with("server "));
        assert!(single.contains("system-reserved=cpu=100m,memory=256Mi"));
        assert!(single.contains("eviction-hard=memory.available<100Mi"));
        assert!(!single.contains("--cluster-init"));
        assert!(k3s_exec(Datastore::Etcd).ends_with("--cluster-init"));
    }

    #[test]
    fn the_console_route_points_the_gateway_at_this_server() {
        let objects = console_objects("kuben.apps.example.com", "203.0.113.7", 3000);
        let kinds: Vec<&str> = objects.iter().filter_map(|o| o["kind"].as_str()).collect();
        assert_eq!(kinds, ["Namespace", "Service", "EndpointSlice", "HTTPRoute"]);
        assert_eq!(
            objects[0]["metadata"]["labels"][MANAGED_BY], "kuben",
            "the Gateway admits it"
        );
        let slice = &objects[2];
        assert_eq!(
            slice["metadata"]["labels"]["kubernetes.io/service-name"],
            "kuben-console"
        );
        assert_eq!(
            slice["metadata"]["labels"]["endpointslice.kubernetes.io/managed-by"],
            MANAGER
        );
        assert_eq!(slice["addressType"], "IPv4");
        assert_eq!(slice["endpoints"][0]["addresses"][0], "203.0.113.7");
        assert_eq!(slice["ports"][0]["port"], 3000);
        let route = &objects[3];
        let view = kuben_platform::projection::RouteView::from(&object(route.clone()).expect("a route"));
        assert_eq!(view.gateways, ["kuben-system/kuben"]);
        assert_eq!(view.domains[0].host, "kuben.apps.example.com");
        assert_eq!(
            view.sections,
            [kuben_platform::controller::gateway::host_listener_name(
                "kuben.apps.example.com"
            )]
        );
        assert_eq!(
            console_objects("kuben.x", "2001:db8::7", 3000)[2]["addressType"],
            "IPv6"
        );
    }

    #[test]
    fn the_config_owns_only_what_setup_decides() {
        let plain = kuben_config(&Wanted::default(), false);
        assert_eq!(
            plain["spec"],
            json!({ "gatewayClassName": "traefik", "gatewayPorts": { "http": 8000, "https": 8443 } })
        );
        let full = kuben_config(
            &Wanted {
                domain: Some("apps.example.com"),
                acme_email: Some("ops@example.com"),
                acme_staging: false,
            },
            true,
        );
        assert_eq!(full["spec"]["clusterIssuer"], ISSUER);
        assert_eq!(full["spec"]["baseDomain"], "apps.example.com");
        assert_eq!(full["metadata"]["labels"][MANAGED_BY], MANAGER);
    }

    #[test]
    fn the_issuer_solves_through_kubens_gateway() {
        let issuer = cluster_issuer("ops@example.com", true);
        assert!(
            issuer["spec"]["acme"]["server"]
                .as_str()
                .is_some_and(|s| s.contains("staging"))
        );
        let parent = &issuer["spec"]["acme"]["solvers"][0]["http01"]["gatewayHTTPRoute"]["parentRefs"][0];
        assert_eq!(parent["name"], "kuben");
        assert_eq!(parent["namespace"], GATEWAY_NAMESPACE);
        assert_eq!(parent["sectionName"], "http");
    }

    #[test]
    fn pinned_objects_parse_and_checksums_are_hex() {
        for value in [
            traefik_config(),
            cert_manager_chart(),
            namespace(),
            cluster_issuer("a@b.c", false),
        ] {
            object(value).expect("a Kubernetes object");
        }
        for sum in [K3S_INSTALLER_SHA256, GATEWAY_API_SHA256] {
            assert_eq!(sum.len(), 64);
            assert!(sum.bytes().all(|b| b.is_ascii_hexdigit()));
        }
        assert_eq!(
            hex(&Sha256::digest(b"abc")),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
    }

    #[test]
    fn the_agent_manifest_is_the_charts_with_its_placeholders_filled() {
        let objects = agent_objects().expect("objects");
        let kinds: Vec<&str> = objects.iter().filter_map(|o| o["kind"].as_str()).collect();
        assert_eq!(
            kinds,
            [
                "ServiceAccount",
                "ClusterRole",
                "ClusterRoleBinding",
                "Role",
                "RoleBinding",
                "Deployment"
            ]
        );
        let text = serde_json::to_string(&objects).expect("json");
        assert!(!text.contains("__"), "every placeholder is filled: {text}");
        // The API server rejects what the typed objects cannot hold.
        for value in &objects {
            let typed = match value["kind"].as_str() {
                Some("Deployment") => serde_json::from_value::<Deployment>(value.clone()).map(drop),
                Some("ServiceAccount") => {
                    serde_json::from_value::<k8s_openapi::api::core::v1::ServiceAccount>(value.clone())
                        .map(drop)
                }
                Some("ClusterRole") => {
                    serde_json::from_value::<k8s_openapi::api::rbac::v1::ClusterRole>(value.clone()).map(drop)
                }
                Some("ClusterRoleBinding") => {
                    serde_json::from_value::<k8s_openapi::api::rbac::v1::ClusterRoleBinding>(value.clone())
                        .map(drop)
                }
                Some("Role") => {
                    serde_json::from_value::<k8s_openapi::api::rbac::v1::Role>(value.clone()).map(drop)
                }
                Some("RoleBinding") => {
                    serde_json::from_value::<k8s_openapi::api::rbac::v1::RoleBinding>(value.clone()).map(drop)
                }
                other => panic!("unexpected kind {other:?}"),
            };
            typed.unwrap_or_else(|e| panic!("{} is not a valid object: {e}", value["kind"]));
        }
        let deployment = objects
            .iter()
            .find(|o| o["kind"] == "Deployment")
            .expect("deployment");
        assert_eq!(deployment["metadata"]["namespace"], GATEWAY_NAMESPACE);
        assert_eq!(
            deployment["spec"]["template"]["metadata"]["labels"]["app.kubernetes.io/name"],
            "kuben-agent"
        );
        let container = &deployment["spec"]["template"]["spec"]["containers"][0];
        assert_eq!(container["command"], json!(["/kuben-agent"]));
        assert!(
            container["image"]
                .as_str()
                .is_some_and(|i| i.starts_with("ghcr.io/teamtem-dev/kuben:"))
        );
        let role = objects.iter().find(|o| o["kind"] == "ClusterRole").expect("role");
        assert!(role["metadata"].get("namespace").is_none(), "cluster-scoped");
    }
}
