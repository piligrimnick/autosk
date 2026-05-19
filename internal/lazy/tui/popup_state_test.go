package tui

import (
	"testing"
)

// TestPopupStateMachine_Menu pins the menu state machine: openMenu
// records OnSelect, popupCursor moves the cursor (bounded), and
// popupAccept invokes OnSelect with the right index then resets the
// popup. (The review's "popup state machine — none tested" remark.)
func TestPopupStateMachine_Menu(t *testing.T) {
	gu := &Gui{st: newState()}
	var got int
	gu.openMenu("pick", []string{"a", "b", "c"}, func(i int) error {
		got = i
		return nil
	})
	if k := gu.st.popup.Kind; k != popupMenu {
		t.Fatalf("kind=%v want popupMenu", k)
	}
	if l := len(gu.st.popup.Lines); l != 3 {
		t.Fatalf("lines=%d want 3", l)
	}
	// Move cursor: +1 +1 → 2 ; +1 wraps to 0 ; -1 wraps to 2.
	moveDown := gu.popupCursor(+1)
	moveUp := gu.popupCursor(-1)
	_ = moveDown(nil, nil)
	_ = moveDown(nil, nil)
	if c := gu.st.popup.Cursor; c != 2 {
		t.Fatalf("cursor after +2: %d want 2", c)
	}
	_ = moveDown(nil, nil)
	if c := gu.st.popup.Cursor; c != 0 {
		t.Fatalf("cursor wrap +1: %d want 0", c)
	}
	_ = moveUp(nil, nil)
	if c := gu.st.popup.Cursor; c != 2 {
		t.Fatalf("cursor wrap -1: %d want 2", c)
	}
	// Accept at cursor=2 → OnSelect(2), popup cleared.
	if err := gu.popupAccept(nil, nil); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got != 2 {
		t.Fatalf("OnSelect got %d want 2", got)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Fatalf("popup not cleared after accept: %v", k)
	}
}

// TestPopupStateMachine_Confirm: openConfirm + yes fires OnAccept;
// popupClose calls OnCancel if set.
func TestPopupStateMachine_Confirm(t *testing.T) {
	gu := &Gui{st: newState()}
	var fired int
	gu.openConfirm("sure?", func() error {
		fired++
		return nil
	})
	if k := gu.st.popup.Kind; k != popupConfirm {
		t.Fatalf("kind=%v want confirm", k)
	}
	if err := gu.popupConfirmYes(nil, nil); err != nil {
		t.Fatalf("yes: %v", err)
	}
	if fired != 1 {
		t.Fatalf("OnAccept fired %d times want 1", fired)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Fatalf("popup not cleared: %v", k)
	}
}

// TestPopupStateMachine_PromptOnCancel: setting OnCancel and then
// popupClose should invoke it.
func TestPopupStateMachine_PromptOnCancel(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.openPrompt("name?", "", func(string) error { return nil })
	var cancelled bool
	gu.st.withLock(func() {
		gu.st.popup.OnCancel = func() error { cancelled = true; return nil }
	})
	if err := gu.popupClose(nil, nil); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !cancelled {
		t.Fatalf("OnCancel not invoked")
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Fatalf("popup not cleared: %v", k)
	}
}
