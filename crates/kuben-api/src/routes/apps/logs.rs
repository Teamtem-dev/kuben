//! What an app says about itself (M2.12): its log lines, once or followed
//! live, and the Kubernetes events of its own objects.
//!
//! A followed log is a Server-Sent Events stream: the one stream besides the
//! tab's (ADR-014), since log lines are too many for the shared one. Each is
//! bounded: a few per user and a hundred on a replica, an hour long, a line
//! at most 16 KiB. The client sets the pace (a slow reader slows the read
//! from the cluster), and closing the connection ends the reads at once.

use std::{
    collections::{HashMap, HashSet},
    convert::Infallible,
    sync::{Arc, Mutex, PoisonError},
    time::Duration,
};

use axum::{
    Json,
    extract::{Path, Query, State},
    response::{
        IntoResponse, Response,
        sse::{Event, KeepAlive, Sse},
    },
};
use futures::{AsyncBufReadExt as _, Stream, StreamExt as _, stream::SelectAll};
use k8s_openapi::api::core::v1::{Event as KubeEvent, Pod};
use kube::{
    Api,
    api::{ListParams, LogParams},
};
use kuben_core::{Error, ids::UserId, perm::Perm};
use kuben_platform::{
    controller::{gateway::host_secret_name, resources::workload_name},
    projection::{PodView, Projections},
};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};

use crate::{
    authz::Authz,
    error::ApiResult,
    routes::scope::{self, AppScope},
    state::ApiState,
};

/// Pods read at once, once or followed.
const MAX_LOG_PODS: usize = 10;
/// Followed logs per user, and on one replica.
const MAX_FOLLOWS_PER_USER: usize = 4;
const MAX_FOLLOWS: usize = 100;
/// A followed log ends after this; the client reconnects if it still wants it.
const FOLLOW_LIMIT: Duration = Duration::from_hours(1);
/// How often a followed log looks for new pods (a rollout, a restart).
const RESCAN: Duration = Duration::from_secs(5);
/// Longer lines are cut.
const MAX_LINE: usize = 16 * 1024;
/// Events returned, newest first.
const MAX_EVENTS: usize = 100;

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct LogQuery {
    /// Lines per pod (1–2000, default 200).
    pub tail: Option<i64>,
    /// Only pods of this process.
    pub process: Option<String>,
    /// Logs of the previous (crashed) container instance.
    pub previous: Option<bool>,
    /// Keep the connection open and send new lines as `text/event-stream`:
    /// `line` events (a [`LogLine`]) and an `end` event (a [`LogEnd`]) when a
    /// pod's log stops or the stream reaches its hour.
    pub follow: Option<bool>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct PodLogs {
    pub pod: String,
    pub process: Option<String>,
    pub lines: Vec<String>,
    /// Why no logs could be read (e.g. the container is still starting).
    pub error: Option<String>,
}

/// One line of a followed log.
#[derive(Debug, Serialize, ToSchema)]
pub struct LogLine {
    pub pod: String,
    pub process: Option<String>,
    /// When the container wrote it (RFC 3339).
    pub time: Option<String>,
    pub line: String,
}

/// A followed log stopped: one pod's (its container ended or could not be
/// read; a restarted container is followed again), or all of them (`pod` is
/// absent: the stream reached its limit).
#[derive(Debug, Serialize, ToSchema)]
pub struct LogEnd {
    pub pod: Option<String>,
    pub error: Option<String>,
}

/// Followed logs open on this replica, per user.
#[derive(Debug, Default)]
pub struct LogStreams {
    open: Mutex<HashMap<UserId, usize>>,
}

/// One open followed log; dropping it (the client left) frees its place.
#[derive(Debug)]
pub struct LogStreamPermit {
    streams: Arc<LogStreams>,
    user: UserId,
}

impl LogStreams {
    /// A place for one more followed log of `user`.
    pub fn acquire(self: &Arc<Self>, user: UserId) -> Result<LogStreamPermit, Error> {
        let mut open = self.open.lock().unwrap_or_else(PoisonError::into_inner);
        let total: usize = open.values().sum();
        let mine = open.entry(user).or_default();
        if *mine >= MAX_FOLLOWS_PER_USER || total >= MAX_FOLLOWS {
            return Err(Error::RateLimited { retry_after_secs: 5 });
        }
        *mine += 1;
        Ok(LogStreamPermit {
            streams: Arc::clone(self),
            user,
        })
    }
}

impl Drop for LogStreamPermit {
    fn drop(&mut self) {
        let mut open = self.streams.open.lock().unwrap_or_else(PoisonError::into_inner);
        if let Some(count) = open.get_mut(&self.user) {
            *count -= 1;
            if *count == 0 {
                open.remove(&self.user);
            }
        }
    }
}

/// The app's pods, of one process when asked, at most [`MAX_LOG_PODS`].
fn pods_of(
    projections: &Projections,
    namespace: &str,
    app: &str,
    process: Option<&str>,
) -> Vec<Arc<PodView>> {
    projections
        .pods_of_app(namespace, app)
        .into_iter()
        .filter(|p| process.is_none_or(|want| p.process.as_deref() == Some(want)))
        .take(MAX_LOG_PODS)
        .collect()
}

fn read_error(err: kube::Error) -> String {
    match err {
        kube::Error::Api(s) => s.message,
        e => e.to_string(),
    }
}

/// Log lines of the app's pods (at most 10 pods): the recent ones, or with
/// `follow=true` a live stream of new ones.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/logs", operation_id = "getAppLogs",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        LogQuery,
    ),
    responses(
        (status = 200, body = Vec<PodLogs>,
            description = "Recent lines per pod; with `follow`, `text/event-stream` of `line` (LogLine) and `end` (LogEnd) events"),
        (status = 429, body = crate::error::Problem, description = "Too many followed logs open"),
        (status = 503, body = crate::error::Problem)
    )
)]
pub async fn logs(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Query(q): Query<LogQuery>,
) -> ApiResult<Response> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppLogsRead, &a.chain())?;
    let pods_api = Api::<Pod>::namespaced(scope::cluster(&state)?, a.namespace());
    let params = LogParams {
        tail_lines: Some(q.tail.unwrap_or(200).clamp(1, 2000)),
        timestamps: true,
        previous: q.previous.unwrap_or(false),
        limit_bytes: Some(1 << 20),
        ..LogParams::default()
    };
    if q.follow.unwrap_or(false) {
        let permit = state.log_streams.acquire(authz.current.user.id)?;
        let follow = Follow {
            api: pods_api,
            projections: Arc::clone(&state.projections),
            namespace: a.namespace().to_owned(),
            app: a.slug().to_owned(),
            process: q.process,
            params: LogParams {
                follow: true,
                previous: false,
                limit_bytes: None,
                ..params
            },
        };
        return Ok(follow.into_sse(permit).into_response());
    }
    let pods = pods_of(&state.projections, a.namespace(), a.slug(), q.process.as_deref());
    let fetches = pods.iter().map(|pod| {
        let api = pods_api.clone();
        let params = LogParams {
            container: pod.process.clone(),
            ..params.clone()
        };
        async move {
            let (lines, error) = match api.logs(&pod.name, &params).await {
                Ok(text) => (text.lines().map(str::to_owned).collect(), None),
                Err(e) => (Vec::new(), Some(read_error(e))),
            };
            PodLogs {
                pod: pod.name.clone(),
                process: pod.process.clone(),
                lines,
                error,
            }
        }
    });
    Ok(Json(futures::future::join_all(fetches).await).into_response())
}

/// What a pod's log stream yields.
enum Piece {
    Line(Event),
    End { pod: String, error: Option<String> },
}

fn json_event(name: &str, data: &impl Serialize) -> Event {
    Event::default()
        .event(name)
        .json_data(data)
        .unwrap_or_else(|_| Event::default().event("error").data("serialize"))
}

/// `2026-09-16T10:00:00.123Z message` → (time, message), cut to [`MAX_LINE`].
fn split_line(raw: &str) -> (Option<String>, String) {
    let (time, line) = match raw.split_once(' ') {
        Some((time, line)) if time.len() >= 20 && time.as_bytes()[4] == b'-' => (Some(time.to_owned()), line),
        _ => (None, raw),
    };
    let mut end = line.len().min(MAX_LINE);
    while !line.is_char_boundary(end) {
        end -= 1;
    }
    (time, line[..end].to_owned())
}

struct Follow {
    api: Api<Pod>,
    projections: Arc<Projections>,
    namespace: String,
    app: String,
    process: Option<String>,
    params: LogParams,
}

impl Follow {
    /// The lines of one pod, then its end.
    fn pod(&self, pod: &PodView, params: &LogParams) -> impl Stream<Item = Piece> + Send + use<> {
        let api = self.api.clone();
        let name = pod.name.clone();
        let process = pod.process.clone();
        let params = LogParams {
            container: process.clone(),
            ..params.clone()
        };
        async_stream::stream! {
            let reader = match api.log_stream(&name, &params).await {
                Ok(reader) => reader,
                Err(e) => {
                    yield Piece::End { pod: name, error: Some(read_error(e)) };
                    return;
                }
            };
            let mut lines = reader.lines();
            let mut error = None;
            while let Some(line) = lines.next().await {
                match line {
                    Ok(raw) => {
                        let (time, line) = split_line(&raw);
                        let data = LogLine { pod: name.clone(), process: process.clone(), time, line };
                        yield Piece::Line(json_event("line", &data));
                    }
                    Err(e) => {
                        error = Some(e.to_string());
                        break;
                    }
                }
            }
            yield Piece::End { pod: name, error };
        }
    }

    fn into_sse(self, permit: LogStreamPermit) -> Sse<impl Stream<Item = Result<Event, Infallible>>> {
        let stream = async_stream::stream! {
            let _permit = permit;
            let limit = tokio::time::sleep(FOLLOW_LIMIT);
            tokio::pin!(limit);
            let mut rescan = tokio::time::interval(RESCAN);
            let mut reads = SelectAll::new();
            let mut following: HashSet<String> = HashSet::new();
            // When a pod's read ended, and why: a pod that is still there is
            // read again from then on, and the same error is told once.
            let mut ended: HashMap<String, (tokio::time::Instant, Option<String>)> = HashMap::new();
            let mut first = true;
            loop {
                tokio::select! {
                    () = &mut limit => {
                        yield Ok(json_event("end", &LogEnd { pod: None, error: Some("the stream reached its limit; reconnect to go on".into()) }));
                        break;
                    }
                    _ = rescan.tick() => {
                        let pods = pods_of(&self.projections, &self.namespace, &self.app, self.process.as_deref());
                        ended.retain(|name, _| pods.iter().any(|p| &p.name == name));
                        let new: Vec<_> = pods.iter().filter(|p| !following.contains(&p.name)).collect();
                        for pod in new {
                            let params = match (first, ended.get(&pod.name)) {
                                (true, _) => self.params.clone(),
                                // Read again: only what came after the end.
                                (false, Some((at, _))) => LogParams {
                                    tail_lines: None,
                                    since_seconds: Some(i64::try_from(at.elapsed().as_secs()).unwrap_or(i64::MAX).max(1)),
                                    ..self.params.clone()
                                },
                                // A new pod: from its start.
                                (false, None) => LogParams { tail_lines: None, ..self.params.clone() },
                            };
                            following.insert(pod.name.clone());
                            reads.push(Box::pin(self.pod(pod, &params)));
                        }
                        first = false;
                    }
                    Some(piece) = reads.next(), if !reads.is_empty() => match piece {
                        Piece::Line(event) => yield Ok(event),
                        Piece::End { pod, error } => {
                            following.remove(&pod);
                            let told = ended.get(&pod).is_some_and(|(_, e)| e == &error);
                            ended.insert(pod.clone(), (tokio::time::Instant::now(), error.clone()));
                            if !told {
                                yield Ok(json_event("end", &LogEnd { pod: Some(pod), error }));
                            }
                        }
                    },
                }
            }
        };
        Sse::new(stream).keep_alive(KeepAlive::new().interval(Duration::from_secs(15)).text("ping"))
    }
}

/// A Kubernetes event about one of the app's objects.
#[derive(Debug, Serialize, ToSchema)]
pub struct AppEvent {
    /// Kind and name of the object (`Pod`, `Deployment`, `HTTPRoute`,
    /// `Certificate`, …).
    pub kind: String,
    pub name: String,
    /// `Normal` or `Warning`.
    #[serde(rename = "type")]
    pub event_type: String,
    pub reason: Option<String>,
    pub message: Option<String>,
    pub count: i32,
    pub first_seen: Option<String>,
    pub last_seen: Option<String>,
    /// The component that reported it.
    pub source: Option<String>,
}

impl From<&KubeEvent> for AppEvent {
    fn from(e: &KubeEvent) -> Self {
        let series = e.series.as_ref();
        let last_seen = series
            .and_then(|s| s.last_observed_time.as_ref())
            .map(|t| t.0)
            .or_else(|| e.last_timestamp.as_ref().map(|t| t.0))
            .or_else(|| e.event_time.as_ref().map(|t| t.0))
            .or_else(|| e.metadata.creation_timestamp.as_ref().map(|t| t.0));
        Self {
            kind: e.involved_object.kind.clone().unwrap_or_default(),
            name: e.involved_object.name.clone().unwrap_or_default(),
            event_type: e.type_.clone().unwrap_or_else(|| "Normal".into()),
            reason: e.reason.clone(),
            message: e.message.clone(),
            count: series.and_then(|s| s.count).or(e.count).unwrap_or(1).max(1),
            first_seen: e
                .first_timestamp
                .as_ref()
                .map(|t| t.0)
                .or(last_seen)
                .map(|t| t.to_string()),
            last_seen: last_seen.map(|t| t.to_string()),
            source: e
                .reporting_component
                .clone()
                .filter(|c| !c.is_empty())
                .or_else(|| e.source.as_ref().and_then(|s| s.component.clone())),
        }
    }
}

/// How many `-`-separated parts `name` has after `prefix-`.
fn parts_after(name: &str, prefix: &str) -> Option<usize> {
    let rest = name.strip_prefix(prefix)?.strip_prefix('-')?;
    (!rest.is_empty()).then(|| rest.split('-').count())
}

/// Whether an object named `name` of `kind` is one of the app's: the app's
/// own name (App, Service, HTTPRoute, …), a workload of one of its
/// processes, or what a workload made (ReplicaSets, Jobs, Pods). A pod the
/// projection knows is the app's by its label; one already gone is judged by
/// its name.
fn belongs(kind: &str, name: &str, app: &str, workloads: &[String], pods: &HashSet<String>) -> bool {
    let made = |max: usize| {
        workloads
            .iter()
            .any(|w| parts_after(name, w).is_some_and(|n| n <= max))
    };
    match kind {
        "Deployment" | "CronJob" | "HorizontalPodAutoscaler" | "PodDisruptionBudget" => {
            workloads.iter().any(|w| w == name)
        }
        // `<deployment>-<hash>`; `<cron>-<time>` or `<cron>-run-<time>`.
        "ReplicaSet" | "Job" => made(2),
        // `<replicaset>-<hash>`, `<job>-<hash>`.
        "Pod" => pods.contains(name) || made(3),
        _ => name == app,
    }
}

/// The app's Kubernetes events (pods, workloads, route, certificates),
/// newest first, at most 100. Kubernetes keeps events for about an hour.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/events", operation_id = "getAppEvents",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 200, body = Vec<AppEvent>), (status = 503, body = crate::error::Problem))
)]
pub async fn events(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<Json<Vec<AppEvent>>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let client = scope::cluster(&state)?;
    let own = app_objects(&state, &a);
    let list = |namespace: &str, fields: Option<&str>| {
        let api = Api::<KubeEvent>::namespaced(client.clone(), namespace);
        let mut params = ListParams::default();
        if let Some(fields) = fields {
            params = params.fields(fields);
        }
        async move { api.list(&params).await.map(|l| l.items) }
    };
    let name = a.slug();
    let mut out: Vec<AppEvent> = list(a.namespace(), None)
        .await
        .map_err(|e| scope::kube_error(e, name))?
        .iter()
        .filter(|e| {
            let o = &e.involved_object;
            belongs(
                o.kind.as_deref().unwrap_or_default(),
                o.name.as_deref().unwrap_or_default(),
                name,
                &own.workloads,
                &own.pods,
            )
        })
        .map(AppEvent::from)
        .collect();
    // The certificates of the app's own listeners live beside the Gateway.
    if let Some((namespace, certificates)) = own.certificates {
        match list(&namespace, Some("involvedObject.kind=Certificate")).await {
            Ok(items) => out.extend(
                items
                    .iter()
                    .filter(|e| {
                        e.involved_object
                            .name
                            .as_ref()
                            .is_some_and(|n| certificates.contains(n))
                    })
                    .map(AppEvent::from),
            ),
            Err(e) => tracing::debug!(error = %e, %namespace, "cannot read certificate events"),
        }
    }
    out.sort_by(|a, b| b.last_seen.cmp(&a.last_seen));
    out.truncate(MAX_EVENTS);
    Ok(Json(out))
}

struct AppObjects {
    workloads: Vec<String>,
    pods: HashSet<String>,
    /// The Gateway's namespace and the certificates of the app's hosts.
    certificates: Option<(String, HashSet<String>)>,
}

fn app_objects(state: &ApiState, a: &AppScope) -> AppObjects {
    let (namespace, name) = (a.namespace(), a.slug());
    let workloads = a
        .view
        .as_ref()
        .map(|v| v.processes.iter().map(|p| workload_name(name, &p.name)).collect())
        .unwrap_or_default();
    let pods = state
        .projections
        .pods_of_app(namespace, name)
        .iter()
        .map(|p| p.name.clone())
        .collect();
    let certificates = state.projections.route(namespace, name).and_then(|route| {
        let (gateway_ns, _) = route.gateways.first()?.split_once('/')?;
        let names: HashSet<String> = state
            .projections
            .exposure(namespace, name)?
            .hosts
            .iter()
            .filter(|h| h.certificate_ready.is_some())
            .map(|h| host_secret_name(&h.host))
            .collect();
        (!names.is_empty()).then(|| (gateway_ns.to_owned(), names))
    });
    AppObjects {
        workloads,
        pods,
        certificates,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_apps_objects_are_told_apart_from_its_neighbours() {
        let workloads = vec!["web-web".to_owned(), "web-nightly".to_owned()];
        let pods = HashSet::from(["web-web-7d9c-x2x9q".to_owned()]);
        let own = |kind: &str, name: &str| belongs(kind, name, "web", &workloads, &pods);
        assert!(own("App", "web"));
        assert!(own("HTTPRoute", "web"));
        assert!(!own("HTTPRoute", "web-api"), "another app's route");
        assert!(own("Deployment", "web-web"));
        assert!(!own("Deployment", "web-web-2"));
        assert!(own("ReplicaSet", "web-web-7d9c5f"));
        assert!(own("Pod", "web-web-7d9c-x2x9q"));
        assert!(own("Pod", "web-web-7d9c5f-abcde"), "a pod already gone");
        assert!(own("CronJob", "web-nightly"));
        assert!(own("Job", "web-nightly-29310240"));
        assert!(own("Job", "web-nightly-run-1757548800"));
        assert!(own("Pod", "web-nightly-run-1757548800-k2j4d"));
        assert!(!own("Pod", "api-web-7d9c5f-abcde"));
        assert!(!own("Pod", "web-web-a-b-c-d"));
        assert!(!own("Service", "api"));
    }

    #[test]
    fn a_followed_log_line_keeps_its_time_and_is_cut_on_a_character() {
        let (time, line) = split_line("2026-09-16T10:00:00.123456789Z hello world");
        assert_eq!(time.as_deref(), Some("2026-09-16T10:00:00.123456789Z"));
        assert_eq!(line, "hello world");
        assert_eq!(split_line("no time here"), (None, "no time here".to_owned()));
        let long = format!("2026-09-16T10:00:00Z {}", "é".repeat(MAX_LINE));
        let (_, cut) = split_line(&long);
        assert!(cut.len() <= MAX_LINE && cut.chars().all(|c| c == 'é'));
    }

    #[test]
    fn followed_logs_are_capped_per_user_and_freed_when_closed() {
        let streams = Arc::new(LogStreams::default());
        let (alice, bob) = (UserId::new(), UserId::new());
        let held: Vec<_> = (0..MAX_FOLLOWS_PER_USER)
            .map(|_| streams.acquire(alice).expect("a place"))
            .collect();
        assert!(matches!(streams.acquire(alice), Err(Error::RateLimited { .. })));
        let other = streams.acquire(bob).expect("others are not affected");
        drop(held);
        drop(other);
        assert!(
            streams
                .open
                .lock()
                .unwrap_or_else(PoisonError::into_inner)
                .is_empty()
        );
        streams.acquire(alice).expect("free again");
    }
}
