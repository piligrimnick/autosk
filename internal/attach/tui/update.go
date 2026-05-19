package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/client"
)

// ----- tea.Msg union ---------------------------------------------------

// streamEventMsg wraps one decoded SSE event from the daemon. The
// stream goroutine in run.go funnels client.Event values into this
// message kind via tea.Cmd.
type streamEventMsg struct {
	ev client.Event
}

// streamClosedMsg signals that the SSE producer has shut down. The
// daemon hangs up after sending a `done` event or when the operator
// detaches. The model uses this to decide whether to quit.
type streamClosedMsg struct{}

// inputResultMsg is the response from a /v1/jobs/{id}/input round
// trip. Non-nil err is rendered into the status bar as a flash.
type inputResultMsg struct {
	resp api.InputResponse
	err  error
}

// abortResultMsg is the response from /v1/jobs/{id}/abort.
type abortResultMsg struct {
	resp api.AbortResponse
	err  error
}

// ----- Update ---------------------------------------------------------

// Update is the Bubble Tea reducer. Order of cases matters:
//  1. tea.WindowSizeMsg first so subviews see the new size.
//  2. our domain msgs (stream + RPC responses).
//  3. tea.KeyMsg, with the bindings that fire RPCs against the
//     daemon. Anything we don't handle is forwarded to the textarea.
//
// The function returns the next model and a tea.Cmd. We never call
// tea.Batch with more than 2 commands in one tick (current model
// state shouldn't need it).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case streamEventMsg:
		return m.onStreamEvent(msg.ev)
	case streamClosedMsg:
		// Daemon closed the stream. Three cases:
		//
		//   a) We attached to an already-terminal run for read-only
		//      inspection (terminalAtConnect=true). Stay in the TUI so
		//      the operator can scroll the transcript; they exit with
		//      Ctrl-C. Without this branch the program would flash and
		//      disappear behind AltScreen restore, leaving nothing for
		//      the operator to see.
		//
		//   b) The run was alive when we attached and then went
		//      terminal during the session: auto-quit so an agent
		//      finishing its workflow doesn't strand the operator in a
		//      dead TUI. Run() prints a one-line summary on stderr
		//      after tea exits.
		//
		//   c) The stream dropped mid-flight on a still-alive run
		//      (network blip, daemon restart, etc.). We don't reconnect
		//      in v1; surface a flash and let the operator decide.
		if m.job != nil && isTerminal(m.job.Status) {
			if m.terminalAtConnect {
				m.addFlash("run is " + m.job.Status + " — read-only; ctrl-c to exit")
				return m, nil
			}
			m.quitting = true
			return m, tea.Quit
		}
		m.addFlash("stream closed")
		return m, nil
	case inputResultMsg:
		return m.onInputResult(msg), nil
	case abortResultMsg:
		return m.onAbortResult(msg), nil
	case tea.KeyMsg:
		return m.onKey(msg)
	}

	// Forward to subviews (mouse wheel, focus events, etc.).
	var taCmd, vpCmd tea.Cmd
	m.textarea, taCmd = m.textarea.Update(msg)
	m.viewport, vpCmd = m.viewport.Update(msg)
	return m, tea.Batch(taCmd, vpCmd)
}

// onStreamEvent applies one decoded SSE event to the model.
//
// Streaming flag: the canonical signal is api.JobResponse.Streaming
// on a status frame (the daemon samples *pi.Runner.IsStreaming()).
// We DON'T flip m.streaming based on transcript-event kind anymore
// because the daemon now emits a status frame whenever pi's
// streaming state changes (see internal/daemon/server/sse.go's
// prevStreaming guard) — the heuristic only existed to paper over
// the missing signal.
func (m Model) onStreamEvent(ev client.Event) (tea.Model, tea.Cmd) {
	switch ev.Type {
	case client.EventTypeMessage:
		if ev.Message != nil {
			m.pushEvent(ev.EventID, *ev.Message)
		}
	case client.EventTypeStatus:
		if ev.Status != nil {
			m.applyStatus(*ev.Status)
		}
	case client.EventTypeDone:
		if ev.Status != nil {
			m.applyStatus(*ev.Status)
		}
		// Don't quit here — let streamClosedMsg do it after the
		// producer goroutine actually exits.
	case client.EventTypeError:
		if ev.Err != nil {
			m.addFlash("stream error: " + truncate(ev.Err.Error(), 80))
		}
	case client.EventTypeUnknown:
		// Surface unknown frames as a faint trace line so they're
		// not silently dropped (useful when the daemon adds a new
		// event type before this client knows about it).
		m.addFlash("unknown event: " + truncate(ev.Raw, 80))
	}
	return m, nil
}

// onInputResult folds the daemon's input-dispatch decision back into
// the status-bar flash. Clearing the textarea is owned by the caller
// (onKey) so this function stays pure.
func (m Model) onInputResult(msg inputResultMsg) Model {
	if msg.err != nil {
		var ae *client.APIError
		if errors.As(msg.err, &ae) {
			m.addFlash(fmt.Sprintf("input rejected (HTTP %d): %s", ae.Status, truncate(ae.Message, 80)))
		} else {
			m.addFlash("input error: " + truncate(msg.err.Error(), 80))
		}
		return m
	}
	m.addFlash("dispatched: " + msg.resp.Dispatched)
	return m
}

func (m Model) onAbortResult(msg abortResultMsg) Model {
	if msg.err != nil {
		var ae *client.APIError
		if errors.As(msg.err, &ae) {
			m.addFlash(fmt.Sprintf("abort rejected (HTTP %d): %s", ae.Status, truncate(ae.Message, 80)))
		} else {
			m.addFlash("abort error: " + truncate(msg.err.Error(), 80))
		}
		return m
	}
	if msg.resp.OK {
		m.addFlash("abort sent")
	}
	return m
}

// onKey dispatches keystrokes to either the daemon (send / follow_up
// / abort / quit) or the textarea (everything else, incl. Enter for
// newline insertion).
func (m Model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.quitting = true
		return m, tea.Quit
	case key.Matches(msg, m.keys.Send):
		if m.runIsTerminal() {
			m.addFlash("run is " + m.job.Status + "; cannot send input")
			return m, nil
		}
		return m.dispatchInput("")
	case key.Matches(msg, m.keys.FollowUp):
		if m.runIsTerminal() {
			m.addFlash("run is " + m.job.Status + "; cannot send input")
			return m, nil
		}
		return m.dispatchInput("follow_up")
	case key.Matches(msg, m.keys.Abort):
		if m.runIsTerminal() {
			m.addFlash("run is " + m.job.Status + "; nothing to abort")
			return m, nil
		}
		return m, sendAbortCmd(m.client, m.jobID)
	case key.Matches(msg, m.keys.ScrollUp):
		m.viewport.LineUp(1)
		return m, nil
	case key.Matches(msg, m.keys.ScrollDn):
		m.viewport.LineDown(1)
		return m, nil
	case key.Matches(msg, m.keys.PageUp):
		m.viewport.HalfViewUp()
		return m, nil
	case key.Matches(msg, m.keys.PageDn):
		m.viewport.HalfViewDown()
		return m, nil
	}
	// Forward to textarea (incl. Enter → newline).
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

// dispatchInput pulls the textarea buffer and fires SendInput against
// the daemon, returning a tea.Cmd that resolves to inputResultMsg.
// The textarea is cleared optimistically — if the daemon rejects the
// message, the flash will surface the error and the operator can
// retype.
func (m Model) dispatchInput(behavior string) (tea.Model, tea.Cmd) {
	text := m.textarea.Value()
	if text == "" {
		m.addFlash("nothing to send")
		return m, nil
	}
	m.textarea.Reset()
	return m, sendInputCmd(m.client, m.jobID, text, behavior)
}

// ----- tea.Cmd factories ----------------------------------------------

// sendInputCmd wraps Client.SendInput in a tea.Cmd. The returned
// command runs on Bubble Tea's IO goroutine, so doing blocking I/O
// here is fine.
func sendInputCmd(c *client.Client, jobID, message, behavior string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := c.SendInput(ctx, jobID, message, behavior)
		return inputResultMsg{resp: resp, err: err}
	}
}

// sendAbortCmd wraps Client.Abort in a tea.Cmd.
func sendAbortCmd(c *client.Client, jobID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := c.Abort(ctx, jobID)
		return abortResultMsg{resp: resp, err: err}
	}
}

// waitForStreamCmd returns a tea.Cmd that blocks on the next event
// off ch and forwards it as streamEventMsg. The Cmd re-schedules
// itself in run.go's pump goroutine — the model only sees one tick
// per event so the reducer stays single-stepped.
func waitForStreamCmd(ch <-chan client.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamClosedMsg{}
		}
		return streamEventMsg{ev: ev}
	}
}
