//! `fakepi` — a test stand-in for `pi --mode rpc`, the Rust port of
//! `internal/daemon/pi/fakepi`. It speaks the same JSON-Lines protocol for the
//! subset the daemon relies on, driven by env vars so tests need not recompile
//! per case:
//!
//!   FAKEPI_SESSION_ID            value returned in get_state.data.sessionId
//!   FAKEPI_SESSION_FILE          value returned in get_state.data.sessionFile
//!   FAKEPI_AGENT_END_DELAY_MS    ms to wait before emitting agent_end
//!   FAKEPI_SCENARIO              "ok"|"no_agent_end"|"prompt_error"|"dialog"
//!   FAKEPI_INJECT_GARBAGE_LINE   literal non-JSON line emitted mid-turn
//!
//! Exits 0 on stdin EOF, 143 on SIGTERM. This bin is compiled by the
//! autosk-core package so the pi-wire integration test can locate it via
//! `env!("CARGO_BIN_EXE_fakepi")`.

use std::io::{BufRead, Write};
use std::thread;
use std::time::Duration;

fn main() {
    let scenario = std::env::var("FAKEPI_SCENARIO").unwrap_or_default();
    let sess_id = env_or("FAKEPI_SESSION_ID", "sess-fake");
    let sess_file = env_or("FAKEPI_SESSION_FILE", "/tmp/fakepi/session.jsonl");
    let delay_ms: u64 = std::env::var("FAKEPI_AGENT_END_DELAY_MS")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(0);
    let garbage = std::env::var("FAKEPI_INJECT_GARBAGE_LINE").unwrap_or_default();

    let stdin = std::io::stdin();
    for line in stdin.lock().lines() {
        let Ok(line) = line else { break };
        let trimmed = line.trim();
        if trimmed.is_empty() {
            continue;
        }
        let v: serde_json::Value = match serde_json::from_str(trimmed) {
            Ok(v) => v,
            Err(e) => {
                emit(
                    &serde_json::json!({"type":"response","id":"","command":"?","success":false,"error":format!("parse: {e}")}),
                );
                continue;
            }
        };
        let id = v
            .get("id")
            .and_then(|x| x.as_str())
            .unwrap_or("")
            .to_string();
        let typ = v
            .get("type")
            .and_then(|x| x.as_str())
            .unwrap_or("")
            .to_string();
        let message = v
            .get("message")
            .and_then(|x| x.as_str())
            .unwrap_or("")
            .to_string();
        handle(
            &typ, &id, &message, &scenario, &sess_id, &sess_file, delay_ms, &garbage,
        );
    }
}

#[allow(clippy::too_many_arguments)]
fn handle(
    typ: &str,
    id: &str,
    message: &str,
    scenario: &str,
    sess_id: &str,
    sess_file: &str,
    delay_ms: u64,
    garbage: &str,
) {
    match typ {
        "get_state" => emit(&serde_json::json!({
            "type":"response","id":id,"command":"get_state","success":true,
            "data":{"sessionId":sess_id,"sessionFile":sess_file,"messageCount":0}
        })),
        "prompt" => {
            if scenario == "prompt_error" {
                emit(
                    &serde_json::json!({"type":"response","id":id,"command":"prompt","success":false,"error":"fake error"}),
                );
                return;
            }
            emit(&serde_json::json!({"type":"response","id":id,"command":"prompt","success":true}));
            if scenario == "dialog" {
                emit(
                    &serde_json::json!({"type":"extension_ui_request","id":"dlg-1","method":"select","title":"pick"}),
                );
            }
            // Simulate a turn on a background thread.
            let scenario = scenario.to_string();
            let garbage = garbage.to_string();
            let msg = message.to_string();
            thread::spawn(move || {
                emit(&serde_json::json!({"type":"agent_start"}));
                emit(&serde_json::json!({"type":"turn_start"}));
                emit(
                    &serde_json::json!({"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":format!("ack: {msg}")}]}}),
                );
                if !garbage.is_empty() {
                    emit_raw_line(&garbage);
                }
                emit(&serde_json::json!({"type":"turn_end","message":{},"toolResults":[]}));
                if scenario == "no_agent_end" {
                    return;
                }
                if delay_ms > 0 {
                    thread::sleep(Duration::from_millis(delay_ms));
                }
                emit(&serde_json::json!({"type":"agent_end","messages":[]}));
            });
        }
        "abort" => {
            emit(&serde_json::json!({"type":"response","id":id,"command":"abort","success":true}))
        }
        "extension_ui_response" => {}
        _ => emit(
            &serde_json::json!({"type":"response","id":id,"command":typ,"success":false,"error":"unknown command"}),
        ),
    }
}

fn emit(v: &serde_json::Value) {
    let mut out = std::io::stdout().lock();
    let _ = writeln!(out, "{v}");
    let _ = out.flush();
}

fn emit_raw_line(s: &str) {
    let mut out = std::io::stdout().lock();
    let _ = writeln!(out, "{s}");
    let _ = out.flush();
}

fn env_or(k: &str, d: &str) -> String {
    match std::env::var(k) {
        Ok(v) if !v.is_empty() => v,
        _ => d.to_string(),
    }
}
