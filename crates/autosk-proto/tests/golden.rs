//! Wire-contract golden tests (plan §8.2).
//!
//! Pins the exact JSON the wire types serialise to — field names, ordering and
//! `omitempty` parity — so the Go and Tauri clients never silently diverge from
//! the server. Also round-trips each shape to guarantee the deserialiser
//! accepts what the serialiser produces.

use autosk_proto::rpc::{error_codes, ErrorObject, Notification, Request, Response};
use autosk_proto::wire::{
    Agent, Comment, EnsureRecord, Job, JobEvent, MessageEvent, ProjectInfo, TaskRef, TaskView,
    UpdateIsolationReport,
};
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

// ---- Phase-3 write-method result shapes (plan §8.2) -----------------------
//
// Every §5 write verb returns one of these serde mirrors (or an ad-hoc object
// built in `server.rs`). Pinning their wire shape here guarantees the Go client
// and the Tauri client deserialise exactly what the daemon emits.

#[test]
fn task_view_shape() {
    // task.create / task.update / task.enroll / task.resume / task.done / ...
    // all return a TaskView. A fully-populated, enrolled task: every derived
    // field is present (the contract has no omitempty — the human renderer and
    // the lazy TUI both index these fields unconditionally).
    let tv = TaskView {
        id: "ask-abc123".into(),
        title: "port writes".into(),
        description: "do the thing".into(),
        status: "work".into(),
        priority: 1,
        author_id: "ag-human".into(),
        author_name: "human".into(),
        workflow_id: "wf-1".into(),
        workflow_name: "feature-dev-generic".into(),
        current_step_id: "st-dev".into(),
        step_name: "dev".into(),
        agent_id: "ag-gen".into(),
        agent_name: "@autogent/generic".into(),
        blocked: false,
        blocked_by: vec![],
        blocks: vec![TaskRef {
            id: "ask-def456".into(),
            status: "new".into(),
        }],
        comment_count: 2,
        metadata: Some(json!({"step_visits": {"st-dev": 1}})),
        created_at: "2023-11-14T22:16:10Z".into(),
        updated_at: "2023-11-14T22:16:20Z".into(),
    };
    assert_json(
        &tv,
        json!({
            "id":"ask-abc123","title":"port writes","description":"do the thing",
            "status":"work","priority":1,"author_id":"ag-human","author_name":"human",
            "workflow_id":"wf-1","workflow_name":"feature-dev-generic",
            "current_step_id":"st-dev","step_name":"dev",
            "agent_id":"ag-gen","agent_name":"@autogent/generic",
            "blocked":false,"blocked_by":[],
            "blocks":[{"id":"ask-def456","status":"new"}],
            "comment_count":2,
            "metadata":{"step_visits":{"st-dev":1}},
            "created_at":"2023-11-14T22:16:10Z","updated_at":"2023-11-14T22:16:20Z"
        }),
    );
    roundtrip(&tv);

    // A fresh `new` task: SQL-NULL metadata serialises as JSON `null` (not
    // omitted) so the client can distinguish "no metadata" from `{}`.
    let bare = TaskView {
        metadata: None,
        ..tv.clone()
    };
    let got: Value = serde_json::to_value(&bare).unwrap();
    assert_eq!(
        got.get("metadata"),
        Some(&Value::Null),
        "null metadata kept"
    );
    roundtrip(&bare);
}

#[test]
fn comment_shape() {
    // comment.add returns the inserted Comment.
    let c = Comment {
        id: 7,
        task_id: "ask-abc123".into(),
        author_id: "ag-rev".into(),
        author_name: "reviewer".into(),
        text: "looks good".into(),
        created_at: "2023-11-14T22:16:10Z".into(),
    };
    assert_json(
        &c,
        json!({
            "id":7,"task_id":"ask-abc123","author_id":"ag-rev",
            "author_name":"reviewer","text":"looks good",
            "created_at":"2023-11-14T22:16:10Z"
        }),
    );
    roundtrip(&c);
}

#[test]
fn agent_shape() {
    // agent.install returns the enriched Agent view; vec fields are always
    // present (defined contract, not omitempty).
    let a = Agent {
        id: "ag-gen".into(),
        name: "@autogent/generic".into(),
        is_human: false,
        source: "npm".into(),
        version: "1.2.3".into(),
        model: "sonnet".into(),
        thinking: "high".into(),
        extra_args: vec!["--foo".into()],
        pi_skills: vec![],
        pi_ext: vec![],
        tasks_owned: 4,
    };
    assert_json(
        &a,
        json!({
            "id":"ag-gen","name":"@autogent/generic","is_human":false,
            "source":"npm","version":"1.2.3","model":"sonnet","thinking":"high",
            "extra_args":["--foo"],"pi_skills":[],"pi_ext":[],"tasks_owned":4
        }),
    );
    roundtrip(&a);
}

#[test]
fn project_info_shape() {
    // project.add / project.list element.
    let p = ProjectInfo {
        root: "/repo".into(),
        db_path: "/repo/.autosk/db".into(),
        name: "repo".into(),
    };
    assert_json(
        &p,
        json!({"root":"/repo","db_path":"/repo/.autosk/db","name":"repo"}),
    );
    roundtrip(&p);
}

#[test]
fn update_isolation_report_shape() {
    // workflow.updateIsolation result. Empty collections + empty strings are
    // omitted so a clean dry-run flip serialises compactly.
    let clean = UpdateIsolationReport {
        workflow: "feature-dev-generic".into(),
        from: "none".into(),
        to: "worktree".into(),
        noop: false,
        dry_run: true,
        ..Default::default()
    };
    assert_json(
        &clean,
        json!({
            "workflow":"feature-dev-generic","from":"none","to":"worktree",
            "noop":false,"dry_run":true
        }),
    );
    roundtrip(&clean);

    // A populated (force) report round-trips with its nested EnsureRecords.
    let forced = UpdateIsolationReport {
        workflow: "wf".into(),
        from: "none".into(),
        to: "worktree".into(),
        noop: false,
        dry_run: false,
        ensured_tasks: vec![EnsureRecord {
            task_id: "ask-1".into(),
            path: "/wt/ask-1".into(),
            branch: "autosk/ask-1".into(),
            existing: false,
        }],
        ..Default::default()
    };
    roundtrip(&forced);
}

// ---- notifications (plan §4.1 / §5) ---------------------------------------

#[test]
fn task_and_project_changed_notifications() {
    // `task-changed`: carries the project's canonical root + db_path so a
    // subscriber knows which project moved.
    let task_changed = Notification {
        method: "task-changed".into(),
        params: json!({"root":"/repo","db_path":"/repo/.autosk/db"}),
    };
    assert_json(
        &task_changed,
        json!({
            "method":"task-changed",
            "params":{"root":"/repo","db_path":"/repo/.autosk/db"}
        }),
    );
    roundtrip(&task_changed);

    // `project-changed`: registry add/remove; empty params object.
    let project_changed = Notification {
        method: "project-changed".into(),
        params: json!({}),
    };
    assert_json(
        &project_changed,
        json!({"method":"project-changed","params":{}}),
    );
    roundtrip(&project_changed);
}

#[test]
fn job_event_notification_payloads() {
    // `job-event` kind=message: carries one MessageEvent + a monotonic event_id
    // (the replay cursor / Last-Event-ID analogue).
    let msg = JobEvent {
        kind: "message".into(),
        job_id: "job-1".into(),
        event_id: 12,
        event: Some(MessageEvent {
            kind: "assistant_message".into(),
            ts: Some("2023-11-14T22:16:10Z".into()),
            text: "hi".into(),
            name: String::new(),
            input: None,
            is_error: false,
            raw: None,
        }),
        job: None,
        error: String::new(),
    };
    let got: Value = serde_json::to_value(&msg).unwrap();
    let obj = got.as_object().unwrap();
    assert_eq!(obj.get("kind"), Some(&json!("message")));
    assert_eq!(obj.get("event_id"), Some(&json!(12)));
    assert!(obj.contains_key("event"), "message frame carries event");
    assert!(!obj.contains_key("job"), "None job omitted");
    assert!(!obj.contains_key("error"), "empty error omitted");
    roundtrip(&msg);

    // kind=done: carries the decorated Job; event_id is omitted (zero) and the
    // transcript event is absent.
    let done = JobEvent {
        kind: "done".into(),
        job_id: "job-1".into(),
        event_id: 0,
        event: None,
        job: Some(Job {
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
        }),
        error: String::new(),
    };
    let got: Value = serde_json::to_value(&done).unwrap();
    let obj = got.as_object().unwrap();
    assert!(!obj.contains_key("event_id"), "zero event_id omitted");
    assert!(!obj.contains_key("event"), "None event omitted");
    assert!(obj.contains_key("job"), "done frame carries job");
    roundtrip(&done);

    // kind=error: carries a message string.
    let err = JobEvent {
        kind: "error".into(),
        job_id: "job-1".into(),
        event_id: 0,
        event: None,
        job: None,
        error: "boom".into(),
    };
    assert_json(
        &err,
        json!({"kind":"error","job_id":"job-1","error":"boom"}),
    );
    roundtrip(&err);
}
