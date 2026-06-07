//! pi-wire integration test: drives the real [`autosk_core::pi`] runner
//! through the `fakepi` stand-in (the Rust port of the Go fakepi contract),
//! exercising the same JSON-Lines turn sequence the daemon relies on.

use std::time::Duration;

use autosk_core::ctx::Ctx;
use autosk_core::pi::{self, EventKind, PiOpts};
use autosk_core::runner::{PiRunner, RunnerError};

const FAKEPI: &str = env!("CARGO_BIN_EXE_fakepi");

fn opts(extra_env: &[(&str, &str)]) -> PiOpts {
    PiOpts {
        pi_bin: FAKEPI.to_string(),
        env: {
            // Inherit the environment + the scenario overrides so the child
            // still finds a working shell etc.
            let mut e: Vec<(String, String)> = std::env::vars().collect();
            for (k, v) in extra_env {
                e.push((k.to_string(), v.to_string()));
            }
            e
        },
        ..Default::default()
    }
}

#[test]
fn happy_turn_prompt_agent_end_get_state() {
    let ctx = Ctx::background();
    let runner = pi::spawn(&ctx, opts(&[("FAKEPI_SESSION_FILE", "/tmp/fp/s.jsonl")])).unwrap();

    runner.send_prompt(&ctx, "hello").unwrap();
    runner.wait_for_agent_end(&ctx).unwrap();

    let info = runner.get_state(&ctx).unwrap();
    assert_eq!(info.session_file, "/tmp/fp/s.jsonl");

    runner.close_stdin().unwrap();
    let (code, res) = runner.wait(&ctx, Duration::from_secs(5));
    assert!(res.is_ok(), "wait: {res:?}");
    assert_eq!(code, 0);
}

#[test]
fn prompt_error_rejected() {
    let ctx = Ctx::background();
    let runner = pi::spawn(&ctx, opts(&[("FAKEPI_SCENARIO", "prompt_error")])).unwrap();
    let err = runner.send_prompt(&ctx, "x").unwrap_err();
    assert!(matches!(err, RunnerError::Rejected(_)), "{err}");
    runner.close_stdin().unwrap();
    let _ = runner.wait(&ctx, Duration::from_secs(5));
}

#[test]
fn no_agent_end_deadline() {
    let ctx = Ctx::background();
    let runner = pi::spawn(&ctx, opts(&[("FAKEPI_SCENARIO", "no_agent_end")])).unwrap();
    runner.send_prompt(&ctx, "x").unwrap();
    // The turn never emits agent_end; a deadline'd wait must time out.
    let turn = ctx.with_timeout(Duration::from_millis(300));
    let err = runner.wait_for_agent_end(&turn).unwrap_err();
    assert!(err.is_deadline_exceeded(), "{err}");
    runner.close_stdin().unwrap();
    let _ = runner.wait(&ctx, Duration::from_secs(5));
}

#[test]
fn garbage_line_surfaces_as_other_and_keeps_parsing() {
    let ctx = Ctx::background();
    let runner = pi::spawn(
        &ctx,
        opts(&[("FAKEPI_INJECT_GARBAGE_LINE", "this is not json {{{")]),
    )
    .unwrap();
    let events = runner.take_events().expect("events");
    runner.send_prompt(&ctx, "x").unwrap();
    // The reader must still observe agent_end despite the garbage line.
    runner.wait_for_agent_end(&ctx).unwrap();

    // Drain events; we should see at least one KindOther (the garbage line)
    // and an AgentEnd, proving the reader resynced past the bad line.
    let mut saw_other = false;
    let mut saw_end = false;
    let deadline = std::time::Instant::now() + Duration::from_secs(2);
    while std::time::Instant::now() < deadline {
        match events.recv_timeout(Duration::from_millis(50)) {
            Ok(ev) => {
                if ev.kind == EventKind::Other {
                    saw_other = true;
                }
                if ev.kind == EventKind::AgentEnd {
                    saw_end = true;
                }
                if saw_other && saw_end {
                    break;
                }
            }
            Err(_) => {
                if saw_end {
                    break;
                }
            }
        }
    }
    assert!(saw_other, "garbage line should surface as KindOther");
    assert!(
        saw_end,
        "agent_end should still arrive after the garbage line"
    );

    runner.close_stdin().unwrap();
    let _ = runner.wait(&ctx, Duration::from_secs(5));
}

#[test]
fn abort_acked() {
    let ctx = Ctx::background();
    let runner = pi::spawn(&ctx, opts(&[])).unwrap();
    runner.abort(&ctx).unwrap();
    runner.close_stdin().unwrap();
    let _ = runner.wait(&ctx, Duration::from_secs(5));
}
