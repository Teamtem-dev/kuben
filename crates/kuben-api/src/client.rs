//! A typed client of this API, for the `kuben` CLI (M2.14). Its request and
//! response types are the handlers' own, the same the OpenAPI document is
//! generated from, so the two cannot drift apart.

use std::{fmt, time::Duration};

use bytes::Bytes;
use futures::{Stream, StreamExt as _};
use http::{Method, Request, StatusCode, header};
use http_body_util::{BodyExt as _, BodyStream, Full, Limited};
use serde::{Serialize, de::DeserializeOwned};
use serde_json::Value;
use uuid::Uuid;

use crate::{
    error::Problem,
    routes::{
        apps::{
            AppDetail, AppDto,
            deployments::{DeploymentDto, StartDeploymentRequest},
            doctor::DoctorReport,
            logs::{LogEnd, LogLine, PodLogs},
            releases::{ReleaseDto, Rollback},
        },
        environments::EnvironmentDto,
        projects::ProjectDto,
    },
    transport::{Schemes, Transport, chain},
};

/// Until a response starts.
const TIMEOUT: Duration = Duration::from_secs(30);
/// Largest JSON body read (logs of ten pods are the biggest).
const MAX_BODY: usize = 32 << 20;

#[derive(Debug)]
pub enum ClientError {
    /// The server answered with a problem.
    Api {
        status: StatusCode,
        code: String,
        detail: Option<String>,
    },
    /// The server could not be reached, or its answer not read.
    Transport(String),
}

impl fmt::Display for ClientError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Api { status, code, detail } => {
                write!(f, "{} ({code})", detail.as_deref().unwrap_or(status.as_str()))
            }
            Self::Transport(reason) => f.write_str(reason),
        }
    }
}

impl std::error::Error for ClientError {}

impl ClientError {
    /// The HTTP status of a problem answer.
    #[must_use]
    pub const fn status(&self) -> Option<StatusCode> {
        match self {
            Self::Api { status, .. } => Some(*status),
            Self::Transport(_) => None,
        }
    }
}

type Result<T> = std::result::Result<T, ClientError>;

/// `project/environment/app`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AppPath {
    pub project: String,
    pub environment: String,
    pub app: String,
}

/// A name the API accepts in a path: lowercase letters, digits and `-`.
fn is_slug(s: &str) -> bool {
    !s.is_empty()
        && s.bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}

impl AppPath {
    /// `project/environment/app`, or `app` with the defaults given.
    pub fn parse(
        text: &str,
        project: Option<&str>,
        environment: Option<&str>,
    ) -> std::result::Result<Self, String> {
        let parts: Vec<&str> = text.split('/').collect();
        let (project, environment, app) = match parts.as_slice() {
            [p, e, a] => (Some(*p), Some(*e), *a),
            [e, a] => (project, Some(*e), *a),
            [a] => (project, environment, *a),
            _ => return Err(format!("`{text}` is not project/environment/app")),
        };
        let (Some(project), Some(environment)) = (project, environment) else {
            return Err(format!(
                "`{text}` needs its project and environment: project/environment/app, or set them with \
                 kuben login --project <p> --environment <e>"
            ));
        };
        for part in [project, environment, app] {
            if !is_slug(part) {
                return Err(format!("`{part}` is not a valid name"));
            }
        }
        Ok(Self {
            project: project.to_owned(),
            environment: environment.to_owned(),
            app: app.to_owned(),
        })
    }

    fn api(&self) -> String {
        format!(
            "/projects/{}/environments/{}/apps/{}",
            self.project, self.environment, self.app
        )
    }
}

impl fmt::Display for AppPath {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}/{}/{}", self.project, self.environment, self.app)
    }
}

/// Which log lines to read.
#[derive(Clone, Debug, Default)]
pub struct LogOptions {
    pub tail: Option<i64>,
    pub process: Option<String>,
    pub previous: bool,
}

impl LogOptions {
    fn query(&self, follow: bool) -> String {
        let mut q = Vec::new();
        if let Some(tail) = self.tail {
            q.push(format!("tail={tail}"));
        }
        if let Some(process) = self.process.as_deref().filter(|p| is_slug(p)) {
            q.push(format!("process={process}"));
        }
        if self.previous {
            q.push("previous=true".into());
        }
        if follow {
            q.push("follow=true".into());
        }
        if q.is_empty() {
            String::new()
        } else {
            format!("?{}", q.join("&"))
        }
    }
}

/// What a followed log sends.
#[derive(Debug)]
pub enum FollowEvent {
    Line(LogLine),
    End(LogEnd),
}

/// Server-sent events, split out of the bytes as they come.
#[derive(Debug, Default)]
pub struct SseParser {
    buffer: String,
}

impl SseParser {
    /// The complete events in `chunk` and what came before it, as
    /// `(event, data)`; comments (keep-alives) are skipped.
    pub fn push(&mut self, chunk: &str) -> Vec<(String, String)> {
        self.buffer.push_str(&chunk.replace("\r\n", "\n"));
        let mut events = Vec::new();
        while let Some(end) = self.buffer.find("\n\n") {
            let block: String = self.buffer.drain(..end + 2).collect();
            let (mut event, mut data) = (String::from("message"), Vec::new());
            for line in block.lines() {
                let (field, value) = line.split_once(':').unwrap_or((line, ""));
                let value = value.strip_prefix(' ').unwrap_or(value);
                match field {
                    "event" => value.clone_into(&mut event),
                    "data" => data.push(value),
                    _ => {}
                }
            }
            if !data.is_empty() {
                events.push((event, data.join("\n")));
            }
        }
        events
    }
}

/// A client of one Kuben server, authenticated with an API token.
#[derive(Clone)]
pub struct ApiClient {
    base: String,
    token: String,
    transport: Transport,
}

impl fmt::Debug for ApiClient {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        // Never the token.
        f.debug_struct("ApiClient")
            .field("base", &self.base)
            .finish_non_exhaustive()
    }
}

impl ApiClient {
    /// `url` is the server's address (`https://kuben.example.com`).
    #[must_use]
    pub fn new(url: &str, token: &str) -> Self {
        Self {
            base: format!("{}/api/v1", url.trim_end_matches('/')),
            token: token.to_owned(),
            transport: Transport::new(Schemes::Any, TIMEOUT),
        }
    }

    async fn send(
        &self,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
        headers: &[(&str, &str)],
    ) -> Result<http::Response<hyper::body::Incoming>> {
        let mut request = Request::builder()
            .method(method)
            .uri(format!("{}{path}", self.base))
            .header(header::AUTHORIZATION, format!("Bearer {}", self.token))
            .header(
                header::USER_AGENT,
                concat!("kuben-cli/", env!("CARGO_PKG_VERSION")),
            )
            .header(header::ACCEPT, "application/json, text/event-stream");
        if body.is_some() {
            request = request.header(header::CONTENT_TYPE, "application/json");
        }
        for (name, value) in headers {
            request = request.header(*name, *value);
        }
        let request = request
            .body(Full::new(Bytes::from(body.unwrap_or_default())))
            .map_err(|e| ClientError::Transport(e.to_string()))?;
        let response = self
            .transport
            .send(request)
            .await
            .map_err(ClientError::Transport)?;
        if response.status().is_success() {
            return Ok(response);
        }
        let status = response.status();
        let body = read(response).await.unwrap_or_default();
        let problem = serde_json::from_slice::<Problem>(&body).ok();
        Err(ClientError::Api {
            status,
            code: problem
                .as_ref()
                .map_or_else(|| status.as_u16().to_string(), |p| p.code.clone()),
            detail: problem.and_then(|p| p.detail),
        })
    }

    async fn call<T: DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        body: Option<&impl Serialize>,
        headers: &[(&str, &str)],
    ) -> Result<T> {
        let body = body
            .map(serde_json::to_vec)
            .transpose()
            .map_err(|e| ClientError::Transport(e.to_string()))?;
        let response = self.send(method, path, body, headers).await?;
        let bytes = read(response).await?;
        serde_json::from_slice(&bytes)
            .map_err(|e| ClientError::Transport(format!("unexpected answer from {path}: {e}")))
    }

    async fn get<T: DeserializeOwned>(&self, path: &str) -> Result<T> {
        self.call(Method::GET, path, None::<&()>, &[]).await
    }

    /// Who the token belongs to.
    pub async fn me(&self) -> Result<Value> {
        self.get("/me").await
    }

    pub async fn projects(&self) -> Result<Vec<ProjectDto>> {
        self.get("/projects").await
    }

    pub async fn environments(&self, project: &str) -> Result<Vec<EnvironmentDto>> {
        self.get(&format!("/projects/{project}/environments")).await
    }

    pub async fn apps(&self, project: &str, environment: &str) -> Result<Vec<AppDto>> {
        self.get(&format!("/projects/{project}/environments/{environment}/apps"))
            .await
    }

    pub async fn app(&self, app: &AppPath) -> Result<AppDetail> {
        self.get(&app.api()).await
    }

    pub async fn releases(&self, app: &AppPath) -> Result<Vec<ReleaseDto>> {
        self.get(&format!("{}/releases", app.api())).await
    }

    pub async fn doctor(&self, app: &AppPath) -> Result<DoctorReport> {
        self.get(&format!("{}/doctor", app.api())).await
    }

    /// Start a deployment; the same `idempotency_key` returns the same run.
    pub async fn deploy(
        &self,
        app: &AppPath,
        request: &StartDeploymentRequest,
        idempotency_key: &str,
    ) -> Result<DeploymentDto> {
        self.call(
            Method::POST,
            &format!("{}/deployments", app.api()),
            Some(request),
            &[("idempotency-key", idempotency_key)],
        )
        .await
    }

    pub async fn deployment(&self, app: &AppPath, run: Uuid) -> Result<DeploymentDto> {
        self.get(&format!("{}/deployments/{run}", app.api())).await
    }

    pub async fn rollback(&self, app: &AppPath, revision: i64) -> Result<AppDto> {
        self.call(
            Method::POST,
            &format!("{}/rollback", app.api()),
            Some(&Rollback { revision }),
            &[],
        )
        .await
    }

    pub async fn logs(&self, app: &AppPath, options: &LogOptions) -> Result<Vec<PodLogs>> {
        self.get(&format!("{}/logs{}", app.api(), options.query(false)))
            .await
    }

    /// New log lines as they are written, until the server ends the stream.
    pub async fn follow_logs(
        &self,
        app: &AppPath,
        options: &LogOptions,
    ) -> Result<impl Stream<Item = Result<FollowEvent>> + use<>> {
        let path = format!("{}/logs{}", app.api(), options.query(true));
        let response = self.send(Method::GET, &path, None, &[]).await?;
        let mut frames = BodyStream::new(response.into_body());
        Ok(async_stream::stream! {
            let mut parser = SseParser::default();
            let mut pending = Vec::new();
            while let Some(frame) = frames.next().await {
                let frame = match frame {
                    Ok(frame) => frame,
                    Err(e) => {
                        yield Err(ClientError::Transport(chain(&e)));
                        return;
                    }
                };
                let Ok(data) = frame.into_data() else { continue };
                pending.extend_from_slice(&data);
                // Only whole characters; the rest waits for the next frame.
                let valid = match std::str::from_utf8(&pending) {
                    Ok(text) => text.len(),
                    Err(e) => e.valid_up_to(),
                };
                let text: String = String::from_utf8_lossy(&pending[..valid]).into_owned();
                pending.drain(..valid);
                for (event, data) in parser.push(&text) {
                    let parsed = match event.as_str() {
                        "line" => serde_json::from_str(&data).map(FollowEvent::Line),
                        "end" => serde_json::from_str(&data).map(FollowEvent::End),
                        _ => continue,
                    };
                    yield parsed.map_err(|e| ClientError::Transport(format!("unexpected event: {e}")));
                }
            }
        })
    }
}

async fn read(response: http::Response<hyper::body::Incoming>) -> Result<Bytes> {
    Limited::new(response.into_body(), MAX_BODY)
        .collect()
        .await
        .map(http_body_util::Collected::to_bytes)
        .map_err(|e| ClientError::Transport(chain(&*e)))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn app_paths_take_defaults_and_refuse_odd_names() {
        let full = AppPath::parse("shop/prod/web", None, None).expect("full");
        assert_eq!(full.api(), "/projects/shop/environments/prod/apps/web");
        assert_eq!(full.to_string(), "shop/prod/web");
        assert_eq!(
            AppPath::parse("web", Some("shop"), Some("dev")).expect("defaults"),
            AppPath {
                project: "shop".into(),
                environment: "dev".into(),
                app: "web".into()
            }
        );
        assert_eq!(
            AppPath::parse("live/web", Some("shop"), Some("dev"))
                .expect("environment given")
                .environment,
            "live"
        );
        assert!(AppPath::parse("web", None, Some("dev")).is_err(), "no project");
        assert!(AppPath::parse("shop/prod/../x", None, None).is_err());
        assert!(AppPath::parse("shop/prod/Web", None, None).is_err());
        assert!(AppPath::parse("a/b/c/d", None, None).is_err());
    }

    #[test]
    fn the_token_never_shows_in_debug_output() {
        let client = ApiClient::new("https://kuben.example.com/", "kbn_secret");
        let shown = format!("{client:?}");
        assert!(shown.contains("https://kuben.example.com/api/v1"), "{shown}");
        assert!(!shown.contains("kbn_secret"), "{shown}");
    }

    #[test]
    fn log_queries_carry_only_what_is_asked() {
        assert_eq!(LogOptions::default().query(false), "");
        let options = LogOptions {
            tail: Some(20),
            process: Some("worker".into()),
            previous: true,
        };
        assert_eq!(
            options.query(true),
            "?tail=20&process=worker&previous=true&follow=true"
        );
        let odd = LogOptions {
            process: Some("a&b".into()),
            ..LogOptions::default()
        };
        assert_eq!(odd.query(false), "", "a name that is no slug is left out");
    }

    #[test]
    fn server_sent_events_are_split_across_chunks() {
        let mut parser = SseParser::default();
        assert!(parser.push("event: line\ndata: {\"a\"").is_empty());
        assert_eq!(
            parser.push(":1}\n\n: ping\n\nevent: end\r\ndata: {}\r\n\r\ndata: x\n"),
            [
                ("line".to_owned(), "{\"a\":1}".to_owned()),
                ("end".to_owned(), "{}".to_owned())
            ]
        );
        assert_eq!(parser.push("\n"), [("message".to_owned(), "x".to_owned())]);
    }
}
