package tui

import (
	"fmt"
	"strings"

	"github.com/jesseduffield/gocui"

	"autosk/internal/store"
)

// bindKeys wires every keybinding the TUI uses. Bindings are
// per-view: the global ones bind ViewName="" (all views).
//
// We split into three tiers:
//
//   - globals (quit, tab switching, palette, filter, help, scope clear)
//   - per-panel (Tasks/Jobs/Workflows/Agents list nav + write verbs)
//   - inspector (tab cycling, Live tab Ctrl-D/F/A, scrolling)
func (gu *Gui) bindKeys() error {
	type binding struct {
		view string
		key  any
		mod  gocui.Modifier
		h    func(*gocui.Gui, *gocui.View) error
	}

	bs := []binding{
		// global quit + help
		{"", gocui.KeyCtrlC, gocui.ModNone, gu.quit},
		{"", 'q', gocui.ModNone, gu.quit},
		{"", '?', gocui.ModNone, gu.openHelp},
		{"", gocui.KeyEsc, gocui.ModNone, gu.handleEsc},
		// panel switching
		{"", '1', gocui.ModNone, gu.focusPanel(panelTasks)},
		{"", '2', gocui.ModNone, gu.focusPanel(panelJobs)},
		{"", '3', gocui.ModNone, gu.focusPanel(panelWorkflows)},
		{"", '4', gocui.ModNone, gu.focusPanel(panelAgents)},
		{"", '0', gocui.ModNone, gu.focusPanel(panelDetail)},
		{"", gocui.KeyTab, gocui.ModNone, gu.cyclePanel(+1)},
		// refresh + scope + filter + palette
		{"", 'R', gocui.ModNone, gu.handleRefresh},
		{"", '*', gocui.ModNone, gu.handleClearScope},
		{"", '/', gocui.ModNone, gu.openFilter},
		{"", ':', gocui.ModNone, gu.openPalette},
		{"", '@', gocui.ModNone, gu.toggleLog},

		// list nav (per panel; gocui binds per-view, so we attach once
		// per side panel + the detail pane)
		{winTasks, 'j', gocui.ModNone, gu.cursorDown(panelTasks)},
		{winTasks, 'k', gocui.ModNone, gu.cursorUp(panelTasks)},
		{winTasks, gocui.KeyArrowDown, gocui.ModNone, gu.cursorDown(panelTasks)},
		{winTasks, gocui.KeyArrowUp, gocui.ModNone, gu.cursorUp(panelTasks)},
		{winTasks, gocui.KeyEnter, gocui.ModNone, gu.tasksEnter},

		{winJobs, 'j', gocui.ModNone, gu.cursorDown(panelJobs)},
		{winJobs, 'k', gocui.ModNone, gu.cursorUp(panelJobs)},
		{winJobs, gocui.KeyArrowDown, gocui.ModNone, gu.cursorDown(panelJobs)},
		{winJobs, gocui.KeyArrowUp, gocui.ModNone, gu.cursorUp(panelJobs)},
		{winJobs, gocui.KeyEnter, gocui.ModNone, gu.jobsEnter},

		{winWorkflows, 'j', gocui.ModNone, gu.cursorDown(panelWorkflows)},
		{winWorkflows, 'k', gocui.ModNone, gu.cursorUp(panelWorkflows)},
		{winWorkflows, gocui.KeyArrowDown, gocui.ModNone, gu.cursorDown(panelWorkflows)},
		{winWorkflows, gocui.KeyArrowUp, gocui.ModNone, gu.cursorUp(panelWorkflows)},
		{winWorkflows, gocui.KeyEnter, gocui.ModNone, gu.workflowsEnter},

		{winAgents, 'j', gocui.ModNone, gu.cursorDown(panelAgents)},
		{winAgents, 'k', gocui.ModNone, gu.cursorUp(panelAgents)},
		{winAgents, gocui.KeyArrowDown, gocui.ModNone, gu.cursorDown(panelAgents)},
		{winAgents, gocui.KeyArrowUp, gocui.ModNone, gu.cursorUp(panelAgents)},
		{winAgents, gocui.KeyEnter, gocui.ModNone, gu.agentsEnter},

		// Tasks write verbs.
		{winTasks, 'n', gocui.ModNone, gu.taskNew},
		{winTasks, 'd', gocui.ModNone, gu.taskDone},
		{winTasks, 'x', gocui.ModNone, gu.taskCancel},
		{winTasks, 'e', gocui.ModNone, gu.taskEnroll},
		{winTasks, 'r', gocui.ModNone, gu.taskResume},
		{winTasks, 'b', gocui.ModNone, gu.taskBlock},
		{winTasks, 'u', gocui.ModNone, gu.taskUnblock},
		{winTasks, 'm', gocui.ModNone, gu.taskComment},
		{winTasks, 'p', gocui.ModNone, gu.taskPriority},
		{winTasks, 'o', gocui.ModNone, gu.taskReopen},
		// Note: there is no 'c claim' binding. The v0.2 schema has no
		// claim verb — tasks self-advance via workflow steps. Use 'e'
		// to enroll, or assign an agent. The help screen reflects this.

		// Workflows write verbs.
		{winWorkflows, 'n', gocui.ModNone, gu.workflowNew},
		{winWorkflows, 'D', gocui.ModNone, gu.workflowDelete},

		// Agents write verbs.
		{winAgents, 'i', gocui.ModNone, gu.agentInstall},
		{winAgents, 'u', gocui.ModNone, gu.agentUninstall},

		// Jobs hotkeys.
		{winJobs, 'a', gocui.ModNone, gu.jobAttachLive},
		{winJobs, 's', gocui.ModNone, gu.jobOpenArchive},
		{winJobs, 'i', gocui.ModNone, gu.jobOpenMeta},
		{winJobs, 'K', gocui.ModNone, gu.jobCancel},

		// Inspector tab cycling + Live tab dispatch.
		{winInspector, '[', gocui.ModNone, gu.inspectorCycleTab(-1)},
		{winInspector, ']', gocui.ModNone, gu.inspectorCycleTab(+1)},
		{winInspector, '1', gocui.ModNone, gu.inspectorJumpTab(tabLive)},
		{winInspector, '2', gocui.ModNone, gu.inspectorJumpTab(tabArchive)},
		{winInspector, '3', gocui.ModNone, gu.inspectorJumpTab(tabMeta)},
		{winInspector, '4', gocui.ModNone, gu.inspectorJumpTab(tabSignals)},
		{winInspector, gocui.KeyEsc, gocui.ModNone, gu.inspectorClose},
		{winInspector, gocui.KeyCtrlO, gocui.ModNone, gu.inspectorClose},

		// Inspector scroll bindings (design plan §6.2): j/k single line,
		// Ctrl-F/Ctrl-B page, g/G start/end. Hooked on the body view.
		{winInspector, 'j', gocui.ModNone, gu.inspectorScroll(+1)},
		{winInspector, 'k', gocui.ModNone, gu.inspectorScroll(-1)},
		{winInspector, gocui.KeyArrowDown, gocui.ModNone, gu.inspectorScroll(+1)},
		{winInspector, gocui.KeyArrowUp, gocui.ModNone, gu.inspectorScroll(-1)},
		{winInspector, gocui.KeyCtrlF, gocui.ModNone, gu.inspectorScrollPage(+1)},
		{winInspector, gocui.KeyCtrlB, gocui.ModNone, gu.inspectorScrollPage(-1)},
		{winInspector, gocui.KeyPgdn, gocui.ModNone, gu.inspectorScrollPage(+1)},
		{winInspector, gocui.KeyPgup, gocui.ModNone, gu.inspectorScrollPage(-1)},
		{winInspector, 'g', gocui.ModNone, gu.inspectorScrollTo(false)},
		{winInspector, 'G', gocui.ModNone, gu.inspectorScrollTo(true)},

		// J/K on the detail pane scroll the Tasks detail viewport.
		{winDetail, 'j', gocui.ModNone, gu.detailScroll(+1)},
		{winDetail, 'k', gocui.ModNone, gu.detailScroll(-1)},

		// Live tab dispatch — bound on the input view.
		{winInspectorIn, gocui.KeyCtrlD, gocui.ModNone, gu.liveSend},
		{winInspectorIn, gocui.KeyCtrlF, gocui.ModNone, gu.liveFollowUp},
		{winInspectorIn, gocui.KeyCtrlA, gocui.ModNone, gu.liveAbort},
		{winInspectorIn, gocui.KeyEsc, gocui.ModNone, gu.inspectorClose},
		{winInspectorIn, gocui.KeyCtrlC, gocui.ModNone, gu.quit},
		// Live tab scroll-back: the body view (winInspector) isn't
		// current while the textarea has focus, so Ctrl-B / PageUp /
		// PageDown have to bind on the input view too. (j / k / g / G
		// would collide with text input and stay body-only.)
		{winInspectorIn, gocui.KeyCtrlB, gocui.ModNone, gu.inspectorScrollPage(-1)},
		{winInspectorIn, gocui.KeyPgup, gocui.ModNone, gu.inspectorScrollPage(-1)},
		{winInspectorIn, gocui.KeyPgdn, gocui.ModNone, gu.inspectorScrollPage(+1)},

		// Popup keys.
		{winPopupMenu, 'j', gocui.ModNone, gu.popupCursor(+1)},
		{winPopupMenu, 'k', gocui.ModNone, gu.popupCursor(-1)},
		{winPopupMenu, gocui.KeyArrowDown, gocui.ModNone, gu.popupCursor(+1)},
		{winPopupMenu, gocui.KeyArrowUp, gocui.ModNone, gu.popupCursor(-1)},
		{winPopupMenu, gocui.KeyEnter, gocui.ModNone, gu.popupAccept},
		{winPopupMenu, gocui.KeyEsc, gocui.ModNone, gu.popupClose},
		{winPopupConfirm, 'y', gocui.ModNone, gu.popupConfirmYes},
		{winPopupConfirm, 'Y', gocui.ModNone, gu.popupConfirmYes},
		{winPopupConfirm, 'n', gocui.ModNone, gu.popupClose},
		{winPopupConfirm, 'N', gocui.ModNone, gu.popupClose},
		{winPopupConfirm, gocui.KeyEnter, gocui.ModNone, gu.popupConfirmYes},
		{winPopupConfirm, gocui.KeyEsc, gocui.ModNone, gu.popupClose},
		{winPopupPrompt, gocui.KeyEnter, gocui.ModNone, gu.popupAccept},
		{winPopupPrompt, gocui.KeyEsc, gocui.ModNone, gu.popupClose},
	}
	for _, b := range bs {
		if err := gu.g.SetKeybinding(b.view, b.key, b.mod, b.h); err != nil {
			return err
		}
	}
	return nil
}

// focusPanel returns a handler that focuses the named panel.
func (gu *Gui) focusPanel(p panelID) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		gu.st.withLock(func() {
			if gu.st.view == StateInspector {
				return
			}
			gu.st.focused = p
		})
		return nil
	}
}

// cyclePanel cycles through the four side panels by step.
func (gu *Gui) cyclePanel(step int) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		gu.st.withLock(func() {
			if gu.st.view == StateInspector {
				return
			}
			n := 4
			next := (int(gu.st.focused) + step + n) % n
			gu.st.focused = panelID(next)
		})
		return nil
	}
}

// cursorDown / cursorUp move the cursor for a given list panel.
func (gu *Gui) cursorDown(p panelID) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		gu.st.withLock(func() {
			switch p {
			case panelTasks:
				gu.st.taskCursor = clampCursor(gu.st.taskCursor+1, len(gu.st.tasks))
			case panelJobs:
				gu.st.jobCursor = clampCursor(gu.st.jobCursor+1, len(gu.st.jobs))
			case panelWorkflows:
				gu.st.workflowCursor = clampCursor(gu.st.workflowCursor+1, len(gu.st.workflows))
			case panelAgents:
				gu.st.agentCursor = clampCursor(gu.st.agentCursor+1, len(gu.st.agents))
			}
		})
		gu.applyScope()
		return nil
	}
}

func (gu *Gui) cursorUp(p panelID) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		gu.st.withLock(func() {
			switch p {
			case panelTasks:
				gu.st.taskCursor = clampCursor(gu.st.taskCursor-1, len(gu.st.tasks))
			case panelJobs:
				gu.st.jobCursor = clampCursor(gu.st.jobCursor-1, len(gu.st.jobs))
			case panelWorkflows:
				gu.st.workflowCursor = clampCursor(gu.st.workflowCursor-1, len(gu.st.workflows))
			case panelAgents:
				gu.st.agentCursor = clampCursor(gu.st.agentCursor-1, len(gu.st.agents))
			}
		})
		gu.applyScope()
		return nil
	}
}

// applyScope re-derives the scope chips from the current cursor in
// Tasks / Workflows. Called after every cursor move so the right
// detail + Jobs filter reflects what's highlighted.
func (gu *Gui) applyScope() {
	gu.st.withLock(func() {
		switch gu.st.focused {
		case panelTasks:
			if t, ok := gu.st.selectedTask(); ok {
				gu.st.scope.TaskID = t.ID
			} else {
				gu.st.scope.TaskID = ""
			}
		case panelWorkflows:
			if w, ok := gu.st.selectedWorkflow(); ok {
				gu.st.scope.WorkflowID = w.ID
				gu.st.scope.WorkflowName = w.Name
			} else {
				gu.st.scope.WorkflowID = ""
				gu.st.scope.WorkflowName = ""
			}
		}
	})
	gu.scheduleRefresh()
}

// handleClearScope clears every scope chip.
func (gu *Gui) handleClearScope(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() { gu.st.scope = scope{} })
	gu.flashf("info", "scope cleared")
	gu.scheduleRefresh()
	return nil
}

// handleRefresh re-fetches every panel on demand.
func (gu *Gui) handleRefresh(*gocui.Gui, *gocui.View) error {
	gu.scheduleRefresh()
	gu.flashf("info", "refresh")
	return nil
}

// handleEsc closes the active popup or inspector; falls back to
// clearing filter chips.
func (gu *Gui) handleEsc(g *gocui.Gui, v *gocui.View) error {
	// Snapshot popup.Kind + view under the lock so concurrent
	// mutations from g.Update closures don't race against these reads.
	var (
		popupKind popupKind
		view      ViewState
	)
	gu.st.withRLock(func() {
		popupKind = gu.st.popup.Kind
		view = gu.st.view
	})
	if popupKind != popupNone {
		return gu.popupClose(g, v)
	}
	if view == StateInspector {
		return gu.inspectorClose(g, v)
	}
	// Otherwise: clear the focused panel's filter.
	gu.st.withLock(func() {
		switch gu.st.focused {
		case panelTasks:
			gu.st.filter.Tasks = ""
		case panelJobs:
			gu.st.filter.Jobs = ""
		case panelWorkflows:
			gu.st.filter.Workflows = ""
		case panelAgents:
			gu.st.filter.Agents = ""
		}
	})
	gu.scheduleRefresh()
	return nil
}

// toggleLog flips command-log visibility.
func (gu *Gui) toggleLog(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() { gu.st.logHide = !gu.st.logHide })
	return nil
}

// tasksEnter cross-links to Jobs (filter by this task) and focuses Jobs.
func (gu *Gui) tasksEnter(*gocui.Gui, *gocui.View) error {
	gu.applyScope()
	gu.st.withLock(func() { gu.st.focused = panelJobs })
	return nil
}

// workflowsEnter cross-links to Tasks (filter by this workflow).
func (gu *Gui) workflowsEnter(*gocui.Gui, *gocui.View) error {
	gu.applyScope()
	gu.st.withLock(func() { gu.st.focused = panelTasks })
	return nil
}

// agentsEnter prompts the user to opt into the agent scope.
//
// Design plan §3.4: the relation is ambiguous (author vs.
// current_step.agent are different concepts), so we force the
// operator to pick ONE. The choice flows into scope.AgentRel and is
// surfaced both on the Tasks-panel scope chip and in the status bar.
func (gu *Gui) agentsEnter(*gocui.Gui, *gocui.View) error {
	a, ok := gu.st.selectedAgentLocked()
	if !ok {
		return nil
	}
	gu.openMenu("Filter Tasks by agent "+a.Name+"?", []string{
		"by author (author_id == " + a.Name + ")",
		"by current step (current_step.agent == " + a.Name + ")",
		"cancel",
	}, func(i int) error {
		var rel agentRel
		switch i {
		case 0:
			rel = agentRelAuthor
		case 1:
			rel = agentRelStep
		default:
			return nil
		}
		gu.st.withLock(func() {
			gu.st.scope.Agent = a.Name
			gu.st.scope.AgentRel = rel
			gu.st.focused = panelTasks
		})
		gu.scheduleRefresh()
		return nil
	})
	return nil
}

// jobsEnter opens the inspector on the highlighted job.
func (gu *Gui) jobsEnter(*gocui.Gui, *gocui.View) error {
	j, ok := gu.st.selectedJobLocked()
	if !ok {
		return nil
	}
	gu.openInspector(j.JobID)
	return nil
}

// openFilter opens the per-panel filter prompt.
func (gu *Gui) openFilter(*gocui.Gui, *gocui.View) error {
	var view ViewState
	gu.st.withRLock(func() { view = gu.st.view })
	if view == StateInspector {
		return nil
	}
	gu.openPrompt("filter: facets like p:1 status:done wf:name agent:name + free text", "", func(value string) error {
		gu.st.withLock(func() {
			switch gu.st.focused {
			case panelTasks:
				gu.st.filter.Tasks = value
			case panelJobs:
				gu.st.filter.Jobs = value
			case panelWorkflows:
				gu.st.filter.Workflows = value
			case panelAgents:
				gu.st.filter.Agents = value
			}
		})
		gu.scheduleRefresh()
		return nil
	})
	return nil
}

// openPalette opens the command palette.
func (gu *Gui) openPalette(*gocui.Gui, *gocui.View) error {
	cmds := []string{
		"task new",
		"task done",
		"task cancel",
		"task reopen",
		"task enroll",
		"task block",
		"task unblock",
		"task comment",
		"workflow create",
		"workflow delete",
		"agent install",
		"job cancel",
		"scope clear",
		"refresh",
		"quit",
	}
	gu.openMenu("command palette", cmds, func(i int) error {
		gu.dispatchPaletteCommand(cmds[i])
		return nil
	})
	return nil
}

func (gu *Gui) dispatchPaletteCommand(cmd string) {
	switch cmd {
	case "task new":
		_ = gu.taskNew(nil, nil)
	case "task done":
		_ = gu.taskDone(nil, nil)
	case "task cancel":
		_ = gu.taskCancel(nil, nil)
	case "task reopen":
		_ = gu.taskReopen(nil, nil)
	case "task enroll":
		_ = gu.taskEnroll(nil, nil)
	case "task block":
		_ = gu.taskBlock(nil, nil)
	case "task unblock":
		_ = gu.taskUnblock(nil, nil)
	case "task comment":
		_ = gu.taskComment(nil, nil)
	case "workflow create":
		_ = gu.workflowNew(nil, nil)
	case "workflow delete":
		_ = gu.workflowDelete(nil, nil)
	case "agent install":
		_ = gu.agentInstall(nil, nil)
	case "job cancel":
		_ = gu.jobCancel(nil, nil)
	case "scope clear":
		_ = gu.handleClearScope(nil, nil)
	case "refresh":
		_ = gu.handleRefresh(nil, nil)
	case "quit":
		gu.cancel()
		gu.g.Update(func(_ *gocui.Gui) error { return gocui.ErrQuit })
	}
}

// openHelp opens the per-binding cheatsheet.
func (gu *Gui) openHelp(*gocui.Gui, *gocui.View) error {
	lines := []string{
		"global:",
		"  1..4 Tab     focus side panel    /   filter",
		"  0            focus detail        :   palette",
		"  R            refresh             *   clear scope",
		"  @            toggle log          ?   help",
		"  q Ctrl-C     quit                Esc back/close",
		"",
		"tasks:",
		"  n new    d done     x cancel   o reopen",
		"  e enroll r resume   b block    u unblock  m comment",
		"  p priority",
		"",
		"jobs:",
		"  Enter inspector (Live default)",
		"  a Live   s Archive   i Meta    K cancel",
		"",
		"workflows:",
		"  n new (from file)   D delete",
		"",
		"agents:",
		"  i install   u uninstall",
		"",
		"inspector:",
		"  [ / ]   1..4   tab cycle/jump",
		"  Esc / Ctrl-O   back to dashboard",
		"  body: j/k Ctrl-F/Ctrl-B PgUp/PgDn g/G",
		"  Live: Ctrl-D send  Ctrl-F follow_up  Ctrl-A abort  Ctrl-B/PgUp scroll-back",
	}
	gu.openMenu("help", lines, func(_ int) error { return gu.popupClose(nil, nil) })
	return nil
}

// taskNew opens the prompt and creates a task.
func (gu *Gui) taskNew(*gocui.Gui, *gocui.View) error {
	gu.openPrompt("new task title:", "", func(title string) error {
		if strings.TrimSpace(title) == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			id, err := gu.ds.CreateTask(gu.ctx, title, "", store.DefaultPriority)
			if err != nil {
				gu.flashf("err", "task new: %v", err)
				return nil
			}
			gu.flashf("info", "created %s", id)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskDone(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.confirmThen(fmt.Sprintf("mark %s done?", t.ID), func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.UpdateStatus(gu.ctx, t.ID, store.StatusDone); err != nil {
				gu.flashf("err", "done: %v", err)
				return nil
			}
			gu.flashf("info", "done %s", t.ID)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

func (gu *Gui) taskCancel(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.confirmThen(fmt.Sprintf("cancel %s?", t.ID), func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.UpdateStatus(gu.ctx, t.ID, store.StatusCancelled); err != nil {
				gu.flashf("err", "cancel: %v", err)
				return nil
			}
			gu.flashf("info", "cancelled %s", t.ID)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

func (gu *Gui) taskReopen(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.g.OnWorker(func(_ gocui.Task) error {
		if err := gu.ds.UpdateStatus(gu.ctx, t.ID, store.StatusNew); err != nil {
			gu.flashf("err", "reopen: %v", err)
			return nil
		}
		gu.flashf("info", "reopened %s", t.ID)
		gu.refreshAll()
		return nil
	})
	return nil
}

// taskClaim was removed: the v0.2 schema has no claim verb. See the
// design plan + the help screen — 'e' enrolls, 'a' (palette) assigns.

func (gu *Gui) taskEnroll(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openPrompt("enroll "+t.ID+" into workflow name:", "", func(wf string) error {
		wf = strings.TrimSpace(wf)
		if wf == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.Enroll(gu.ctx, t.ID, wf); err != nil {
				gu.flashf("err", "enroll: %v", err)
				return nil
			}
			gu.flashf("info", "enroll %s -> %s ok", t.ID, wf)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskResume(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openPrompt("resume "+t.ID+" to step (empty=current):", "", func(step string) error {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.Resume(gu.ctx, t.ID, strings.TrimSpace(step)); err != nil {
				gu.flashf("err", "resume: %v", err)
				return nil
			}
			gu.flashf("info", "resumed %s", t.ID)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskBlock(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openPrompt("block "+t.ID+" by id:", "", func(blocker string) error {
		blocker = strings.TrimSpace(blocker)
		if blocker == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.Block(gu.ctx, t.ID, blocker); err != nil {
				gu.flashf("err", "block: %v", err)
				return nil
			}
			gu.flashf("info", "blocked %s <- %s", t.ID, blocker)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskUnblock(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openPrompt("unblock "+t.ID+" from id:", "", func(blocker string) error {
		blocker = strings.TrimSpace(blocker)
		if blocker == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.Unblock(gu.ctx, t.ID, blocker); err != nil {
				gu.flashf("err", "unblock: %v", err)
				return nil
			}
			gu.flashf("info", "unblocked %s <- %s", t.ID, blocker)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskComment(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openPrompt("comment on "+t.ID+":", "", func(text string) error {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.AddComment(gu.ctx, t.ID, text); err != nil {
				gu.flashf("err", "comment: %v", err)
				return nil
			}
			gu.flashf("info", "comment on %s", t.ID)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskPriority(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openMenu("priority for "+t.ID, []string{"0", "1", "2", "3"}, func(i int) error {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.UpdatePriority(gu.ctx, t.ID, i); err != nil {
				gu.flashf("err", "priority: %v", err)
				return nil
			}
			gu.flashf("info", "P%d on %s", i, t.ID)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) workflowNew(*gocui.Gui, *gocui.View) error {
	gu.openPrompt("workflow file path:", "", func(path string) error {
		path = strings.TrimSpace(path)
		if path == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			name, err := gu.ds.CreateWorkflow(gu.ctx, path)
			if err != nil {
				gu.flashf("err", "workflow new: %v", err)
				return nil
			}
			gu.flashf("info", "workflow %s created", name)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) workflowDelete(*gocui.Gui, *gocui.View) error {
	w, ok := gu.st.selectedWorkflowLocked()
	if !ok {
		return nil
	}
	gu.confirmThen("delete workflow "+w.Name+"?", func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.DeleteWorkflow(gu.ctx, w.Name); err != nil {
				gu.flashf("err", "wf delete: %v", err)
				return nil
			}
			gu.flashf("info", "wf %s deleted", w.Name)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

// agentInstall and agentUninstall are intentionally informational in
// v1: the daemon has no /v1/agents endpoint so live mode returns the
// same error as offline. Rather than ask the user to type a name and
// THEN show an error, we surface up-front that the verb shells out
// outside the TUI. The hotkeys stay bound so the help screen line
// 'i install / u uninstall' is honest (it points to the workaround).
func (gu *Gui) agentInstall(*gocui.Gui, *gocui.View) error {
	gu.openMenu(
		"agent install isn't supported from lazy yet",
		[]string{
			"quit lazy and run: autosk agent install <pkg>",
			"close",
		},
		func(_ int) error { return nil },
	)
	return nil
}

func (gu *Gui) agentUninstall(*gocui.Gui, *gocui.View) error {
	a, _ := gu.st.selectedAgentLocked()
	name := a.Name
	if name == "" {
		name = "<pkg>"
	}
	gu.openMenu(
		"agent uninstall isn't supported from lazy yet",
		[]string{
			"quit lazy and run: autosk agent uninstall " + name,
			"close",
		},
		func(_ int) error { return nil },
	)
	return nil
}

func (gu *Gui) jobAttachLive(*gocui.Gui, *gocui.View) error {
	j, ok := gu.st.selectedJobLocked()
	if !ok {
		return nil
	}
	gu.openInspectorAtTab(j.JobID, tabLive)
	return nil
}
func (gu *Gui) jobOpenArchive(*gocui.Gui, *gocui.View) error {
	j, ok := gu.st.selectedJobLocked()
	if !ok {
		return nil
	}
	gu.openInspectorAtTab(j.JobID, tabArchive)
	return nil
}
func (gu *Gui) jobOpenMeta(*gocui.Gui, *gocui.View) error {
	j, ok := gu.st.selectedJobLocked()
	if !ok {
		return nil
	}
	gu.openInspectorAtTab(j.JobID, tabMeta)
	return nil
}

func (gu *Gui) jobCancel(*gocui.Gui, *gocui.View) error {
	j, ok := gu.st.selectedJobLocked()
	if !ok {
		return nil
	}
	gu.confirmThen("cancel job "+j.JobID+"?", func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.CancelJob(gu.ctx, j.JobID); err != nil {
				gu.flashf("err", "cancel job: %v", err)
				return nil
			}
			gu.flashf("info", "job %s cancelled", j.JobID)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

// confirmThen opens a confirm popup; on yes runs f.
func (gu *Gui) confirmThen(prompt string, f func()) {
	gu.openConfirm(prompt, func() error { f(); return nil })
}


