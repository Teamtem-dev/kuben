//! The Doctor's evidence graph (M5.5; plan §14.5).
//!
//! Every layer between an app's source and a visitor is a node, with what
//! was observed of it:
//! build → release → deployment → pods → endpoints (with the Service) →
//! route (with the Gateway) → reachable, and DNS and the Gateway → TLS →
//! reachable.
//!
//! An edge points from a cause to what it affects. [`diagnose`] turns the
//! graph into ranked findings:
//! - A failing node whose causes are all fine is a **root cause**, with high
//!   confidence.
//! - A failing node below another failing node is a **symptom** of it.
//! - A failing node below a node that could not be observed is a possible
//!   root cause, with low confidence: nothing unobserved is assumed fine.
//! - With no failure but unobserved nodes, the answer is **unknown**, not
//!   healthy.

use std::collections::{BTreeMap, BTreeSet};

use serde::Serialize;

use crate::doctor::Status;

/// A layer of the path.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize)]
#[serde(rename_all = "camelCase")]
pub enum Layer {
    Build,
    Release,
    Deployment,
    Pods,
    Service,
    Endpoints,
    Gateway,
    Route,
    Dns,
    Tls,
}

impl Layer {
    #[must_use]
    pub const fn id(self) -> &'static str {
        match self {
            Self::Build => "build",
            Self::Release => "release",
            Self::Deployment => "deployment",
            Self::Pods => "pods",
            Self::Service => "service",
            Self::Endpoints => "endpoints",
            Self::Gateway => "gateway",
            Self::Route => "route",
            Self::Dns => "dns",
            Self::Tls => "tls",
        }
    }

    /// The layers this one depends on.
    #[must_use]
    pub const fn causes(self) -> &'static [Self] {
        match self {
            Self::Build | Self::Gateway | Self::Dns | Self::Service => &[],
            Self::Release => &[Self::Build],
            Self::Deployment => &[Self::Release],
            Self::Pods => &[Self::Deployment],
            Self::Endpoints => &[Self::Pods, Self::Service],
            Self::Route => &[Self::Endpoints, Self::Gateway],
            Self::Tls => &[Self::Dns, Self::Gateway],
        }
    }
}

/// What was observed of one layer.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Node {
    pub layer: Layer,
    pub status: Status,
    /// One line on what the layer is (`Deployment web-web`).
    pub subject: String,
    /// What was seen, one fact per entry.
    pub evidence: Vec<String>,
    /// Unix milliseconds of the observation, when known.
    pub observed_at: Option<i64>,
    /// What to do when it is not fine.
    pub action: Option<String>,
}

impl Node {
    #[must_use]
    pub fn new(layer: Layer, status: Status, subject: impl Into<String>) -> Self {
        Self {
            layer,
            status,
            subject: subject.into(),
            evidence: Vec::new(),
            observed_at: None,
            action: None,
        }
    }

    #[must_use]
    pub fn fact(mut self, fact: impl Into<String>) -> Self {
        self.evidence.push(fact.into());
        self
    }

    #[must_use]
    pub fn action(mut self, action: impl Into<String>) -> Self {
        self.action = Some(action.into());
        self
    }

    #[must_use]
    pub const fn at(mut self, observed_at: i64) -> Self {
        self.observed_at = Some(observed_at);
        self
    }
}

/// An edge from a cause to what it affects.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
pub struct Edge {
    pub from: Layer,
    pub to: Layer,
}

/// The observed path; layers that do not apply (no build for an image app,
/// no route for a worker) are left out.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize)]
pub struct Graph {
    pub nodes: Vec<Node>,
    pub edges: Vec<Edge>,
}

impl Graph {
    /// The graph of `nodes`, with the edges between those present.
    #[must_use]
    pub fn of(mut nodes: Vec<Node>) -> Self {
        nodes.sort_by_key(|n| n.layer);
        nodes.dedup_by_key(|n| n.layer);
        let present: BTreeSet<Layer> = nodes.iter().map(|n| n.layer).collect();
        let edges = nodes
            .iter()
            .flat_map(|n| {
                n.layer
                    .causes()
                    .iter()
                    .filter(|c| present.contains(c))
                    .map(move |c| Edge {
                        from: *c,
                        to: n.layer,
                    })
            })
            .collect();
        Self { nodes, edges }
    }

    fn node(&self, layer: Layer) -> Option<&Node> {
        self.nodes.iter().find(|n| n.layer == layer)
    }

    /// Every layer `layer` depends on, directly or not.
    fn ancestors(&self, layer: Layer) -> BTreeSet<Layer> {
        let mut seen = BTreeSet::new();
        let mut todo = vec![layer];
        while let Some(current) = todo.pop() {
            for edge in self.edges.iter().filter(|e| e.to == current) {
                if seen.insert(edge.from) {
                    todo.push(edge.from);
                }
            }
        }
        seen
    }
}

/// How sure a finding is.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize)]
#[serde(rename_all = "camelCase")]
pub enum Confidence {
    Low,
    Medium,
    High,
}

/// One conclusion of the Doctor.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Finding {
    /// `rootCause`, `symptom`, `possibleCause` or `unknown`.
    pub kind: &'static str,
    pub layer: Layer,
    pub status: Status,
    pub confidence: Confidence,
    pub summary: String,
    pub evidence: Vec<String>,
    /// The failing layers this one explains, or that may explain it.
    pub related: Vec<Layer>,
    pub action: Option<String>,
}

fn failing(status: Status) -> bool {
    matches!(status, Status::Fail | Status::Warn)
}

/// Ranked findings of `graph`: root causes first, then possible causes,
/// symptoms and unknowns; worse before milder; upstream before downstream.
#[must_use]
pub fn diagnose(graph: &Graph) -> Vec<Finding> {
    let mut findings = Vec::new();
    let mut explained: BTreeMap<Layer, Vec<Layer>> = BTreeMap::new();
    for node in graph.nodes.iter().filter(|n| failing(n.status)) {
        let ancestors = graph.ancestors(node.layer);
        let failing_causes: Vec<Layer> = ancestors
            .iter()
            .copied()
            .filter(|a| graph.node(*a).is_some_and(|n| n.status == Status::Fail))
            .collect();
        let unknown_causes: Vec<Layer> = ancestors
            .iter()
            .copied()
            .filter(|a| graph.node(*a).is_some_and(|n| n.status == Status::Unknown))
            .collect();
        let (kind, confidence, related) = if !failing_causes.is_empty() && node.status == Status::Fail {
            for cause in &failing_causes {
                explained.entry(*cause).or_default().push(node.layer);
            }
            ("symptom", Confidence::Medium, failing_causes)
        } else if !unknown_causes.is_empty() {
            ("possibleCause", Confidence::Low, unknown_causes)
        } else {
            let confidence = if node.evidence.is_empty() {
                Confidence::Medium
            } else {
                Confidence::High
            };
            ("rootCause", confidence, Vec::new())
        };
        findings.push(Finding {
            kind,
            layer: node.layer,
            status: node.status,
            confidence,
            summary: node.subject.clone(),
            evidence: node.evidence.clone(),
            related,
            action: node.action.clone(),
        });
    }
    for f in &mut findings {
        if f.kind == "rootCause"
            && let Some(effects) = explained.get(&f.layer)
        {
            f.related = effects.clone();
        }
    }
    if findings.is_empty() {
        for node in graph.nodes.iter().filter(|n| n.status == Status::Unknown) {
            findings.push(Finding {
                kind: "unknown",
                layer: node.layer,
                status: Status::Unknown,
                confidence: Confidence::Low,
                summary: node.subject.clone(),
                evidence: node.evidence.clone(),
                related: Vec::new(),
                action: node.action.clone(),
            });
        }
    }
    let rank = |kind: &str| match kind {
        "rootCause" => 0,
        "possibleCause" => 1,
        "symptom" => 2,
        _ => 3,
    };
    findings.sort_by(|a, b| {
        rank(a.kind)
            .cmp(&rank(b.kind))
            .then(b.status.cmp(&a.status))
            .then(a.layer.cmp(&b.layer))
    });
    findings
}

#[cfg(test)]
mod tests {
    use super::*;

    fn node(layer: Layer, status: Status) -> Node {
        Node::new(layer, status, layer.id()).fact(format!("{} observed", layer.id()))
    }

    fn healthy() -> Vec<Node> {
        [
            Layer::Release,
            Layer::Deployment,
            Layer::Pods,
            Layer::Service,
            Layer::Endpoints,
            Layer::Gateway,
            Layer::Route,
            Layer::Dns,
            Layer::Tls,
        ]
        .into_iter()
        .map(|l| node(l, Status::Ok))
        .collect()
    }

    fn with(mut nodes: Vec<Node>, layer: Layer, status: Status) -> Vec<Node> {
        for n in &mut nodes {
            if n.layer == layer {
                n.status = status;
            }
        }
        nodes
    }

    #[test]
    fn edges_connect_present_layers_only() {
        let graph = Graph::of(vec![
            node(Layer::Pods, Status::Ok),
            node(Layer::Endpoints, Status::Ok),
        ]);
        assert_eq!(
            graph.edges,
            [Edge {
                from: Layer::Pods,
                to: Layer::Endpoints
            }]
        );
        assert!(diagnose(&Graph::of(healthy())).is_empty(), "nothing to report");
    }

    #[test]
    fn a_failing_cause_explains_its_symptoms() {
        let nodes = with(healthy(), Layer::Pods, Status::Fail);
        let nodes = with(nodes, Layer::Endpoints, Status::Fail);
        let nodes = with(nodes, Layer::Route, Status::Fail);
        let findings = diagnose(&Graph::of(nodes));
        assert_eq!(findings.len(), 3);
        assert_eq!((findings[0].kind, findings[0].layer), ("rootCause", Layer::Pods));
        assert_eq!(findings[0].confidence, Confidence::High);
        assert_eq!(findings[0].related, [Layer::Endpoints, Layer::Route]);
        assert!(
            findings[1..]
                .iter()
                .all(|f| f.kind == "symptom" && f.related.contains(&Layer::Pods))
        );
    }

    #[test]
    fn unobserved_causes_lower_confidence() {
        let nodes = with(healthy(), Layer::Deployment, Status::Unknown);
        let nodes = with(nodes, Layer::Pods, Status::Fail);
        let findings = diagnose(&Graph::of(nodes));
        assert_eq!(findings[0].kind, "possibleCause");
        assert_eq!(findings[0].confidence, Confidence::Low);
        assert_eq!(findings[0].related, [Layer::Deployment]);
    }

    #[test]
    fn nothing_observed_is_unknown_not_fine() {
        let nodes = with(healthy(), Layer::Dns, Status::Unknown);
        let findings = diagnose(&Graph::of(nodes));
        assert_eq!(findings.len(), 1);
        assert_eq!(
            (findings[0].kind, findings[0].status),
            ("unknown", Status::Unknown)
        );
    }

    #[test]
    fn independent_failures_are_both_root_causes() {
        let nodes = with(healthy(), Layer::Dns, Status::Fail);
        let nodes = with(nodes, Layer::Pods, Status::Fail);
        let nodes = with(nodes, Layer::Tls, Status::Fail);
        let findings = diagnose(&Graph::of(nodes));
        let roots: Vec<Layer> = findings
            .iter()
            .filter(|f| f.kind == "rootCause")
            .map(|f| f.layer)
            .collect();
        assert_eq!(roots, [Layer::Pods, Layer::Dns]);
        assert_eq!(findings.last().map(|f| f.layer), Some(Layer::Tls));
        let warn = with(healthy(), Layer::Gateway, Status::Warn);
        assert_eq!(diagnose(&Graph::of(warn))[0].kind, "rootCause");
    }
}
