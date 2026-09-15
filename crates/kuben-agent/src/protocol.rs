//! The AgentLink protocol (ADR-027, plan §12): the messages the cluster agent
//! and the hub exchange over the mTLS stream the agent opens.
//!
//! * A frame is a big-endian `u32` length followed by that many bytes of
//!   JSON, at most [`MAX_FRAME`]. A peer that announces more is cut off
//!   before anything is allocated.
//! * The agent speaks first: [`Message::Hello`] offers protocol versions and
//!   capabilities. The hub answers [`Message::Welcome`] with the highest
//!   version both speak and the features both know, or with
//!   [`Message::Refused`]. A build speaks its own version N and N-1
//!   ([`SUPPORTED_VERSIONS`]), so a hub at N serves agents at N and N-1.
//! * A message type this build does not know decodes as
//!   [`Message::Unknown`] instead of breaking the link, so a newer peer may
//!   add messages. A command that needs a feature outside the negotiated set
//!   is refused with [`Refusal::UnsupportedCapability`] and never applied.

use std::{collections::BTreeSet, io, ops::RangeInclusive};

use serde::{Deserialize, Serialize};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

/// The protocol version of this build.
pub const PROTOCOL_VERSION: u32 = 1;
const OLDEST_VERSION: u32 = if PROTOCOL_VERSION > 1 {
    PROTOCOL_VERSION - 1
} else {
    1
};
/// The versions this build speaks: N and N-1.
pub const SUPPORTED_VERSIONS: RangeInclusive<u32> = OLDEST_VERSION..=PROTOCOL_VERSION;

/// Largest frame either side sends or accepts, in bytes. Execution
/// envelopes are bounded far below it (128 KiB).
pub const MAX_FRAME: usize = 1024 * 1024;

/// One AgentLink message.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "camelCase", rename_all_fields = "camelCase")]
pub enum Message {
    /// The agent's opening: who it is and what it can do.
    Hello {
        protocol_versions: Vec<u32>,
        agent_version: String,
        cluster_id: String,
        #[serde(default)]
        capabilities: BTreeSet<String>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        kubernetes_version: Option<String>,
    },
    /// The hub accepts: the version and features both sides agreed on, and
    /// how often the agent sends a heartbeat.
    Welcome {
        protocol_version: u32,
        hub_version: String,
        heartbeat_ms: u64,
        features: BTreeSet<String>,
    },
    /// The other side will not go on; the link closes after this.
    Refused {
        reason: Refusal,
        message: String,
    },
    Heartbeat {
        seq: u64,
    },
    /// An anonymous agent asks for its identity: the bootstrap token, its
    /// cluster and a CSR (PEM) signed by its device key.
    Enroll {
        token: Token,
        cluster_id: String,
        csr: String,
    },
    /// A linked agent asks for a fresh certificate before its own expires:
    /// a CSR (PEM) signed by the same device key.
    Renew {
        csr: String,
    },
    /// The hub's answer to [`Message::Enroll`] and [`Message::Renew`]: the
    /// client certificate (PEM) and when it expires (Unix seconds).
    Enrolled {
        certificate: String,
        not_after: i64,
    },
    HeartbeatAck {
        seq: u64,
    },
    /// A message type this build does not know.
    #[serde(other)]
    Unknown,
}

/// Why one side refused.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum Refusal {
    /// No protocol version in common.
    UnsupportedProtocol,
    /// A command needs a feature outside the negotiated set.
    UnsupportedCapability,
    /// The certificate names no enrolled cluster.
    UnknownCluster,
    /// The cluster's enrollment was revoked: re-enroll.
    Revoked,
    /// The message does not fit the protocol at this point.
    BadRequest,
    /// The bootstrap token is unknown, expired, for another cluster or
    /// redeemed by another device (one answer for all, to reveal nothing).
    InvalidToken,
    /// A reason this build does not know.
    #[serde(other)]
    Other,
}

/// A bootstrap token on the wire: serialized as the plain string, never
/// shown by `Debug`, so logging a message cannot leak it.
#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(transparent)]
pub struct Token(pub String);

impl std::fmt::Debug for Token {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Token(***)")
    }
}

/// What both sides agreed on in the handshake.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Negotiated {
    pub version: u32,
    pub features: BTreeSet<String>,
}

impl Negotiated {
    /// Refuse a command that needs `feature` unless both sides know it.
    pub fn require(&self, feature: &str) -> Result<(), Refusal> {
        if self.features.contains(feature) {
            Ok(())
        } else {
            Err(Refusal::UnsupportedCapability)
        }
    }
}

/// The hub's side of the handshake: the highest version in `versions` the
/// agent offers, and the features in `features` the agent also offers.
pub fn negotiate(
    versions: &RangeInclusive<u32>,
    features: &BTreeSet<String>,
    offered_versions: &[u32],
    offered_features: &BTreeSet<String>,
) -> Result<Negotiated, Refusal> {
    let version = offered_versions
        .iter()
        .copied()
        .filter(|v| versions.contains(v))
        .max()
        .ok_or(Refusal::UnsupportedProtocol)?;
    Ok(Negotiated {
        version,
        features: features.intersection(offered_features).cloned().collect(),
    })
}

#[derive(Debug, thiserror::Error)]
pub enum FrameError {
    #[error("the link failed: {0}")]
    Io(#[from] io::Error),
    #[error("a frame of {len} bytes exceeds the {max}-byte limit")]
    TooLarge { len: usize, max: usize },
    #[error("a frame is not a valid message: {0}")]
    Malformed(#[from] serde_json::Error),
}

fn too_large(len: usize) -> FrameError {
    FrameError::TooLarge { len, max: MAX_FRAME }
}

/// Send `message` as one frame.
pub async fn write_frame<W: AsyncWrite + Unpin>(w: &mut W, message: &Message) -> Result<(), FrameError> {
    let body = serde_json::to_vec(message)?;
    if body.len() > MAX_FRAME {
        return Err(too_large(body.len()));
    }
    let len = u32::try_from(body.len()).map_err(|_| too_large(body.len()))?;
    w.write_all(&len.to_be_bytes()).await?;
    w.write_all(&body).await?;
    w.flush().await?;
    Ok(())
}

/// The next message, or `None` when the peer closed the stream between
/// frames. A stream that ends inside a frame is an error.
pub async fn read_frame<R: AsyncRead + Unpin>(r: &mut R) -> Result<Option<Message>, FrameError> {
    let mut header = [0u8; 4];
    if r.read(&mut header[..1]).await? == 0 {
        return Ok(None);
    }
    r.read_exact(&mut header[1..]).await?;
    let len = usize::try_from(u32::from_be_bytes(header)).unwrap_or(usize::MAX);
    if len > MAX_FRAME {
        return Err(too_large(len));
    }
    let mut body = vec![0; len];
    r.read_exact(&mut body).await?;
    Ok(Some(serde_json::from_slice(&body)?))
}

#[cfg(test)]
mod tests {
    use tokio::io::duplex;

    use super::*;

    fn set(items: &[&str]) -> BTreeSet<String> {
        items.iter().map(|s| (*s).to_owned()).collect()
    }

    fn hello() -> Message {
        Message::Hello {
            protocol_versions: vec![PROTOCOL_VERSION],
            agent_version: "1.1.2".into(),
            cluster_id: "0199a0c0-0000-7000-8000-000000000001".into(),
            capabilities: set(&["applicationRuntime"]),
            kubernetes_version: Some("v1.32.2".into()),
        }
    }

    #[tokio::test]
    async fn frames_round_trip_and_a_clean_close_ends_the_stream() {
        let (mut a, mut b) = duplex(4096);
        write_frame(&mut a, &hello()).await.expect("write");
        write_frame(&mut a, &Message::Heartbeat { seq: 7 })
            .await
            .expect("write");
        drop(a);
        assert_eq!(read_frame(&mut b).await.expect("read"), Some(hello()));
        assert_eq!(
            read_frame(&mut b).await.expect("read"),
            Some(Message::Heartbeat { seq: 7 })
        );
        assert_eq!(read_frame(&mut b).await.expect("read"), None);
    }

    #[test]
    fn messages_have_a_stable_camel_case_shape() {
        let json = serde_json::to_value(Message::Heartbeat { seq: 1 }).expect("json");
        assert_eq!(json, serde_json::json!({ "type": "heartbeat", "seq": 1 }));
        let json = serde_json::to_value(hello()).expect("json");
        assert_eq!(json["type"], "hello");
        assert_eq!(json["protocolVersions"], serde_json::json!([PROTOCOL_VERSION]));
        assert_eq!(json["clusterId"], "0199a0c0-0000-7000-8000-000000000001");
        let refused = serde_json::to_value(Message::Refused {
            reason: Refusal::UnsupportedCapability,
            message: "no".into(),
        })
        .expect("json");
        assert_eq!(refused["reason"], "unsupportedCapability");
    }

    #[tokio::test]
    async fn an_unknown_message_or_reason_does_not_break_the_link() {
        let (mut a, mut b) = duplex(4096);
        for body in [
            r#"{"type":"futureThing","anything":[1,2,3]}"#,
            r#"{"type":"refused","reason":"futureReason","message":"x"}"#,
        ] {
            let len = u32::try_from(body.len()).expect("len");
            a.write_all(&len.to_be_bytes()).await.expect("header");
            a.write_all(body.as_bytes()).await.expect("body");
        }
        assert_eq!(read_frame(&mut b).await.expect("read"), Some(Message::Unknown));
        assert_eq!(
            read_frame(&mut b).await.expect("read"),
            Some(Message::Refused {
                reason: Refusal::Other,
                message: "x".into()
            })
        );
    }

    #[tokio::test]
    async fn an_oversized_frame_is_refused_before_it_is_read() {
        let (mut a, mut b) = duplex(64);
        let len = u32::try_from(MAX_FRAME + 1).expect("len");
        a.write_all(&len.to_be_bytes()).await.expect("header");
        match read_frame(&mut b).await {
            Err(FrameError::TooLarge { len, max }) => assert_eq!((len, max), (MAX_FRAME + 1, MAX_FRAME)),
            other => panic!("expected TooLarge, got {other:?}"),
        }

        let huge = Message::Refused {
            reason: Refusal::BadRequest,
            message: "x".repeat(MAX_FRAME),
        };
        assert!(matches!(
            write_frame(&mut a, &huge).await,
            Err(FrameError::TooLarge { .. })
        ));
    }

    #[tokio::test]
    async fn a_stream_that_ends_inside_a_frame_is_an_error() {
        let (mut a, mut b) = duplex(64);
        a.write_all(&10_u32.to_be_bytes()).await.expect("header");
        a.write_all(b"{\"ty").await.expect("part of the body");
        drop(a);
        assert!(matches!(read_frame(&mut b).await, Err(FrameError::Io(_))));
    }

    #[tokio::test]
    async fn a_frame_that_is_not_a_message_is_malformed() {
        let (mut a, mut b) = duplex(64);
        a.write_all(&2_u32.to_be_bytes()).await.expect("header");
        a.write_all(b"[]").await.expect("body");
        assert!(matches!(read_frame(&mut b).await, Err(FrameError::Malformed(_))));
    }

    #[test]
    fn the_handshake_picks_the_highest_common_version_and_shared_features() {
        let hub = set(&["applicationRuntime", "executionTask"]);
        let agreed = negotiate(&(1..=2), &hub, &[1, 2, 3], &set(&["executionTask", "logs"])).expect("agreed");
        assert_eq!(agreed.version, 2);
        assert_eq!(agreed.features, set(&["executionTask"]));
        agreed.require("executionTask").expect("both know it");
        assert_eq!(agreed.require("logs"), Err(Refusal::UnsupportedCapability));
        assert_eq!(
            agreed.require("applicationRuntime"),
            Err(Refusal::UnsupportedCapability)
        );

        assert_eq!(
            negotiate(&(2..=3), &hub, &[1], &BTreeSet::new()),
            Err(Refusal::UnsupportedProtocol)
        );
    }

    #[test]
    fn a_build_speaks_its_version_and_the_one_before() {
        assert!(SUPPORTED_VERSIONS.contains(&PROTOCOL_VERSION));
        assert!(*SUPPORTED_VERSIONS.start() >= 1);
        assert!(PROTOCOL_VERSION - SUPPORTED_VERSIONS.start() <= 1);
    }
}
