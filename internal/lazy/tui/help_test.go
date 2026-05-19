package tui

import (
	"strings"
	"testing"
)

// TestHelpScreen_NoClaimBinding pins the regression that the v0.2
// schema has no claim verb, so the help screen must not advertise
// 'c claim'. Previously the help line read 'n new  c claim  d done'
// but the binding was either a no-op flash or a fallback to enroll.
// Either way the line was misleading; the binding is removed and the
// help line must not contain it.
func TestHelpScreen_NoClaimBinding(t *testing.T) {
	gu := &Gui{st: newState()}
	// openHelp calls openMenu → requestRedraw; the latter no-ops when
	// gu.g is nil so we can drive it without a real gocui.Gui.
	_ = gu.openHelp(nil, nil)
	lines := gu.st.popup.Lines
	for _, line := range lines {
		if strings.Contains(line, "claim") {
			t.Fatalf("help advertises 'claim' but the v0.2 schema has no claim verb: %q", line)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("help body empty")
	}
}
