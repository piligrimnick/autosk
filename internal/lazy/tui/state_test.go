package tui

import "testing"

func TestStateFocusStackPushPop(t *testing.T) {
	s := newState()
	if s.focused != panelTasks {
		t.Fatalf("initial focus = %v want Tasks", s.focused)
	}
	s.pushFocus(panelSessions)
	if s.focused != panelSessions {
		t.Fatalf("after push: focus = %v want Sessions", s.focused)
	}
	s.popFocus()
	if s.focused != panelTasks {
		t.Fatalf("after pop: focus = %v want Tasks", s.focused)
	}
}

func TestStateLogBuffer(t *testing.T) {
	s := newState()
	for i := 0; i < 250; i++ {
		s.appendLog("entry")
	}
	if len(s.logBuf) != 200 {
		t.Fatalf("log buffer should cap at 200, got %d", len(s.logBuf))
	}
}

func TestScopeIsEmpty(t *testing.T) {
	if !(scope{}).IsEmpty() {
		t.Fatal("zero scope must be empty")
	}
	if (scope{TaskID: "ask-000001"}).IsEmpty() {
		t.Fatal("scope with TaskID is not empty")
	}
	if (scope{WorkflowName: "feature-dev"}).IsEmpty() {
		t.Fatal("scope with WorkflowName is not empty")
	}
}
