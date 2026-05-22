package tui

import (
	"testing"

	"autosk/internal/lazy/datasource"
)

// TestTaskCommentOpensSingleCompose pins the Phase 3 change: the
// `m` (comment) hotkey now opens the new single-pane multi-line
// compose popup (popupSingleCompose) instead of the one-line
// popupPrompt the original implementation used. The Hint slot
// surfaces a "markdown ok" label so the operator knows the body is
// rendered as markdown in the detail pane after submit.
func TestTaskCommentOpensSingleCompose(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa", Title: "x"}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks

	if err := gu.taskComment(nil, nil); err != nil {
		t.Fatalf("taskComment: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupSingleCompose {
		t.Fatalf("popup kind = %v, want popupSingleCompose", k)
	}
	if gu.st.popup.Hint == "" {
		t.Errorf("comment compose must carry a Hint label, got empty")
	}
	if gu.st.popup.OnAccept == nil {
		t.Errorf("OnAccept not recorded")
	}
}

// TestTaskCommentEmptySubmitIsSilentCancel pins the "empty body =
// silent cancel" semantics carried over from the prompt
// implementation. An OnAccept call with whitespace-only text MUST
// NOT touch the datasource and MUST NOT flash anything.
func TestTaskCommentEmptySubmitIsSilentCancel(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa", Title: "x"}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks

	_ = gu.taskComment(nil, nil)
	accept := gu.st.popup.OnAccept
	if accept == nil {
		t.Fatalf("OnAccept not registered")
	}
	if err := accept("   \n  "); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if gu.st.flash.Text != "" {
		t.Errorf("empty submit should NOT flash, got %+v", gu.st.flash)
	}
}
