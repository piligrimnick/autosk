package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestResume_CapExceeded_HintsResetThenSucceeds walks the user-visible
// CLI escape hatch from a parked, capped task:
//
//  1. Enroll a task into a 2-step capped workflow (caps small).
//  2. Force the task into human (raw SQL, mirroring
//     TestEnroll_HumanFeedback_Rejected).
//  3. `autosk resume --to dev` — expect an error whose message contains
//     `cannot enter step` AND the concrete task id (regression guard
//     against the literal `<id>` placeholder leaking into the hint).
//  4. `autosk metadata reset-visits --step dev` — expect success.
//  5. `autosk resume --to dev` — expect success, work at dev,
//     counter dev=1.
//
// This is the "what does an operator type" coverage that the engine
// unit tests miss.
func TestResume_CapExceeded_HintsResetThenSucceeds(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	wfPath := writeCappedWorkflow(t, dir)
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "loop-resume")
	if _, err := runRoot(t, dir, "enroll", id, "--workflow", "capped"); err != nil {
		t.Fatal(err)
	}
	// First enroll bumps step_visits[dev]=1 (cap is 2). Bump the
	// counter once more so resume --to dev would hit the cap exactly.
	if _, err := runRoot(t, dir, "metadata", "set", id,
		"--key", "step_visits."+devStepID(t, dir, "capped"), "--value", "2"); err != nil {
		t.Fatal(err)
	}
	// Park the task in human so resume is the only path
	// forward.
	q := fmt.Sprintf("UPDATE tasks SET status='human' WHERE id='%s'", id)
	if out, err := runRoot(t, dir, "sql", "--write", q); err != nil {
		t.Fatalf("force human: %v\n%s", err, out)
	}

	// 3. resume --to dev hits the cap. The hint must include this
	//    task's id, not a `<id>` placeholder.
	_, err := runRoot(t, dir, "resume", id, "--to", "dev")
	if err == nil {
		t.Fatal("expected cap-fire error from resume --to dev")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cannot enter step") {
		t.Errorf("expected `cannot enter step` in error, got: %v", err)
	}
	if !strings.Contains(msg, id) {
		t.Errorf("hint must include concrete task id %q (no placeholder leak), got: %v", id, err)
	}
	if strings.Contains(msg, "<id>") {
		t.Errorf("hint must not contain the literal `<id>` placeholder, got: %v", err)
	}

	// 4. reset visits for dev specifically.
	if _, err := runRoot(t, dir, "metadata", "reset-visits", id, "--step", "dev"); err != nil {
		t.Fatalf("reset-visits: %v", err)
	}

	// 5. resume --to dev now succeeds; counter is back at 1.
	if _, err := runRoot(t, dir, "resume", id, "--to", "dev"); err != nil {
		t.Fatalf("resume after reset: %v", err)
	}
	st := statusOf(t, dir, id)
	if st["status"] != "work" {
		t.Fatalf("status after resume: %v", st["status"])
	}
	md, _ := st["metadata"].(map[string]any)
	sv, _ := md["step_visits"].(map[string]any)
	devID := devStepID(t, dir, "capped")
	if v, _ := sv[devID].(float64); int(v) != 1 {
		t.Fatalf("dev counter should be 1 after reset+resume, got %v (full sv=%v)", sv[devID], sv)
	}
}

// devStepID looks up the dev step's id for the named workflow via
// `workflow show --json`. Used to keep tests resilient to id format
// drift.
func devStepID(t *testing.T, dir, wfName string) string {
	t.Helper()
	wfBody, err := runRoot(t, dir, "workflow", "show", wfName, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var wfShow map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(wfBody)), &wfShow); err != nil {
		t.Fatalf("unmarshal wf show: %v\n%s", err, wfBody)
	}
	steps, _ := wfShow["steps"].([]any)
	for _, s := range steps {
		sm, _ := s.(map[string]any)
		if sm["name"] == "dev" {
			id, _ := sm["id"].(string)
			return id
		}
	}
	t.Fatalf("dev step not found in workflow %q (steps=%v)", wfName, steps)
	return ""
}

// TestResume_NoTo_DoesNotBumpCounter is the companion regression test
// for the documented "resume without --to is not a transition" rule.
// After `enroll`, step_visits[dev]=1. After `resume` with no --to, the
// counter must still be 1 (no second bump).
func TestResume_NoTo_DoesNotBumpCounter(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	wfPath := writeCappedWorkflow(t, dir)
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "no-bump")
	if _, err := runRoot(t, dir, "enroll", id, "--workflow", "capped"); err != nil {
		t.Fatal(err)
	}
	// Park.
	q := fmt.Sprintf("UPDATE tasks SET status='human' WHERE id='%s'", id)
	if _, err := runRoot(t, dir, "sql", "--write", q); err != nil {
		t.Fatal(err)
	}
	// resume with no --to.
	if _, err := runRoot(t, dir, "resume", id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// step_visits[dev] must still be 1.
	st := statusOf(t, dir, id)
	md, _ := st["metadata"].(map[string]any)
	sv, _ := md["step_visits"].(map[string]any)
	devID := devStepID(t, dir, "capped")
	if v, _ := sv[devID].(float64); int(v) != 1 {
		t.Fatalf("dev counter should still be 1 (resume without --to is not a transition), got %v", sv[devID])
	}
}
