// Package tui hosts the Bubble Tea client for `autosk attach`.
//
// The TUI is fed exclusively from the daemon's SSE stream
// (api.MessageEvent for transcript frames, api.JobResponse for
// status/done frames) — there is no local pi process, no extension,
// no second writer to the session file. See docs/attach.md for the
// operator manual and docs/plans/20260519-Attach-Plan-v2.md for the
// locked design decisions (the v1 plan at
// docs/plans/20260519-Attach-Plan.md is preserved for the paper
// trail).
//
// This file owns the rendering layer: turning one api.MessageEvent
// into a coloured terminal block. Layout (transcript viewport +
// textarea + status bar) lives in model.go; key bindings in
// update.go; the SSE pump in run.go.
package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"autosk/internal/daemon/api"
)

// styles is the single source of theme tokens for the TUI. Kept on
// the package level (not the model) so the test helpers can render
// blocks without bringing up a full tea.Program.
type styles struct {
	user         lipgloss.Style
	assistant    lipgloss.Style
	thinking     lipgloss.Style
	toolName     lipgloss.Style
	toolArgs     lipgloss.Style
	toolResultOK lipgloss.Style
	toolResultEr lipgloss.Style
	muted        lipgloss.Style
	dim          lipgloss.Style
	ts           lipgloss.Style
	statusBar    lipgloss.Style
	streaming    lipgloss.Style
	idle         lipgloss.Style
	terminal     lipgloss.Style
}

// defaultStyles returns a sensible colour theme. Adaptive colours so
// the TUI works on both light and dark terminals.
func defaultStyles() styles {
	return styles{
		user:         lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#6f42c1", Dark: "#c792ea"}),
		assistant:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#005cc5", Dark: "#82aaff"}),
		thinking:     lipgloss.NewStyle().Italic(true).Foreground(lipgloss.AdaptiveColor{Light: "#6a737d", Dark: "#7e848d"}),
		toolName:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#22863a", Dark: "#85e89d"}),
		toolArgs:     lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#586069", Dark: "#959da5"}),
		toolResultOK: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#22863a", Dark: "#85e89d"}),
		toolResultEr: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#d73a49", Dark: "#f97583"}),
		muted:        lipgloss.NewStyle().Faint(true).Foreground(lipgloss.AdaptiveColor{Light: "#6a737d", Dark: "#7e848d"}),
		dim:          lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#959da5", Dark: "#6a737d"}),
		ts:           lipgloss.NewStyle().Faint(true).Foreground(lipgloss.AdaptiveColor{Light: "#959da5", Dark: "#6a737d"}),
		statusBar:    lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#24292e", Dark: "#e1e4e8"}).Background(lipgloss.AdaptiveColor{Light: "#e1e4e8", Dark: "#2f363d"}).Padding(0, 1),
		streaming:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#22863a", Dark: "#85e89d"}),
		idle:         lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#6a737d", Dark: "#7e848d"}),
		terminal:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#d73a49", Dark: "#f97583"}),
	}
}

// renderEvent turns one daemon transcript event into a block of
// styled text suitable for the viewport. width is the wrap-target;
// values < 20 disable wrapping (tests don't care about visual layout).
//
// Visibility breakdown of the daemon transcript event kinds:
//
//   - The five message-bearing kinds — user_text, assistant_text,
//     assistant_thinking, tool_call, tool_result — render as full
//     coloured blocks.
//   - The nine metadata kinds — thinking_level_change, model_change,
//     compaction, branch_summary, session, session_info, label,
//     custom, custom_message — render as a muted "(kind)" placeholder.
//   - Any other (unknown) kind, including "other", falls through to
//     the default branch and renders as the dim raw payload, so the
//     viewport never silently loses an event when the daemon adds a
//     new kind ahead of the client.
//   - An empty Kind string is the only case that produces an empty
//     block; pushEvent treats that as "do not append to the
//     viewport" (but still bumps eventCount for the post-mortem
//     summary).
func renderEvent(s styles, ev api.MessageEvent, width int) string {
	switch ev.Kind {
	case "user_text":
		return renderUser(s, ev, width)
	case "assistant_text":
		return renderAssistant(s, ev, width)
	case "assistant_thinking":
		return renderThinking(s, ev, width)
	case "tool_call":
		return renderToolCall(s, ev, width)
	case "tool_result":
		return renderToolResult(s, ev, width)
	case "thinking_level_change", "model_change", "compaction", "branch_summary",
		"session", "session_info", "label", "custom", "custom_message":
		return s.muted.Render(fmt.Sprintf("(%s)", ev.Kind))
	case "":
		return ""
	default:
		// Unknown kinds: dump raw payload so nothing is silently lost.
		if ev.Raw != nil {
			if b, err := json.Marshal(ev.Raw); err == nil {
				return s.dim.Render(string(b))
			}
		}
		return s.dim.Render(fmt.Sprintf("(%s) %s", ev.Kind, ev.Text))
	}
}

// renderUser prints the user message in a bold accent colour with a
// timestamp gutter. Multi-line text is wrapped to width-margin.
func renderUser(s styles, ev api.MessageEvent, width int) string {
	header := s.user.Render("user  ") + " " + s.ts.Render(formatTS(ev.TS))
	body := wrap(ev.Text, width)
	return header + "\n" + body
}

func renderAssistant(s styles, ev api.MessageEvent, width int) string {
	header := s.assistant.Render("agent ") + " " + s.ts.Render(formatTS(ev.TS))
	body := wrap(ev.Text, width)
	return header + "\n" + body
}

func renderThinking(s styles, ev api.MessageEvent, width int) string {
	header := s.thinking.Render("…think") + " " + s.ts.Render(formatTS(ev.TS))
	body := s.thinking.Render(wrap(ev.Text, width))
	return header + "\n" + body
}

func renderToolCall(s styles, ev api.MessageEvent, width int) string {
	header := s.toolName.Render("tool→ ") + " " + s.ts.Render(formatTS(ev.TS)) + " " + s.toolName.Render(ev.Name)
	args := summariseInput(ev.Input)
	if args != "" {
		args = s.toolArgs.Render(wrap(args, width))
		return header + "\n" + args
	}
	return header
}

func renderToolResult(s styles, ev api.MessageEvent, width int) string {
	tag := s.toolResultOK.Render("ok")
	if ev.IsError {
		tag = s.toolResultEr.Render("ERR")
	}
	header := s.toolName.Render("tool←") + " " + s.ts.Render(formatTS(ev.TS)) + " " + tag + " " + s.toolName.Render(ev.Name)
	// We intentionally do NOT truncate the body here: long
	// tool_result payloads (e.g. compile errors, stack traces) are
	// exactly the case where the operator most wants the full text.
	// The viewport handles arbitrary length and the wrap helper
	// reflows on resize. Compare with renderUser/renderAssistant
	// which already never truncate — see review on render.go:170.
	if ev.Text == "" {
		return header
	}
	return header + "\n" + wrap(ev.Text, width)
}

// formatTS prints HH:MM:SS in the local zone; empty when ts is zero.
func formatTS(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.Local().Format("15:04:05")
}

// summariseInput returns a compact one-or-two-line representation of a
// tool's args. For maps it picks the first few entries and elides the
// rest; for everything else it falls back to JSON encoding.
func summariseInput(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case map[string]any:
		return summariseMap(x)
	case string:
		return x
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return truncate(string(b), 200)
	}
}

func summariseMap(m map[string]any) string {
	var parts []string
	for k, v := range m {
		var sv string
		switch x := v.(type) {
		case string:
			sv = x
		case json.RawMessage:
			sv = string(x)
		default:
			b, err := json.Marshal(v)
			if err != nil {
				sv = fmt.Sprintf("%v", v)
			} else {
				sv = string(b)
			}
		}
		parts = append(parts, k+"="+truncate(sv, 80))
		if len(parts) == 4 {
			parts = append(parts, "…")
			break
		}
	}
	return strings.Join(parts, " ")
}

// wrap is a small word-wrap helper. width<=0 disables wrapping.
func wrap(s string, width int) string {
	if width <= 0 || width >= 1<<20 {
		return s
	}
	if width < 20 {
		return s
	}
	var out strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(wrapLine(line, width))
	}
	return out.String()
}

func wrapLine(line string, width int) string {
	if len(line) <= width {
		return line
	}
	words := strings.Fields(line)
	if len(words) == 0 {
		return line
	}
	var (
		out  strings.Builder
		col  int
		head = true
	)
	for _, w := range words {
		if head {
			out.WriteString(w)
			col = len(w)
			head = false
			continue
		}
		if col+1+len(w) > width {
			out.WriteByte('\n')
			out.WriteString(w)
			col = len(w)
		} else {
			out.WriteByte(' ')
			out.WriteString(w)
			col += 1 + len(w)
		}
	}
	return out.String()
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return s[:n-1] + "…"
}

// renderStatusBar composes the bottom-of-screen status line. job is
// the latest api.JobResponse from the SSE stream; flash is an
// ephemeral one-shot message ("dispatched: steer", "abort sent") that
// rides on top of the static line for a few cycles.
func renderStatusBar(s styles, width int, job *api.JobResponse, jobID string, streaming bool, flash string) string {
	if job == nil {
		// Pre-status: only the job id is known.
		return s.statusBar.Width(width).Render(fmt.Sprintf("attach %s · connecting…", jobID))
	}
	streamLabel := s.idle.Render("idle")
	if streaming {
		streamLabel = s.streaming.Render("streaming")
	}
	terminalNote := ""
	if isTerminal(job.Status) {
		terminalNote = " · " + s.terminal.Render(job.Status)
	}
	left := fmt.Sprintf("attach %s · %s · %s · attached: %d · corrections: %d/%d%s",
		jobID, job.Status, streamLabel, job.AttachCount,
		job.CorrectionsUsed, job.MaxCorrections, terminalNote)
	right := flash
	gap := width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	line := left + strings.Repeat(" ", gap) + right
	return s.statusBar.Width(width).Render(line)
}

func isTerminal(status string) bool {
	switch status {
	case "done", "failed", "cancelled":
		return true
	default:
		return false
	}
}
