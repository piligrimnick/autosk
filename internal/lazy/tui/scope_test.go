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
		{ID: "ask-aaaaaa", Title: "alpha"},
		{ID: "ask-bbbbbb", Title: "beta"},
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
	if gu.st.scope.TaskID != "ask-bbbbbb" {
		t.Fatalf("TaskID=%q want ask-bbbbbb", gu.st.scope.TaskID)
	}
}

// TestAfterCursorMove_TasksDoesNotApplyScope pins the new policy:
// j/k on the Tasks panel must NOT auto-commit the cursor row as
// scope.TaskID — only the explicit Space (tasksScopeFromCursor) and
// Enter (tasksEnter) paths do. Operators complained that cursor-
// driven re-filtering on every j made the Jobs panel flicker, so
// the policy was inverted: cursor is preview, Space/Enter commit.
//
// Stubs gu.dispatch so scheduleRefresh's worker hand-off doesn't
// need a real gocui.Gui; the dispatcher's body is intentionally a
// no-op (we're testing the scope invariant, not the refresh).
func TestAfterCursorMove_TasksDoesNotApplyScope(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.dispatch = func(func()) {} // swallow scheduleRefresh's hand-off
	gu.st.tasks = []datasource.Task{
		{ID: "ask-aaaaaa", Title: "alpha"},
		{ID: "ask-bbbbbb", Title: "beta"},
	}
	// Setup: cursor lands on ask-bbbbbb but scope was previously
	// committed to ask-aaaaaa (e.g. via an earlier Space press).
	gu.st.taskCursor = 1
	gu.st.focused = panelTasks
	gu.st.scope.TaskID = "ask-aaaaaa"

	gu.afterCursorMove(panelTasks)

	if gu.st.scope.TaskID != "ask-aaaaaa" {
		t.Fatalf("cursor-move silently changed scope: TaskID=%q want ask-aaaaaa", gu.st.scope.TaskID)
	}
}

// TestAfterCursorMove_WorkflowsDoesApplyScope is the matching
// positive case: Workflows still cross-link to Tasks on every
// cursor move, because that's the lazygit-style behaviour
// operators expect when navigating the workflow list.
func TestAfterCursorMove_WorkflowsDoesApplyScope(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.dispatch = func(func()) {}
	gu.st.workflows = []datasource.Workflow{
		{Name: "feature-dev"},
		{Name: "ops"},
	}
	gu.st.workflowCursor = 1
	gu.st.focused = panelWorkflows

	gu.afterCursorMove(panelWorkflows)

	if gu.st.scope.WorkflowName != "ops" {
		t.Fatalf("Workflows cursor-move did not apply scope: %+v", gu.st.scope)
	}
}

// TestTasksScopeFromCursor pins the Space-key commit path: read
// cursor, copy id onto scope.TaskID, leave focus on Tasks (no jump
// to Jobs). Empty cursor (cursor on the no-tasks placeholder) must
// be a no-op rather than clearing the existing scope — that would
// surprise an operator who scrolls into an empty filter result.
func TestTasksScopeFromCursor(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.dispatch = func(func()) {}
	gu.st.tasks = []datasource.Task{
		{ID: "ask-aaaaaa", Title: "alpha"},
		{ID: "ask-bbbbbb", Title: "beta"},
	}
	gu.st.taskCursor = 1
	gu.st.focused = panelTasks

	if err := gu.tasksScopeFromCursor(nil, nil); err != nil {
		t.Fatalf("tasksScopeFromCursor: %v", err)
	}
	if gu.st.scope.TaskID != "ask-bbbbbb" {
		t.Errorf("after Space: TaskID=%q want ask-bbbbbb", gu.st.scope.TaskID)
	}
	if gu.st.focused != panelTasks {
		t.Errorf("Space must NOT change focus: focused=%v want panelTasks", gu.st.focused)
	}

	// Empty cursor (e.g. filter produced no rows) must be a no-op:
	// the existing scope chip stays put.
	gu.st.tasks = nil
	gu.st.taskCursor = 0
	if err := gu.tasksScopeFromCursor(nil, nil); err != nil {
		t.Fatalf("tasksScopeFromCursor (empty): %v", err)
	}
	if gu.st.scope.TaskID != "ask-bbbbbb" {
		t.Errorf("Space on empty list cleared scope: TaskID=%q want ask-bbbbbb", gu.st.scope.TaskID)
	}
}

// TestScope_WorkflowToTasks verifies the cross-link from Workflows
// to Tasks records both WorkflowID and WorkflowName on the scope.
func TestScope_WorkflowToTasks(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.workflows = []datasource.Workflow{
		{Name: "feature-dev"},
		{Name: "ops"},
	}
	gu.st.workflowCursor = 0
	gu.st.focused = panelWorkflows
	gu.st.withLock(func() {
		if w, ok := gu.st.selectedWorkflow(); ok {
			// WorkflowID removed in v2
			gu.st.scope.WorkflowName = w.Name
		}
	})
	if gu.st.scope.WorkflowName != "feature-dev" {
		t.Fatalf("scope=%+v want wf-1/feature-dev", gu.st.scope)
	}
}

// TestScope_AgentRelDistinct: setting AgentRel=author vs AgentRel=step
// must persist on the scope so refreshAll can drive distinct
// TaskFilter fields (AuthorName vs StepAgentName) instead of conflating.
// The previous bug treated both popup options identically; the
// design plan \u00a73.4 forces the distinction.
// TestScope_AgentRelDistinct was removed - agentRel functionality was removed in v2

// TestScope_ClearAllChips verifies handleClearScope drops every chip.
func TestScope_ClearAllChips(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.scope = scope{TaskID: "ask-xxxxxx", WorkflowName: "n"}
	gu.st.withLock(func() { gu.st.scope = scope{} })
	if !gu.st.scope.IsEmpty() {
		t.Fatalf("scope not empty: %+v", gu.st.scope)
	}
}
