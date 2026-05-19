package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/client"
)

// Run is the public entry point invoked by `autosk attach`. It opens
// the SSE stream (with attach=true so the daemon's attach counter is
// incremented), constructs a Bubble Tea program around the Model,
// and pumps stream events into the model via tea.Msg.
//
// Run returns a non-nil error when:
//   - The initial stream open fails (e.g. unknown job / wrong cwd).
//     The error is surfaced verbatim so the caller can render it on
//     the operator's terminal before exit.
//   - The Bubble Tea program crashes. The model attempts to surface
//     terminal failures via the error bar, but a panic in a sub-view
//     bypasses that and percolates up here.
//
// On clean quit (Ctrl-C / terminal done / stream closed) Run returns
// nil. Closing the stream context is what releases the attach
// counter on the daemon side.
func Run(ctx context.Context, c *client.Client, jobID string) error {
	if c == nil {
		return errors.New("tui.Run: client is nil")
	}
	if jobID == "" {
		return errors.New("tui.Run: jobID is empty")
	}

	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	handle, err := c.Stream(streamCtx, jobID, client.StreamOptions{
		Attach: true,
		Full:   true,
	})
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	defer handle.Close()

	m := New(c, jobID)

	prog := tea.NewProgram(
		m,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
	)

	// Pump stream events into the program. We send one streamEventMsg
	// per decoded SSE frame; when the channel closes (daemon hangs
	// up or context cancels), send a final streamClosedMsg.
	go pumpStreamToProgram(handle.Events, prog)

	// Bubble Tea's program.Run() blocks until tea.Quit fires.
	final, err := prog.Run()
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	// After AltScreen restore the user is back at their old prompt
	// with no trace of the session. Print a one-line summary on
	// stderr so they have context for the exit, especially when the
	// auto-quit fired because the run went terminal mid-session or
	// when we attached to an already-terminal run for inspection.
	printExitSummary(os.Stderr, final, jobID)
	return nil
}

// printExitSummary writes a short context line so the operator's
// terminal doesn't "snap back to prompt" without explanation. The
// final model carries the last status snapshot.
//
// EventCount (not len(m.events)) is used here so the displayed total
// reflects the full transcript including kinds that renderEvent
// intentionally drops to empty (model_change, compaction, session,
// …) — see review on run.go:107.
func printExitSummary(w io.Writer, final tea.Model, jobID string) {
	m, ok := final.(Model)
	if !ok {
		return
	}
	job := m.Job()
	switch {
	case job == nil:
		fmt.Fprintf(w, "autosk attach %s: detached (no status received)\n", jobID)
	case isTerminal(job.Status) && m.TerminalAtConnect():
		// Read-only inspection: only happens via Ctrl-C, since
		// streamClosedMsg with terminalAtConnect doesn't auto-quit.
		fmt.Fprintf(w, "autosk attach %s: %s (read-only inspection — %d events)\n",
			jobID, summariseTerminal(job), m.EventCount())
	case isTerminal(job.Status):
		fmt.Fprintf(w, "autosk attach %s: run %s during session (%s)\n",
			jobID, job.Status, summariseTerminal(job))
	default:
		fmt.Fprintf(w, "autosk attach %s: detached (run was %s)\n", jobID, job.Status)
	}
}

// summariseTerminal renders the exit code + duration of a terminal
// run for the post-mortem stderr summary.
func summariseTerminal(j *api.JobResponse) string {
	parts := []string{}
	if j.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("exit %d", *j.ExitCode))
	}
	if j.DurationMS > 0 {
		parts = append(parts, time.Duration(j.DurationMS*int64(time.Millisecond)).Round(time.Second).String())
	}
	if j.Error != "" {
		parts = append(parts, "error: "+j.Error)
	}
	if len(parts) == 0 {
		return j.Status
	}
	return j.Status + " · " + strings.Join(parts, ", ")
}

// pumpStreamToProgram forwards every event off ch into the Bubble
// Tea program's message bus. The double goroutine (this pump + the
// tea.Cmd reader inside Bubble Tea) lets us deliver out-of-band
// events without blocking the model's update loop.
//
// We send messages via prog.Send rather than returning a tea.Cmd
// because Cmds are run on Bubble Tea's IO goroutine and a long-lived
// pump is exactly what Cmd-shaped APIs are not for.
func pumpStreamToProgram(ch <-chan client.Event, prog *tea.Program) {
	for ev := range ch {
		prog.Send(streamEventMsg{ev: ev})
	}
	prog.Send(streamClosedMsg{})
}
