package tui

import (
	"strings"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/theme"
)

// openEnrollPicker pushes the two-pane workflow + step picker used by
// both `e` (enroll) and `r` (resume) on the Tasks panel. The caller
// is expected to have already filtered synthetic single:<agent>
// workflows out of `workflows`; the picker treats whatever it
// receives as authoritative.
//
//   - workflowCursor seeds the workflow pane's cursor (typically the
//     index of the task's current workflow when present, 0 otherwise).
//   - stepCursor seeds the step pane's cursor (typically the index of
//     the task's current step inside the highlighted workflow, 0
//     otherwise).
//   - workflowLocked=true makes the workflow pane read-only and
//     mounts the picker with focus already on the step pane (the
//     resume flow: the workflow is implicit and the operator only
//     has to choose the step).
//   - onPick fires from the Enter-on-step path with the
//     (workflowName, stepName) pair the operator confirmed.
//
// Callers must guard against an empty `workflows` slice — the
// taskEnroll / taskResume handlers flash a message and skip the
// open in that case rather than mounting an empty popup the user
// can't escape from gracefully (Esc still works, but there is
// nothing to act on).
func (gu *Gui) openEnrollPicker(title string, workflows []datasource.Workflow, workflowCursor, stepCursor int, workflowLocked bool, onPick func(wfName, stepName string) error) {
	if workflowCursor < 0 || workflowCursor >= len(workflows) {
		workflowCursor = 0
	}
	if stepCursor < 0 {
		stepCursor = 0
	}
	active := pickerPaneWorkflow
	if workflowLocked {
		// Resume: operator is forced onto the step pane; the
		// workflow is implicit.
		active = pickerPaneStep
	}
	gu.st.withLock(func() {
		gu.st.popup = popupState{
			Kind:           popupEnroll,
			Title:          title,
			Workflows:      workflows,
			WorkflowCursor: workflowCursor,
			StepCursor:     stepCursor,
			ActivePane:     active,
			WorkflowLocked: workflowLocked,
			OnPick:         onPick,
		}
	})
	gu.requestRedraw()
}

// enrollPickerCursor moves the cursor in the active pane by `step`,
// wrapping around the end. Bound on j / k / arrow keys and the
// mouse wheel on both panes; the workflow pane silently rejects
// moves when WorkflowLocked is set (the resume flow's single-row
// pane has nothing to navigate to).
func (gu *Gui) enrollPickerCursor(pane pickerPane, step int) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		gu.st.withLock(func() {
			if gu.st.popup.Kind != popupEnroll {
				return
			}
			switch pane {
			case pickerPaneWorkflow:
				if gu.st.popup.WorkflowLocked {
					return
				}
				n := len(gu.st.popup.Workflows)
				if n == 0 {
					return
				}
				gu.st.popup.WorkflowCursor = (gu.st.popup.WorkflowCursor + step + n) % n
				// Moving the workflow cursor invalidates any step
				// cursor we picked under the previous workflow row
				// (steps don't have a natural cross-workflow
				// identity). Reset to row 0 so the right pane
				// always re-renders with a sensible selection.
				gu.st.popup.StepCursor = 0
			case pickerPaneStep:
				wfs := gu.st.popup.Workflows
				if len(wfs) == 0 {
					return
				}
				// Defensive WorkflowCursor bounds check (review R6).
				// In practice openEnrollPicker clamps the cursor at
				// open time and the workflow-pane handler wraps via
				// `% n`, but the assertion is two lines and the file
				// is new — picking up the same shape StepCursor
				// already uses keeps the panic surface zero under
				// future popup-state mutations.
				if gu.st.popup.WorkflowCursor < 0 || gu.st.popup.WorkflowCursor >= len(wfs) {
					return
				}
				wf := wfs[gu.st.popup.WorkflowCursor]
				n := len(wf.Steps)
				if n == 0 {
					return
				}
				gu.st.popup.StepCursor = (gu.st.popup.StepCursor + step + n) % n
			}
		})
		gu.requestRedraw()
		return nil
	}
}

// enrollPickerWorkflowAccept handles Enter on the workflow pane:
// moves focus to the step pane with the step cursor at row 0 (the
// first step is the natural CLI default and matches enroll's
// first_step semantics for the picked workflow). No-op when the
// workflow is locked (the resume flow never owns this pane).
func (gu *Gui) enrollPickerWorkflowAccept(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupEnroll {
			return
		}
		if gu.st.popup.WorkflowLocked {
			return
		}
		gu.st.popup.ActivePane = pickerPaneStep
		gu.st.popup.StepCursor = 0
	})
	gu.requestRedraw()
	return nil
}

// enrollPickerStepAccept handles Enter on the step pane: reads the
// highlighted (workflow, step) names, clears the popup, then fires
// OnPick. Following the same pattern as taskComposeConfirm /
// singleComposeConfirm: state is cleared BEFORE OnPick runs so a
// callback that wants to re-open another popup (e.g. to surface an
// error) can do so cleanly.
func (gu *Gui) enrollPickerStepAccept(*gocui.Gui, *gocui.View) error {
	var (
		wfName, stepName string
		onPick           func(string, string) error
	)
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupEnroll {
			return
		}
		if len(gu.st.popup.Workflows) == 0 {
			return
		}
		// Defensive WorkflowCursor bounds check (review R6) —
		// symmetric with the StepCursor guard below. Cheap and
		// future-proofs the dispatch against popup-state mutations
		// that bypass openEnrollPicker's clamp.
		if gu.st.popup.WorkflowCursor < 0 || gu.st.popup.WorkflowCursor >= len(gu.st.popup.Workflows) {
			return
		}
		wf := gu.st.popup.Workflows[gu.st.popup.WorkflowCursor]
		if len(wf.Steps) == 0 {
			return
		}
		if gu.st.popup.StepCursor < 0 || gu.st.popup.StepCursor >= len(wf.Steps) {
			return
		}
		wfName = wf.Name
		stepName = wf.Steps[gu.st.popup.StepCursor].Name
		onPick = gu.st.popup.OnPick
		gu.st.popup = popupState{}
	})
	if onPick != nil {
		return onPick(wfName, stepName)
	}
	return nil
}

// enrollPickerStepEscape handles Esc on the step pane: moves focus
// back to the workflow pane, preserving the workflow cursor (so the
// operator doesn't lose their selection on a back-step). When the
// workflow pane is locked (the resume flow) there is nothing to
// return to, so Esc closes the popup entirely.
//
// We compute the close intent under a single lock (review R5) and
// fire popupClose AFTER releasing it — popupClose acquires its own
// write lock so calling it from inside withLock would deadlock.
func (gu *Gui) enrollPickerStepEscape(g *gocui.Gui, v *gocui.View) error {
	var close bool
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupEnroll {
			return
		}
		if gu.st.popup.WorkflowLocked {
			close = true
			return
		}
		gu.st.popup.ActivePane = pickerPaneWorkflow
	})
	if close {
		return gu.popupClose(g, v)
	}
	gu.requestRedraw()
	return nil
}

// layoutEnrollPicker draws the two side-by-side framed list views
// for the workflow + step picker. Geometry: outer panel uses the
// same composePanelWidth formula the other popups use so visual
// proportions match. The panel splits 40/60 between workflow and
// step columns (steps tend to have longer names than workflow
// names, so the right pane gets the wider share).
//
// The pane currently owning input focus paints its frame in the
// PopupBox accent; the inactive pane drops to default fg. This
// mirrors the focus affordance layoutTaskCompose / layoutDashboard
// use so the operator can see at a glance which pane the next
// keystroke will hit.
//
// Bodies are written via writeView (idempotent body cache); the
// renderer reads the popup state's WorkflowCursor / StepCursor to
// know which row to highlight with the `▶ ` prefix and the accent
// color, matching renderMenuBody.
func (gu *Gui) layoutEnrollPicker(g *gocui.Gui, termW, termH int) {
	var (
		title      string
		workflows  []datasource.Workflow
		wfCursor   int
		stepCursor int
		active     pickerPane
		wfLocked   bool
	)
	gu.st.withRLock(func() {
		title = gu.st.popup.Title
		workflows = gu.st.popup.Workflows
		wfCursor = gu.st.popup.WorkflowCursor
		stepCursor = gu.st.popup.StepCursor
		active = gu.st.popup.ActivePane
		wfLocked = gu.st.popup.WorkflowLocked
	})

	// Width: composePanelWidth already enforces the
	// min(termW-2, 80) ceiling so we trust its output as-is —
	// piling another floor on top would push the popup off-screen
	// on terminals narrower than 22 cells (see review R2).
	panelWidth := composePanelWidth(termW)
	// Height: room for the typical case of a workflow with 3..6
	// steps plus headroom for taller workflows. Cap at 3/4 of the
	// screen so the popup doesn't swallow the whole terminal on
	// a long step list. Floor at 9 (frame + a few visible rows)
	// so the popup is usable on the dashboard's minimum height,
	// but never above the screen-aware cap — termH<12 keeps the
	// cap and the popup shrinks gracefully (review R2).
	panelHeight := enrollPickerHeight(workflows, wfCursor)
	maxH := termH * 3 / 4
	if panelHeight > maxH {
		panelHeight = maxH
	}
	if panelHeight < 9 {
		panelHeight = 9
	}
	if panelHeight > maxH {
		// Floor pushed us past the cap on a tiny terminal — clamp
		// to the cap so the popup stays inside the screen.
		panelHeight = maxH
	}

	x0 := termW/2 - panelWidth/2
	y0 := termH/2 - panelHeight/2 - panelHeight%2
	x1 := termW/2 + panelWidth/2 - 1
	y1 := y0 + panelHeight - 1

	// 40/60 split between workflow and step columns. The split runs
	// down the middle column; both sub-panes carry their own frame
	// so the divider reads as two rounded boxes side-by-side. On a
	// very narrow panel (panelWidth < 20) the "min 8 + min 12"
	// floors don't fit; in that case we fall back to a plain half
	// split so the layout stays well-formed (each pane gets at
	// least 1 cell of content).
	wfWidth := (panelWidth * 4) / 10
	if panelWidth >= 20 {
		if wfWidth < 8 {
			wfWidth = 8
		}
		if wfWidth > panelWidth-12 {
			wfWidth = panelWidth - 12
		}
	} else if wfWidth < 1 {
		wfWidth = 1
	}
	wfX0, wfY0 := x0, y0
	wfX1, wfY1 := x0+wfWidth-1, y1
	stX0, stY0 := wfX1+1, y0
	stX1, stY1 := x1, y1

	activeColor := theme.Active().PopupBox.Gocui()
	inactiveColor := gocui.ColorDefault

	var wfColor, stColor gocui.Attribute
	if active == pickerPaneWorkflow {
		wfColor = activeColor
		stColor = inactiveColor
	} else {
		wfColor = inactiveColor
		stColor = activeColor
	}

	// Title placement (review R9): the popup carries one action
	// title (e.g. "Resume ask-X — pick step") and we route it onto
	// the pane the operator is actually acting on. For enroll
	// (workflowLocked=false) the action lands on the workflow pane
	// and the step pane gets the generic "Step" label; for resume
	// (workflowLocked=true) the workflow pane is non-navigable so
	// the action lands on the step pane and the workflow pane
	// shows the inert "Workflow (locked)" label.
	wfTitle := "Workflow"
	stTitle := "Step"
	if wfLocked {
		wfTitle = "Workflow (locked)"
		if title != "" {
			stTitle = title
		}
	} else if title != "" {
		wfTitle = title
	}
	wfV, err := g.SetView(winEnrollWorkflowList, wfX0, wfY0, wfX1, wfY1, 0)
	if err != nil && !isUnknownView(err) {
		return
	}
	if wfV != nil {
		wfV.Frame = true
		wfV.FrameRunes = roundedFrameRunes
		wfV.FrameColor = wfColor
		wfV.TitleColor = wfColor
		wfV.Title = wfTitle
		wfV.Wrap = false
		gu.writeView(winEnrollWorkflowList, wfTitle, renderEnrollWorkflowBody(workflows, wfCursor))
	}

	// Step pane.
	stV, err := g.SetView(winEnrollStepList, stX0, stY0, stX1, stY1, 0)
	if err != nil && !isUnknownView(err) {
		return
	}
	if stV != nil {
		stV.Frame = true
		stV.FrameRunes = roundedFrameRunes
		stV.FrameColor = stColor
		stV.TitleColor = stColor
		stV.Title = stTitle
		stV.Wrap = false
		var steps []datasource.WorkflowStep
		if wfCursor >= 0 && wfCursor < len(workflows) {
			steps = workflows[wfCursor].Steps
		}
		gu.writeView(winEnrollStepList, stTitle, renderEnrollStepBody(steps, stepCursor))
	}

	// Route focus to the active pane.
	var focusName string
	if active == pickerPaneWorkflow {
		focusName = winEnrollWorkflowList
	} else {
		focusName = winEnrollStepList
	}
	if _, err := g.SetCurrentView(focusName); err != nil && !isUnknownView(err) {
		return
	}
}

// enrollPickerHeight returns the panel outer height (in rows,
// including frame) needed to fit the taller of the two columns
// (workflow list vs the currently-highlighted workflow's step
// list). The screen-aware cap (3/4 of the terminal) and the floor
// (>= 9) are applied externally in layoutEnrollPicker; this helper
// is purely content-shape driven so the layout math is testable
// without a gocui screen and the helper has no false dependency on
// the terminal size (review R1).
func enrollPickerHeight(workflows []datasource.Workflow, wfCursor int) int {
	wfRows := len(workflows)
	if wfRows < 1 {
		wfRows = 1
	}
	stepRows := 1
	if wfCursor >= 0 && wfCursor < len(workflows) {
		if n := len(workflows[wfCursor].Steps); n > stepRows {
			stepRows = n
		}
	}
	rows := wfRows
	if stepRows > rows {
		rows = stepRows
	}
	// Frame top + frame bottom + content rows.
	return rows + 2
}

// renderEnrollWorkflowBody renders the workflow column. One row per
// workflow, name only (the plan explicitly says no isolation badge
// here — that lives on the dedicated Workflows panel).
func renderEnrollWorkflowBody(workflows []datasource.Workflow, cursor int) string {
	if len(workflows) == 0 {
		return "  (no workflows)\n"
	}
	var b strings.Builder
	for i, w := range workflows {
		if i == cursor {
			b.WriteString("▶ " + styleAccent.Render(w.Name) + "\n")
		} else {
			b.WriteString("  " + w.Name + "\n")
		}
	}
	return b.String()
}

// renderEnrollStepBody renders the step column for the highlighted
// workflow. One row per step, name only. An empty step slice (the
// workflow has no rows hydrated, or no workflow is highlighted)
// renders an inert placeholder.
func renderEnrollStepBody(steps []datasource.WorkflowStep, cursor int) string {
	if len(steps) == 0 {
		return "  (no steps)\n"
	}
	var b strings.Builder
	for i, s := range steps {
		if i == cursor {
			b.WriteString("▶ " + styleAccent.Render(s.Name) + "\n")
		} else {
			b.WriteString("  " + s.Name + "\n")
		}
	}
	return b.String()
}

// filterPickerWorkflows returns the subset of `in` that should
// appear in the picker: real workflows only. Synthetic
// single:<agent> workflows are pinned to one step ("do") and the
// CLI directs operators to use `autosk enroll --agent NAME` to
// invoke them; surfacing them in the picker would imply they're
// switchable through it, which they aren't.
func filterPickerWorkflows(in []datasource.Workflow) []datasource.Workflow {
	out := make([]datasource.Workflow, 0, len(in))
	for _, w := range in {
		if w.IsSynthetic {
			continue
		}
		out = append(out, w)
	}
	return out
}
