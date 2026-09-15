//! The agent's state on disk and how it comes by an identity.
//!
//! * `device.key`: the device key, made on the first start and never sent
//!   anywhere;
//! * `agent.crt`: the certificate the hub issued for that key.
//!
//! Both are readable by the agent's user alone, in a directory only it may
//! enter. The bootstrap token is read from a file or stdin, and only when
//! the agent has to enroll; it is never stored.

use std::{
    fs, io,
    io::{Read as _, Write as _},
    path::{Path, PathBuf},
};

use rustls::pki_types::{CertificateDer, ServerName, pem::PemObject};
use time::OffsetDateTime;
use tokio_rustls::TlsConnector;
use x509_parser::prelude::{FromDer, X509Certificate};

use crate::{
    enroll::{DeviceKey, EnrollError, request_enrollment},
    link::Connector,
    tls::{Identity, TlsError, agent_config},
};

pub const DEVICE_KEY: &str = "device.key";
pub const CERTIFICATE: &str = "agent.crt";

#[derive(Debug, thiserror::Error)]
pub enum StateError {
    #[error("{path}: {source}")]
    Io { path: PathBuf, source: io::Error },
    #[error("{path}: {reason}")]
    Invalid { path: PathBuf, reason: String },
    #[error(transparent)]
    Enroll(#[from] EnrollError),
    #[error(transparent)]
    Tls(#[from] TlsError),
    #[error("cannot reach the hub to enroll: {0}")]
    Connect(io::Error),
    #[error("TLS with the hub failed while enrolling: {0}")]
    Handshake(io::Error),
    #[error(
        "this agent has no valid certificate: enroll with a bootstrap token (--token-file or --token-stdin)"
    )]
    NeedsEnrollment,
}

fn io_error(path: &Path) -> impl FnOnce(io::Error) -> StateError + '_ {
    move |source| StateError::Io {
        path: path.to_owned(),
        source,
    }
}

fn invalid(path: &Path, reason: &dyn std::fmt::Display) -> StateError {
    StateError::Invalid {
        path: path.to_owned(),
        reason: reason.to_string(),
    }
}

/// Write `content` to `path` as a new file readable by its owner only; an
/// existing file is replaced, so a leftover never keeps a wider mode.
fn write_owner_only(path: &Path, content: &str) -> Result<(), StateError> {
    fs::remove_file(path).ok();
    let mut options = fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    options
        .open(path)
        .and_then(|mut file| file.write_all(content.as_bytes()))
        .map_err(io_error(path))
}

/// A certificate the agent keeps.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct StoredCertificate {
    pub pem: String,
    pub not_after: OffsetDateTime,
    /// The SubjectPublicKeyInfo the certificate is for.
    pub public_key_info: Vec<u8>,
}

impl StoredCertificate {
    /// Read the expiry and key of a certificate in PEM.
    pub fn parse(pem: &str) -> Result<Self, String> {
        let der = CertificateDer::from_pem_slice(pem.as_bytes()).map_err(|e| e.to_string())?;
        let (_, cert) = X509Certificate::from_der(&der).map_err(|e| e.to_string())?;
        let not_after = OffsetDateTime::from_unix_timestamp(cert.validity().not_after.timestamp())
            .map_err(|e| e.to_string())?;
        Ok(Self {
            pem: pem.to_owned(),
            not_after,
            public_key_info: cert.public_key().raw.to_vec(),
        })
    }
}

/// The agent's state directory.
#[derive(Clone, Debug)]
pub struct State {
    dir: PathBuf,
}

impl State {
    /// Open `dir`, creating it (entered by its owner only) if needed.
    pub fn open(dir: &Path) -> Result<Self, StateError> {
        fs::create_dir_all(dir).map_err(io_error(dir))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            fs::set_permissions(dir, fs::Permissions::from_mode(0o700)).map_err(io_error(dir))?;
        }
        Ok(Self { dir: dir.to_owned() })
    }

    fn path(&self, name: &str) -> PathBuf {
        self.dir.join(name)
    }

    /// The device key, made and stored on the first call.
    pub fn device_key(&self) -> Result<DeviceKey, StateError> {
        let path = self.path(DEVICE_KEY);
        match fs::read_to_string(&path) {
            Ok(pem) => DeviceKey::from_pem(&pem).map_err(|e| invalid(&path, &e)),
            Err(e) if e.kind() == io::ErrorKind::NotFound => {
                let key = DeviceKey::generate()?;
                write_owner_only(&path, &key.to_pem())?;
                Ok(key)
            }
            Err(e) => Err(io_error(&path)(e)),
        }
    }

    /// The stored certificate, if any.
    pub fn certificate(&self) -> Result<Option<StoredCertificate>, StateError> {
        let path = self.path(CERTIFICATE);
        match fs::read_to_string(&path) {
            Ok(pem) => StoredCertificate::parse(&pem)
                .map(Some)
                .map_err(|reason| invalid(&path, &reason)),
            Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(io_error(&path)(e)),
        }
    }

    pub fn save_certificate(&self, pem: &str) -> Result<(), StateError> {
        write_owner_only(&self.path(CERTIFICATE), pem)
    }
}

/// The CA certificates in the PEM file at `path`: the hub CA to pin.
pub fn pinned_ca(path: &Path) -> Result<Vec<CertificateDer<'static>>, StateError> {
    let pem = fs::read(path).map_err(io_error(path))?;
    let cas = CertificateDer::pem_slice_iter(&pem)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| invalid(path, &e))?;
    if cas.is_empty() {
        return Err(invalid(path, &"no certificate in the file"));
    }
    Ok(cas)
}

/// Where the bootstrap token comes from.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum TokenSource {
    None,
    File(PathBuf),
    Stdin,
}

fn trimmed(text: &str) -> Option<String> {
    let token = text.trim();
    (!token.is_empty()).then(|| token.to_owned())
}

/// The bootstrap token from `source`, surrounding whitespace removed.
pub fn read_token(source: &TokenSource) -> Result<Option<String>, StateError> {
    match source {
        TokenSource::None => Ok(None),
        TokenSource::File(path) => Ok(trimmed(&fs::read_to_string(path).map_err(io_error(path))?)),
        TokenSource::Stdin => {
            let mut text = String::new();
            io::stdin()
                .read_to_string(&mut text)
                .map_err(io_error(Path::new("<stdin>")))?;
            Ok(trimmed(&text))
        }
    }
}

/// How the agent reaches the hub to enroll.
#[derive(Debug)]
pub struct HubAddress<'a, C> {
    pub connector: &'a C,
    /// The hub CA the agent pins.
    pub pinned: &'a [CertificateDer<'static>],
    /// The name the hub's certificate carries.
    pub name: &'a ServerName<'static>,
}

/// The agent's identity: the stored certificate while it is valid at `now`
/// and made for `key`; else a fresh enrollment with the token `token`
/// yields (read only then), stored for the next start.
pub async fn ensure_identity<C: Connector>(
    state: &State,
    key: &DeviceKey,
    hub: &HubAddress<'_, C>,
    cluster_id: &str,
    token: impl FnOnce() -> Result<Option<String>, StateError>,
    now: OffsetDateTime,
) -> Result<Identity, StateError> {
    if let Some(stored) = state.certificate()? {
        if stored.not_after > now && stored.public_key_info == key.public_key_info() {
            return Ok(key.identity(&stored.pem)?);
        }
        tracing::info!(not_after = %stored.not_after, "the stored certificate is expired or for another key");
    }
    let Some(token) = token()? else {
        return Err(StateError::NeedsEnrollment);
    };
    let tcp = hub.connector.connect().await.map_err(StateError::Connect)?;
    let mut tls = TlsConnector::from(agent_config(hub.pinned, None)?)
        .connect(hub.name.clone(), tcp)
        .await
        .map_err(StateError::Handshake)?;
    let enrolled = request_enrollment(&mut tls, &token, cluster_id, key).await?;
    state.save_certificate(&enrolled.certificate_pem)?;
    tracing::info!(device = %key.device_id(), cluster = cluster_id, "enrolled");
    Ok(key.identity(&enrolled.certificate_pem)?)
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::*;
    use crate::enroll::{ClusterCa, Csr};

    fn temp_dir(name: &str) -> PathBuf {
        std::env::temp_dir().join(format!(
            "kuben-agent-{name}-{}-{}",
            std::process::id(),
            OffsetDateTime::now_utc().unix_timestamp_nanos()
        ))
    }

    #[test]
    fn the_device_key_is_made_once_and_kept_private() {
        let dir = temp_dir("key");
        let state = State::open(&dir).expect("state");
        let key = state.device_key().expect("made");
        assert_eq!(state.device_key().expect("read").device_id(), key.device_id());
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = |p: &Path| fs::metadata(p).expect("metadata").permissions().mode() & 0o777;
            assert_eq!(mode(&dir.join(DEVICE_KEY)), 0o600);
            assert_eq!(mode(&dir), 0o700);
        }
        fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn a_stored_certificate_carries_its_expiry_and_key() {
        let dir = temp_dir("cert");
        let state = State::open(&dir).expect("state");
        assert_eq!(state.certificate().expect("read"), None);
        let key = state.device_key().expect("key");
        let ca = ClusterCa::generate("kuben cluster CA").expect("ca");
        let now = OffsetDateTime::from_unix_timestamp(1_789_000_000).expect("time");
        let csr = Csr::parse(&key.csr_pem("primary").expect("csr")).expect("parse");
        let issued = ca
            .issue("primary", &csr, Duration::from_hours(24), now)
            .expect("issue");
        state.save_certificate(&issued.certificate_pem).expect("save");
        let stored = state.certificate().expect("read").expect("stored");
        assert_eq!(stored.not_after, issued.not_after);
        assert_eq!(stored.public_key_info, key.public_key_info());

        fs::write(dir.join(CERTIFICATE), "garbage").expect("write");
        assert!(matches!(state.certificate(), Err(StateError::Invalid { .. })));
        fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn tokens_come_trimmed_from_a_file_and_never_from_nowhere() {
        let dir = temp_dir("token");
        fs::create_dir_all(&dir).expect("dir");
        let file = dir.join("token");
        fs::write(&file, "kbt_abc\n").expect("write");
        assert_eq!(
            read_token(&TokenSource::File(file.clone())).expect("read"),
            Some("kbt_abc".into())
        );
        fs::write(&file, " \n").expect("write");
        assert_eq!(read_token(&TokenSource::File(file)).expect("read"), None);
        assert_eq!(read_token(&TokenSource::None).expect("none"), None);
        fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn the_pinned_ca_file_must_hold_a_certificate() {
        let dir = temp_dir("ca");
        fs::create_dir_all(&dir).expect("dir");
        let ca = ClusterCa::generate("kuben CA").expect("ca");
        let pem = format!(
            "-----BEGIN CERTIFICATE-----\n{}\n-----END CERTIFICATE-----\n",
            base64_lines(ca.certificate())
        );
        let file = dir.join("ca.pem");
        fs::write(&file, pem).expect("write");
        assert_eq!(pinned_ca(&file).expect("pinned"), vec![ca.certificate().clone()]);
        fs::write(&file, "").expect("write");
        assert!(matches!(pinned_ca(&file), Err(StateError::Invalid { .. })));
        fs::remove_dir_all(&dir).ok();
    }

    /// Standard base64 of `der`, 64 characters a line (as PEM has it).
    fn base64_lines(der: &[u8]) -> String {
        const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
        let mut out = String::new();
        for chunk in der.chunks(3) {
            let b = [chunk[0], *chunk.get(1).unwrap_or(&0), *chunk.get(2).unwrap_or(&0)];
            let n = (u32::from(b[0]) << 16) | (u32::from(b[1]) << 8) | u32::from(b[2]);
            for i in 0..4 {
                if i <= chunk.len() {
                    out.push(char::from(ALPHABET[((n >> (18 - 6 * i)) & 63) as usize]));
                } else {
                    out.push('=');
                }
            }
        }
        out.as_bytes()
            .chunks(64)
            .map(|line| String::from_utf8_lossy(line).into_owned())
            .collect::<Vec<_>>()
            .join("\n")
    }
}
