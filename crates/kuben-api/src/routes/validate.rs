//! Input validation shared by the REST handlers. Errors are user-facing
//! (`422 validation_failed`), so messages say what is allowed.

use kuben_core::Error;

fn invalid(msg: String) -> Error {
    Error::Validation(msg)
}

/// RFC 1123 label: `a-z`, `0-9`, `-`; no leading/trailing `-`.
pub fn dns_label(field: &str, value: &str, max: usize) -> Result<(), Error> {
    let ok = !value.is_empty()
        && value.len() <= max
        && value
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
        && !value.starts_with('-')
        && !value.ends_with('-');
    if ok {
        Ok(())
    } else {
        Err(invalid(format!(
            "{field} must use a-z, 0-9 and '-', must not start or end with '-', and be at most {max} characters"
        )))
    }
}

/// Fully qualified hostname, e.g. `api.example.com`.
pub fn hostname(host: &str) -> Result<(), Error> {
    let labels: Vec<&str> = host.split('.').collect();
    let ok = host.len() <= 253
        && labels.len() >= 2
        && labels
            .iter()
            .all(|l| dns_label("label", &l.to_ascii_lowercase(), 63).is_ok());
    if ok {
        Ok(())
    } else {
        Err(invalid(format!("`{host}` is not a valid hostname")))
    }
}

/// POSIX-style environment variable name.
pub fn env_var_name(name: &str) -> Result<(), Error> {
    let mut chars = name.chars();
    let ok = name.len() <= 256
        && chars.next().is_some_and(|c| c.is_ascii_alphabetic() || c == '_')
        && chars.all(|c| c.is_ascii_alphanumeric() || c == '_');
    if ok {
        Ok(())
    } else {
        Err(invalid(format!(
            "`{name}` is not a valid environment variable name"
        )))
    }
}

/// Container image reference (syntax only; the registry is not contacted).
pub fn image(image: &str) -> Result<(), Error> {
    let image = image.trim();
    let ok = !image.is_empty()
        && image.len() <= 512
        && !image.starts_with('-')
        && image.chars().all(|c| c.is_ascii_graphic());
    if ok {
        Ok(())
    } else {
        Err(invalid(
            "image must be a container image reference, e.g. nginx:1.27".into(),
        ))
    }
}

/// Key inside a Secret (`[-._a-zA-Z0-9]+`).
pub fn secret_key(key: &str) -> Result<(), Error> {
    let ok = !key.is_empty()
        && key.len() <= 253
        && key
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'));
    if ok {
        Ok(())
    } else {
        Err(invalid(format!("`{key}` is not a valid secret key")))
    }
}

/// Kubernetes quantity such as `500m`, `2`, `1Gi` (syntax check only).
pub fn quantity(field: &str, value: &str) -> Result<(), Error> {
    let ok = !value.is_empty()
        && value.len() <= 32
        && value.starts_with(|c: char| c.is_ascii_digit())
        && value.chars().all(|c| c.is_ascii_alphanumeric() || c == '.');
    if ok {
        Ok(())
    } else {
        Err(invalid(format!(
            "{field} must be a quantity such as 500m, 2 or 4Gi"
        )))
    }
}

/// Plausible email address (syntax only; no delivery is attempted).
pub fn email(value: &str) -> Result<(), Error> {
    let ok = value.len() <= 254
        && !value.chars().any(char::is_whitespace)
        && value.split_once('@').is_some_and(|(local, domain)| {
            !local.is_empty() && domain.contains('.') && !domain.contains('@')
        });
    if ok {
        Ok(())
    } else {
        Err(invalid(format!("`{value}` is not a valid email address")))
    }
}

/// IANA time zone name such as `Europe/Berlin` (syntax only).
pub fn time_zone(value: &str) -> Result<(), Error> {
    let ok = !value.is_empty()
        && value.len() <= 64
        && value
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '/' | '_' | '-' | '+'));
    if ok {
        Ok(())
    } else {
        Err(invalid(format!("`{value}` is not a valid time zone")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn labels() {
        assert!(dns_label("n", "shop-2", 40).is_ok());
        for bad in ["", "Shop", "-a", "a-", "a_b", &"a".repeat(41)] {
            assert!(dns_label("n", bad, 40).is_err(), "{bad}");
        }
    }

    #[test]
    fn hostnames() {
        assert!(hostname("api.example.com").is_ok());
        assert!(hostname("API.Example.com").is_ok());
        for bad in ["localhost", "a..b", "-a.com", "a.com/x"] {
            assert!(hostname(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn env_names_images_keys_quantities() {
        assert!(env_var_name("DATABASE_URL").is_ok());
        assert!(env_var_name("_X1").is_ok());
        assert!(env_var_name("1X").is_err() && env_var_name("A-B").is_err());
        assert!(image("ghcr.io/acme/api@sha256:abc").is_ok());
        assert!(image("nginx latest").is_err() && image("").is_err() && image("--help").is_err());
        assert!(secret_key("tls.crt").is_ok() && secret_key("a/b").is_err());
        assert!(quantity("cpu", "500m").is_ok() && quantity("memory", "4Gi").is_ok());
        assert!(quantity("cpu", "lots").is_err());
    }

    #[test]
    fn emails_and_time_zones() {
        assert!(email("carol@example.com").is_ok());
        for bad in ["carol", "@x.io", "a@b", "a b@c.io", "a@b@c.io"] {
            assert!(email(bad).is_err(), "{bad}");
        }
        assert!(time_zone("Europe/Berlin").is_ok() && time_zone("UTC").is_ok());
        assert!(time_zone("Europe Berlin").is_err() && time_zone("").is_err());
    }
}
