package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"autosk/internal/daemon/runstore"
	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/markdown"
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
	styleStatusNew    lipgloss.Style
	styleStatusWork   lipgloss.Style
	styleStatusHuman  lipgloss.Style
	styleStatusDone   lipgloss.Style
	styleStatusCancel lipgloss.Style

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
//
// Also invalidates the cached glamour renderer in lazy/markdown so a
// theme swap flips both caches in lockstep. The renderer is rebuilt
// lazily on the next markdown.Render call against the new palette.
func RebuildStyles() {
	markdown.RebuildStyles()
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
	//	new     cyan   (Scope hue, non-bold)
	//	work    purple (PopupBox hue)
	//	human   yellow (Warn hue — shares the workflow entity colour on
	//	               purpose: a row waiting on an operator sits in
	//	               the same visual category as the workflow it's
	//	               pinned to)
	//	done    green  (OK)
	//	cancel  gray   (Muted)
	styleStatusNew = lipgloss.NewStyle().Foreground(p.Scope.Lipgloss())
	styleStatusWork = lipgloss.NewStyle().Foreground(p.PopupBox.Lipgloss())
	styleStatusHuman = lipgloss.NewStyle().Foreground(p.Warn.Lipgloss())
	styleStatusDone = lipgloss.NewStyle().Foreground(p.OK.Lipgloss())
	styleStatusCancel = lipgloss.NewStyle().Foreground(p.Muted.Lipgloss())

	// Per-entity colour styles. See the var block above for the
	// rationale. None of these are bold: the column position and the
	// hue itself already do the work — bolding a Cyrillic title in
	// addition would only make the row noisier.
	//
	// Step + work status share the PopupBox hue on purpose:
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

// (renderSignalTarget lives in inspector.go and is reused here.)

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
	case store.StatusWork:
		return styleStatusWork
	case store.StatusHuman:
		return styleStatusHuman
	case store.StatusDone:
		return styleStatusDone
	case store.StatusCancel:
		return styleStatusCancel
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
	case runstore.StatusCancel:
		return "‣ cancel"
	}
	return j.Status
}

// statusColumnWidth caps the rendered status string at this many
// display cells. Every status enum value is at most 6 cells wide
// ("cancel"), so the column never has to ellipsis-truncate; the
// cap is the visual budget the panel reserves for the column. The
// title column always starts at the same x regardless of which
// enum value the row carries.
const statusColumnWidth = 6

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
//   - any other job (queued / done / failed / cancel) → a static
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
//
// width is the inner cell width of the detail view (innerWidth of
// winDetail). It's forwarded to renderTaskDetail / renderWorkflowDetail
// so the markdown render of task.Description / workflow.Description /
// comment.Text can wrap at the pane's width. A width of 0 means the
// gocui view hasn't been sized yet (first layout pass before the
// Manager wires the view); markdown.Render handles that by emitting
// the raw text instead of running glamour at width=0.
func renderDetail(s *state, width int) string {
	switch s.focused {
	case panelTasks:
		if t, ok := s.selectedTask(); ok {
			return renderTaskDetail(t, s.comments[t.ID], s.signals[t.ID], width)
		}
	case panelJobs:
		if j, ok := s.selectedJob(); ok {
			return renderJobDetail(j)
		}
	case panelWorkflows:
		if w, ok := s.selectedWorkflow(); ok {
			return renderWorkflowDetail(w, width)
		}
	case panelAgents:
		if a, ok := s.selectedAgent(); ok {
			return renderAgentDetail(a)
		}
	}
	return styleMuted.Render("(nothing selected)")
}

func renderTaskDetail(t datasource.Task, comments []datasource.Comment, signals []datasource.Signal, width int) string {
	var b strings.Builder

	// Header row: P{prio} <id> <status> <wf:step> <agent>.
	//
	// No "task" label literal — the gocui frame title ("[0] Detail")
	// already identifies the pane, and the row reads as a single
	// scan line keyed on the id. Priority is muted-gray to match the
	// Tasks panel column-1 styling so the two views speak the same
	// visual language. workflow:step is composed by
	// renderWorkflowStep (yellow / colon / purple) and the agent
	// picks up its entity hue. Trailing tokens are skipped when
	// empty so a task without a workflow doesn't render "(no-wf)"
	// at the top of every pane open.
	hdr := []string{
		styleMuted.Render(fmt.Sprintf("P%d", t.Priority)),
		renderTaskID(t.ID),
		styleForTaskStatus(t.Status).Render(string(t.Status)),
	}
	if t.WorkflowName != "" {
		hdr = append(hdr, renderWorkflowStep(t.WorkflowName, t.StepName))
	}
	if t.AgentName != "" {
		hdr = append(hdr, renderAgentName(t.AgentName))
	}
	b.WriteString(strings.Join(hdr, " ") + "\n")

	// Optional "blocked by" row: only when the task is actually
	// blocked AND we have ids to point at. The word "blocked" wears
	// styleErr (red) so a blocked task screams at the operator from
	// the top of the pane; the ids themselves stay in the task-id
	// hue for cross-pane consistency.
	if t.Blocked && len(t.BlockedBy) > 0 {
		ids := make([]string, 0, len(t.BlockedBy))
		for _, id := range t.BlockedBy {
			ids = append(ids, renderTaskID(id))
		}
		b.WriteString(styleErr.Render("blocked") + " by: " + strings.Join(ids, ", ") + "\n")
	}

	// Stats row: created + comment count. FormatDateTimeSmart drops
	// the date for today's events so the common case stays compact
	// ("created: 14:31:47") and only older tasks render the full
	// "YYYY-MM-DD HH:MM:SS". Machine-facing wire formats stay on
	// RFC3339 UTC and intentionally do NOT route through timeformat.
	fmt.Fprintf(&b, "created: %s, comments: %d\n",
		timeformat.FormatDateTimeSmart(t.CreatedAt), t.CommentCount)
	if t.AuthorName != "" {
		fmt.Fprintf(&b, "author: %s\n", renderAgentName(t.AuthorName))
	}

	// First-frame fallback: when innerWidth returns 0 (the gocui view
	// hasn't been sized yet) we skip the box-drawing entirely and
	// fall back to plain-text sections so the pane still renders
	// something useful. markdown.Render also short-circuits at
	// width<=0, so the description leaks through verbatim — exercised
	// by TestRenderTaskDetail_ZeroWidth.
	if width <= 0 {
		if t.Title != "" {
			b.WriteString("\n" + t.Title + "\n")
		}
		if t.Description != "" {
			b.WriteString("\n" + markdown.Render(t.Description, 0) + "\n")
		} else {
			b.WriteString("\n" + styleMuted.Render("(no description)") + "\n")
		}
		for _, c := range tailComments(comments) {
			if strings.TrimSpace(c.Text) == "" {
				continue
			}
			fmt.Fprintf(&b, "\n%s %s\n%s\n",
				timeformat.FormatDateTimeSmart(c.CreatedAt),
				renderAgentName(c.AuthorName), c.Text)
		}
		return b.String()
	}

	// Box content width: total outer width minus 2 frame cells minus
	// 2 cells of inner padding (one each side). Floor at 1 so a
	// narrow but non-zero pane still feeds markdown.Render a usable
	// wrap width instead of degenerating into raw passthrough.
	contentW := width - 4
	if contentW < 1 {
		contentW = 1
	}

	// ── Title box ────────────────────────────────────────────────
	titleBody := wrapPlain(t.Title, contentW)
	if titleBody == "" {
		titleBody = styleMuted.Render("(untitled)")
	}
	b.WriteString("\n" + drawLabeledBox(styleMuted.Render("Title"), titleBody, width) + "\n")

	// ── Description box ──────────────────────────────────────────
	var descBody string
	if t.Description == "" {
		// Acceptance criterion #4: "(no description)" placeholder is
		// pinned verbatim. Routing the empty string through glamour
		// would just give us a blank box body.
		descBody = styleMuted.Render("(no description)")
	} else {
		descBody = markdown.Render(t.Description, contentW)
	}
	b.WriteString("\n" + drawLabeledBox(styleMuted.Render("Description"), descBody, width) + "\n")

	// ── Per-comment boxes ────────────────────────────────────────
	// Each comment is its own labeled box; the label is
	// "<smart-time> <author>" where smart-time drops the date for
	// today's events and the author wears its agent hue. The "last
	// 5" cap is preserved so long comment chains stay browsable
	// from the Tasks panel without burying the rest of the pane.
	// Empty-body comments are skipped entirely — an empty box would
	// just be visual noise (autosk_comment rejects empty bodies
	// upstream so this is defensive).
	for _, c := range tailComments(comments) {
		if strings.TrimSpace(c.Text) == "" {
			continue
		}
		body := markdown.Render(c.Text, contentW)
		if body == "" {
			continue
		}
		label := timeformat.FormatDateTimeSmart(c.CreatedAt) +
			" " + renderAgentName(c.AuthorName)
		b.WriteString("\n" + drawLabeledBox(label, body, width) + "\n")
	}

	// ── Recent signals (N) box ───────────────────────────────────
	// Show ALL signals rather than the old last-3 cap: the operator
	// asked for the full kickback history visible at a glance, and
	// the box renders inside a scrollable pane so a tall list isn't
	// a usability problem. SignalsForTask returns newest-first.
	if len(signals) > 0 {
		lines := make([]string, 0, len(signals))
		for _, s := range signals {
			lines = append(lines, fmt.Sprintf("%s %s → %s",
				timeformat.FormatDateTimeSmart(s.CreatedAt),
				renderStepName(s.StepName), s.Target))
		}
		label := styleMuted.Render(fmt.Sprintf("Recent signals (%d)", len(signals)))
		b.WriteString("\n" + drawLabeledBox(label, strings.Join(lines, "\n"), width) + "\n")
	}

	return b.String()
}

// tailComments returns the trailing slice of comments to render —
// the design pins this at the last 5, so long chains stay browsable
// from the Tasks panel without taking over the pane.
func tailComments(cs []datasource.Comment) []datasource.Comment {
	const cap = 5
	if len(cs) <= cap {
		return cs
	}
	return cs[len(cs)-cap:]
}

// drawLabeledBox renders body inside a rounded box of the given
// outer width with label embedded in the top border:
//
//	╭─ Label ───────────────╮
//	│ body line 1           │
//	│ body line 2           │
//	╰───────────────────────╯
//
// width is the total OUTER width in cells (frame included). The
// frame (corners + sides + dashes) wears styleMuted so it reads as
// chrome, not content. label is passed already styled — callers
// hand in either a plain string (label width measured via
// lipgloss.Width) or a pre-styled value (e.g. an agent name in its
// entity hue for a comment header). body may carry ANSI escapes
// and newlines; each visible line is padded to the inner content
// width with plain spaces so the right border lines up regardless
// of SGR noise.
//
// width<6 collapses the call to a frame-less body — the layout's
// first-frame race (innerWidth==0) is already handled by
// renderTaskDetail one level up, but this defends against an
// impossibly narrow pane mid-resize.
func drawLabeledBox(label, body string, width int) string {
	if width < 6 {
		return body
	}
	inner := width - 2    // cells between │ and │
	contentW := inner - 2 // 1-cell padding on each side
	labelW := lipgloss.Width(label)

	var top string
	if labelW == 0 {
		top = styleMuted.Render("╭" + strings.Repeat("─", inner) + "╮")
	} else {
		// Layout: ╭─ <label> <N dashes> ╮ →
		// 1 + 1 + 1 + labelW + 1 + N + 1 = width → N = width-labelW-5.
		dashes := width - labelW - 5
		if dashes < 1 {
			dashes = 1
		}
		top = styleMuted.Render("╭─ ") + label + styleMuted.Render(" "+strings.Repeat("─", dashes)+"╮")
	}
	bottom := styleMuted.Render("╰" + strings.Repeat("─", inner) + "╯")

	// Render each body line padded to contentW visible cells. Lines
	// wider than contentW are emitted unpadded; the markdown / wrap
	// helpers feeding us are already sized to contentW, so overflow
	// only happens when an unwrappable token (long URL, code-fence
	// content) exceeds the budget. Better to let the terminal wrap
	// than to truncate authored content silently.
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	rendered := make([]string, 0, len(lines))
	for _, line := range lines {
		w := lipgloss.Width(line)
		pad := contentW - w
		if pad < 0 {
			pad = 0
		}
		rendered = append(rendered, styleMuted.Render("│ ")+line+strings.Repeat(" ", pad)+styleMuted.Render(" │"))
	}
	return top + "\n" + strings.Join(rendered, "\n") + "\n" + bottom
}

// wrapPlain word-wraps plain (ANSI-free) text s to at most width
// runes per line. Breaks on whitespace; words longer than width are
// hard-broken at the width boundary so the box never grows wider
// than its budget. Used by the Title box where the input is
// operator-authored plain text and markdown.Render would be
// overkill.
func wrapPlain(s string, width int) string {
	if width < 1 {
		return s
	}
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	var out []string
	var line []rune
	flush := func() {
		if len(line) > 0 {
			out = append(out, string(line))
			line = line[:0]
		}
	}
	for _, word := range strings.Fields(s) {
		rs := []rune(word)
		wLen := len(rs)
		if wLen > width {
			flush()
			for len(rs) > width {
				out = append(out, string(rs[:width]))
				rs = rs[width:]
			}
			line = append(line, rs...)
			continue
		}
		switch {
		case len(line) == 0:
			line = append(line, rs...)
		case len(line)+1+wLen <= width:
			line = append(line, ' ')
			line = append(line, rs...)
		default:
			flush()
			line = append(line, rs...)
		}
	}
	flush()
	return strings.Join(out, "\n")
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

func renderWorkflowDetail(w datasource.Workflow, width int) string {
	var b strings.Builder
	fmt.Fprintln(&b,
		styleHeader.Render("workflow")+" "+renderWorkflowName(w.Name))
	if w.IsSynthetic {
		fmt.Fprintln(&b, styleMuted.Render("synthetic single-step workflow"))
	}
	if w.Description != "" {
		// Workflow.Description is operator-authored markdown (same
		// shape as Task.Description). Render it through the same
		// glamour pipeline so headings / lists / code fences match
		// the visual language of the task pane.
		fmt.Fprintln(&b, markdown.Render(w.Description, width))
	}
	fmt.Fprintf(&b, "first step: %s\n", renderStepName(w.FirstStep))
	fmt.Fprintf(&b, "tasks: %d\n", w.TaskCount)
	b.WriteString(styleMuted.Render("─ steps ─") + "\n")
	for _, s := range w.Steps {
		// Format the next-target list with per-entry colouring: step
		// names go red; lifecycle terminals (done/cancel/
		// human) take their task-status hue. workflow's own
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
