//! OCI artifact references (ADR-026): a release names its images by digest,
//! never by tag.

use std::{fmt, str::FromStr};

use serde::{Deserialize, Serialize};

/// An OCI content digest: `sha256:` with 64 or `sha512:` with 128 lowercase
/// hex digits.
#[derive(Clone, Debug, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct Digest(String);

#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
#[error("not an OCI sha256 or sha512 digest: {0:?}")]
pub struct InvalidDigest(pub String);

impl Digest {
    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl FromStr for Digest {
    type Err = InvalidDigest;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let hex_len = match s.split_once(':') {
            Some(("sha256", hex)) => (hex.len() == 64).then_some(hex),
            Some(("sha512", hex)) => (hex.len() == 128).then_some(hex),
            _ => None,
        };
        match hex_len {
            Some(hex)
                if hex
                    .bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)) =>
            {
                Ok(Self(s.to_owned()))
            }
            _ => Err(InvalidDigest(s.to_owned())),
        }
    }
}

impl TryFrom<String> for Digest {
    type Error = InvalidDigest;

    fn try_from(s: String) -> Result<Self, Self::Error> {
        s.parse()
    }
}

impl From<Digest> for String {
    fn from(d: Digest) -> Self {
        d.0
    }
}

impl fmt::Display for Digest {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const SHA256: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    #[test]
    fn accepts_only_full_lowercase_digests() {
        assert_eq!(SHA256.parse::<Digest>().expect("sha256").as_str(), SHA256);
        let sha512 = format!("sha512:{}", "a".repeat(128));
        assert!(sha512.parse::<Digest>().is_ok());
        for bad in [
            "nginx:1.27",
            "sha256:abc",
            &SHA256.to_uppercase(),
            &format!("{SHA256}0"),
            &SHA256.replace("sha256", "md5"),
            "",
        ] {
            assert!(bad.parse::<Digest>().is_err(), "{bad:?} is not a digest");
        }
    }

    #[test]
    fn serde_validates_too() {
        let ok: Digest = serde_json::from_str(&format!("{SHA256:?}")).expect("valid");
        assert_eq!(
            serde_json::to_string(&ok).expect("serialize"),
            format!("{SHA256:?}")
        );
        assert!(serde_json::from_str::<Digest>("\"latest\"").is_err());
    }
}
