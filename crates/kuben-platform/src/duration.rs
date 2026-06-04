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
