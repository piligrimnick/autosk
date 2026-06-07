//! Unix-second → RFC3339 UTC formatting.
//!
//! On-disk timestamps are unix-second `INTEGER`s; the wire contract
//! (`autosk-proto`) is RFC3339 UTC. This mirrors Go's
//! `time.Unix(u, 0).UTC()` JSON marshaling, which for whole-second times
//! renders `YYYY-MM-DDTHH:MM:SSZ` with no fractional part.
//!
//! Implemented without a date dependency (no `chrono`/`time`) via Howard
//! Hinnant's `civil_from_days` algorithm — the same one `<chrono>` and Go's
//! `time` package use internally — so the result is correct for any year.

/// Formats a unix-second timestamp as an RFC3339 UTC string with whole-second
/// precision (`YYYY-MM-DDTHH:MM:SSZ`).
pub fn rfc3339_utc(unix: i64) -> String {
    let days = unix.div_euclid(86_400);
    let secs = unix.rem_euclid(86_400);
    let (y, m, d) = civil_from_days(days);
    let hh = secs / 3_600;
    let mm = (secs % 3_600) / 60;
    let ss = secs % 60;
    format!("{y:04}-{m:02}-{d:02}T{hh:02}:{mm:02}:{ss:02}Z")
}

/// Converts a day count relative to the unix epoch (1970-01-01) into a
/// proleptic-Gregorian `(year, month, day)`. Howard Hinnant, "chrono-Compatible
/// Low-Level Date Algorithms".
fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = z - era * 146_097; // [0, 146096]
    let yoe = (doe - doe / 1_460 + doe / 36_524 - doe / 146_096) / 365; // [0, 399]
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100); // [0, 365]
    let mp = (5 * doy + 2) / 153; // [0, 11]
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32; // [1, 31]
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32; // [1, 12]
    (if m <= 2 { y + 1 } else { y }, m, d)
}

#[cfg(test)]
mod tests {
    use super::rfc3339_utc;

    #[test]
    fn known_epochs() {
        assert_eq!(rfc3339_utc(0), "1970-01-01T00:00:00Z");
        assert_eq!(rfc3339_utc(1_700_000_000), "2023-11-14T22:13:20Z");
        // Leap-year day.
        assert_eq!(rfc3339_utc(1_582_934_400), "2020-02-29T00:00:00Z");
    }
}
