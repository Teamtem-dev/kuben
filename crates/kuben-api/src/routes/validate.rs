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
