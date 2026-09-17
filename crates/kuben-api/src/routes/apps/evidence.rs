//! The observations behind an app's evidence graph (M5.5): SQL for builds
//! and runs, the cluster for the Deployment, Service and EndpointSlices, the
//! projections for pods, and the Doctor's own checks for the Gateway,
//! route, DNS and TLS.

use k8s_openapi::api::{apps::v1::Deployment, core::v1::Service, discovery::v1::EndpointSlice};
use kube::{Api, Client, api::ListParams};
use kuben_core::ops::RunPhase;
use kuben_crd::labels;
use kuben_platform::{
    doctor::{Check, Status},
    evidence::{Graph, Layer, Node},
    projection::PodPhase,
};

use crate::{error::ApiResult, routes::scope::AppScope, state::ApiState};

/// Pod reasons that stop an app by themselves.
const FATAL_REASONS: [&str; 6] = [
    "CrashLoopBackOff",
    "OOMKilled",
    "ImagePullBackOff",
    "ErrImagePull",
    "CreateContainerConfigError",
    "InvalidImageName",
];

fn worst(checks: &[Check], ids: &[&str]) -> Option<(Status, Vec<String>)> {
    let picked: Vec<&Check> = checks.iter().filter(|c| ids.contains(&c.id)).collect();
    let status = picked.iter().map(|c| c.status).max()?;
    let facts = picked
        .iter()
        .filter(|c| c.status != Status::Ok)
        .map(|c| format!("{}: {}", c.subject, c.detail))
        .collect();
    Some((status, facts))
}

fn from_checks(layer: Layer, subject: &str, checks: &[Check], ids: &[&str]) -> Option<Node> {
    let (status, facts) = worst(checks, ids)?;
    let mut node = Node::new(layer, status, subject);
    node.evidence = facts;
    let hint = checks
        .iter()
        .filter(|c| ids.contains(&c.id) && c.status != Status::Ok)
        .find_map(|c| c.hint.clone());
    node.action = hint;
    Some(node)
}

async fn source_nodes(state: &ApiState, a: &AppScope) -> ApiResult<Vec<Node>> {
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    let mut nodes = Vec::new();
    if tenant.binding_of_target(a.app.target).await?.is_some() {
        let build = tenant.builds_of_target(a.app.target, 1).await?.into_iter().next();
        nodes.push(match build {
            None => {
                Node::new(Layer::Build, Status::Unknown, "no build yet").fact("the source has not been built")
            }
            Some(b) => {
                let phase = format!("{:?}", b.phase);
                let status = match phase.as_str() {
                    "Failed" => Status::Fail,
                    "Cancelled" | "Blocked" => Status::Warn,
                    _ => Status::Ok,
                };
                let mut node = Node::new(Layer::Build, status, format!("build of {}", b.commit.short()))
                    .fact(format!("phase {phase}"))
                    .at(b.created_at);
                if let Some(code) = &b.failure {
                    node = node
                        .fact(format!("failure {code}"))
                        .action("Read the build log (app → Builds).");
                }
                node
            }
        });
    }
    let run = tenant.runs(a.app.target, 1).await?.into_iter().next();
    nodes.push(match run {
        None => Node::new(Layer::Release, Status::Warn, "never deployed").action("Deploy the app."),
        Some(r) => {
            let status = match r.phase {
                RunPhase::Failed | RunPhase::RecoveryFailed | RunPhase::ManualActionRequired => Status::Fail,
                RunPhase::AwaitingApproval | RunPhase::Blocked => Status::Warn,
                _ => Status::Ok,
            };
            let mut node = Node::new(Layer::Release, status, format!("revision {}", r.generation.0))
                .fact(format!("run {} is {}", r.run, r.phase.as_str()))
                .at(r.created_at);
            if let Some(image) = &r.image {
                node = node.fact(format!("image {image}"));
            }
            match r.phase {
                RunPhase::AwaitingApproval => node.action("Ask an approver to approve the run."),
                RunPhase::Failed => {
                    node.action("Read the run's timeline, fix the cause and deploy again, or roll back.")
                }
                _ => node,
            }
        }
    });
    Ok(nodes)
}

fn deployment_node(found: kube::Result<Vec<Deployment>>) -> Node {
    let deployments = match found {
        Ok(d) => d,
        Err(e) => {
            return Node::new(Layer::Deployment, Status::Unknown, "workloads")
                .fact(format!("cannot read: {e}"));
        }
    };
    if deployments.is_empty() {
        return Node::new(Layer::Deployment, Status::Fail, "no Deployment")
            .fact("no Deployment carries the app's label")
            .action("Check the run's timeline: the app was not written to the cluster.");
    }
    let mut status = Status::Ok;
    let mut node = Node::new(
        Layer::Deployment,
        Status::Ok,
        format!("{} Deployment(s)", deployments.len()),
    );
    for d in &deployments {
        let name = d.metadata.name.clone().unwrap_or_default();
        for c in d.status.iter().flat_map(|s| s.conditions.iter().flatten()) {
            let failed = (c.type_ == "Available" && c.status == "False")
                || (c.type_ == "Progressing" && c.reason.as_deref() == Some("ProgressDeadlineExceeded"));
            if failed {
                status = Status::Fail;
                node.evidence.push(format!(
                    "{name}: {} {} ({})",
                    c.type_,
                    c.status,
                    c.reason.as_deref().unwrap_or("-")
                ));
            }
        }
    }
    node.status = status;
    node
}

fn pods_node(state: &ApiState, a: &AppScope) -> Node {
    let pods = state.projections.pods_of_app(a.namespace(), a.slug());
    if pods.is_empty() {
        return Node::new(Layer::Pods, Status::Fail, "no pods").fact("no pod of the app is known");
    }
    let ready = pods.iter().filter(|p| p.ready).count();
    let mut node = Node::new(
        Layer::Pods,
        Status::Ok,
        format!("{ready} of {} pods ready", pods.len()),
    );
    for p in &pods {
        if let Some(reason) = p.reason.as_deref().filter(|r| FATAL_REASONS.contains(r)) {
            node.status = Status::Fail;
            node.evidence
                .push(format!("{}: {reason} ({} restarts)", p.name, p.restarts));
        } else if p.phase == PodPhase::Pending {
            node.evidence.push(format!("{}: pending", p.name));
        }
    }
    if node.status == Status::Ok && ready == 0 {
        node.status = Status::Fail;
    } else if node.status == Status::Ok && ready < pods.len() {
        node.status = Status::Warn;
    }
    if node.status != Status::Ok {
        node.action = Some("Read the app's logs and events (app → Logs).".into());
    }
    node
}

async fn network_nodes(client: &Client, a: &AppScope) -> Vec<Node> {
    let service = Api::<Service>::namespaced(client.clone(), a.namespace())
        .get_opt(a.slug())
        .await;
    let service_node = match &service {
        Ok(Some(_)) => Node::new(Layer::Service, Status::Ok, format!("Service {}", a.slug())),
        Ok(None) => Node::new(Layer::Service, Status::Fail, format!("Service {}", a.slug()))
            .fact("the Service is missing"),
        Err(e) => Node::new(Layer::Service, Status::Unknown, "Service").fact(format!("cannot read: {e}")),
    };
    let slices = Api::<EndpointSlice>::namespaced(client.clone(), a.namespace())
        .list(&ListParams::default().labels(&format!("kubernetes.io/service-name={}", a.slug())))
        .await;
    let endpoints_node = match slices {
        Err(e) => Node::new(Layer::Endpoints, Status::Unknown, "endpoints").fact(format!("cannot read: {e}")),
        Ok(list) => {
            let ready = list
                .items
                .iter()
                .flat_map(|s| s.endpoints.iter())
                .filter(|e| e.conditions.as_ref().and_then(|c| c.ready).unwrap_or(false))
                .count();
            let status = if ready == 0 { Status::Fail } else { Status::Ok };
            Node::new(Layer::Endpoints, status, format!("{ready} ready endpoint(s)"))
                .fact(format!("{} EndpointSlice(s)", list.items.len()))
        }
    };
    vec![service_node, endpoints_node]
}

/// The evidence graph of `a`, from the Doctor's `checks` and fresh
/// observations.
pub async fn graph(state: &ApiState, a: &AppScope, client: &Client, checks: &[Check]) -> ApiResult<Graph> {
    let mut nodes = source_nodes(state, a).await?;
    let deployments = Api::<Deployment>::namespaced(client.clone(), a.namespace())
        .list(&ListParams::default().labels(&format!("{}={}", labels::APP, a.slug())))
        .await
        .map(|l| l.items);
    nodes.push(deployment_node(deployments));
    nodes.push(pods_node(state, a));
    let serves = checks.iter().any(|c| c.id == "route" || c.id == "dns");
    if serves {
        nodes.extend(network_nodes(client, a).await);
        nodes.extend(from_checks(
            Layer::Gateway,
            "Gateway",
            checks,
            &["gateway-class", "gateway", "port-80", "port-443"],
        ));
        nodes.extend(from_checks(Layer::Route, "route", checks, &["route"]));
        nodes.extend(from_checks(
            Layer::Dns,
            "DNS",
            checks,
            &["dns", "claim", "delegation"],
        ));
        nodes.extend(from_checks(
            Layer::Tls,
            "certificates",
            checks,
            &["issuer", "certificate"],
        ));
    }
    Ok(Graph::of(nodes))
}

#[cfg(test)]
mod tests {
    use k8s_openapi::api::apps::v1::{DeploymentCondition, DeploymentStatus};

    use super::*;

    #[test]
    fn deployments_fail_on_their_conditions() {
        let failed = Deployment {
            metadata: kube::api::ObjectMeta {
                name: Some("web-web".into()),
                ..Default::default()
            },
            status: Some(DeploymentStatus {
                conditions: Some(vec![DeploymentCondition {
                    type_: "Progressing".into(),
                    status: "False".into(),
                    reason: Some("ProgressDeadlineExceeded".into()),
                    ..Default::default()
                }]),
                ..Default::default()
            }),
            ..Default::default()
        };
        let node = deployment_node(Ok(vec![failed]));
        assert_eq!(node.status, Status::Fail);
        assert!(node.evidence[0].contains("ProgressDeadlineExceeded"));
        assert_eq!(deployment_node(Ok(vec![])).status, Status::Fail);
    }

    #[test]
    fn checks_fold_into_layers() {
        let check = |id: &'static str, status| Check {
            id,
            subject: "a.example.com".into(),
            status,
            detail: "d".into(),
            hint: Some("h".into()),
        };
        let checks = [
            check("dns", Status::Ok),
            check("claim", Status::Warn),
            check("route", Status::Ok),
        ];
        let dns = from_checks(Layer::Dns, "DNS", &checks, &["dns", "claim"]).expect("node");
        assert_eq!(
            (dns.status, dns.evidence.len(), dns.action.as_deref()),
            (Status::Warn, 1, Some("h"))
        );
        assert!(
            from_checks(Layer::Tls, "TLS", &checks, &["certificate"]).is_none(),
            "nothing checked"
        );
    }
}
