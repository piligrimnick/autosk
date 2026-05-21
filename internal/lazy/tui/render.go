package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"autosk/internal/daemon/runstore"
	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/theme"
	"autosk/internal/store"
	"autosk/internal/timeformat"
)

// Styles are derived from the active theme.Palette at package-load
// time. Render code stays palette-agnostic: every literal colour lives
// in internal/lazy/theme, so swapping palettes is a one-line change
// there and a RebuildStyles() call here.
//
// The slot → style mapping is intentionally narrow (8 styles total) —
// see internal/lazy/theme/theme.go's Palette doc for which UI element
// each one feeds.
var (
	styleHeader lipgloss.Style
	styleMuted  lipgloss.Style
	styleAccent lipgloss.Style
	styleWarn   lipgloss.Style
	styleErr    lipgloss.Style
	styleOK     lipgloss.Style
	styleScope  lipgloss.Style
	styleFilter lipgloss.Style

	// Per-task-status colours for the Tasks panel. The palette slot
	// each one taps is picked for the hue, not for the slot's primary
	// semantic role — see styleForTaskStatus's comment for the
	// rationale and the requested mapping.
	styleStatusNew           lipgloss.Style
	styleStatusInWorkflow    lipgloss.Style
	styleStatusHumanFeedback lipgloss.Style
	styleStatusDone          lipgloss.Style
	styleStatusCancelled     lipgloss.Style

	// Per-entity colours. Every reference to a task / job / agent /
	// workflow / step in the TUI — panel rows, detail panes, status
	// bar chips, signals — funnels through these styles (or the
	// matching renderEntity* helpers below) so the same hue means the
	// same kind of thing across the whole dashboard.
	//
	// Hues are picked for distinctness (no two entity types share a
	// colour) and reuse existing palette slots so the TokyoNight
	// invariant from theme_test.go still holds.
	styleTaskID       lipgloss.Style // blue    (Focus hue)    — task ids
	styleJobID        lipgloss.Style // magenta (Accent hue)   — job (run) ids
	styleAgentName    lipgloss.Style // cyan    (Scope hue)    — agent name / id
	styleWorkflowName lipgloss.Style // yellow  (Warn hue)     — workflow name / id
	styleStepName     lipgloss.Style // purple  (PopupBox hue) — step name / id
)

func init() { RebuildStyles() }

// RebuildStyles rebuilds the cached lipgloss styles from the currently
// active theme.Palette. Call after theme.SetActive(...) so a runtime
// palette switch picks up on the next frame; tests use it to scope a
// palette swap to one test case.
func RebuildStyles() {
	p := theme.Active()
	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(p.Header.Lipgloss())
	styleMuted = lipgloss.NewStyle().Foreground(p.Muted.Lipgloss())
	styleAccent = lipgloss.NewStyle().Foreground(p.Accent.Lipgloss())
	styleWarn = lipgloss.NewStyle().Foreground(p.Warn.Lipgloss())
	styleErr = lipgloss.NewStyle().Foreground(p.Err.Lipgloss())
	styleOK = lipgloss.NewStyle().Foreground(p.OK.Lipgloss())
	styleScope = lipgloss.NewStyle().Foreground(p.Scope.Lipgloss()).Bold(true)
	styleFilter = lipgloss.NewStyle().Foreground(p.Filter.Lipgloss())

	// Task-status colours. We borrow hues from existing palette slots
	// (no new palette entries) but drop the bold / decorative bits so
	// the status column reads as data, not as a chip:
	//
	//	new              cyan   (Scope hue, non-bold)
	//	in_workflow      purple (PopupBox hue)
	//	human_feedback   yellow (Warn hue — shares the workflow entity
	//	                        colour on purpose: a row waiting on an
	//	                        operator sits in the same visual category
	//	                        as the workflow it's pinned to)
	//	done             green  (OK)
	//	cancelled        gray   (Muted)
	styleStatusNew = lipgloss.NewStyle().Foreground(p.Scope.Lipgloss())
	styleStatusInWorkflow = lipgloss.NewStyle().Foreground(p.PopupBox.Lipgloss())
	styleStatusHumanFeedback = lipgloss.NewStyle().Foreground(p.Warn.Lipgloss())
	styleStatusDone = lipgloss.NewStyle().Foreground(p.OK.Lipgloss())
	styleStatusCancelled = lipgloss.NewStyle().Foreground(p.Muted.Lipgloss())

	// Per-entity colour styles. See the var block above for the
	// rationale. None of these are bold: the column position and the
	// hue itself already do the work — bolding a Cyrillic title in
	// addition would only make the row noisier.
	//
	// Step + in_workflow status share the PopupBox hue on purpose:
	// they're semantically the same thing ("a workflow step is
	// driving this row"), just rendered in two different columns.
	styleTaskID = lipgloss.NewStyle().Foreground(p.Focus.Lipgloss())
	styleJobID = lipgloss.NewStyle().Foreground(p.Accent.Lipgloss())
	styleAgentName = lipgloss.NewStyle().Foreground(p.Scope.Lipgloss())
	styleWorkflowName = lipgloss.NewStyle().Foreground(p.Warn.Lipgloss())
	styleStepName = lipgloss.NewStyle().Foreground(p.PopupBox.Lipgloss())
}

// ---- entity rendering helpers -------------------------------------------
//
// Use these wherever a task id / job id / agent name / workflow name /
// step name is printed. They wrap the value in the canonical entity
// style so a future palette tweak (or a renamed slot) lands in one
// place. Empty values short-circuit to "" so callers don't have to
// guard the common "if name != ''" pattern at every site.
//
// For columns that need %-width padding, pad INSIDE the Render call
// (`style.Render(fmt.Sprintf("%-9s", id))`): ANSI escapes added by
// lipgloss would otherwise inflate the byte width past what %-Ns
// can reason about. The colour-distinctness invariant is pinned by
// TestEntityStyles_Distinct.

func renderTaskID(id string) string {
	if id == "" {
		return ""
	}
	return styleTaskID.Render(id)
}

func renderJobID(id string) string {
	if id == "" {
		return ""
	}
	return styleJobID.Render(id)
}

func renderAgentName(name string) string {
	if name == "" {
		return ""
	}
	return styleAgentName.Render(name)
}

func renderWorkflowName(name string) string {
	if name == "" {
		return ""
	}
	return styleWorkflowName.Render(name)
}

func renderStepName(name string) string {
	if name == "" {
		return ""
	}
	return styleStepName.Render(name)
}

// renderWorkflowStep composes a "workflow:step" label with each half
// in its entity colour and the colon left in the default foreground.
// Returns a muted "(no-wf)" when wf is empty (legacy daemon rows
// without a workflow id). step may also be empty, in which case the
// trailing ":step" is dropped entirely — "feature-dev:" reads worse
// than "feature-dev".
func renderWorkflowStep(wf, step string) string {
	if wf == "" {
		return styleMuted.Render("(no-wf)")
	}
	if step == "" {
		return renderWorkflowName(wf)
	}
	return renderWorkflowName(wf) + ":" + renderStepName(step)
}

// workflowStepPlainWidth returns the on-screen width (in cells) of
// the string that renderWorkflowStep produces, so callers that need
// to pad the column to a fixed width can compute the trailing-space
// count without counting ANSI escape bytes.
func workflowStepPlainWidth(wf, step string) int {
	if wf == "" {
		return utf8.RuneCountInString("(no-wf)")
	}
	if step == "" {
		return utf8.RuneCountInString(wf)
	}
	return utf8.RuneCountInString(wf) + 1 + utf8.RuneCountInString(step)
}

// styleForTaskStatus returns the lipgloss style used to paint the
// status column for a task. Unknown statuses fall back to Muted so a
// future enum value doesn't crash the renderer.
func styleForTaskStatus(s store.Status) lipgloss.Style {
	switch s {
	case store.StatusNew:
		return styleStatusNew
	case store.StatusInWorkflow:
		return styleStatusInWorkflow
	case store.StatusHumanFeedback:
		return styleStatusHumanFeedback
	case store.StatusDone:
		return styleStatusDone
	case store.StatusCancelled:
		return styleStatusCancelled
	}
	return styleMuted
}

// spinnerFrames is the braille "dots" animation used to mark a task
// whose row currently has an active job. Ten frames, advanced by the
// Gui's spinner ticker (~100ms cadence). The braille block is U+2800,
// so every frame is exactly one cell wide — the title column lands
// at the same x whether the row spins or not.
var spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerFrame returns the spinner glyph for the given tick, indexed
// modulo the frame count. Pulled out so a future test can pin the
// frame order without poking at the slice directly.
func spinnerFrame(tick uint64) string {
	return spinnerFrames[tick%uint64(len(spinnerFrames))]
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

// statusColumnWidth caps the rendered status string at this many
// display cells. Statuses longer than this are truncated with an
// ellipsis (truncate replaces the last rune with …) so the title
// column always starts at the same column position regardless of
// which enum value the row carries. 7 fits "done" / "new" without
// padding bloat and collapses "in_workflow" / "human_feedback" /
// "cancelled" into recognisable abbreviations ("in_wor…", "human_…",
// "cancel…").
const statusColumnWidth = 7

// renderTasksPanel returns the rendered text for the Tasks panel
// and the number of leading header lines (scope / filter notes) so
// the caller can sync the gocui view's cursor-row index past them —
// the row-highlight (v.SelBgColor) lives on the data rows, not on
// the meta header.
//
// Column order (lazygit git-log layout: leftmost column is the
// glance-scan key, rightmost is the long human-readable label that
// extends to the right edge):
//
//	[P{prio}-Muted] [id-TaskID] [job-marker] [status-coloured] [title-default]
//
// Priority is leftmost because it's the most common scan axis ("what
// matters most right now?") and uses the same muted gray as other
// metadata so the bright Accent id stays the visual anchor for each
// row. The status column is coloured per-value (styleForTaskStatus)
// so the operator can spot terminal vs. open work by hue alone.
//
// The job-marker column is a single cell with three states, driven
// by jobIdx (built from the current jobs snapshot):
//
//   - active job (running) → braille spinner frame in styleTaskID
//     (blue), animated by Gui.spinnerLoop at ~100ms.
//   - any other job (queued / done / failed / cancelled) → a static
//     ">" in styleJobID (magenta) so the operator can see at a
//     glance which task rows are tied to a job, even after the run
//     finished. ASCII `>` on purpose: keeps width math trivial and
//     renders the same way under tmux / windows-console where wider
//     codepoints can shift columns.
//   - no jobs → blank space, preserving column alignment.
//
// spinnerTick advances on a separate ~100ms ticker
// (Gui.spinnerLoop), independent of the 2s data refresh.
//
// The title column gets the remaining width minus all fixed columns
// and is truncated with an ellipsis when it doesn't fit; width<=0
// falls back to a generous default so the panel still renders
// during the very first layout pass before gocui has sized the
// view.
//
// The cursor itself is no longer drawn here — gocui's Highlight +
// SelBgColor paints the row's background. That keeps per-column
// foregrounds intact on the selected row (instead of overpainting
// the whole row with Accent like the old `▶ ` marker did).
func renderTasksPanel(
	tasks []datasource.Task, _ int,
	scope scope, filter string,
	width int, spinnerTick uint64, jobIdx taskJobIndex,
) (string, int) {
	var b strings.Builder
	header := 0
	if scope.WorkflowName != "" {
		fmt.Fprintf(&b, "%s\n", styleMuted.Render(fmt.Sprintf("(scoped by wf=%s)", scope.WorkflowName)))
		header++
	}
	if scope.Agent != "" {
		rel := scope.AgentRel.String()
		if rel != "" {
			fmt.Fprintf(&b, "%s\n", styleMuted.Render(fmt.Sprintf("(scoped by agent=%s as %s)", scope.Agent, rel)))
		} else {
			fmt.Fprintf(&b, "%s\n", styleMuted.Render(fmt.Sprintf("(scoped by agent=%s)", scope.Agent)))
		}
		header++
	}
	if filter != "" {
		fmt.Fprintf(&b, "%s\n", styleFilter.Render("/ "+filter))
		header++
	}
	if len(tasks) == 0 {
		b.WriteString(styleMuted.Render("(no tasks)") + "\n")
		return b.String(), header
	}
	// Fixed-column widths. Status is truncated/padded to
	// statusColumnWidth. Id is padded to 7 cells — the exact width
	// of an "as-XXXX" id (prefix-NNNN, 2+4 chars). The previous
	// idW=8 left a cell of "slack for future id schemes" but the
	// id package's contract is fixed-width per project, so the
	// slack only showed up on screen as an unsightly double space
	// before the > / spinner marker. The total below counts the
	// inter-column separators too — keep it in sync with the
	// Fprintf format string below.
	const (
		prioW   = 2 // "P0".."P3"
		idW     = 7 // "as-XXXX" (no trailing slack)
		markerW = 1 // spinner braille frame, >, or blank
		// 4 single-space separators between the 5 columns.
		fixedW = prioW + 1 + idW + 1 + markerW + 1 + statusColumnWidth + 1
	)
	spin := spinnerFrame(spinnerTick)
	for _, t := range tasks {
		prio := styleMuted.Render(fmt.Sprintf("P%d", t.Priority))
		id := styleTaskID.Render(fmt.Sprintf("%-*s", idW, t.ID))
		// Job-marker cell: 3-state, all exactly 1 cell wide so the
		// status column lands at the same x regardless of which state
		// fires. See renderTasksPanel's doc comment for the rationale.
		marker := " "
		switch {
		case jobIdx.Active[t.ID]:
			marker = styleTaskID.Render(spin)
		case jobIdx.Any[t.ID]:
			marker = styleJobID.Render(">")
		}
		statusRaw := truncate(string(t.Status), statusColumnWidth)
		// truncate may shrink the string below statusColumnWidth (it
		// only adds an ellipsis when the input WAS longer than n). Pad
		// with spaces in that case so the title column lines up; the
		// padding is plain (uncoloured) text appended OUTSIDE the
		// styled span so the trailing whitespace doesn't pick up the
		// status hue.
		statusPad := statusColumnWidth - utf8.RuneCountInString(statusRaw)
		status := styleForTaskStatus(t.Status).Render(statusRaw)
		if statusPad > 0 {
			status += strings.Repeat(" ", statusPad)
		}
		titleW := width - fixedW
		if titleW < 8 {
			// Either width wasn't supplied yet (first frame) or the
			// panel is impossibly narrow. Fall back to a fixed truncation
			// so the row still renders something useful.
			titleW = 30
		}
		title := truncate(t.Title, titleW)
		fmt.Fprintf(&b, "%s %s %s %s %s\n", prio, id, marker, status, title)
	}
	return b.String(), header
}

// renderJobsPanel renders the Jobs panel. See renderTasksPanel for
// the per-column colour rationale; jobs use:
//
//	[age-Muted] [jobid-JobID] [glyph-Muted] [wf-WorkflowName:step-StepName] [attached-Muted] [*live*-OK]
//
// The wf:step column is composed in entity colours via
// renderWorkflowStep (yellow workflow + default-colour colon +
// purple step). Trailing-space padding is computed against the
// visible width (workflowStepPlainWidth) so ANSI escapes don't
// bleed into the %-20s budget.
//
// Age sits leftmost (lazygit-commits layout) so glance-scanning a
// long Jobs list answers "when did this run start?" without horizontal
// eye motion. The 4-char column accommodates humanAge's longest output
// (e.g. "23h", "7d", or the rare "100d") without misaligning the rest
// of the row.
func renderJobsPanel(jobs []datasource.Job, _ int, scope scope, filter string) (string, int) {
	var b strings.Builder
	header := 0
	if scope.TaskID != "" {
		// Format: "(filter: <id-blue>, press * to reset)".
		// The reset hint is muted so it sits at the back of the line
		// while the id stays the visual anchor.
		fmt.Fprintf(&b, "%s%s%s\n",
			styleMuted.Render("(filter: "),
			renderTaskID(scope.TaskID),
			styleMuted.Render(", press * to reset)"))
		header++
	}
	if filter != "" {
		fmt.Fprintf(&b, "%s\n", styleFilter.Render("/ "+filter))
		header++
	}
	if len(jobs) == 0 {
		b.WriteString(styleMuted.Render("(no jobs)") + "\n")
		return b.String(), header
	}
	const wfstepCol = 20
	for _, j := range jobs {
		age := styleMuted.Render(fmt.Sprintf("%-4s", humanAge(j.CreatedAt)))
		id := styleJobID.Render(fmt.Sprintf("%-9s", j.JobID))
		glyph := styleMuted.Render(fmt.Sprintf("%-9s", glyphForJobStatus(j)))
		wfstep := renderWorkflowStep(j.WorkflowName, j.StepName)
		if pad := wfstepCol - workflowStepPlainWidth(j.WorkflowName, j.StepName); pad > 0 {
			wfstep += strings.Repeat(" ", pad)
		}
		attached := ""
		if j.AttachCount > 0 {
			attached = styleMuted.Render(fmt.Sprintf(" (%d)", j.AttachCount))
		}
		live := ""
		if j.Streaming {
			live = " " + styleOK.Render("*live*")
		}
		fmt.Fprintf(&b, "%s %s %s %s%s%s\n", age, id, glyph, wfstep, attached, live)
	}
	return b.String(), header
}

// renderWorkflowsPanel renders the Workflows panel:
//
//	[name-WorkflowName] [N steps-Muted] [first=stepname-StepName] [(synthetic)-Muted]
//
// The `first=` label stays muted while the step value itself wears
// the StepName hue so a glance at the list groups by workflow first,
// step second.
func renderWorkflowsPanel(wfs []datasource.Workflow, _ int, filter string) (string, int) {
	var b strings.Builder
	header := 0
	if filter != "" {
		fmt.Fprintf(&b, "%s\n", styleFilter.Render("/ "+filter))
		header++
	}
	if len(wfs) == 0 {
		b.WriteString(styleMuted.Render("(no workflows)") + "\n")
		return b.String(), header
	}
	for _, w := range wfs {
		name := styleWorkflowName.Render(fmt.Sprintf("%-24s", w.Name))
		steps := styleMuted.Render(fmt.Sprintf("%d steps", len(w.Steps)))
		first := styleMuted.Render("first=") + renderStepName(w.FirstStep)
		synth := ""
		if w.IsSynthetic {
			synth = " " + styleMuted.Render("(synthetic)")
		}
		fmt.Fprintf(&b, "%s %s  %s%s\n", name, steps, first, synth)
	}
	return b.String(), header
}

// renderAgentsPanel renders the Agents panel:
//
//	[glyph-Muted] [name-AgentName] [version-Muted] [source-Muted]
//
// Version drops from Warn (yellow) to Muted so it doesn't compete
// with the workflow column elsewhere on the dashboard — yellow is
// the canonical workflow hue and surfacing a version chip in the
// same colour is misleading.
func renderAgentsPanel(ags []datasource.Agent, _ int, filter string) (string, int) {
	var b strings.Builder
	header := 0
	if filter != "" {
		fmt.Fprintf(&b, "%s\n", styleFilter.Render("/ "+filter))
		header++
	}
	if len(ags) == 0 {
		b.WriteString(styleMuted.Render("(no agents)") + "\n")
		return b.String(), header
	}
	for _, a := range ags {
		gRune := "○"
		switch a.Source {
		case "installed":
			gRune = "●"
		case "db_only":
			gRune = "!"
		}
		glyph := styleMuted.Render(gRune)
		name := styleAgentName.Render(fmt.Sprintf("%-26s", a.Name))
		ver := a.Version
		if ver == "" {
			ver = "—"
		}
		verStyled := styleMuted.Render(fmt.Sprintf("%-9s", ver))
		source := styleMuted.Render(a.Source)
		fmt.Fprintf(&b, "%s %s %s %s\n", glyph, name, verStyled, source)
	}
	return b.String(), header
}

// renderDetail renders the right detail pane for whatever is focused.
//
// The caller already holds the model's RLock; s.comments[t.ID] and
// s.signals[t.ID] are read directly here to avoid re-locking.
func renderDetail(s *state) string {
	switch s.focused {
	case panelTasks:
		if t, ok := s.selectedTask(); ok {
			return renderTaskDetail(t, s.comments[t.ID], s.signals[t.ID])
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

func renderTaskDetail(t datasource.Task, comments []datasource.Comment, signals []datasource.Signal) string {
	var b strings.Builder
	// Header line: "task <id-blue>  <status-coloured>  P<n>". The label
	// "task" stays bold-blue (styleHeader) so the line still reads as
	// the pane header; the id picks up styleTaskID for cross-pane
	// consistency with the Tasks panel column.
	fmt.Fprintln(&b,
		styleHeader.Render("task")+" "+renderTaskID(t.ID)+
			"  "+styleForTaskStatus(t.Status).Render(string(t.Status))+
			fmt.Sprintf("  P%d", t.Priority))
	if t.WorkflowName != "" {
		fmt.Fprintf(&b, "wf=%s step=%s agent=%s\n",
			renderWorkflowName(t.WorkflowName),
			renderStepName(t.StepName),
			renderAgentName(t.AgentName))
	}
	if t.AuthorName != "" {
		fmt.Fprintf(&b, "author: %s\n", renderAgentName(t.AuthorName))
	}
	fmt.Fprintf(&b, "blocked: %v   comments: %d\n", t.Blocked, t.CommentCount)
	// Local-TZ DateTime for the operator. Machine-facing wire formats
	// (JSON output, daemon HTTP API, RunContextSeed) stay on RFC3339
	// UTC and intentionally do NOT route through timeformat.
	fmt.Fprintf(&b, "created: %s\n", timeformat.FormatDateTime(t.CreatedAt))
	b.WriteString(styleMuted.Render("─ description ─") + "\n")
	desc := t.Description
	if desc == "" {
		desc = styleMuted.Render("(no description)")
	}
	b.WriteString(desc + "\n")
	if len(t.BlockedBy) > 0 {
		b.WriteString("\n" + styleMuted.Render(fmt.Sprintf("─ blocked by (%d) ─", len(t.BlockedBy))) + "\n")
		for _, id := range t.BlockedBy {
			fmt.Fprintf(&b, "  ↑ %s\n", renderTaskID(id))
		}
	}
	if len(t.Blocks) > 0 {
		b.WriteString("\n" + styleMuted.Render(fmt.Sprintf("─ blocks (%d) ─", len(t.Blocks))) + "\n")
		for _, id := range t.Blocks {
			fmt.Fprintf(&b, "  ↓ %s\n", renderTaskID(id))
		}
	}
	// Design plan §4: Tasks detail pane includes the last ≤5 comments.
	if len(comments) > 0 {
		n := len(comments)
		start := 0
		if n > 5 {
			start = n - 5
		}
		b.WriteString("\n" + styleMuted.Render(fmt.Sprintf("─ comments (%d) ─", n)) + "\n")
		for _, c := range comments[start:] {
			// Smart timeline: time-only for today's events, full datetime
			// for anything older so a comment from yesterday is still
			// readable when the operator opens the pane mid-morning.
			fmt.Fprintf(&b, "  %s %s: %s\n",
				timeformat.FormatDateTimeSmart(c.CreatedAt), c.AuthorName, truncate(c.Text, 70))
		}
	}
	// Design plan §4: Tasks detail pane includes the tail of the last
	// open kickback chain. SignalsForTask returns rows newest-first,
	// so the first ≤3 entries are the freshest signals across all
	// runs of this task.
	if len(signals) > 0 {
		n := len(signals)
		end := n
		if end > 3 {
			end = 3
		}
		b.WriteString("\n" + styleMuted.Render(fmt.Sprintf("─ recent signals (%d) ─", n)) + "\n")
		for _, s := range signals[:end] {
			fmt.Fprintf(&b, "  %s %s → %s\n",
				timeformat.FormatDateTimeSmart(s.CreatedAt), s.StepName, s.Target)
		}
	}
	return b.String()
}

func renderJobDetail(j datasource.Job) string {
	var b strings.Builder
	fmt.Fprintln(&b,
		styleHeader.Render("job")+" "+renderJobID(j.JobID)+
			"  "+j.Status)
	fmt.Fprintf(&b, "task: %s\n", renderTaskID(j.TaskID))
	fmt.Fprintf(&b, "wf:step: %s   agent: %s\n",
		renderWorkflowStep(j.WorkflowName, j.StepName),
		renderAgentName(j.AgentName))
	fmt.Fprintf(&b, "streaming: %v   attached: %d\n", j.Streaming, j.AttachCount)
	fmt.Fprintf(&b, "corrections: %d/%d\n", j.CorrectionsUsed, j.MaxCorrections)
	fmt.Fprintf(&b, "session: %s\n", j.SessionPath)
	if j.PID != nil {
		fmt.Fprintf(&b, "pid: %d\n", *j.PID)
	}
	fmt.Fprintf(&b, "created: %s\n", timeformat.FormatDateTime(j.CreatedAt))
	if j.StartedAt != nil {
		fmt.Fprintf(&b, "started: %s\n", timeformat.FormatDateTime(*j.StartedAt))
	}
	if j.FinishedAt != nil {
		fmt.Fprintf(&b, "finished: %s\n", timeformat.FormatDateTime(*j.FinishedAt))
	}
	if j.Error != "" {
		b.WriteString(styleErr.Render("error: "+j.Error) + "\n")
	}
	return b.String()
}

func renderWorkflowDetail(w datasource.Workflow) string {
	var b strings.Builder
	fmt.Fprintln(&b,
		styleHeader.Render("workflow")+" "+renderWorkflowName(w.Name))
	if w.IsSynthetic {
		fmt.Fprintln(&b, styleMuted.Render("synthetic single-step workflow"))
	}
	if w.Description != "" {
		fmt.Fprintln(&b, w.Description)
	}
	fmt.Fprintf(&b, "first step: %s\n", renderStepName(w.FirstStep))
	fmt.Fprintf(&b, "tasks: %d\n", w.TaskCount)
	b.WriteString(styleMuted.Render("─ steps ─") + "\n")
	for _, s := range w.Steps {
		// Format the next-target list with per-entry colouring: step
		// names go red; lifecycle terminals (done/cancelled/
		// human_feedback) take their task-status hue. workflow's own
		// step graph mixes both, so we colour each token before
		// joining instead of styling the joined string.
		var tokens []string
		for _, n := range s.NextSteps {
			tokens = append(tokens, renderStepName(n))
		}
		for _, st := range s.NextStatus {
			tokens = append(tokens, styleForTaskStatus(store.Status(st)).Render(st))
		}
		nexts := strings.Join(tokens, ", ")
		if nexts == "" {
			nexts = styleMuted.Render("(none)")
		}
		fmt.Fprintf(&b, "  %s  agent=%s  next=%s  tasks=%d\n",
			renderStepName(s.Name), renderAgentName(s.AgentName), nexts, s.TaskCount)
	}
	return b.String()
}

func renderAgentDetail(a datasource.Agent) string {
	var b strings.Builder
	fmt.Fprintln(&b,
		styleHeader.Render("agent")+" "+renderAgentName(a.Name)+
			"  "+styleMuted.Render("("+a.Source+")"))
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
	// Surface live-datasource hiccups (daemon was reachable but a
	// read errored and silently fell back to the DB). Compose tracks a
	// cumulative counter; we show a chip when it advances between
	// refresh ticks. The compare is guarded against underflow even
	// though the counter is monotonic; future refactors that reset it
	// (e.g. on reconnect) shouldn't make the chip blow up.
	if s.fallbacksNow > s.fallbacksLast {
		parts = append(parts, styleWarn.Render(fmt.Sprintf("flaky+%d", s.fallbacksNow-s.fallbacksLast)))
	}
	scopeStr := styleMuted.Render("—")
	if !s.scope.IsEmpty() {
		// Each scope chip wears the entity colour of its value (task /
		// workflow / agent). Labels stay default-coloured so the
		// status bar reads as a sequence of "label=<value>" pairs and
		// the values themselves are visually grouped with their panel
		// counterparts.
		var bits []string
		if s.scope.TaskID != "" {
			bits = append(bits, "task="+renderTaskID(s.scope.TaskID))
		}
		if s.scope.WorkflowName != "" {
			bits = append(bits, "wf="+renderWorkflowName(s.scope.WorkflowName))
		}
		if s.scope.Agent != "" {
			rel := s.scope.AgentRel.String()
			chip := "agent=" + renderAgentName(s.scope.Agent)
			if rel != "" {
				chip += " " + styleMuted.Render("("+rel+")")
			}
			bits = append(bits, chip)
		}
		scopeStr = strings.Join(bits, " ")
	}
	parts = append(parts, "scope: "+scopeStr)
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

// truncate crops s to at most n runes, appending an ellipsis. Counts
// runes, not bytes, so multibyte titles (e.g. Cyrillic) don't get
// sliced inside a codepoint. Doesn't account for east-asian wide
// glyphs that occupy 2 cells — acceptable for v1; revisit with
// golang.org/x/text/width if column alignment goes off.
func truncate(s string, n int) string {
	if n < 1 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	rs := []rune(s)
	return string(rs[:n-1]) + "…"
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
			// Smart timeline: today → time only, older → full datetime.
			stamp = timeformat.FormatDateTimeSmart(e.TS) + " "
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
	// Two lines: first line is "run <id-purple>  <status>", second
	// line is the tab strip. Lipgloss renders inline ANSI here (the
	// gocui view passes ANSI through). The label "run" stays bold-blue
	// so the pane header reads as a header; the id picks up styleJobID
	// to match the Jobs panel column.
	head := styleHeader.Render("run") + " " + renderJobID(insp.JobID)
	hdr := fmt.Sprintf("%s  %s%s\n%s", head, insp.Job.Status, stream, strings.Join(rendered, " "))
	return hdr
}
