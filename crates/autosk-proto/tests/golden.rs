//! Wire-contract golden tests (plan §8.2).
//!
//! Pins the exact JSON the wire types serialise to — field names, ordering and
//! `omitempty` parity — so the Go and Tauri clients never silently diverge from
//! the server. Also round-trips each shape to guarantee the deserialiser
//! accepts what the serialiser produces.

use autosk_proto::rpc::{error_codes, ErrorObject, Request, Response};
use autosk_proto::wire::{Job, MessageEvent, TaskRef};
use serde_json::{json, Value};

fn roundtrip<T>(value: &T)
where
    T: serde::Serialize + serde::de::DeserializeOwned + PartialEq + std::fmt::Debug,
{
    let bytes = serde_json::to_vec(value).expect("serialize");
    let back: T = serde_json::from_slice(&bytes).expect("deserialize");
    assert_eq!(&back, value, "round-trip mismatch");
}

/// Asserts `value` serialises to exactly `expected` (compared as parsed JSON so
/// the assertion is order-insensitive at the serde level but field-exact).
fn assert_json<T: serde::Serialize>(value: &T, expected: Value) {
    let got: Value = serde_json::to_value(value).expect("to_value");
    assert_eq!(got, expected);
}

#[test]
fn job_omitempty_parity() {
    // A terminal job: optional pointers present, derived labels present.
    let done = Job {
        job_id: "job-1".into(),
        task_id: "ask-1".into(),
        step_id: "st-1".into(),
        status: "done".into(),
        transition_id: Some(7),
        pi_session_id: "s".into(),
        session_path: String::new(),
        pid: None,
        exit_code: Some(0),
        error: String::new(),
        corrections_used: 0,
        max_corrections: 3,
        created_at: "2023-11-14T22:16:10Z".into(),
        started_at: Some("2023-11-14T22:16:11Z".into()),
        finished_at: Some("2023-11-14T22:16:20Z".into()),
        duration_ms: 9000,
        attach_count: 0,
        streaming: false,
        workflow_name: "feature-dev".into(),
        step_name: "dev".into(),
        agent_name: "@autogent/generic".into(),
    };
    assert_json(
        &done,
        json!({
            "job_id":"job-1","task_id":"ask-1","step_id":"st-1","status":"done",
            "transition_id":7,"pi_session_id":"s","exit_code":0,
            "corrections_used":0,"max_corrections":3,
            "created_at":"2023-11-14T22:16:10Z",
            "started_at":"2023-11-14T22:16:11Z","finished_at":"2023-11-14T22:16:20Z",
            "duration_ms":9000,"attach_count":0,"streaming":false,
            "workflow_name":"feature-dev","step_name":"dev","agent_name":"@autogent/generic"
        }),
    );
    roundtrip(&done);

    // A running job: empty-string and None fields must be omitted entirely.
    let running = Job {
        job_id: "job-2".into(),
        task_id: "ask-1".into(),
        step_id: "st-1".into(),
        status: "running".into(),
        transition_id: None,
        pi_session_id: String::new(),
        session_path: String::new(),
        pid: Some(4242),
        exit_code: None,
        error: String::new(),
        corrections_used: 1,
        max_corrections: 3,
        created_at: "2023-11-14T22:16:30Z".into(),
        started_at: Some("2023-11-14T22:16:31Z".into()),
        finished_at: None,
        duration_ms: 0,
        attach_count: 0,
        streaming: false,
        workflow_name: String::new(),
        step_name: String::new(),
        agent_name: String::new(),
    };
    let got: Value = serde_json::to_value(&running).unwrap();
    let obj = got.as_object().unwrap();
    assert!(!obj.contains_key("transition_id"), "None pointer omitted");
    assert!(!obj.contains_key("exit_code"), "None pointer omitted");
    assert!(!obj.contains_key("finished_at"), "None timestamp omitted");
    assert!(!obj.contains_key("pi_session_id"), "empty string omitted");
    assert!(!obj.contains_key("error"), "empty string omitted");
    assert_eq!(obj.get("pid"), Some(&json!(4242)));
    // Derived labels are always present (defined contract, not omitempty).
    assert!(obj.contains_key("workflow_name"));
    roundtrip(&running);
}

#[test]
fn message_event_omitempty() {
    let ev = MessageEvent {
        kind: "tool_call".into(),
        ts: Some("2023-11-14T22:16:10Z".into()),
        text: String::new(),
        name: "Read".into(),
        input: Some(json!({"path":"a.rs"})),
        is_error: false,
        raw: Some(json!({"type":"message"})),
    };
    assert_json(
        &ev,
        json!({
            "kind":"tool_call","ts":"2023-11-14T22:16:10Z","name":"Read",
            "input":{"path":"a.rs"},"raw":{"type":"message"}
        }),
    );
    roundtrip(&ev);

    // No timestamp / no input / not error → those keys vanish.
    let bare = MessageEvent {
        kind: "session".into(),
        ts: None,
        text: String::new(),
        name: String::new(),
        input: None,
        is_error: false,
        raw: Some(json!({"type":"session"})),
    };
    assert_json(&bare, json!({"kind":"session","raw":{"type":"session"}}));
}

#[test]
fn rpc_envelope() {
    let req = Request {
        id: 1,
        method: "task.list".into(),
        params: Some(json!({"cwd":"/repo"})),
    };
    assert_json(
        &req,
        json!({"id":1,"method":"task.list","params":{"cwd":"/repo"}}),
    );
    roundtrip(&req);

    // params omitted when None (e.g. `version`).
    let no_params = Request {
        id: 2,
        method: "version".into(),
        params: None,
    };
    assert_json(&no_params, json!({"id":2,"method":"version"}));

    let ok = Response::ok(1, json!([1, 2, 3]));
    assert_json(&ok, json!({"id":1,"result":[1,2,3]}));

    let err = Response {
        id: 3,
        result: None,
        error: Some(ErrorObject {
            code: error_codes::PROJECT_NOT_FOUND,
            message: "no .autosk/db".into(),
            details: None,
        }),
    };
    assert_json(
        &err,
        json!({"id":3,"error":{"code":1001,"message":"no .autosk/db"}}),
    );
    roundtrip(&err);
}

#[test]
fn task_ref_shape() {
    let r = TaskRef {
        id: "ask-1".into(),
        status: "new".into(),
    };
    assert_json(&r, json!({"id":"ask-1","status":"new"}));
    roundtrip(&r);
}
