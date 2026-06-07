//! Transcript projection parity (`job.messages`).
//!
//! Mirrors the Go `internal/daemon/transcript` test expectations: one assistant
//! message expands into multiple events (thinking / text / tool_call); a
//! tool_result carries its name + is_error; unknown entry types collapse to
//! `kind="other"`; an empty / missing file yields no events.

use autosk_core::transcript::read_messages;

fn write_session(lines: &[&str]) -> (tempfile::TempDir, String) {
    let dir = tempfile::tempdir().expect("tempdir");
    let path = dir.path().join("session.jsonl");
    std::fs::write(&path, lines.join("\n")).expect("write session");
    (dir, path.to_string_lossy().to_string())
}

#[test]
fn projects_assistant_and_tool_events() {
    let (_g, path) = write_session(&[
        r#"{"type":"session","id":"s1","timestamp":"2023-11-14T22:16:00Z"}"#,
        r#"{"type":"message","timestamp":"2023-11-14T22:16:01Z","message":{"role":"user","content":"hello"}}"#,
        r#"{"type":"message","timestamp":"2023-11-14T22:16:02Z","message":{"role":"assistant","content":[
            {"type":"thinking","thinking":"hmm"},
            {"type":"text","text":"hi there"},
            {"type":"toolCall","name":"Read","arguments":{"path":"a.rs"}}
        ]}}"#,
        r#"{"type":"message","timestamp":"2023-11-14T22:16:03Z","message":{"role":"toolResult","toolName":"Read","isError":true,"content":"boom"}}"#,
        r#"{"type":"mystery","timestamp":"2023-11-14T22:16:04Z"}"#,
    ]);

    let events = read_messages(&path, true, 0).expect("read");
    let kinds: Vec<&str> = events.iter().map(|e| e.kind.as_str()).collect();
    assert_eq!(
        kinds,
        vec![
            "session",
            "user_text",
            "assistant_thinking",
            "assistant_text",
            "tool_call",
            "tool_result",
            "other",
        ]
    );

    let tool_call = &events[4];
    assert_eq!(tool_call.name, "Read");
    assert_eq!(
        tool_call.input.as_ref().unwrap()["path"],
        serde_json::json!("a.rs")
    );

    let tool_result = &events[5];
    assert_eq!(tool_result.name, "Read");
    assert!(tool_result.is_error);
    assert_eq!(tool_result.text, "boom");

    // user text flattened from a bare string.
    assert_eq!(events[1].text, "hello");
    // ts threaded through.
    assert_eq!(events[0].ts.as_deref(), Some("2023-11-14T22:16:00Z"));
}

#[test]
fn tail_limit_keeps_last_n() {
    let (_g, path) = write_session(&[
        r#"{"type":"session"}"#,
        r#"{"type":"message","message":{"role":"user","content":"one"}}"#,
        r#"{"type":"message","message":{"role":"user","content":"two"}}"#,
        r#"{"type":"message","message":{"role":"user","content":"three"}}"#,
    ]);
    let tail = read_messages(&path, false, 2).expect("read tail");
    assert_eq!(tail.len(), 2);
    assert_eq!(tail[0].text, "two");
    assert_eq!(tail[1].text, "three");

    // full=true ignores the limit.
    let full = read_messages(&path, true, 2).expect("read full");
    assert_eq!(full.len(), 4);
}

#[test]
fn malformed_timestamp_degrades_to_none() {
    // A non-RFC3339 `timestamp` must NOT be forwarded verbatim: the Go
    // job.messages client decodes ts into time.Time and a bad string would
    // error the whole call. It is dropped (omitted) instead, like Go's
    // parseTimestamp zero-time fallback. A valid millisecond timestamp passes
    // through unchanged.
    let (_g, path) = write_session(&[
        r#"{"type":"session","timestamp":"not a date"}"#,
        r#"{"type":"message","timestamp":"2023-11-14T22:16:01.250Z","message":{"role":"user","content":"hi"}}"#,
        r#"{"type":"message","timestamp":"2023-13-01T00:00:00Z","message":{"role":"user","content":"bad month"}}"#,
    ]);
    let events = read_messages(&path, true, 0).expect("read");
    assert_eq!(events.len(), 3);
    assert_eq!(events[0].ts, None, "garbage timestamp dropped");
    assert_eq!(
        events[1].ts.as_deref(),
        Some("2023-11-14T22:16:01.250Z"),
        "valid millis timestamp preserved verbatim"
    );
    assert_eq!(events[2].ts, None, "out-of-range month dropped");
}

#[test]
fn impossible_calendar_timestamps_degrade_to_none() {
    // Go's time.Time.UnmarshalJSON rejects a leap second (:60), impossible
    // calendar days (Feb 30, Apr 31, Feb 29 in a non-leap year), and a
    // lowercase `z` zone designator (only uppercase `Z` is treated as UTC).
    // Forwarding any of these verbatim would error the whole job.messages
    // decode client-side, so they must degrade to None like the
    // generic-garbage case above.
    let (_g, path) = write_session(&[
        r#"{"type":"session","timestamp":"2023-11-14T22:16:60Z"}"#,
        r#"{"type":"session","timestamp":"2023-02-30T00:00:00Z"}"#,
        r#"{"type":"session","timestamp":"2023-04-31T00:00:00Z"}"#,
        r#"{"type":"session","timestamp":"2023-02-29T00:00:00Z"}"#,
        r#"{"type":"session","timestamp":"2023-11-14T22:16:30z"}"#,
        r#"{"type":"session","timestamp":"2024-02-29T00:00:00Z"}"#,
    ]);
    let events = read_messages(&path, true, 0).expect("read");
    assert_eq!(events.len(), 6);
    assert_eq!(events[0].ts, None, "leap second :60 dropped");
    assert_eq!(events[1].ts, None, "Feb 30 dropped");
    assert_eq!(events[2].ts, None, "Apr 31 dropped");
    assert_eq!(events[3].ts, None, "Feb 29 in a non-leap year dropped");
    assert_eq!(events[4].ts, None, "lowercase z zone dropped");
    assert_eq!(
        events[5].ts.as_deref(),
        Some("2024-02-29T00:00:00Z"),
        "Feb 29 in a leap year preserved"
    );
}

#[test]
fn snake_case_tool_result_role_is_other() {
    // Go only matches the camelCase "toolResult" role; "tool_result" is not a
    // real pi shape and must collapse to "other" so both projectors agree.
    let (_g, path) = write_session(&[
        r#"{"type":"message","message":{"role":"tool_result","toolName":"Read","content":"x"}}"#,
    ]);
    let events = read_messages(&path, true, 0).expect("read");
    assert_eq!(events.len(), 1);
    assert_eq!(events[0].kind, "other");
}

#[test]
fn missing_or_empty_path_is_no_events() {
    assert!(read_messages("", false, 0).expect("empty path").is_empty());
    assert!(read_messages("/nonexistent/session.jsonl", false, 0)
        .expect("missing file")
        .is_empty());
}
