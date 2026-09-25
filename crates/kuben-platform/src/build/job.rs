//! The objects of one build attempt (ADR-028), rendered as pure data.
//!
//! ```text
//! BuildRun kbuild-<attempt>            the attempt in the cluster; owns the rest
//! ├── Secret kbuild-<attempt>-source   the repository-scoped fetch token
//! └── Job kbuild-<attempt>             one pod, never retried by Kubernetes
//!     ├── init fetch   git fetch of exactly one commit (the only mount of the token)
//!     ├── init plan    Dockerfile or Railpack; the Railpack plan
//!     └── build        rootless BuildKit: build, push, report the digest
//! ```
//!
//! The pod gets no service-account token, runs as an unprivileged user with
//! mandatory budgets and a deadline, and receives untrusted values (paths,
//! commit, image) only as environment variables of fixed scripts. A context
//! or Dockerfile directory that resolves outside the checkout (a symlink) is
//! refused before BuildKit reads it, and the fetch token is mounted in the
//! fetch container alone.

use std::{collections::BTreeMap, time::Duration};

use k8s_openapi::{
    ByteString,
    api::{batch::v1::Job, core::v1::Secret},
};
use kube::api::ObjectMeta;
use kuben_core::{
    ops::outcome::{BUILD_CONTAINER, FETCH_CONTAINER, SCAN_CONTAINER},
    source::BuildStrategy,
};
use kuben_crd::{BuildRun, BuildRunSpec, BuildStrategy as CrdStrategy};
use kuben_store::repo::BuildAttempt;
use serde_json::json;

/// Label naming the attempt on every object of a build.
pub const ATTEMPT_LABEL: &str = "kuben.dev/build-attempt";
/// The init container that picks the strategy and writes the Railpack plan.
pub const PLAN_CONTAINER: &str = "plan";
const MANAGED_BY: &str = "app.kubernetes.io/managed-by";
const TOKEN_KEY: &str = "token";
const TOKEN_DIR: &str = "/var/run/kuben/source";
const DOCKER_DIR: &str = "/docker";
const USER: i64 = 1000;
/// Owner and group may read the token (the pod's `fsGroup`).
const TOKEN_MODE: i32 = 0o440;
const HELPER_CPU: &str = "100m";
const HELPER_MEMORY: &str = "256Mi";

const FETCH_SCRIPT: &str = r#"set -eu
export HOME=/workspace/home
mkdir -p "$HOME" /workspace/src
cd /workspace/src
git init -q .
auth=$(printf 'x-access-token:%s' "$(cat /var/run/kuben/source/token)" | base64 | tr -d '\n')
git -c "http.extraHeader=Authorization: Basic $auth" fetch -q --depth 1 --no-tags "$KUBEN_CLONE_URL" "$KUBEN_COMMIT"
git checkout -q --detach FETCH_HEAD
test "$(git rev-parse HEAD)" = "$KUBEN_COMMIT"
rm -rf .git
"#;

const PLAN_SCRIPT: &str = r#"set -eu
fail() { printf '%s' "$1" >/dev/termination-log; exit 2; }
inside() { case "$1" in /workspace/src|/workspace/src/*) ;; *) fail "$2 leaves the repository";; esac; }
[ -d "/workspace/src/$KUBEN_CONTEXT" ] || fail "the build context '$KUBEN_CONTEXT' does not exist"
ctx=$(cd "/workspace/src/$KUBEN_CONTEXT" && pwd -P)
inside "$ctx" "the build context"
strategy=$KUBEN_STRATEGY
if [ "$strategy" = auto ]; then
  if [ -f "$ctx/$KUBEN_DOCKERFILE" ]; then strategy=dockerfile; else strategy=railpack; fi
fi
if [ "$strategy" = dockerfile ]; then
  [ -f "$ctx/$KUBEN_DOCKERFILE" ] || fail "there is no '$KUBEN_DOCKERFILE' in the build context"
else
  command -v railpack >/dev/null 2>&1 || fail "the repository has no Dockerfile and Railpack is not configured on this server (build.railpack_image and build.railpack_frontend)"
  mkdir -p /workspace/plan
  railpack prepare "$ctx" --plan-out /workspace/plan/railpack-plan.json
fi
printf '%s' "$strategy" >/workspace/strategy
"#;

const BUILD_SCRIPT: &str = r#"set -eu
fail() { printf '%s' "$1" >/dev/termination-log; exit 2; }
inside() { case "$1" in /workspace/src|/workspace/src/*) ;; *) fail "$2 leaves the repository";; esac; }
strategy=$(cat /workspace/strategy)
ctx=$(cd "/workspace/src/$KUBEN_CONTEXT" && pwd -P)
inside "$ctx" "the build context"
if [ "$strategy" = dockerfile ]; then
  dir=$(cd "$(dirname "$ctx/$KUBEN_DOCKERFILE")" && pwd -P)
  inside "$dir" "the Dockerfile"
  set -- --frontend dockerfile.v0 --local "dockerfile=$dir" --opt "filename=$(basename "$KUBEN_DOCKERFILE")"
else
  set -- --frontend gateway.v0 --opt "source=$KUBEN_RAILPACK_FRONTEND" --local dockerfile=/workspace/plan
fi
output="type=image,name=$KUBEN_IMAGE,push=true"
if [ "$KUBEN_INSECURE_REGISTRY" = true ]; then output="$output,registry.insecure=true"; fi
buildctl-daemonless.sh build "$@" --local "context=$ctx" --output "$output" --metadata-file /workspace/metadata.json
digest=$(tr ',' '\n' </workspace/metadata.json | sed -n 's/.*"containerimage\.digest": *"\(sha256:[0-9a-f]\{64\}\)".*/\1/p' | head -n 1)
[ -n "$digest" ] || fail "the build wrote no image digest"
printf '%s' "$digest" >/workspace/digest
printf '{"digest":"%s","strategy":"%s"}' "$digest" "$strategy" >/dev/termination-log
"#;

/// Writes the SBOM of the pushed image and scans it. It always exits 0: a
/// scan that cannot run reports `unavailable`, never a clean image. The SBOM
/// goes to the log (gzip, base64, between markers); the summary to the
/// termination log, which holds 4 KiB.
pub const SCAN_SCRIPT: &str = r#"set -u
report() { printf '{"status":"unavailable","scanner":"%s","detail":"%s"}' "${scanner:-}" "$1" >/dev/termination-log; exit 0; }
cd /workspace
export TRIVY_CACHE_DIR=/workspace/trivy TRIVY_NO_PROGRESS=true TRIVY_DISABLE_VEX_NOTICE=true
scanner="trivy $(trivy --version 2>/dev/null | sed -n 's/^Version: *//p' | head -n 1)"
digest=${KUBEN_DIGEST:-$(cat /workspace/digest 2>/dev/null)}
case "$digest" in sha256:*) ;; *) report "no image digest to scan";; esac
insecure=""
if [ "$KUBEN_INSECURE_REGISTRY" = true ]; then insecure="--insecure"; fi
trivy image --quiet $insecure --format cyclonedx --output sbom.json "$KUBEN_REPOSITORY@$digest" 2>scan.err || report "the SBOM could not be written"
trivy sbom --quiet --scanners vuln --format template   --template '{{ range . }}{{ range .Vulnerabilities }}{{ .Severity }}:{{ .VulnerabilityID }}{{ "
" }}{{ end }}{{ end }}'   --output found.txt sbom.json 2>scan.err || report "the vulnerability database is not available"
db=$(trivy version --format json 2>/dev/null | tr ',' '
' | sed -n 's/.*"UpdatedAt": *"\([^"]*\)".*/\1/p' | head -n 1)
sort -u found.txt | tr -cd 'A-Za-z0-9:._
-' >unique.txt
n() { grep -c "^$1:" unique.txt || true; }
findings=$( (grep '^CRITICAL:' unique.txt; grep '^HIGH:' unique.txt) | head -n 40 | sed 's/.*/"&"/' | paste -sd, -)
printf '{"status":"ok","scanner":"%s","db":"%s","counts":{"critical":%s,"high":%s,"medium":%s,"low":%s,"unknown":%s},"findings":[%s]}'   "$scanner" "$db" "$(n CRITICAL)" "$(n HIGH)" "$(n MEDIUM)" "$(n LOW)" "$(n UNKNOWN)" "$findings" >/dev/termination-log
echo kuben-sbom-begin
gzip -c sbom.json | base64 | tr -d '
'
echo
echo kuben-sbom-end
"#;

/// Where and with what a build runs.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BuildSettings {
    pub namespace: String,
    pub buildkit_image: String,
    pub fetch_image: String,
    pub railpack_image: Option<String>,
    pub railpack_frontend: Option<String>,
    pub cpu_request: String,
    pub cpu_limit: String,
    pub memory: String,
    pub ephemeral_storage: String,
    pub deadline: Duration,
    pub push_secret: Option<String>,
    pub insecure_registry: bool,
    /// `(label key, value)` of the required build pool.
    pub node_pool: Option<(String, String)>,
    /// The Trivy image; `None` builds without a scan.
    pub scanner_image: Option<String>,
    pub scanner_memory: String,
}

impl BuildSettings {
    /// The settings of `cfg`, in `namespace`.
    #[must_use]
    pub fn from_config(cfg: &kuben_core::config::BuildCfg, namespace: String) -> Self {
        Self {
            namespace,
            buildkit_image: cfg.buildkit_image.clone(),
            fetch_image: cfg.fetch_image.clone(),
            // Railpack needs both images; with either missing the plan step
            // explains that only Dockerfile builds run.
            railpack_image: cfg
                .railpack_image
                .clone()
                .filter(|i| !i.is_empty())
                .filter(|_| cfg.railpack_frontend.as_deref().is_some_and(|f| !f.is_empty())),
            railpack_frontend: cfg.railpack_frontend.clone().filter(|f| !f.is_empty()),
            cpu_request: cfg.cpu_request.clone(),
            cpu_limit: cfg.cpu_limit.clone(),
            memory: cfg.memory.clone(),
            ephemeral_storage: cfg.ephemeral_storage.clone(),
            deadline: Duration::from_secs(cfg.deadline_secs.max(60)),
            push_secret: cfg.push_secret.clone().filter(|s| !s.is_empty()),
            insecure_registry: cfg.insecure_registry,
            node_pool: cfg
                .node_pool
                .as_deref()
                .and_then(|p| p.split_once('='))
                .map(|(k, v)| (k.trim().to_owned(), v.trim().to_owned())),
            scanner_image: Some(cfg.scanner_image.clone()).filter(|i| !i.is_empty()),
            scanner_memory: cfg.scanner_memory.clone(),
        }
    }
}

/// The deterministic name of `attempt`'s objects.
#[must_use]
pub fn name(attempt: &BuildAttempt) -> String {
    format!("kbuild-{}", attempt.id.as_uuid().simple())
}

/// The name of the fetch-token Secret.
#[must_use]
pub fn secret_name(attempt: &BuildAttempt) -> String {
    format!("{}-source", name(attempt))
}

/// The key of the token in the Secret.
#[must_use]
pub const fn token_key() -> &'static str {
    TOKEN_KEY
}

fn labels(attempt: &BuildAttempt) -> BTreeMap<String, String> {
    BTreeMap::from([
        (ATTEMPT_LABEL.to_owned(), attempt.id.to_string()),
        (MANAGED_BY.to_owned(), "kuben".to_owned()),
        ("kuben.dev/target".to_owned(), attempt.target.to_string()),
    ])
}

const fn crd_strategy(s: BuildStrategy) -> CrdStrategy {
    match s {
        BuildStrategy::Auto => CrdStrategy::Auto,
        BuildStrategy::Dockerfile => CrdStrategy::Dockerfile,
        BuildStrategy::Railpack => CrdStrategy::Railpack,
    }
}

/// The attempt's `BuildRun`, the owner of its Job and Secret.
#[must_use]
pub fn build_run(attempt: &BuildAttempt, settings: &BuildSettings) -> BuildRun {
    let mut run = BuildRun::new(
        &name(attempt),
        BuildRunSpec {
            app: attempt.target.to_string(),
            repo: attempt.repository.to_string(),
            git_ref: attempt.commit.to_string(),
            path: attempt.recipe.context.as_str().to_owned(),
            strategy: crd_strategy(attempt.recipe.strategy),
            image: attempt.push_reference(),
        },
    );
    run.metadata.namespace = Some(settings.namespace.clone());
    run.metadata.labels = Some(labels(attempt));
    run
}

/// The Secret with the fetch `token`, owned by `owner`.
#[must_use]
pub fn source_secret(
    attempt: &BuildAttempt,
    settings: &BuildSettings,
    owner: &BuildRun,
    token: &str,
) -> Secret {
    Secret {
        metadata: ObjectMeta {
            name: Some(secret_name(attempt)),
            namespace: Some(settings.namespace.clone()),
            labels: Some(labels(attempt)),
            owner_references: owner_reference(owner).map(|o| vec![o]),
            ..ObjectMeta::default()
        },
        type_: Some("Opaque".into()),
        immutable: Some(true),
        data: Some(BTreeMap::from([(
            TOKEN_KEY.to_owned(),
            ByteString(token.as_bytes().to_vec()),
        )])),
        ..Secret::default()
    }
}

fn owner_reference(
    owner: &BuildRun,
) -> Option<k8s_openapi::apimachinery::pkg::apis::meta::v1::OwnerReference> {
    use kube::Resource as _;
    owner.controller_owner_ref(&())
}

fn env(pairs: &[(&str, &str)]) -> serde_json::Value {
    pairs
        .iter()
        .map(|(name, value)| json!({ "name": name, "value": value }))
        .collect()
}

fn helper_resources(ephemeral: &str) -> serde_json::Value {
    json!({
        "requests": { "cpu": HELPER_CPU, "memory": HELPER_MEMORY, "ephemeral-storage": "64Mi" },
        "limits": { "memory": HELPER_MEMORY, "ephemeral-storage": ephemeral },
    })
}

fn restricted() -> serde_json::Value {
    json!({
        "runAsNonRoot": true,
        "runAsUser": USER,
        "runAsGroup": USER,
        "allowPrivilegeEscalation": false,
        "capabilities": { "drop": ["ALL"] },
        "seccompProfile": { "type": "RuntimeDefault" },
    })
}

/// The attempt's Job, owned by `owner`.
///
/// # Errors
///
/// When the rendered object does not deserialize (a bug).
pub fn job(
    attempt: &BuildAttempt,
    settings: &BuildSettings,
    owner: &BuildRun,
    clone_url: &str,
) -> Result<Job, serde_json::Error> {
    let deadline = i64::try_from(settings.deadline.as_secs()).unwrap_or(i64::MAX);
    let (build, mut volumes) = build_container(attempt, settings);
    volumes.push(json!({ "name": "source", "secret": {
        "secretName": secret_name(attempt), "defaultMode": TOKEN_MODE,
    } }));
    let affinity = settings.node_pool.as_ref().map(|(key, value)| {
        json!({ "nodeAffinity": { "requiredDuringSchedulingIgnoredDuringExecution": {
            "nodeSelectorTerms": [{ "matchExpressions": [{ "key": key, "operator": "In", "values": [value] }] }],
        } } })
    });
    let tolerations = settings.node_pool.as_ref().map_or_else(Vec::new, |(key, value)| {
        vec![json!({ "key": key, "operator": "Equal", "value": value, "effect": "NoSchedule" })]
    });
    let labels = labels(attempt);
    let mut inits = init_containers(attempt, settings, clone_url);
    // With a scanner the build runs to its end first; the scan follows.
    let main = match scan_container(settings, &attempt.image_repository, None) {
        Some(scan) => {
            if let Some(list) = inits.as_array_mut() {
                list.push(build);
            }
            vec![scan]
        }
        None => vec![build],
    };
    let value = json!({
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": name(attempt),
            "namespace": settings.namespace,
            "labels": labels,
            "ownerReferences": owner_reference(owner).map(|o| vec![o]),
        },
        "spec": {
            "backoffLimit": 0,
            "activeDeadlineSeconds": deadline,
            "podFailurePolicy": { "rules": [{
                "action": "FailJob",
                "onPodConditions": [{ "type": "DisruptionTarget", "status": "True" }],
            }] },
            "template": {
                "metadata": { "labels": labels },
                "spec": {
                    "restartPolicy": "Never",
                    "automountServiceAccountToken": false,
                    "enableServiceLinks": false,
                    "securityContext": {
                        "runAsNonRoot": true,
                        "runAsUser": USER,
                        "runAsGroup": USER,
                        "fsGroup": USER,
                    },
                    "affinity": affinity,
                    "tolerations": tolerations,
                    "initContainers": inits,
                    "containers": main,
                    "volumes": volumes,
                },
            },
        },
    });
    serde_json::from_value(value)
}

/// The container that writes the SBOM of `repository@digest` and scans it;
/// the digest the build wrote when `digest` is `None`. `None` without a
/// scanner.
#[must_use]
pub fn scan_container(
    settings: &BuildSettings,
    repository: &str,
    digest: Option<&str>,
) -> Option<serde_json::Value> {
    let image = settings.scanner_image.as_deref()?;
    let insecure = if settings.insecure_registry {
        "true"
    } else {
        "false"
    };
    let mut vars = vec![
        ("KUBEN_REPOSITORY", repository),
        ("KUBEN_INSECURE_REGISTRY", insecure),
        ("HOME", "/workspace/home"),
    ];
    if let Some(digest) = digest {
        vars.push(("KUBEN_DIGEST", digest));
    }
    let mut mounts = vec![json!({ "name": "workspace", "mountPath": "/workspace" })];
    if settings.push_secret.is_some() {
        vars.push(("DOCKER_CONFIG", DOCKER_DIR));
        mounts.push(json!({ "name": "push", "mountPath": DOCKER_DIR, "readOnly": true }));
    }
    Some(json!({
        "name": SCAN_CONTAINER,
        "image": image,
        "command": ["sh", "-c", SCAN_SCRIPT],
        "env": env(&vars),
        "securityContext": restricted(),
        "resources": {
            "requests": { "cpu": HELPER_CPU, "memory": settings.scanner_memory, "ephemeral-storage": "256Mi" },
            "limits": { "memory": settings.scanner_memory, "ephemeral-storage": settings.ephemeral_storage },
        },
        // The log carries the SBOM; only the file is the report.
        "terminationMessagePolicy": "File",
        "volumeMounts": mounts,
    }))
}

/// The fetch and plan containers.
fn init_containers(attempt: &BuildAttempt, settings: &BuildSettings, clone_url: &str) -> serde_json::Value {
    let dockerfile = attempt.recipe.dockerfile_path();
    let fetch_env = env(&[
        ("KUBEN_CLONE_URL", clone_url),
        ("KUBEN_COMMIT", attempt.commit.as_str()),
    ]);
    let plan_env = env(&[
        ("KUBEN_CONTEXT", attempt.recipe.context.as_str()),
        ("KUBEN_DOCKERFILE", dockerfile.as_str()),
        ("KUBEN_STRATEGY", attempt.recipe.strategy.as_str()),
        ("HOME", "/workspace/home"),
    ]);
    json!([
        {
            "name": FETCH_CONTAINER,
            "image": settings.fetch_image,
            "command": ["sh", "-c", FETCH_SCRIPT],
            "env": fetch_env,
            "securityContext": restricted(),
            "resources": helper_resources(&settings.ephemeral_storage),
            "terminationMessagePolicy": "FallbackToLogsOnError",
            "volumeMounts": [
                { "name": "workspace", "mountPath": "/workspace" },
                { "name": "source", "mountPath": TOKEN_DIR, "readOnly": true },
            ],
        },
        {
            "name": PLAN_CONTAINER,
            "image": settings.railpack_image.as_deref().unwrap_or(&settings.fetch_image),
            "command": ["sh", "-c", PLAN_SCRIPT],
            "env": plan_env,
            "securityContext": restricted(),
            "resources": helper_resources(&settings.ephemeral_storage),
            "terminationMessagePolicy": "FallbackToLogsOnError",
            "volumeMounts": [{ "name": "workspace", "mountPath": "/workspace" }],
        },
    ])
}

/// The BuildKit container and the volumes it needs besides the token.
fn build_container(
    attempt: &BuildAttempt,
    settings: &BuildSettings,
) -> (serde_json::Value, Vec<serde_json::Value>) {
    let dockerfile = attempt.recipe.dockerfile_path();
    let push_ref = attempt.push_reference();
    let insecure = if settings.insecure_registry {
        "true"
    } else {
        "false"
    };
    let mut build_env = vec![
        ("KUBEN_CONTEXT", attempt.recipe.context.as_str()),
        ("KUBEN_DOCKERFILE", dockerfile.as_str()),
        ("KUBEN_IMAGE", push_ref.as_str()),
        (
            "KUBEN_RAILPACK_FRONTEND",
            settings.railpack_frontend.as_deref().unwrap_or_default(),
        ),
        ("KUBEN_INSECURE_REGISTRY", insecure),
        ("BUILDKITD_FLAGS", "--oci-worker-no-process-sandbox"),
    ];
    let mut mounts = vec![
        json!({ "name": "workspace", "mountPath": "/workspace" }),
        json!({ "name": "workspace", "mountPath": "/home/user/.local/share/buildkit", "subPath": "buildkit" }),
    ];
    let mut volumes =
        vec![json!({ "name": "workspace", "emptyDir": { "sizeLimit": settings.ephemeral_storage } })];
    if let Some(secret) = &settings.push_secret {
        build_env.push(("DOCKER_CONFIG", DOCKER_DIR));
        mounts.push(json!({ "name": "push", "mountPath": DOCKER_DIR, "readOnly": true }));
        volumes.push(json!({ "name": "push", "secret": {
            "secretName": secret, "defaultMode": TOKEN_MODE,
            "items": [{ "key": ".dockerconfigjson", "path": "config.json" }],
        } }));
    }
    let container = json!({
        "name": BUILD_CONTAINER,
        "image": settings.buildkit_image,
        "command": ["sh", "-c", BUILD_SCRIPT],
        "env": env(&build_env),
        // Rootless BuildKit needs its own user namespaces: unconfined for
        // this container only, never host-wide.
        "securityContext": {
            "runAsNonRoot": true,
            "runAsUser": USER,
            "runAsGroup": USER,
            "seccompProfile": { "type": "Unconfined" },
            "appArmorProfile": { "type": "Unconfined" },
        },
        "resources": {
            "requests": {
                "cpu": settings.cpu_request,
                "memory": settings.memory,
                "ephemeral-storage": settings.ephemeral_storage,
            },
            "limits": {
                "cpu": settings.cpu_limit,
                "memory": settings.memory,
                "ephemeral-storage": settings.ephemeral_storage,
            },
        },
        "terminationMessagePolicy": "FallbackToLogsOnError",
        "volumeMounts": mounts,
    });
    (container, volumes)
}

#[cfg(test)]
pub(crate) mod tests {
    use kuben_core::{
        ids::{ApplicationId, BuildAttemptId, OperationId, ProjectId, SourceBindingId, TargetId},
        ops::{BuildPhase, SourceEpoch},
        source::BuildRecipe,
    };
    use uuid::Uuid;

    use super::*;

    #[test]
    fn the_scan_report_carries_the_database_date() {
        // 1.2.0 had a raw 0x01 byte where sed's `\1` belongs: every report
        // held a control character, did not parse, and every scan counted as
        // unavailable.
        assert!(!SCAN_SCRIPT.contains('\u{1}'));
        assert!(SCAN_SCRIPT.contains(r#"".*/\1/p' | head -n 1)"#));
    }

    pub(crate) fn attempt(recipe: BuildRecipe) -> BuildAttempt {
        BuildAttempt {
            id: BuildAttemptId::from_uuid(Uuid::from_u128(0x0192_f3a1_0000_7000_8000_0000_0000_0001)),
            org: kuben_core::ids::OrgId::from_uuid(Uuid::from_u128(1)),
            project: ProjectId::from_uuid(Uuid::from_u128(2)),
            application: ApplicationId::from_uuid(Uuid::from_u128(3)),
            target: TargetId::from_uuid(Uuid::from_u128(4)),
            binding: SourceBindingId::from_uuid(Uuid::from_u128(5)),
            attempt_no: 2,
            commit: "0123456789abcdef0123456789abcdef01234567".parse().expect("sha"),
            source_epoch: SourceEpoch(3),
            build_config_revision: 0,
            lifecycle_uid: Uuid::from_u128(6),
            repository: "acme/shop".parse().expect("repo"),
            recipe,
            image_repository: "registry.local/acme/shop".into(),
            installation_id: 7,
            branch: "main".parse().expect("branch"),
            phase: BuildPhase::Queued,
            blocked_reason: None,
            failure: None,
            failure_detail: None,
            reported_digest: None,
            digest: None,
            job_name: None,
            release: None,
            run: None,
            deploy_decision: None,
            operation: OperationId::from_uuid(Uuid::from_u128(8)),
            cancel_requested: false,
            created_at: 0,
            started_at: None,
            finished_at: None,
        }
    }

    pub(crate) fn settings() -> BuildSettings {
        let mut cfg = kuben_core::config::BuildCfg {
            push_secret: Some("push".into()),
            node_pool: Some("kuben.dev/pool = build".into()),
            deadline_secs: 900,
            ..kuben_core::config::BuildCfg::default()
        };
        cfg.railpack_image = Some(String::new());
        BuildSettings::from_config(&cfg, "kuben-builds".into())
    }

    fn rendered(recipe: BuildRecipe) -> (BuildRun, serde_json::Value) {
        let a = attempt(recipe);
        let s = settings();
        let mut owner = build_run(&a, &s);
        owner.metadata.uid = Some("uid-1".into());
        let job = job(&a, &s, &owner, "https://github.com/acme/shop.git").expect("job");
        (owner, serde_json::to_value(job).expect("json"))
    }

    fn container<'a>(pod: &'a serde_json::Value, list: &str, name: &str) -> &'a serde_json::Value {
        pod[list]
            .as_array()
            .expect("containers")
            .iter()
            .find(|c| c["name"] == name)
            .expect("container")
    }

    #[test]
    fn names_are_deterministic_and_short() {
        let a = attempt(BuildRecipe::default());
        assert_eq!(name(&a), "kbuild-0192f3a1000070008000000000000001");
        assert!(secret_name(&a).len() <= 63);
        assert_eq!(a.push_reference(), "registry.local/acme/shop:0123456789ab-2");
    }

    #[test]
    fn build_pods_get_no_service_account_token() {
        let (_, job) = rendered(BuildRecipe::default());
        let pod = &job["spec"]["template"]["spec"];
        assert_eq!(pod["automountServiceAccountToken"], false);
        assert_eq!(pod["enableServiceLinks"], false);
        assert!(pod.get("serviceAccountName").is_none());
        assert_eq!(pod["securityContext"]["runAsNonRoot"], true);
        assert_eq!(
            job["spec"]["backoffLimit"], 0,
            "a retry is a new attempt, never the Job's"
        );
        assert_eq!(job["spec"]["activeDeadlineSeconds"], 900);
    }

    #[test]
    fn only_the_fetch_container_sees_the_token() {
        let (_, job) = rendered(BuildRecipe::default());
        let pod = &job["spec"]["template"]["spec"];
        let mounts_source = |c: &serde_json::Value| {
            c["volumeMounts"]
                .as_array()
                .expect("mounts")
                .iter()
                .any(|m| m["name"] == "source")
        };
        assert!(mounts_source(container(pod, "initContainers", FETCH_CONTAINER)));
        assert!(!mounts_source(container(pod, "initContainers", PLAN_CONTAINER)));
        assert!(!mounts_source(container(pod, "initContainers", BUILD_CONTAINER)));
        let fetch = container(pod, "initContainers", FETCH_CONTAINER);
        assert_eq!(fetch["securityContext"]["allowPrivilegeEscalation"], false);
        assert_eq!(fetch["securityContext"]["capabilities"]["drop"][0], "ALL");
    }

    #[test]
    fn budgets_are_mandatory_and_memory_is_guaranteed() {
        let (_, job) = rendered(BuildRecipe::default());
        let pod = &job["spec"]["template"]["spec"];
        let build = container(pod, "initContainers", BUILD_CONTAINER);
        let res = &build["resources"];
        assert_eq!(res["requests"]["memory"], res["limits"]["memory"]);
        for key in ["cpu", "memory", "ephemeral-storage"] {
            assert!(res["limits"][key].is_string(), "limit {key}");
            assert!(res["requests"][key].is_string(), "request {key}");
        }
        let workspace = &pod["volumes"][0];
        assert_eq!(workspace["emptyDir"]["sizeLimit"], "10Gi");
        for init in pod["initContainers"].as_array().expect("init") {
            assert!(init["resources"]["limits"]["memory"].is_string());
        }
    }

    #[test]
    fn untrusted_values_travel_as_environment_only() {
        let recipe = BuildRecipe {
            context: "apps/$(rm -rf ~)".parse().expect("path"),
            ..BuildRecipe::default()
        };
        let (_, job) = rendered(recipe);
        let pod = &job["spec"]["template"]["spec"];
        for (list, name) in [
            ("initContainers", FETCH_CONTAINER),
            ("initContainers", PLAN_CONTAINER),
            ("initContainers", BUILD_CONTAINER),
            ("containers", SCAN_CONTAINER),
        ] {
            let c = container(pod, list, name);
            let script = c["command"][2].as_str().expect("script");
            assert!(!script.contains("rm -rf ~"), "{name} embeds an input");
            assert!(!script.contains("acme"), "{name} embeds an input");
        }
        let plan = container(pod, "initContainers", PLAN_CONTAINER);
        assert!(
            plan["env"]
                .as_array()
                .expect("env")
                .iter()
                .any(|e| e["value"] == "apps/$(rm -rf ~)")
        );
        let scan = container(pod, "containers", SCAN_CONTAINER);
        assert!(
            scan["env"]
                .as_array()
                .expect("env")
                .iter()
                .any(|e| e["name"] == "KUBEN_REPOSITORY" && e["value"] == "registry.local/acme/shop")
        );
        assert!(
            BUILD_SCRIPT.contains(r#"inside "$ctx""#),
            "symlinked contexts are refused"
        );
        assert!(
            BUILD_SCRIPT.contains(r#"inside "$dir""#),
            "symlinked Dockerfiles are refused"
        );
    }

    #[test]
    fn the_scan_follows_the_build_and_never_fails_it() {
        let (_, job) = rendered(BuildRecipe::default());
        let pod = &job["spec"]["template"]["spec"];
        let inits: Vec<&str> = pod["initContainers"]
            .as_array()
            .expect("init")
            .iter()
            .filter_map(|c| c["name"].as_str())
            .collect();
        assert_eq!(inits, [FETCH_CONTAINER, PLAN_CONTAINER, BUILD_CONTAINER]);
        let scan = container(pod, "containers", SCAN_CONTAINER);
        assert_eq!(scan["image"], "aquasec/trivy:0.74.0");
        assert_eq!(
            scan["terminationMessagePolicy"], "File",
            "the log carries the SBOM"
        );
        assert_eq!(
            scan["resources"]["requests"]["memory"],
            scan["resources"]["limits"]["memory"]
        );
        assert_eq!(scan["securityContext"]["allowPrivilegeEscalation"], false);
        assert!(!scan.to_string().contains("\"source\""), "no fetch token");
        assert!(SCAN_SCRIPT.lines().all(|l| !l.trim_start().starts_with("exit 1")));
        assert!(SCAN_SCRIPT.contains(r#""status":"unavailable""#));
        assert!(BUILD_SCRIPT.contains("/workspace/digest"));
        let pinned =
            scan_container(&settings(), "registry.local/acme/shop", Some("sha256:abc")).expect("scan");
        assert!(
            pinned["env"]
                .as_array()
                .expect("env")
                .iter()
                .any(|e| e["name"] == "KUBEN_DIGEST")
        );

        let unscanned = BuildSettings {
            scanner_image: None,
            ..settings()
        };
        let a = attempt(BuildRecipe::default());
        let job =
            serde_json::to_value(super::job(&a, &unscanned, &build_run(&a, &unscanned), "u").expect("job"))
                .expect("json");
        let pod = &job["spec"]["template"]["spec"];
        assert_eq!(
            container(pod, "containers", BUILD_CONTAINER)["name"],
            BUILD_CONTAINER
        );
        assert_eq!(pod["initContainers"].as_array().map(Vec::len), Some(2));
    }

    #[test]
    fn production_pools_are_required_not_preferred() {
        let (_, job) = rendered(BuildRecipe::default());
        let pod = &job["spec"]["template"]["spec"];
        let term = &pod["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
            [0];
        assert_eq!(term["matchExpressions"][0]["key"], "kuben.dev/pool");
        assert_eq!(term["matchExpressions"][0]["values"][0], "build");
        assert_eq!(pod["tolerations"][0]["effect"], "NoSchedule");
        let unpinned = BuildSettings {
            node_pool: None,
            ..settings()
        };
        let a = attempt(BuildRecipe::default());
        let job = super::job(&a, &unpinned, &build_run(&a, &unpinned), "u").expect("job");
        assert!(
            job.spec
                .expect("spec")
                .template
                .spec
                .expect("pod")
                .affinity
                .is_none()
        );
    }

    #[test]
    fn push_credentials_and_railpack_are_optional() {
        let (_, job) = rendered(BuildRecipe::default());
        let pod = &job["spec"]["template"]["spec"];
        let build = container(pod, "initContainers", BUILD_CONTAINER);
        assert!(
            build["env"]
                .as_array()
                .expect("env")
                .iter()
                .any(|e| e["name"] == "DOCKER_CONFIG")
        );
        let plan = container(pod, "initContainers", PLAN_CONTAINER);
        assert_eq!(
            plan["image"], "alpine/git:2.49.1",
            "an empty Railpack image is no Railpack"
        );
        let bare = BuildSettings {
            push_secret: None,
            railpack_image: Some("railpack:1".into()),
            ..settings()
        };
        let a = attempt(BuildRecipe::default());
        let job = serde_json::to_value(super::job(&a, &bare, &build_run(&a, &bare), "u").expect("job"))
            .expect("json");
        let pod = &job["spec"]["template"]["spec"];
        assert_eq!(pod["volumes"].as_array().expect("volumes").len(), 2);
        assert_eq!(
            container(pod, "initContainers", PLAN_CONTAINER)["image"],
            "railpack:1"
        );
    }

    #[test]
    fn everything_is_owned_by_the_build_run() {
        let (owner, job) = rendered(BuildRecipe::default());
        assert_eq!(job["metadata"]["ownerReferences"][0]["kind"], "BuildRun");
        assert_eq!(job["metadata"]["ownerReferences"][0]["controller"], true);
        let a = attempt(BuildRecipe::default());
        let secret = source_secret(&a, &settings(), &owner, "ghs_x");
        assert_eq!(secret.immutable, Some(true));
        assert_eq!(secret.metadata.owner_references.expect("owner")[0].uid, "uid-1");
        assert_eq!(
            secret.metadata.labels.expect("labels")[ATTEMPT_LABEL],
            a.id.to_string()
        );
        let run = build_run(&a, &settings());
        assert_eq!(run.spec.git_ref, a.commit.as_str());
        assert_eq!(run.metadata.namespace.as_deref(), Some("kuben-builds"));
    }
}
