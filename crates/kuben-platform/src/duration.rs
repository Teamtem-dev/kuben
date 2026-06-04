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
    use super::*;

    #[test]
    fn parses_units_and_compounds() {
        assert_eq!(parse("90s"), Some(Duration::from_secs(90)));
        assert_eq!(parse("15m"), Some(Duration::from_mins(15)));
        assert_eq!(parse("168h"), Some(Duration::from_hours(168)));
        assert_eq!(parse("14d"), Some(Duration::from_hours(14 * 24)));
        assert_eq!(parse("1h30m"), Some(Duration::from_mins(90)));
        assert_eq!(parse("0s"), Some(Duration::ZERO));
    }

    #[test]
    fn rejects_garbage() {
        for bad in ["", "  ", "10", "h", "1x", "-1h", "1.5h", "99999999999999999999d"] {
            assert_eq!(parse(bad), None, "{bad:?}");
        }
    }
}
