//! Transcript reader — the Rust port of `internal/daemon/transcript`.
//!
//! Reads pi's `session.jsonl` and projects each entry into one or more
//! `autosk-proto::wire::MessageEvent`s, the shape `job.messages` returns.
//! Unknown entry/block types collapse to `kind="other"` with the raw object
//! preserved, exactly as the Go projector does.

use std::path::Path;

use serde_json::Value;

use crate::error::{Error, Result};
use autosk_proto::wire::MessageEvent;

/// Reads the whole transcript at `path` and returns projected events in
/// on-disk order. When `full` is false and `limit > 0`, only the last `limit`
/// events are returned (mirrors `Offline.Messages`).
///
/// Returns `Ok(vec![])` when the run has no session file yet (the Go offline
/// path treats a missing file as "no events"); `path` empty also yields an
/// empty list.
pub fn read_messages(path: &str, full: bool, limit: i64) -> Result<Vec<MessageEvent>> {
    if path.is_empty() {
        return Ok(Vec::new());
    }
    let p = Path::new(path);
    if !p.exists() {
        return Ok(Vec::new());
    }
    let data = std::fs::read_to_string(p).map_err(Error::Io)?;
    let mut events = parse_all(&data)?;
    if !full && limit > 0 && (events.len() as i64) > limit {
        let start = events.len() - limit as usize;
        events = events.split_off(start);
    }
    Ok(events)
}

/// Parses concatenated JSON values (JSONL) into projected events, matching the
/// Go `json.Decoder` stream behaviour.
fn parse_all(data: &str) -> Result<Vec<MessageEvent>> {
    let mut out = Vec::new();
    let stream = serde_json::Deserializer::from_str(data).into_iter::<Value>();
    for item in stream {
        let raw = match item {
            Ok(v) => v,
            Err(e) => {
                if e.is_eof() {
                    break;
                }
                return Err(Error::Migration(format!("transcript: decode: {e}")));
            }
        };
        out.extend(project_entry(&raw));
    }
    Ok(out)
}

fn project_entry(raw: &Value) -> Vec<MessageEvent> {
    let typ = raw.get("type").and_then(Value::as_str).unwrap_or("");
    let ts = raw
        .get("timestamp")
        .and_then(Value::as_str)
        .and_then(normalize_timestamp);
    let simple = |kind: &str| {
        vec![MessageEvent {
            kind: kind.to_string(),
            ts: ts.clone(),
            text: String::new(),
            name: String::new(),
            input: None,
            is_error: false,
            raw: Some(raw.clone()),
        }]
    };
    match typ {
        "session" => simple("session"),
        "thinking_level_change" => simple("thinking_level_change"),
        "model_change" => simple("model_change"),
        "compaction" => simple("compaction"),
        "branch_summary" => simple("branch_summary"),
        "label" => simple("label"),
        "session_info" => simple("session_info"),
        "custom" => simple("custom"),
        "custom_message" => simple("custom_message"),
        "message" => project_message(raw.get("message"), &ts, raw),
        _ => simple("other"),
    }
}

fn project_message(msg: Option<&Value>, ts: &Option<String>, parent: &Value) -> Vec<MessageEvent> {
    let other = || {
        vec![MessageEvent {
            kind: "other".to_string(),
            ts: ts.clone(),
            text: String::new(),
            name: String::new(),
            input: None,
            is_error: false,
            raw: Some(parent.clone()),
        }]
    };
    let Some(msg) = msg else {
        return other();
    };
    let role = msg.get("role").and_then(Value::as_str).unwrap_or("");
    match role {
        "user" => {
            let text = flatten_text_content(msg.get("content"));
            vec![MessageEvent {
                kind: "user_text".to_string(),
                ts: ts.clone(),
                text,
                name: String::new(),
                input: None,
                is_error: false,
                raw: Some(parent.clone()),
            }]
        }
        "assistant" => project_assistant_content(msg.get("content"), ts, parent),
        "toolResult" => {
            let text = flatten_text_content(msg.get("content"));
            vec![MessageEvent {
                kind: "tool_result".to_string(),
                ts: ts.clone(),
                text,
                name: msg
                    .get("toolName")
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .to_string(),
                input: None,
                is_error: msg.get("isError").and_then(Value::as_bool).unwrap_or(false),
                raw: Some(parent.clone()),
            }]
        }
        _ => other(),
    }
}

fn project_assistant_content(
    content: Option<&Value>,
    ts: &Option<String>,
    parent: &Value,
) -> Vec<MessageEvent> {
    let Some(content) = content else {
        return vec![other_event(ts, parent)];
    };
    // content may be a string (legacy) or an array of blocks.
    if let Some(s) = content.as_str() {
        return vec![MessageEvent {
            kind: "assistant_text".to_string(),
            ts: ts.clone(),
            text: s.to_string(),
            name: String::new(),
            input: None,
            is_error: false,
            raw: Some(parent.clone()),
        }];
    }
    let Some(blocks) = content.as_array() else {
        return vec![other_event(ts, parent)];
    };
    let mut out = Vec::new();
    for b in blocks {
        let btype = b.get("type").and_then(Value::as_str).unwrap_or("");
        match btype {
            "text" => {
                let text = b.get("text").and_then(Value::as_str).unwrap_or("");
                if text.trim().is_empty() {
                    continue;
                }
                out.push(MessageEvent {
                    kind: "assistant_text".to_string(),
                    ts: ts.clone(),
                    text: text.to_string(),
                    name: String::new(),
                    input: None,
                    is_error: false,
                    raw: Some(parent.clone()),
                });
            }
            "thinking" => out.push(MessageEvent {
                kind: "assistant_thinking".to_string(),
                ts: ts.clone(),
                text: b
                    .get("thinking")
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .to_string(),
                name: String::new(),
                input: None,
                is_error: false,
                raw: Some(parent.clone()),
            }),
            "toolCall" => out.push(MessageEvent {
                kind: "tool_call".to_string(),
                ts: ts.clone(),
                text: String::new(),
                name: b
                    .get("name")
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .to_string(),
                input: b.get("arguments").cloned(),
                is_error: false,
                raw: Some(parent.clone()),
            }),
            _ => out.push(other_event(ts, parent)),
        }
    }
    out
}

fn other_event(ts: &Option<String>, parent: &Value) -> MessageEvent {
    MessageEvent {
        kind: "other".to_string(),
        ts: ts.clone(),
        text: String::new(),
        name: String::new(),
        input: None,
        is_error: false,
        raw: Some(parent.clone()),
    }
}

/// Normalises pi's raw `timestamp` string the way the Go projector's
/// `parseTimestamp` does: a value that parses as RFC3339 (with optional
/// fractional seconds and a `Z` or `±HH:MM` offset) is kept; anything else —
/// empty, malformed, or out of range — becomes `None`.
///
/// Why this matters: the Go `job.messages` client decodes each event's `ts`
/// into a `time.Time`, which fails to unmarshal a non-RFC3339 string and would
/// blank the **whole** transcript under `--rpc`. The Go offline path never hits
/// that because `parseTimestamp` degrades a bad value to the zero time; `None`
/// here is the wire equivalent (the field is omitted, decoding to the zero
/// time). Valid values pass through verbatim — the cosmetic normalisation Go's
/// `time.Time` marshaling applies (trimming trailing fractional zeros,
/// rewriting `+00:00` as `Z`) is intentionally not replicated: it decodes to
/// the same instant client-side.
fn normalize_timestamp(s: &str) -> Option<String> {
    if valid_rfc3339(s) {
        Some(s.to_string())
    } else {
        None
    }
}

/// Reports whether `s` is an RFC3339 timestamp Go's `time.Parse(RFC3339Nano)` /
/// `time.Time.UnmarshalJSON` would accept: `YYYY-MM-DDTHH:MM:SS`, an optional
/// `.` + fractional digits, then a required `Z` (uppercase only) or `±HH:MM`
/// offset, with the date+clock component ranges validated against the real
/// calendar — so a structurally-valid but impossible value is rejected exactly
/// like Go: month 13, second 60 (Go's parser does not accept leap seconds), an
/// out-of-range day for the month (Feb 30, Apr 31, Feb 29 in a non-leap year),
/// and a lowercase `z` zone designator (Go treats only uppercase `Z` as UTC)
/// all degrade to `None`.
///
/// On two points this validator is deliberately STRICTER than Go, both in the
/// safe (over-reject → `None`, never a whole-slice decode failure) direction on
/// inputs pi never emits, so the "matching Go" claim above is scoped to the
/// date+clock fields: (1) the numeric zone offset is capped at `±23:59`,
/// whereas Go quirkily accepts `+24:00` / `+23:60` (but not `+99:00`); and
/// (2) only `.` is accepted as the fractional separator, whereas Go also
/// accepts the RFC3339 §5.6 comma (`,5Z`).
fn valid_rfc3339(s: &str) -> bool {
    let b = s.as_bytes();
    // Shortest valid form: 1970-01-01T00:00:00Z (20 bytes).
    if b.len() < 20 {
        return false;
    }
    let two = |i: usize| -> Option<u32> {
        if b[i].is_ascii_digit() && b[i + 1].is_ascii_digit() {
            Some((u32::from(b[i] - b'0')) * 10 + u32::from(b[i + 1] - b'0'))
        } else {
            None
        }
    };
    // Date: YYYY-MM-DD.
    if !b[0..4].iter().all(u8::is_ascii_digit) || b[4] != b'-' || b[7] != b'-' {
        return false;
    }
    let year = u32::from(b[0] - b'0') * 1000
        + u32::from(b[1] - b'0') * 100
        + u32::from(b[2] - b'0') * 10
        + u32::from(b[3] - b'0');
    match (two(5), two(8)) {
        // Day is validated against the actual length of the month (leap-year
        // aware), so impossible calendar dates are rejected like Go does.
        (Some(mo), Some(d)) if (1..=12).contains(&mo) && d >= 1 && d <= days_in_month(year, mo) => {
        }
        _ => return false,
    }
    // 'T' separator + HH:MM:SS.
    if b[10] != b'T' || b[13] != b':' || b[16] != b':' {
        return false;
    }
    match (two(11), two(14), two(17)) {
        // Go's RFC3339 parser rejects second 60, so no leap-second allowance.
        (Some(h), Some(mi), Some(se)) if h <= 23 && mi <= 59 && se <= 59 => {}
        _ => return false,
    }
    let mut i = 19;
    // Optional fractional seconds: '.' followed by 1+ digits.
    if i < b.len() && b[i] == b'.' {
        i += 1;
        let start = i;
        while i < b.len() && b[i].is_ascii_digit() {
            i += 1;
        }
        if i == start {
            return false; // a bare '.' with no digits
        }
    }
    // Required zone: uppercase `Z` or ±HH:MM. Go's RFC3339 parser treats only
    // uppercase `Z` as UTC and rejects lowercase `z`, so we drop the `z` arm.
    match b.get(i) {
        Some(b'Z') => i += 1,
        Some(b'+' | b'-') => {
            i += 1;
            // Need exactly HH:MM (5 bytes) and valid ranges. The cap is
            // intentionally tighter than Go's quirky offset rule (Go accepts
            // +24:00 / +23:60 but rejects +99:00); over-rejection here only
            // degrades to None on inputs pi never emits.
            if i + 5 > b.len() || b[i + 2] != b':' {
                return false;
            }
            match (two(i), two(i + 3)) {
                (Some(zh), Some(zm)) if zh <= 23 && zm <= 59 => {}
                _ => return false,
            }
            i += 5;
        }
        _ => return false,
    }
    i == b.len() // no trailing junk
}

/// Number of days in `month` (1..=12) of `year`, using the Gregorian leap-year
/// rule (divisible by 4, except centuries not divisible by 400) — matching the
/// calendar validation in Go's `time.Time.UnmarshalJSON`.
fn days_in_month(year: u32, month: u32) -> u32 {
    match month {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 => {
            if (year % 4 == 0 && year % 100 != 0) || year % 400 == 0 {
                29
            } else {
                28
            }
        }
        _ => 0,
    }
}

/// Collapses an array-of-blocks-or-string content into a single text string,
/// joining text blocks with `\n` (mirrors `flattenTextContent`).
fn flatten_text_content(content: Option<&Value>) -> String {
    let Some(content) = content else {
        return String::new();
    };
    if let Some(s) = content.as_str() {
        return s.to_string();
    }
    let Some(blocks) = content.as_array() else {
        return String::new();
    };
    let mut parts = Vec::new();
    for b in blocks {
        if b.get("type").and_then(Value::as_str) == Some("text") {
            if let Some(t) = b.get("text").and_then(Value::as_str) {
                if !t.is_empty() {
                    parts.push(t.to_string());
                }
            }
        }
    }
    parts.join("\n")
}
