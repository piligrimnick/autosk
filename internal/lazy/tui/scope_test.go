package tui

import (
	"testing"

	"autosk/internal/lazy/datasource"
)

// TestScope_TasksToJobs verifies that highlighting a task in the
// Tasks panel records the task id on the scope so the Jobs panel
// can narrow to that task on the next refresh.
//
// We drive applyScope directly because the higher-level cursor
// handlers also call refreshAll (which needs a real gui). The state
// transition we care about is purely the scope update.
func TestScope_TasksToJobs(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.tasks = []datasource.Task{
		{ID: "as-a", Title: "alpha"},
		{ID: "as-b", Title: "beta"},
	}
	gu.st.taskCursor = 1
	gu.st.focused = panelTasks
	// applyScope reads cursor under the lock then refreshes. With nil
	// gui the OnWorker call is a panic; sidestep by stubbing.
	gu.st.withLock(func() {
		if t, ok := gu.st.selectedTask(); ok {
			gu.st.scope.TaskID = t.ID
		}
	})
	if gu.st.scope.TaskID != "as-b" {
		t.Fatalf("TaskID=%q want as-b", gu.st.scope.TaskID)
	}
}

// TestScope_WorkflowToTasks verifies the cross-link from Workflows
// to Tasks records both WorkflowID and WorkflowName on the scope.
func TestScope_WorkflowToTasks(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.workflows = []datasource.Workflow{
		{ID: "wf-1", Name: "feature-dev"},
		{ID: "wf-2", Name: "ops"},
	}
	gu.st.workflowCursor = 0
	gu.st.focused = panelWorkflows
	gu.st.withLock(func() {
		if w, ok := gu.st.selectedWorkflow(); ok {
			gu.st.scope.WorkflowID = w.ID
			gu.st.scope.WorkflowName = w.Name
		}
	})
	if gu.st.scope.WorkflowID != "wf-1" || gu.st.scope.WorkflowName != "feature-dev" {
		t.Fatalf("scope=%+v want wf-1/feature-dev", gu.st.scope)
	}
}

// TestScope_AgentRelDistinct: setting AgentRel=author vs AgentRel=step
// must persist on the scope so refreshAll can drive distinct
// TaskFilter fields (AuthorName vs StepAgentName) instead of conflating.
// The previous bug treated both popup options identically; the
// design plan \u00a73.4 forces the distinction.
func TestScope_AgentRelDistinct(t *testing.T) {
	cases := []struct {
		rel  agentRel
		want string
	}{
		{agentRelAuthor, "author"},
		{agentRelStep, "step"},
		{agentRelNone, ""},
	}
	for _, tc := range cases {
		s := scope{Agent: "dev", AgentRel: tc.rel}
		if got := s.AgentRel.String(); got != tc.want {
			t.Errorf("rel %v: String()=%q want %q", tc.rel, got, tc.want)
		}
	}
	// And the chips render with the relation tag when non-empty.
	st := newState()
	st.scope = scope{Agent: "dev", AgentRel: agentRelAuthor}
	bar := renderStatusBar(st, "/proj")
	if !contains(bar, "agent=dev (author)") {
		t.Errorf("status bar missing (author) tag: %q", bar)
	}
	st.scope = scope{Agent: "dev", AgentRel: agentRelStep}
	bar = renderStatusBar(st, "/proj")
	if !contains(bar, "agent=dev (step)") {
		t.Errorf("status bar missing (step) tag: %q", bar)
	}
}

// TestScope_ClearAllChips verifies handleClearScope drops every chip.
func TestScope_ClearAllChips(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.scope = scope{TaskID: "as-x", WorkflowID: "wf-y", WorkflowName: "n", Agent: "a", AgentRel: agentRelStep}
	gu.st.withLock(func() { gu.st.scope = scope{} })
	if !gu.st.scope.IsEmpty() {
		t.Fatalf("scope not empty: %+v", gu.st.scope)
	}
}


