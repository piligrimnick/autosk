package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/client"
)

// keyMap pins the TUI's keyboard shortcuts. Exposed as a struct (vs.
// constants) so the model.View() can render the help line off the
// same source of truth and so tests can match keys by Help().Key.
type keyMap struct {
	Send     key.Binding
	FollowUp key.Binding
	Abort    key.Binding
	Quit     key.Binding
	ScrollUp key.Binding
	ScrollDn key.Binding
	PageUp   key.Binding
	PageDn   key.Binding
}

// defaultKeyMap mirrors the bindings the user picked in the design
// interview. Enter inserts a newline (textarea default); the explicit
// bindings below trigger the daemon round-trips.
//
//	ctrl+d           — send (daemon decides prompt|steer)
//	ctrl+f           — send as follow_up
//	ctrl+a           — abort the in-flight pi turn
//	ctrl+c, ctrl+q   — quit (closes SSE → daemon detaches)
//
// We use Ctrl-* because Bubble Tea / terminals don't reliably pass
// Alt/Meta combos. macOS Terminal in particular eats Alt by default.
//
// Note on Ctrl-Enter: the original spec listed "Ctrl-D / Ctrl-Enter"
// as the send chord, but most terminals (macOS Terminal, iTerm2 in
// default config, the linux console) do NOT differentiate Ctrl-Enter
// from a plain Enter — they emit the same \r byte. Bubble Tea would
// happily accept the binding, but it would fire essentially never,
// and where it did fire it'd surprise the user whose Enter just
// happens to send. We dropped it deliberately; Ctrl-D is the one
// chord we expect to work on every terminal the TUI is likely to
// run on. If a future terminal-specific binding is desired (e.g.
// kitty's CSI-u mode), it can be added here without breaking the
// existing contract.
func defaultKeyMap() keyMap {
	return keyMap{
		Send:     key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("ctrl+d", "send")),
		FollowUp: key.NewBinding(key.WithKeys("ctrl+f"), key.WithHelp("ctrl+f", "send as follow_up")),
		Abort:    key.NewBinding(key.WithKeys("ctrl+a"), key.WithHelp("ctrl+a", "abort turn")),
		Quit:     key.NewBinding(key.WithKeys("ctrl+c", "ctrl+q"), key.WithHelp("ctrl+c", "quit")),
		ScrollUp: key.NewBinding(key.WithKeys("ctrl+k"), key.WithHelp("ctrl+k", "scroll up")),
		ScrollDn: key.NewBinding(key.WithKeys("ctrl+j"), key.WithHelp("ctrl+j", "scroll down")),
		PageUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "page up")),
		PageDn:   key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("pgdown", "page down")),
	}
}

// Model is the Bubble Tea root model for `autosk attach`. Field
// visibility is package-private so the API is the Run function in
// run.go; tests in this package can still poke at the model.
type Model struct {
	// dependencies
	client *client.Client
	jobID  string
	// composable subviews
	viewport viewport.Model
	textarea textarea.Model
	// view config
	keys   keyMap
	styles styles
	width  int
	height int
	// stream-derived state
	job               *api.JobResponse
	streaming         bool
	events            []renderedEvent
	eventCount        int // total transcript events observed (incl. those renderEvent drops)
	flash             string
	flashUntil        time.Time
	terminalAtConnect bool // true if the very first status snapshot was already terminal
	seenStatus        bool // pinned once we receive the initial status event
	// lifecycle
	quitting bool
}

// TerminalAtConnect reports whether the operator attached to an
// already-terminal run. Run() consults this after the TUI exits to
// pick a sensible stderr summary line.
func (m Model) TerminalAtConnect() bool { return m.terminalAtConnect }

// EventCount reports the total number of transcript events observed
// from the stream, including kinds that renderEvent intentionally
// drops (model_change/compaction/session/empty/etc.). Run() uses it
// for the post-mortem summary so the number reflects the real
// session length, not the count of visually-rendered blocks.
func (m Model) EventCount() int { return m.eventCount }

// Job returns the latest api.JobResponse known to the model, or nil
// if no status frame ever arrived. Exposed for the Run() post-mortem
// summary.
func (m Model) Job() *api.JobResponse {
	if m.job == nil {
		return nil
	}
	cp := *m.job
	return &cp
}

// runIsTerminal reports whether the latest known status is one of
// done/failed/cancelled. Used to short-circuit input dispatch so the
// operator's keystrokes don't 409 against the daemon.
func (m Model) runIsTerminal() bool {
	return m.job != nil && isTerminal(m.job.Status)
}

// renderedEvent is a cache entry: one rendered transcript block
// (pre-styled string) tied to the SSE event id that produced it.
// The raw MessageEvent is kept so the model can re-render on resize.
type renderedEvent struct {
	id    int
	block string
	raw   *api.MessageEvent
}

// New constructs a fresh Model. Bubble Tea programs are typically
// built with tea.NewProgram(m); see Run() for the wired-up entry.
func New(c *client.Client, jobID string) Model {
	ta := textarea.New()
	ta.Placeholder = "type a message · ctrl+d send · ctrl+f follow_up · ctrl+a abort · ctrl+c quit"
	ta.Prompt = "▎ "
	ta.CharLimit = 0
	ta.SetWidth(80)
	ta.SetHeight(4)
	ta.ShowLineNumbers = false
	// textarea owns its own Enter binding (newline) — we don't bind
	// Enter on the parent model, so multi-line input "just works".
	ta.Focus()

	vp := viewport.New(80, 16)
	vp.SetContent("")

	return Model{
		client:   c,
		jobID:    jobID,
		viewport: vp,
		textarea: ta,
		keys:     defaultKeyMap(),
		styles:   defaultStyles(),
		width:    80,
		height:   24,
	}
}

// Init is the Bubble Tea entry point. We don't kick off the SSE
// stream here because Run() needs to plumb the StreamHandle.Events
// channel into a tea.Cmd factory that lives on the model. Init only
// has to return nil for the initial render.
func (m Model) Init() tea.Cmd {
	return textarea.Blink
}

// View composes the three regions: scrollback + input + status bar.
// Layout is straightforward column-major; word-wrap happens inside
// renderEvent and the textarea handles its own width.
func (m Model) View() string {
	if m.quitting {
		// Print one last frame so the user's terminal isn't blank.
		return ""
	}
	// Layout is driven by resize() on tea.WindowSizeMsg; View() does
	// not resize the viewport itself (an earlier draft had a no-op
	// branch here and was a maintenance tax — see review comment on
	// model.go:170).
	transcript := m.viewport.View()
	input := m.textarea.View()
	flash := m.activeFlash()
	status := renderStatusBar(m.styles, m.width, m.job, m.jobID, m.streaming, flash)

	return lipgloss.JoinVertical(lipgloss.Left, transcript, input, status)
}

// activeFlash returns m.flash if it's still within the show-deadline,
// otherwise "". Used by View() to fade ephemeral messages without
// touching the model from a timer.
func (m Model) activeFlash() string {
	if m.flash == "" {
		return ""
	}
	if time.Now().After(m.flashUntil) {
		return ""
	}
	return m.flash
}

// rebuildViewport regenerates the viewport's content from m.events.
// We rebuild on every event arrival rather than appending: it keeps
// the layout calculation honest about width changes and is O(N) for
// the typical conversation sizes (≤ a few hundred blocks). For very
// long runs the rebuild can be made incremental later.
func (m *Model) rebuildViewport() {
	if len(m.events) == 0 {
		m.viewport.SetContent(m.styles.muted.Render("(no transcript yet)"))
		return
	}
	parts := make([]string, 0, len(m.events))
	for _, e := range m.events {
		parts = append(parts, e.block)
	}
	m.viewport.SetContent(strings.Join(parts, "\n\n"))
	m.viewport.GotoBottom()
}

// resize reflows the three regions for the given terminal size and
// re-rebuilds the viewport content (because wrap-width changes).
func (m *Model) resize(w, h int) {
	m.width = w
	m.height = h
	m.textarea.SetWidth(w)
	statusH := 1
	inputH := m.textarea.Height() + 1
	vpH := h - statusH - inputH
	if vpH < 4 {
		vpH = 4
	}
	m.viewport.Width = w
	m.viewport.Height = vpH
	// Re-render events with the new wrap width.
	m.rerenderAll()
}

// rerenderAll re-renders each cached event's MessageEvent at the
// current viewport width. We keep the raw payload alongside the
// styled block on every renderedEvent so resize can reflow without
// re-fetching from the daemon.
func (m *Model) rerenderAll() {
	for i := range m.events {
		if m.events[i].raw != nil {
			m.events[i].block = renderEvent(m.styles, *m.events[i].raw, m.width-2)
		}
	}
	m.rebuildViewport()
}

// newRenderedEvent is the canonical constructor for the scrollback
// cache. It keeps the raw event so resize-induced re-renders don't
// have to re-derive style from a styled string.
func newRenderedEvent(id int, block string, raw *api.MessageEvent) renderedEvent {
	return renderedEvent{id: id, block: block, raw: raw}
}

// addFlash sets a one-shot status overlay (e.g. "dispatched: steer")
// that View() displays for the next ~2 seconds.
func (m *Model) addFlash(s string) {
	m.flash = s
	m.flashUntil = time.Now().Add(2 * time.Second)
}

// pushEvent appends a fully-rendered event to the scrollback and
// reflows the viewport. The event count is bumped regardless of
// whether renderEvent produced a visible block — empty-render kinds
// (model_change, compaction, session, …) are still part of the
// transcript and should be counted by EventCount.
func (m *Model) pushEvent(id int, ev api.MessageEvent) {
	m.eventCount++
	block := renderEvent(m.styles, ev, m.width-2)
	if block == "" {
		return
	}
	raw := ev // capture a copy
	m.events = append(m.events, newRenderedEvent(id, block, &raw))
	m.rebuildViewport()
}

// applyStatus folds an api.JobResponse into the model.
//
// The first status snapshot we ever receive pins terminalAtConnect:
// when it's already terminal, we treat the session as a historical
// read-only inspection (no auto-quit when the stream closes). When
// the run goes terminal mid-session we keep the auto-quit behaviour
// so an agent finishing its workflow doesn't strand the operator in
// a dead TUI.
//
// Streaming: the daemon authoritatively flips JobResponse.Streaming
// based on pi's IsStreaming() (see internal/daemon/server/sse.go).
// We use that exclusively — there used to be an event-kind heuristic
// in onStreamEvent that flipped on assistant_text and never reliably
// went out; the daemon-sourced signal replaces it entirely.
//
// Note on the pre-status window: between stream-open and the first
// status event, runIsTerminal() returns false and seenStatus is
// false. Input dispatch in that window is allowed — the daemon will
// 409 on terminal jobs anyway, and the window is small (one HTTP
// response burst). We do not gate dispatch on seenStatus to keep
// the keystroke latency unconditional.
func (m *Model) applyStatus(s api.JobResponse) {
	if !m.seenStatus {
		m.seenStatus = true
		m.terminalAtConnect = isTerminal(s.Status)
	}
	m.job = &s
	m.streaming = s.Streaming
	if isTerminal(s.Status) {
		m.streaming = false
	}
}

// derivedHelpLine renders the keybinding cheatsheet in two columns.
// Not used today (we keep the status bar dense) but plumbed so a
// future "?" toggle can surface it without re-deriving the keys.
//
//nolint:unused // referenced by future help overlay.
func (m Model) derivedHelpLine() string {
	parts := []string{
		fmt.Sprintf("%s send", m.keys.Send.Help().Key),
		fmt.Sprintf("%s follow_up", m.keys.FollowUp.Help().Key),
		fmt.Sprintf("%s abort", m.keys.Abort.Help().Key),
		fmt.Sprintf("%s quit", m.keys.Quit.Help().Key),
	}
	return strings.Join(parts, "  ")
}


