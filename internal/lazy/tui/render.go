package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"autosk/internal/daemon/runstore"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
)

// Styles are kept in one struct so theme tweaks happen in one place.
// All colours go through lipgloss's adaptive palette so dark/light
// terminals both render reasonably.
var (
	styleHeader  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleMuted   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleAccent  = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleScope   = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	styleFilter  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
)

// glyphForTaskStatus returns the one-rune status glyph for a task.
func glyphForTaskStatus(s store.Status) string {
	switch s {
	case store.StatusNew:
		return "●"
	case store.StatusInWorkflow:
		return "⏳"
	case store.StatusHumanFeedback:
		return "✋"
	case store.StatusDone:
		return "✓"
	case store.StatusCancelled:
		return "✗"
	}
	return "?"
}

// glyphForJobStatus returns the icon + label for a job's status.
func glyphForJobStatus(j datasource.Job) string {
	switch runstore.RunStatus(j.Status) {
	case runstore.StatusQueued:
		return "· queued"
	case runstore.StatusRunning:
		if j.Streaming {
			return "◐ stream"
		}
		return "◑ idle"
	case runstore.StatusDone:
		return "◯ done"
	case runstore.StatusFailed:
		return "✗ failed"
	case runstore.StatusCancelled:
		return "‣ cancel"
	}
	return j.Status
}

// renderTasksPanel returns the rendered text for the Tasks panel.
func renderTasksPanel(tasks []datasource.Task, cursor int, scope scope, filter string) string {
	var b strings.Builder
	if scope.WorkflowName != "" {
		fmt.Fprintf(&b, "%s\n", styleMuted.Render(fmt.Sprintf("(scoped by wf=%s)", scope.WorkflowName)))
	}
	if scope.Agent != "" {
		fmt.Fprintf(&b, "%s\n", styleMuted.Render(fmt.Sprintf("(scoped by agent=%s)", scope.Agent)))
	}
	if filter != "" {
		fmt.Fprintf(&b, "%s\n", styleFilter.Render("/ "+filter))
	}
	if len(tasks) == 0 {
		b.WriteString(styleMuted.Render("(no tasks)") + "\n")
		return b.String()
	}
	for i, t := range tasks {
		marker := "  "
		if i == cursor {
			marker = "▶ "
		}
		title := t.Title
		if len(title) > 30 {
			title = title[:27] + "…"
		}
		stepStr := ""
		if t.StepName != "" {
			stepStr = " ▶" + t.StepName
		}
		line := fmt.Sprintf("%s%-9s %s P%d %-14s%s %s",
			marker, t.ID, glyphForTaskStatus(t.Status), t.Priority, t.Status, stepStr, title)
		if i == cursor {
			line = styleAccent.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// renderJobsPanel renders the Jobs panel.
func renderJobsPanel(jobs []datasource.Job, cursor int, scope scope, filter string) string {
	var b strings.Builder
	if scope.TaskID != "" {
		fmt.Fprintf(&b, "%s\n", styleMuted.Render("(filtered by task "+scope.TaskID+")"))
	}
	if filter != "" {
		fmt.Fprintf(&b, "%s\n", styleFilter.Render("/ "+filter))
	}
	if len(jobs) == 0 {
		b.WriteString(styleMuted.Render("(no jobs)") + "\n")
		return b.String()
	}
	for i, j := range jobs {
		marker := "  "
		if i == cursor {
			marker = "▶ "
		}
		age := humanAge(j.CreatedAt)
		attached := ""
		if j.AttachCount > 0 {
			attached = fmt.Sprintf(" (%d)", j.AttachCount)
		}
		live := ""
		if j.Streaming {
			live = " " + styleOK.Render("*live*")
		}
		wfstep := j.WorkflowName + ":" + j.StepName
		if j.WorkflowName == "" {
			wfstep = "(no-wf)"
		}
		line := fmt.Sprintf("%s%-9s %-9s %-20s %-5s%s%s",
			marker, j.JobID, glyphForJobStatus(j), wfstep, age, attached, live)
		if i == cursor {
			line = styleAccent.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// renderWorkflowsPanel renders the Workflows panel.
func renderWorkflowsPanel(wfs []datasource.Workflow, cursor int, filter string) string {
	var b strings.Builder
	if filter != "" {
		fmt.Fprintf(&b, "%s\n", styleFilter.Render("/ "+filter))
	}
	if len(wfs) == 0 {
		b.WriteString(styleMuted.Render("(no workflows)") + "\n")
		return b.String()
	}
	for i, w := range wfs {
		marker := "  "
		if i == cursor {
			marker = "▶ "
		}
		synth := ""
		if w.IsSynthetic {
			synth = " " + styleMuted.Render("(synthetic)")
		}
		line := fmt.Sprintf("%s%-24s %d steps  first=%s%s",
			marker, w.Name, len(w.Steps), w.FirstStep, synth)
		if i == cursor {
			line = styleAccent.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// renderAgentsPanel renders the Agents panel.
func renderAgentsPanel(ags []datasource.Agent, cursor int, filter string) string {
	var b strings.Builder
	if filter != "" {
		fmt.Fprintf(&b, "%s\n", styleFilter.Render("/ "+filter))
	}
	if len(ags) == 0 {
		b.WriteString(styleMuted.Render("(no agents)") + "\n")
		return b.String()
	}
	for i, a := range ags {
		marker := "  "
		if i == cursor {
			marker = "▶ "
		}
		glyph := "○"
		switch a.Source {
		case "installed":
			glyph = "●"
		case "db_only":
			glyph = "!"
		}
		v := a.Version
		if v == "" {
			v = "—"
		}
		line := fmt.Sprintf("%s%s %-26s %-9s %s", marker, glyph, a.Name, v, a.Source)
		if i == cursor {
			line = styleAccent.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// renderDetail renders the right detail pane for whatever is focused.
func renderDetail(s *state) string {
	switch s.focused {
	case panelTasks:
		if t, ok := s.selectedTask(); ok {
			return renderTaskDetail(t, s.comments(s, t.ID))
		}
	case panelJobs:
		if j, ok := s.selectedJob(); ok {
			return renderJobDetail(j)
		}
	case panelWorkflows:
		if w, ok := s.selectedWorkflow(); ok {
			return renderWorkflowDetail(w)
		}
	case panelAgents:
		if a, ok := s.selectedAgent(); ok {
			return renderAgentDetail(a)
		}
	}
	return styleMuted.Render("(nothing selected)")
}

// state.comments uses the local cache that helpers.refresh hydrates.
// Implemented as a method on *state-by-value here so the renderer
// doesn't have to know about the cache shape.
func (s *state) comments(_ *state, _ string) []datasource.Comment { return nil }

func renderTaskDetail(t datasource.Task, _ []datasource.Comment) string {
	var b strings.Builder
	fmt.Fprintln(&b, styleHeader.Render(fmt.Sprintf("task %s  %s  P%d", t.ID, t.Status, t.Priority)))
	if t.WorkflowName != "" {
		fmt.Fprintf(&b, "wf=%s step=%s agent=%s\n", t.WorkflowName, t.StepName, t.AgentName)
	}
	if t.AuthorName != "" {
		fmt.Fprintf(&b, "author: %s\n", t.AuthorName)
	}
	fmt.Fprintf(&b, "blocked: %v   comments: %d\n", t.Blocked, t.CommentCount)
	fmt.Fprintf(&b, "created: %s\n", t.CreatedAt.Format(time.RFC3339))
	b.WriteString(styleMuted.Render("─ description ─") + "\n")
	desc := t.Description
	if desc == "" {
		desc = styleMuted.Render("(no description)")
	}
	b.WriteString(desc + "\n")
	if len(t.BlockedBy) > 0 {
		b.WriteString("\n" + styleMuted.Render(fmt.Sprintf("─ blocked by (%d) ─", len(t.BlockedBy))) + "\n")
		for _, id := range t.BlockedBy {
			fmt.Fprintf(&b, "  ↑ %s\n", id)
		}
	}
	if len(t.Blocks) > 0 {
		b.WriteString("\n" + styleMuted.Render(fmt.Sprintf("─ blocks (%d) ─", len(t.Blocks))) + "\n")
		for _, id := range t.Blocks {
			fmt.Fprintf(&b, "  ↓ %s\n", id)
		}
	}
	return b.String()
}

func renderJobDetail(j datasource.Job) string {
	var b strings.Builder
	fmt.Fprintln(&b, styleHeader.Render(fmt.Sprintf("job %s  %s", j.JobID, j.Status)))
	fmt.Fprintf(&b, "task: %s\n", j.TaskID)
	fmt.Fprintf(&b, "wf:step: %s:%s   agent: %s\n", j.WorkflowName, j.StepName, j.AgentName)
	fmt.Fprintf(&b, "streaming: %v   attached: %d\n", j.Streaming, j.AttachCount)
	fmt.Fprintf(&b, "corrections: %d/%d\n", j.CorrectionsUsed, j.MaxCorrections)
	fmt.Fprintf(&b, "session: %s\n", j.SessionPath)
	if j.PID != nil {
		fmt.Fprintf(&b, "pid: %d\n", *j.PID)
	}
	fmt.Fprintf(&b, "created: %s\n", j.CreatedAt.Format(time.RFC3339))
	if j.StartedAt != nil {
		fmt.Fprintf(&b, "started: %s\n", j.StartedAt.Format(time.RFC3339))
	}
	if j.FinishedAt != nil {
		fmt.Fprintf(&b, "finished: %s\n", j.FinishedAt.Format(time.RFC3339))
	}
	if j.Error != "" {
		b.WriteString(styleErr.Render("error: "+j.Error) + "\n")
	}
	return b.String()
}

func renderWorkflowDetail(w datasource.Workflow) string {
	var b strings.Builder
	fmt.Fprintln(&b, styleHeader.Render(fmt.Sprintf("workflow %s", w.Name)))
	if w.IsSynthetic {
		fmt.Fprintln(&b, styleMuted.Render("synthetic single-step workflow"))
	}
	if w.Description != "" {
		fmt.Fprintln(&b, w.Description)
	}
	fmt.Fprintf(&b, "first step: %s\n", w.FirstStep)
	fmt.Fprintf(&b, "tasks: %d\n", w.TaskCount)
	b.WriteString(styleMuted.Render("─ steps ─") + "\n")
	for _, s := range w.Steps {
		nexts := strings.Join(append(append([]string{}, s.NextSteps...), s.NextStatus...), ", ")
		if nexts == "" {
			nexts = "(none)"
		}
		fmt.Fprintf(&b, "  %s  agent=%s  next=%s  tasks=%d\n", s.Name, s.AgentName, nexts, s.TaskCount)
	}
	return b.String()
}

func renderAgentDetail(a datasource.Agent) string {
	var b strings.Builder
	fmt.Fprintln(&b, styleHeader.Render(fmt.Sprintf("agent %s  (%s)", a.Name, a.Source)))
	if a.Version != "" {
		fmt.Fprintf(&b, "version: %s\n", a.Version)
	}
	if a.Model != "" {
		fmt.Fprintf(&b, "model: %s\n", a.Model)
	}
	if a.Thinking != "" {
		fmt.Fprintf(&b, "thinking: %s\n", a.Thinking)
	}
	if len(a.ExtraArgs) > 0 {
		fmt.Fprintf(&b, "extra args: %s\n", strings.Join(a.ExtraArgs, " "))
	}
	if len(a.PiExt) > 0 {
		fmt.Fprintf(&b, "pi_extensions:\n")
		for _, p := range a.PiExt {
			fmt.Fprintf(&b, "  %s\n", p)
		}
	}
	if len(a.PiSkills) > 0 {
		fmt.Fprintf(&b, "pi_skills:\n")
		for _, p := range a.PiSkills {
			fmt.Fprintf(&b, "  %s\n", p)
		}
	}
	fmt.Fprintf(&b, "tasks owned: %d\n", a.TasksOwned)
	return b.String()
}

// renderStatusBar returns the bottom-line status string.
func renderStatusBar(s *state, projectRoot string) string {
	var parts []string
	parts = append(parts, "autosk")
	parts = append(parts, truncate(projectRoot, 40))
	daemonStr := "daemon=" + s.health.Daemon
	switch s.health.Daemon {
	case "ok":
		daemonStr = styleOK.Render(daemonStr)
	case "stale":
		daemonStr = styleWarn.Render(daemonStr)
	default:
		daemonStr = styleErr.Render(daemonStr)
	}
	parts = append(parts, daemonStr)
	if s.health.Daemon == "ok" {
		parts = append(parts, fmt.Sprintf("workers=%d q=%d r=%d", s.health.Workers, s.health.Queued, s.health.Running))
	}
	scopeStr := "—"
	if !s.scope.IsEmpty() {
		var bits []string
		if s.scope.TaskID != "" {
			bits = append(bits, "task="+s.scope.TaskID)
		}
		if s.scope.WorkflowName != "" {
			bits = append(bits, "wf="+s.scope.WorkflowName)
		}
		if s.scope.Agent != "" {
			bits = append(bits, "agent="+s.scope.Agent)
		}
		scopeStr = strings.Join(bits, " ")
	}
	parts = append(parts, "scope: "+styleScope.Render(scopeStr))
	parts = append(parts, "?=help")
	return strings.Join(parts, "  ")
}

// renderCommandLog returns the bottom-right log block.
func renderCommandLog(lines []string, flash flashState) string {
	var b strings.Builder
	if flash.Text != "" && time.Since(flash.CreatedAt) < 4*time.Second {
		style := styleAccent
		switch flash.Level {
		case "warn":
			style = styleWarn
		case "err":
			style = styleErr
		}
		b.WriteString(style.Render("⚡ "+flash.Text) + "\n")
	}
	start := 0
	if len(lines) > 8 {
		start = len(lines) - 8
	}
	for _, l := range lines[start:] {
		b.WriteString(styleMuted.Render(l) + "\n")
	}
	return b.String()
}

// truncate returns s, cropped to n with an ellipsis.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return s[:n-1] + "…"
}

// humanAge returns "3m", "1h", "2d" etc.
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// renderTranscript turns a list of MessageEvents into a single-string
// block. Used by both the Archive and Live tabs.
func renderTranscript(events []datasource.MessageEvent) string {
	var b strings.Builder
	for _, e := range events {
		stamp := ""
		if !e.TS.IsZero() {
			stamp = e.TS.Format("15:04:05") + " "
		}
		head := styleHeader.Render(stamp + e.Kind)
		if e.Name != "" {
			head += " " + styleAccent.Render(e.Name)
		}
		b.WriteString(head + "\n")
		if e.Text != "" {
			b.WriteString("  " + strings.ReplaceAll(e.Text, "\n", "\n  ") + "\n")
		}
	}
	return b.String()
}

// renderInspectorHeader renders the tab strip + run-id line.
func renderInspectorHeader(insp inspectorState) string {
	tabs := []string{tabLive.String(), tabArchive.String(), tabMeta.String(), tabSignals.String()}
	rendered := make([]string, len(tabs))
	for i, name := range tabs {
		mark := "[" + name + "]"
		if inspectorTab(i) == insp.Tab {
			rendered[i] = styleAccent.Render(mark)
		} else {
			rendered[i] = styleMuted.Render(mark)
		}
	}
	stream := ""
	if insp.Streaming {
		stream = " " + styleOK.Render("streaming")
	}
	// Two lines: first line is run-id + status, second line is the tab strip.
	// Lipgloss renders inline ANSI here (the gocui view passes ANSI through).
	job := "run " + insp.JobID
	hdr := fmt.Sprintf("%s  %s%s\n%s", styleHeader.Render(job), insp.Job.Status, stream, strings.Join(rendered, " "))
	return hdr
}
