//! Managed secret values at rest (ADR-030, M4.4).
//!
//! Every revision gets a random data key (DEK). The values are sealed with
//! AES-256-GCM under the DEK, bound by associated data to the organization,
//! secret and revision; the DEK is sealed under a versioned key-encryption
//! key (KEK), bound to the same identity and the KEK version. Moving a
//! ciphertext to another row, organization or revision makes it unreadable.
//! The KEKs live in a keyring file outside the database and its backups;
//! the newest version seals, every listed version opens.

use std::{
    collections::BTreeMap,
    fmt,
    io::Write as _,
    path::{Path, PathBuf},
};

use base64::{Engine as _, engine::general_purpose::STANDARD};
use kuben_store::{Store, repo::SealedBytes as Sealed};
use ring::{
    aead::{AES_256_GCM, Aad, LessSafeKey, NONCE_LEN, Nonce, UnboundKey},
    rand::{SecureRandom, SystemRandom},
};
use zeroize::Zeroizing;

/// Label of a revision object: the SQL id of its secret.
pub const SECRET_ID: &str = "kuben.dev/secret-id";
/// Label of a revision object: its revision.
pub const SECRET_REVISION: &str = "kuben.dev/secret-revision";

const KEY_LEN: usize = 32;
/// Most keys a keyring holds.
const MAX_VERSIONS: usize = 64;

/// Why a keyring or a sealed value could not be used.
#[derive(Debug, thiserror::Error)]
pub enum SecretError {
    #[error("cannot read the keyring {path}: {reason}")]
    Keyring { path: PathBuf, reason: String },
    #[error("the keyring has no key version {0}")]
    UnknownKey(u32),
    #[error("a sealed value does not open: wrong key, identity or content")]
    Open,
    #[error("the system random source failed")]
    Random,
}

/// Who a sealed value belongs to: the associated data of both seals.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Identity<'a> {
    pub org: &'a str,
    pub secret: &'a str,
    pub revision: u64,
}

impl Identity<'_> {
    fn value_aad(&self) -> Vec<u8> {
        format!("kuben/secret/v1|{}|{}|{}", self.org, self.secret, self.revision).into_bytes()
    }

    fn key_aad(&self, version: u32) -> Vec<u8> {
        format!(
            "kuben/dek/v1|{}|{}|{}|{version}",
            self.org, self.secret, self.revision
        )
        .into_bytes()
    }
}

/// The key-encryption keys, by version.
pub struct Keyring {
    keys: BTreeMap<u32, Zeroizing<[u8; KEY_LEN]>>,
    rng: SystemRandom,
}

impl fmt::Debug for Keyring {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Keyring")
            .field("versions", &self.keys.keys().collect::<Vec<_>>())
            .finish_non_exhaustive()
    }
}

fn aead(key: &[u8]) -> Result<LessSafeKey, SecretError> {
    UnboundKey::new(&AES_256_GCM, key)
        .map(LessSafeKey::new)
        .map_err(|_| SecretError::Open)
}

fn seal_with(key: &[u8], rng: &SystemRandom, aad: &[u8], plaintext: &[u8]) -> Result<Vec<u8>, SecretError> {
    let mut nonce = [0u8; NONCE_LEN];
    rng.fill(&mut nonce).map_err(|_| SecretError::Random)?;
    let mut out = plaintext.to_vec();
    aead(key)?
        .seal_in_place_append_tag(Nonce::assume_unique_for_key(nonce), Aad::from(aad), &mut out)
        .map_err(|_| SecretError::Open)?;
    let mut sealed = nonce.to_vec();
    sealed.extend_from_slice(&out);
    Ok(sealed)
}

fn open_with(key: &[u8], aad: &[u8], sealed: &[u8]) -> Result<Zeroizing<Vec<u8>>, SecretError> {
    if sealed.len() < NONCE_LEN {
        return Err(SecretError::Open);
    }
    let (nonce, body) = sealed.split_at(NONCE_LEN);
    let nonce = Nonce::try_assume_unique_for_key(nonce).map_err(|_| SecretError::Open)?;
    let mut buf = Zeroizing::new(body.to_vec());
    let plain = aead(key)?
        .open_in_place(nonce, Aad::from(aad), &mut buf)
        .map_err(|_| SecretError::Open)?;
    Ok(Zeroizing::new(plain.to_vec()))
}

impl Keyring {
    /// A keyring of exactly these keys (tests, restores).
    #[must_use]
    pub fn from_keys(keys: impl IntoIterator<Item = (u32, [u8; KEY_LEN])>) -> Self {
        Self {
            keys: keys.into_iter().map(|(v, k)| (v, Zeroizing::new(k))).collect(),
            rng: SystemRandom::new(),
        }
    }

    /// Parse `version:base64-key` lines; blank lines and `#` comments are
    /// ignored.
    fn parse(text: &str) -> Result<Self, String> {
        let mut keys = BTreeMap::new();
        for line in text
            .lines()
            .map(str::trim)
            .filter(|l| !l.is_empty() && !l.starts_with('#'))
        {
            let (version, key) = line.split_once(':').ok_or("a line is not `version:key`")?;
            let version: u32 = version.trim().parse().map_err(|_| "a version is not a number")?;
            let bytes = Zeroizing::new(STANDARD.decode(key.trim()).map_err(|_| "a key is not base64")?);
            let key: [u8; KEY_LEN] = bytes.as_slice().try_into().map_err(|_| "a key is not 32 bytes")?;
            if version == 0 || keys.insert(version, Zeroizing::new(key)).is_some() {
                return Err("versions must be unique and above zero".into());
            }
        }
        if keys.is_empty() || keys.len() > MAX_VERSIONS {
            return Err(format!("a keyring holds 1 to {MAX_VERSIONS} keys"));
        }
        Ok(Self {
            keys,
            rng: SystemRandom::new(),
        })
    }

    /// Read the keyring at `path`, which must exist and be private.
    pub fn load(path: &Path) -> Result<Self, SecretError> {
        let err = |reason: String| SecretError::Keyring {
            path: path.to_owned(),
            reason,
        };
        let text = Zeroizing::new(std::fs::read_to_string(path).map_err(|e| err(e.to_string()))?);
        check_private(path).map_err(err)?;
        Self::parse(&text).map_err(err)
    }

    /// Write `text`, a keyring, to `path` (mode 0600) when nothing is there.
    pub fn install(path: &Path, text: &str) -> Result<Self, SecretError> {
        let keyring = Self::parse(text).map_err(|reason| SecretError::Keyring {
            path: path.to_owned(),
            reason,
        })?;
        write_private(path, text.as_bytes()).map_err(|e| SecretError::Keyring {
            path: path.to_owned(),
            reason: e.to_string(),
        })?;
        Ok(keyring)
    }

    /// Read the keyring at `path`, creating it with one fresh key (mode 0600)
    /// when it does not exist.
    pub fn load_or_create(path: &Path) -> Result<Self, SecretError> {
        let err = |reason: String| SecretError::Keyring {
            path: path.to_owned(),
            reason,
        };
        match std::fs::read_to_string(path) {
            Ok(text) => {
                check_private(path).map_err(err)?;
                Self::parse(&text).map_err(err)
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                let mut key = Zeroizing::new([0u8; KEY_LEN]);
                SystemRandom::new()
                    .fill(key.as_mut())
                    .map_err(|_| SecretError::Random)?;
                let text = Zeroizing::new(format!(
                    "# Kuben secret keyring: version:base64 key. Back it up apart from the database.\n1:{}\n",
                    STANDARD.encode(key.as_ref())
                ));
                write_private(path, text.as_bytes()).map_err(|e| err(e.to_string()))?;
                Ok(Self::from_keys([(1, *key)]))
            }
            Err(e) => Err(err(e.to_string())),
        }
    }

    /// Every version with the fingerprint of its key, which identifies the
    /// key without revealing it.
    #[must_use]
    pub fn fingerprints(&self) -> Vec<(u32, [u8; 32])> {
        self.keys
            .iter()
            .map(|(version, key)| {
                let mut ctx = ring::digest::Context::new(&ring::digest::SHA256);
                ctx.update(b"kuben/kek-fingerprint/v1|");
                ctx.update(key.as_slice());
                let mut out = [0u8; 32];
                out.copy_from_slice(ctx.finish().as_ref());
                (*version, out)
            })
            .collect()
    }

    /// The version that seals new values.
    #[must_use]
    pub fn current(&self) -> u32 {
        self.keys.keys().next_back().copied().unwrap_or(0)
    }

    fn key(&self, version: u32) -> Result<&[u8], SecretError> {
        self.keys
            .get(&version)
            .map(|k| k.as_slice())
            .ok_or(SecretError::UnknownKey(version))
    }

    /// Seal `plaintext` for `who` under the current key.
    pub fn seal(&self, who: Identity<'_>, plaintext: &[u8]) -> Result<Sealed, SecretError> {
        let version = self.current();
        let mut dek = Zeroizing::new([0u8; KEY_LEN]);
        self.rng.fill(dek.as_mut()).map_err(|_| SecretError::Random)?;
        Ok(Sealed {
            ciphertext: seal_with(dek.as_ref(), &self.rng, &who.value_aad(), plaintext)?,
            wrapped_key: seal_with(self.key(version)?, &self.rng, &who.key_aad(version), dek.as_ref())?,
            key_version: version,
        })
    }

    /// Open `sealed`, which must belong to `who`.
    pub fn open(&self, who: Identity<'_>, sealed: &Sealed) -> Result<Zeroizing<Vec<u8>>, SecretError> {
        let dek = open_with(
            self.key(sealed.key_version)?,
            &who.key_aad(sealed.key_version),
            &sealed.wrapped_key,
        )?;
        open_with(&dek, &who.value_aad(), &sealed.ciphertext)
    }

    /// Seal the values of a revision: a JSON object of key to value.
    pub fn seal_values(
        &self,
        who: Identity<'_>,
        values: &BTreeMap<String, String>,
    ) -> Result<Sealed, SecretError> {
        let json = Zeroizing::new(serde_json::to_vec(values).map_err(|_| SecretError::Open)?);
        self.seal(who, &json)
    }

    /// Open the values of a revision sealed by [`Keyring::seal_values`].
    pub fn open_values(
        &self,
        who: Identity<'_>,
        sealed: &Sealed,
    ) -> Result<BTreeMap<String, String>, SecretError> {
        let json = self.open(who, sealed)?;
        serde_json::from_slice(&json).map_err(|_| SecretError::Open)
    }

    /// `sealed` with its data key sealed again under the current key; the
    /// values are not touched.
    pub fn rewrap(&self, who: Identity<'_>, sealed: &Sealed) -> Result<Sealed, SecretError> {
        let version = self.current();
        let dek = open_with(
            self.key(sealed.key_version)?,
            &who.key_aad(sealed.key_version),
            &sealed.wrapped_key,
        )?;
        Ok(Sealed {
            ciphertext: sealed.ciphertext.clone(),
            wrapped_key: seal_with(self.key(version)?, &self.rng, &who.key_aad(version), &dek)?,
            key_version: version,
        })
    }
}

/// Revisions resealed per transaction.
const RESEAL_BATCH: i64 = 100;

/// Check `keyring` against the keys this installation has used, then seal
/// the data keys of older revisions under its current key. Fails when the
/// keyring holds another key under a known version: sealing with it would
/// make secrets unreadable to the other replicas. The number resealed.
pub async fn prepare(store: &Store, keyring: &Keyring) -> anyhow::Result<usize> {
    let check = store.check_keyring(&keyring.fingerprints()).await?;
    if !check.mismatched.is_empty() {
        anyhow::bail!(
            "the secret keyring holds other keys than this installation's under versions {:?}: every replica must read the same keyring (secrets.keyring_file)",
            check.mismatched
        );
    }
    if !check.missing.is_empty() {
        tracing::warn!(
            versions = ?check.missing,
            "the secret keyring lacks keys this installation used: revisions sealed with them cannot be delivered"
        );
    }
    let current = keyring.current();
    let mut resealed = 0;
    for org in store.org_ids().await? {
        let org_text = org.to_string();
        loop {
            let mut tenant = store.tenant(org).await?;
            let stale = tenant.stale_seals(current, RESEAL_BATCH).await?;
            let mut moved = 0;
            for s in &stale {
                let secret = s.secret.to_string();
                let who = Identity {
                    org: &org_text,
                    secret: &secret,
                    revision: s.revision,
                };
                match keyring.rewrap(who, &s.sealed) {
                    Ok(new) => moved += usize::from(tenant.reseal(s, &new).await?),
                    Err(e) => {
                        tracing::warn!(%org, %secret, revision = s.revision, error = %e, "a secret revision cannot be resealed");
                    }
                }
            }
            tenant.commit().await?;
            resealed += moved;
            if moved == 0 || stale.len() < usize::try_from(RESEAL_BATCH).unwrap_or(usize::MAX) {
                break;
            }
        }
    }
    if resealed > 0 {
        tracing::info!(
            resealed,
            key_version = current,
            "secret revisions resealed under the current key"
        );
    }
    Ok(resealed)
}

#[cfg(unix)]
fn check_private(path: &Path) -> Result<(), String> {
    use std::os::unix::fs::PermissionsExt as _;
    let mode = std::fs::metadata(path)
        .map_err(|e| e.to_string())?
        .permissions()
        .mode();
    // Nobody else may read it and no group may write it. Group read stays
    // allowed: Kubernetes adds it to Secret volumes of pods with `fsGroup`.
    let exposed = mode & 0o027;
    if exposed != 0 {
        return Err(format!(
            "the keyring is readable by others or writable by its group (mode {:o}); chmod 600 it",
            mode & 0o777
        ));
    }
    Ok(())
}

#[cfg(not(unix))]
fn check_private(_path: &Path) -> Result<(), String> {
    Ok(())
}

fn write_private(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir)?;
    }
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    let mut file = options.open(path)?;
    file.write_all(bytes)?;
    file.sync_all()
}

/// The Kubernetes Secret that carries revision `revision` of secret `name`.
/// Secret names made before M4.4 are DNS labels, so a name with a dot never
/// collides with one of them.
#[must_use]
pub fn object_name(name: &str, revision: u64) -> String {
    format!("{name}.r{revision}")
}

/// Docker Hub's registry name in image references.
pub const DOCKER_HUB: &str = "docker.io";

/// Credentials for pulling from a private registry: the values of a
/// `registry` secret.
#[derive(Clone, PartialEq, Eq)]
pub struct RegistryLogin {
    pub username: String,
    pub password: String,
}

impl fmt::Debug for RegistryLogin {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("RegistryLogin")
            .field("username", &self.username)
            .finish_non_exhaustive()
    }
}

impl RegistryLogin {
    const USERNAME: &str = "username";
    const PASSWORD: &str = "password";

    /// The login in the values of a `registry` secret.
    #[must_use]
    pub fn from_values(values: &BTreeMap<String, String>) -> Option<Self> {
        Some(Self {
            username: values.get(Self::USERNAME)?.clone(),
            password: values.get(Self::PASSWORD)?.clone(),
        })
    }

    /// The values a `registry` secret stores.
    #[must_use]
    pub fn values(&self) -> BTreeMap<String, String> {
        BTreeMap::from([
            (Self::USERNAME.to_owned(), self.username.clone()),
            (Self::PASSWORD.to_owned(), self.password.clone()),
        ])
    }

    /// An `Authorization: Basic …` value.
    #[must_use]
    pub fn basic(&self) -> String {
        format!(
            "Basic {}",
            STANDARD.encode(format!("{}:{}", self.username, self.password))
        )
    }

    /// The `.dockerconfigjson` of a pull secret for `registry` (a registry
    /// name as image references carry it).
    #[must_use]
    pub fn docker_config(&self, registry: &str) -> String {
        // The kubelet knows Docker Hub by its legacy index URL.
        let key = if registry == DOCKER_HUB {
            "https://index.docker.io/v1/"
        } else {
            registry
        };
        serde_json::json!({ "auths": { key: {
            "username": self.username,
            "password": self.password,
            "auth": STANDARD.encode(format!("{}:{}", self.username, self.password)),
        } } })
        .to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ring(versions: &[u32]) -> Keyring {
        Keyring::from_keys(
            versions
                .iter()
                .map(|v| (*v, [u8::try_from(*v).unwrap_or(0); KEY_LEN])),
        )
    }

    const WHO: Identity<'static> = Identity {
        org: "org-1",
        secret: "sec-1",
        revision: 3,
    };

    #[test]
    fn values_round_trip() {
        let k = ring(&[1]);
        let sealed = k.seal(WHO, b"{\"DB_PASSWORD\":\"hunter2\"}").expect("seal");
        assert_eq!(sealed.key_version, 1);
        assert!(!sealed.ciphertext.windows(7).any(|w| w == b"hunter2"));
        assert_eq!(
            k.open(WHO, &sealed).expect("open").as_slice(),
            b"{\"DB_PASSWORD\":\"hunter2\"}"
        );
        let again = k.seal(WHO, b"{\"DB_PASSWORD\":\"hunter2\"}").expect("seal");
        assert_ne!(
            again.ciphertext, sealed.ciphertext,
            "fresh nonce and key every time"
        );
        assert!(!format!("{sealed:?}").contains("hunter2"));
    }

    #[test]
    fn values_are_sealed_as_one_object() {
        let k = ring(&[1]);
        let values = BTreeMap::from([("url".to_owned(), "postgres://x".to_owned())]);
        let sealed = k.seal_values(WHO, &values).expect("seal");
        assert_eq!(k.open_values(WHO, &sealed).expect("open"), values);
        let not_an_object = k.seal(WHO, b"[1]").expect("seal");
        assert!(k.open_values(WHO, &not_an_object).is_err());
    }

    #[test]
    fn a_value_moved_to_another_identity_does_not_open() {
        let k = ring(&[1]);
        let sealed = k.seal(WHO, b"v").expect("seal");
        for other in [
            Identity { org: "org-2", ..WHO },
            Identity {
                secret: "sec-2",
                ..WHO
            },
            Identity { revision: 4, ..WHO },
        ] {
            assert!(
                matches!(k.open(other, &sealed), Err(SecretError::Open)),
                "{other:?}"
            );
        }
        let mut tampered = sealed.clone();
        let last = tampered.ciphertext.len() - 1;
        tampered.ciphertext[last] ^= 1;
        assert!(matches!(k.open(WHO, &tampered), Err(SecretError::Open)));
        let relabelled = Sealed {
            key_version: 2,
            ..sealed.clone()
        };
        assert!(matches!(
            k.open(WHO, &relabelled),
            Err(SecretError::UnknownKey(2))
        ));
        let truncated = Sealed {
            ciphertext: vec![1, 2],
            ..sealed
        };
        assert!(matches!(k.open(WHO, &truncated), Err(SecretError::Open)));
    }

    #[test]
    fn rotation_keeps_old_revisions_readable_and_rewraps_them() {
        let old = ring(&[1]);
        let sealed = old.seal(WHO, b"v").expect("seal");
        let rotated = ring(&[1, 2]);
        assert_eq!(rotated.current(), 2);
        assert_eq!(rotated.open(WHO, &sealed).expect("old key").as_slice(), b"v");
        let rewrapped = rotated.rewrap(WHO, &sealed).expect("rewrap");
        assert_eq!(rewrapped.key_version, 2);
        assert_eq!(rewrapped.ciphertext, sealed.ciphertext, "values untouched");
        let retired = ring(&[2]);
        assert_eq!(retired.open(WHO, &rewrapped).expect("new key").as_slice(), b"v");
        assert!(retired.open(WHO, &sealed).is_err(), "the retired key is gone");
        let wrong = Keyring::from_keys([(1, [9; KEY_LEN])]);
        assert!(
            matches!(wrong.open(WHO, &sealed), Err(SecretError::Open)),
            "another installation"
        );
    }

    #[test]
    fn registry_logins_become_pull_secrets() {
        let login = RegistryLogin {
            username: "bot".into(),
            password: "s3cret".into(),
        };
        assert_eq!(RegistryLogin::from_values(&login.values()), Some(login.clone()));
        assert_eq!(RegistryLogin::from_values(&BTreeMap::new()), None);
        assert_eq!(login.basic(), "Basic Ym90OnMzY3JldA==");
        assert!(!format!("{login:?}").contains("s3cret"));
        let config: serde_json::Value = serde_json::from_str(&login.docker_config("ghcr.io")).expect("json");
        assert_eq!(config["auths"]["ghcr.io"]["auth"], "Ym90OnMzY3JldA==");
        let hub: serde_json::Value = serde_json::from_str(&login.docker_config(DOCKER_HUB)).expect("json");
        assert_eq!(hub["auths"]["https://index.docker.io/v1/"]["username"], "bot");
    }

    #[test]
    fn fingerprints_name_keys_without_revealing_them() {
        let a = Keyring::from_keys([(1, [1; KEY_LEN]), (2, [2; KEY_LEN])]).fingerprints();
        let b = Keyring::from_keys([(1, [1; KEY_LEN]), (2, [9; KEY_LEN])]).fingerprints();
        assert_eq!(a.len(), 2);
        assert_eq!(a[0], b[0]);
        assert_ne!(a[1], b[1]);
        assert_ne!(a[0].1, [1; KEY_LEN]);
    }

    #[test]
    fn keyrings_parse_strictly() {
        let key = STANDARD.encode([7u8; KEY_LEN]);
        let k = Keyring::parse(&format!("# comment\n\n1:{key}\n 3 : {key}\n")).expect("keyring");
        assert_eq!(k.current(), 3);
        for bad in [
            String::new(),
            format!("0:{key}"),
            format!("1:{key}\n1:{key}"),
            "1:short".to_owned(),
            format!("x:{key}"),
            key.clone(),
            format!("1:{}", STANDARD.encode([7u8; 16])),
        ] {
            assert!(Keyring::parse(&bad).is_err(), "{bad:?}");
        }
        assert!(!format!("{k:?}").contains(&key));
    }

    #[test]
    fn a_new_keyring_is_private_and_reused() {
        let dir = std::env::temp_dir().join(format!("kuben-keyring-{}", uuid::Uuid::now_v7()));
        let path = dir.join("secrets.keyring");
        let first = Keyring::load_or_create(&path).expect("create");
        let sealed = first.seal(WHO, b"v").expect("seal");
        let second = Keyring::load_or_create(&path).expect("load");
        assert_eq!(second.open(WHO, &sealed).expect("same key").as_slice(), b"v");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = std::fs::metadata(&path).expect("meta").permissions().mode();
            assert_eq!(mode & 0o777, 0o600);
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o640)).expect("chmod");
            assert!(
                Keyring::load_or_create(&path).is_ok(),
                "group read, as fsGroup makes it"
            );
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).expect("chmod");
            assert!(matches!(
                Keyring::load_or_create(&path),
                Err(SecretError::Keyring { .. })
            ));
        }
        std::fs::remove_dir_all(dir).expect("cleanup");
    }

    #[test]
    fn a_restored_keyring_is_installed_once_and_loaded() {
        let dir = std::env::temp_dir().join(format!("kuben-keyring-{}", uuid::Uuid::now_v7()));
        let path = dir.join("secrets.keyring");
        assert!(Keyring::load(&path).is_err(), "missing");
        let text = format!("1:{}\n", STANDARD.encode([5u8; KEY_LEN]));
        let installed = Keyring::install(&path, &text).expect("install");
        assert!(
            Keyring::install(&path, &text).is_err(),
            "never over an existing file"
        );
        assert!(Keyring::install(&dir.join("other"), "garbage").is_err());
        let loaded = Keyring::load(&path).expect("load");
        assert_eq!(loaded.fingerprints(), installed.fingerprints());
        std::fs::remove_dir_all(dir).expect("cleanup");
    }

    #[test]
    fn revision_objects_are_named_after_the_secret() {
        assert_eq!(object_name("db", 12), "db.r12");
    }
}
