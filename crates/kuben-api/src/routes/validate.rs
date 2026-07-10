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

