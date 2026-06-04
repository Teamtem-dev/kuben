//! Human durations used in CRD fields: `90s`, `15m`, `168h`, `14d`, `1h30m`.

use std::time::Duration;

/// Parse a duration made of `<integer><unit>` segments (`s`, `m`, `h`, `d`).
/// Returns `None` for empty, malformed or overflowing input.
#[must_use]
pub fn parse(input: &str) -> Option<Duration> {
    let s = input.trim();
    if s.is_empty() {
        return None;
    }
    let mut total: u64 = 0;
    let mut digits = String::new();
    for c in s.chars() {
        if c.is_ascii_digit() {
            digits.push(c);
            continue;
        }
        let unit: u64 = match c {
            's' => 1,
            'm' => 60,
            'h' => 3_600,
            'd' => 86_400,
            _ => return None,
        };
        let n: u64 = digits.parse().ok()?;
        digits.clear();
        total = total.checked_add(n.checked_mul(unit)?)?;
    }
    if !digits.is_empty() {
        return None; // trailing number without unit
    }
    Some(Duration::from_secs(total))
}

#[cfg(test)]
mod tests {
