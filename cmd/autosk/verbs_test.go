package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// These verb tests run the in-process cobra root against a real, isolated
// autoskd (see daemon_harness_test.go). They exercise the proto-v2 CLI wiring
// (right method called, output rendered), not the daemon's engine semantics —
// the latter are covered by the daemon's own conformance suite.

// initProject creates a fresh project dir + initializes it, returning the dir.
func initProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	return dir
}

// createTask creates a task and returns its id (the bare-id stdout of create).
func createTask(t *testing.T, dir, title string, extra ...string) string {
	t.Helper()
	args := append([]string{"create", title}, extra...)
	out, err := runRoot(t, dir, args...)
	if err != nil {
		t.Fatalf("create %q: %v\n%s", title, err, out)
	}
	id := strings.TrimSpace(out)
	if !strings.HasPrefix(id, "ask-") {
		t.Fatalf("create did not print a task id, got: %q", out)
	}
	return id
}

func TestInit_CreatesProjectIdempotent(t *testing.T) {
	dir := t.TempDir()
	out, err := runRoot(t, dir, "init")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "initialized") {
		t.Errorf("expected 'initialized' line:\n%s", out)
	}
	// Idempotent: a second init is a no-op success.
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatalf("second init: %v", err)
	}
}

func TestCreateShowList(t *testing.T) {
	dir := initProject(t)
	id := createTask(t, dir, "first task", "--description", "hello world")

	show, err := runRoot(t, dir, "show", id, "--json")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, show)
	}
	var tv map[string]any
	if err := json.Unmarshal([]byte(show), &tv); err != nil {
		t.Fatalf("unmarshal show json: %v\n%s", err, show)
	}
	if tv["id"] != id || tv["title"] != "first task" || tv["status"] != "new" {
		t.Errorf("unexpected show: %v", tv)
	}
	if _, hasPriority := tv["priority"]; hasPriority {
		t.Errorf("v2 task should not carry priority: %v", tv)
	}

	list, err := runRoot(t, dir, "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, list)
	}
	if !strings.Contains(list, id) {
		t.Errorf("list missing task %s:\n%s", id, list)
	}
	// The list table dropped the priority column.
	if strings.Contains(list, "\tP\t") || strings.Contains(list, " P ") {
		t.Errorf("list table should not have a priority column:\n%s", list)
	}
}

func TestUpdateTitleDescription(t *testing.T) {
	dir := initProject(t)
	id := createTask(t, dir, "old title")
	if _, err := runRoot(t, dir, "update", id, "--title", "new title"); err != nil {
		t.Fatalf("update: %v", err)
	}
	show, _ := runRoot(t, dir, "show", id, "--json")
	var tv map[string]any
	_ = json.Unmarshal([]byte(show), &tv)
	if tv["title"] != "new title" {
		t.Errorf("title not updated: %v", tv["title"])
	}
}

func TestCommentAddListEditDelete(t *testing.T) {
	dir := initProject(t)
	id := createTask(t, dir, "with comments")

	add, err := runRoot(t, dir, "comment", "add", id, "first comment", "--json")
	if err != nil {
		t.Fatalf("comment add: %v\n%s", err, add)
	}
	var c map[string]any
	if err := json.Unmarshal([]byte(add), &c); err != nil {
		t.Fatalf("unmarshal comment: %v\n%s", err, add)
	}
	cid, _ := c["id"].(string)
	if cid == "" {
		t.Fatalf("comment add returned no id: %s", add)
	}

	if _, err := runRoot(t, dir, "comment", "edit", id, cid, "edited"); err != nil {
		t.Fatalf("comment edit: %v", err)
	}
	list, _ := runRoot(t, dir, "comment", "list", id)
	if !strings.Contains(list, "edited") {
		t.Errorf("edited comment not listed:\n%s", list)
	}

	if _, err := runRoot(t, dir, "comment", "delete", id, cid); err != nil {
		t.Fatalf("comment delete: %v", err)
	}
	listJSON, _ := runRoot(t, dir, "comment", "list", id, "--json")
	var cs []any
	_ = json.Unmarshal([]byte(listJSON), &cs)
	if len(cs) != 0 {
		t.Errorf("expected no comments after delete, got %d:\n%s", len(cs), listJSON)
	}
}

func TestBlockUnblock(t *testing.T) {
	dir := initProject(t)
	a := createTask(t, dir, "A (blocker)")
	b := createTask(t, dir, "B (blocked)")
	if _, err := runRoot(t, dir, "block", b, a); err != nil {
		t.Fatalf("block: %v", err)
	}
	show, _ := runRoot(t, dir, "show", b, "--json")
	var tv map[string]any
	_ = json.Unmarshal([]byte(show), &tv)
	if blocked, _ := tv["blocked"].(bool); !blocked {
		t.Errorf("B should be blocked: %v", tv)
	}
	if _, err := runRoot(t, dir, "unblock", b, a); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	show2, _ := runRoot(t, dir, "show", b, "--json")
	var tv2 map[string]any
	_ = json.Unmarshal([]byte(show2), &tv2)
	if blocked, _ := tv2["blocked"].(bool); blocked {
		t.Errorf("B should be unblocked: %v", tv2)
	}
}

func TestDoneCancelReopen(t *testing.T) {
	dir := initProject(t)
	id := createTask(t, dir, "lifecycle")
	if _, err := runRoot(t, dir, "done", id); err != nil {
		t.Fatalf("done: %v", err)
	}
	show, _ := runRoot(t, dir, "show", id, "--json")
	var tv map[string]any
	_ = json.Unmarshal([]byte(show), &tv)
	if tv["status"] != "done" {
		t.Errorf("expected done, got %v", tv["status"])
	}
	if _, err := runRoot(t, dir, "reopen", id); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	show2, _ := runRoot(t, dir, "show", id, "--json")
	var tv2 map[string]any
	_ = json.Unmarshal([]byte(show2), &tv2)
	// A never-enrolled task reopens to the new backlog.
	if tv2["status"] != "new" {
		t.Errorf("expected new after reopen, got %v", tv2["status"])
	}
}

func TestWorkflowRegistryReadOnly(t *testing.T) {
	dir := initProject(t)
	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatalf("workflow list: %v\n%s", err, list)
	}
	if !strings.Contains(list, "human-flow") {
		t.Errorf("workflow list missing the fixture workflow:\n%s", list)
	}
	show, err := runRoot(t, dir, "workflow", "show", "human-flow", "--json")
	if err != nil {
		t.Fatalf("workflow show: %v\n%s", err, show)
	}
	var wf map[string]any
	if err := json.Unmarshal([]byte(show), &wf); err != nil {
		t.Fatalf("unmarshal workflow: %v\n%s", err, show)
	}
	if wf["name"] != "human-flow" || wf["first_step"] != "review" {
		t.Errorf("unexpected workflow show: %v", wf)
	}
}

func TestEnrollIntoWorkflow(t *testing.T) {
	dir := initProject(t)
	id := createTask(t, dir, "to enroll")
	out, err := runRoot(t, dir, "enroll", id, "--workflow", "human-flow", "--json")
	if err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}
	var tv map[string]any
	if err := json.Unmarshal([]byte(out), &tv); err != nil {
		t.Fatalf("unmarshal enroll json: %v\n%s", err, out)
	}
	// The human-first step parks the task at status=human, step=review.
	if tv["status"] != "human" || tv["workflow"] != "human-flow" || tv["step"] != "review" {
		t.Errorf("unexpected enroll result: %v", tv)
	}
}

func TestEnrollMutuallyExclusive(t *testing.T) {
	dir := initProject(t)
	id := createTask(t, dir, "x")
	if _, err := runRoot(t, dir, "enroll", id, "--workflow", "human-flow", "--agent", "foo"); err == nil {
		t.Fatal("expected --workflow + --agent to be rejected")
	}
}

// TestEnroll_Agent covers the `enroll --agent NAME` path: it must materialise
// the single-step workflow single:<NAME> via task.enroll {agent}, NOT fire a
// spurious task.enroll {workflow:""} first. Enrolling into the fixture `stub`
// agent runs it in-process, so after asserting the workflow name we poll until
// the async run reaches done — both to confirm the single:stub flow actually
// drives the agent and so the daemon finishes writing .autosk/sessions before
// the tempdir teardown (avoids a RemoveAll-vs-still-writing cleanup race).
func TestEnroll_Agent(t *testing.T) {
	dir := initProject(t)
	id := createTask(t, dir, "to enroll into agent")
	out, err := runRoot(t, dir, "enroll", id, "--agent", "stub", "--json")
	if err != nil {
		t.Fatalf("enroll --agent: %v\n%s", err, out)
	}
	var tv map[string]any
	if err := json.Unmarshal([]byte(out), &tv); err != nil {
		t.Fatalf("unmarshal enroll json: %v\n%s", err, out)
	}
	if tv["workflow"] != "single:stub" {
		t.Errorf("enroll --agent should materialise single:stub, got: %v", tv)
	}

	// Let the in-process stub agent finish (it transits the task to done).
	var status string
	for i := 0; i < 80; i++ {
		show, _ := runRoot(t, dir, "show", id, "--json")
		var sv map[string]any
		_ = json.Unmarshal([]byte(show), &sv)
		if s, _ := sv["status"].(string); s == "done" {
			status = s
			break
		}
		time.Sleep(75 * time.Millisecond)
	}
	if status != "done" {
		t.Fatalf("single:stub did not drive the agent to done; last status=%q", status)
	}
}

func TestCreateWithWorkflowEnrolls(t *testing.T) {
	dir := initProject(t)
	out, err := runRoot(t, dir, "create", "enrolled-on-create", "--workflow", "human-flow", "--json")
	if err != nil {
		t.Fatalf("create --workflow: %v\n%s", err, out)
	}
	var tv map[string]any
	if err := json.Unmarshal([]byte(out), &tv); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if tv["workflow"] != "human-flow" || tv["status"] != "human" {
		t.Errorf("create --workflow should enroll: %v", tv)
	}
}

// TestE2E_StubAgentFlow is the end-to-end smoke (acceptance §2): init → create
// → enroll into a workflow whose in-process stub agent runs, writes a
// transcript, and transits the task to done → the session transcript is visible
// via `autosk session transcript`. No pi is needed (the stub agent runs inside
// the daemon).
func TestE2E_StubAgentFlow(t *testing.T) {
	dir := initProject(t)
	id := createTask(t, dir, "e2e smoke")
	if _, err := runRoot(t, dir, "enroll", id, "--workflow", "auto-flow"); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// The stub agent runs asynchronously; poll until the task reaches done.
	var status string
	for i := 0; i < 80; i++ {
		show, _ := runRoot(t, dir, "show", id, "--json")
		var tv map[string]any
		_ = json.Unmarshal([]byte(show), &tv)
		if s, _ := tv["status"].(string); s == "done" {
			status = s
			break
		}
		time.Sleep(75 * time.Millisecond)
	}
	if status != "done" {
		t.Fatalf("task did not reach done (stub agent did not run); last status=%q", status)
	}

	// The session is listed and its transcript carries the stub's custom entry.
	sessJSON, err := runRoot(t, dir, "session", "list", "--task", id, "--json")
	if err != nil {
		t.Fatalf("session list: %v\n%s", err, sessJSON)
	}
	var sessions []map[string]any
	if err := json.Unmarshal([]byte(sessJSON), &sessions); err != nil || len(sessions) == 0 {
		t.Fatalf("expected at least one session, got: %s (err=%v)", sessJSON, err)
	}
	sid, _ := sessions[0]["id"].(string)
	if sid == "" {
		t.Fatalf("session has no id: %s", sessJSON)
	}
	transcript, err := runRoot(t, dir, "session", "transcript", sid)
	if err != nil {
		t.Fatalf("session transcript: %v\n%s", err, transcript)
	}
	if !strings.Contains(transcript, "note") {
		t.Errorf("transcript missing the stub's custom entry:\n%s", transcript)
	}
}

func TestResumeFromHuman(t *testing.T) {
	dir := initProject(t)
	id := createTask(t, dir, "resume me")
	if _, err := runRoot(t, dir, "enroll", id, "--workflow", "human-flow"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// resume --to a sibling human step.
	out, err := runRoot(t, dir, "resume", id, "--to", "accept", "--json")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	var tv map[string]any
	_ = json.Unmarshal([]byte(out), &tv)
	if tv["step"] != "accept" {
		t.Errorf("resume --to accept should relocate the step: %v", tv)
	}
}
