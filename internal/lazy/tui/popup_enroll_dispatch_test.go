package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
)

// fakeEnrollDS records the (workflow, step) pair EnrollWorkflow is called with
// so the test can assert taskEnroll forwards the operator's selected step
// instead of dropping it to first_step (issue #8).
type fakeEnrollDS struct {
	refreshFakeDS
	calls               int
	gotID, gotWF, gotSt string
}

func (f *fakeEnrollDS) EnrollWorkflow(_ context.Context, id, workflow, step string) error {
	f.calls++
	f.gotID = id
	f.gotWF = workflow
	f.gotSt = step
	return nil
}

// TestTaskEnrollForwardsSelectedStep pins the fix for issue #8: selecting a
// non-first step in the lazy enroll picker must reach EnrollWorkflow (and hence
// the daemon) with exactly that step, not an empty string that would restart at
// first_step. If taskEnroll ever drops stepName again, this fails.
func TestTaskEnrollForwardsSelectedStep(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      120,
		Height:     40,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()

	ds := &fakeEnrollDS{}
	gu := &Gui{g: g, st: newState(), ds: ds, ctx: context.Background()}
	// Synchronous dispatcher so the onPick worker body runs inline.
	gu.dispatch = func(f func()) { f() }
	gu.st.tasks = []datasource.Task{{ID: "ask-abc123", Title: "x"}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks
	gu.st.workflows = []datasource.Workflow{{
		Name:      "feature-dev",
		FirstStep: "dev",
		Steps: []datasource.WorkflowStep{
			{Name: "dev"},
			{Name: "review"},
			{Name: "docs"},
		},
	}}

	if err := gu.taskEnroll(nil, nil); err != nil {
		t.Fatalf("taskEnroll: %v", err)
	}
	if gu.st.popup.Kind != popupEnroll {
		t.Fatalf("enroll picker not open: kind=%v", gu.st.popup.Kind)
	}

	// Select the non-first step ("review", index 1) under the first
	// (and only) workflow, then confirm on the step pane.
	gu.st.popup.WorkflowCursor = 0
	gu.st.popup.StepCursor = 1
	if err := gu.enrollPickerStepAccept(nil, nil); err != nil {
		t.Fatalf("enrollPickerStepAccept: %v", err)
	}

	if ds.calls != 1 {
		t.Fatalf("EnrollWorkflow called %d times, want 1", ds.calls)
	}
	if ds.gotID != "ask-abc123" || ds.gotWF != "feature-dev" || ds.gotSt != "review" {
		t.Errorf("EnrollWorkflow got (%q, %q, %q), want (ask-abc123, feature-dev, review)",
			ds.gotID, ds.gotWF, ds.gotSt)
	}
	// The success flash must report the step actually sent.
	if gu.st.flash.Level != "info" {
		t.Errorf("flash level = %q, want info; flash=%+v", gu.st.flash.Level, gu.st.flash)
	}
	if want := "review"; !strings.Contains(gu.st.flash.Text, want) {
		t.Errorf("flash %q does not mention step %q", gu.st.flash.Text, want)
	}
}
