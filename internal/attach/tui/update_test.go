package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/client"
)

// newTestModel returns a Model wired up against an httptest server
// whose handlers record the incoming requests. Tests assert against
// both the model state (after Update) and the recorded requests
// (after the tea.Cmd is invoked).
func newTestModel(t *testing.T, mux *http.ServeMux) (Model, func() *recording) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := client.New(client.Options{
		Sock:       "/unused",
		Cwd:        t.TempDir(),
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	// Replace the underlying transport so non-URL requests routed
	// through our Client.do build correctly: client.do is hardcoded
	// to "http://autosk" + path. With httptest we want the requests
	// to land on the test server, so we patch the http.Client's
	// Transport dialer to point everything at srv.URL.
	c.HTTPClient().Transport = &redirectTransport{base: srv.URL, inner: srv.Client().Transport}
	m := New(c, "job-test")
	m.resize(120, 30)
	rec := &recording{}
	mux.HandleFunc("POST /v1/jobs/{job_id}/input", func(w http.ResponseWriter, r *http.Request) {
		var req api.InputRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		rec.mu.Lock()
		rec.inputs = append(rec.inputs, req)
		rec.mu.Unlock()
		dispatched := "prompt"
		if req.StreamingBehavior == "follow_up" {
			dispatched = "follow_up"
		}
		_ = json.NewEncoder(w).Encode(api.InputResponse{
			JobID:      r.PathValue("job_id"),
			Dispatched: dispatched,
		})
	})
	mux.HandleFunc("POST /v1/jobs/{job_id}/abort", func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.aborts++
		rec.mu.Unlock()
		_ = json.NewEncoder(w).Encode(api.AbortResponse{JobID: r.PathValue("job_id"), OK: true})
	})
	return m, func() *recording { return rec }
}

type recording struct {
	mu     sync.Mutex
	inputs []api.InputRequest
	aborts int
}

// redirectTransport rewrites every request's URL onto the httptest
// base URL while preserving the path and query.
type redirectTransport struct {
	base  string
	inner http.RoundTripper
}

func (rt *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Re-target everything at the httptest server.
	newURL, err := req.URL.Parse(rt.base + req.URL.Path + "?" + req.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.URL = newURL
	clone.Host = newURL.Host
	return rt.inner.RoundTrip(clone)
}

// runCmd executes a tea.Cmd and returns the resulting tea.Msg. tests
// use this to drive a one-shot Cmd to completion without bringing up
// a tea.Program.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// keyFromRunes builds a tea.KeyMsg for a Ctrl-letter combo. Bubble
// Tea exposes the high-level tea.KeyCtrlD etc. constants; we use
// them where available.
func ctrlKey(r rune) tea.KeyMsg {
	switch r {
	case 'd':
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	case 'f':
		return tea.KeyMsg{Type: tea.KeyCtrlF}
	case 'a':
		return tea.KeyMsg{Type: tea.KeyCtrlA}
	case 'c':
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case 'q':
		return tea.KeyMsg{Type: tea.KeyCtrlQ}
	}
	return tea.KeyMsg{}
}

// TestUpdate_QuitOnCtrlC: Ctrl-C sets m.quitting and returns tea.Quit.
func TestUpdate_QuitOnCtrlC(t *testing.T) {
	m, _ := newTestModel(t, http.NewServeMux())
	out, cmd := m.Update(ctrlKey('c'))
	got := out.(Model)
	if !got.quitting {
		t.Fatal("quitting flag not set on Ctrl-C")
	}
	if cmd == nil {
		t.Fatal("Ctrl-C returned nil cmd; want tea.Quit")
	}
}

// TestUpdate_SendOnCtrlD: typing text + Ctrl-D fires SendInput and
// clears the textarea.
func TestUpdate_SendOnCtrlD(t *testing.T) {
	mux := http.NewServeMux()
	m, getRec := newTestModel(t, mux)
	m.textarea.SetValue("hello agent")

	out, cmd := m.Update(ctrlKey('d'))
	got := out.(Model)
	if got.textarea.Value() != "" {
		t.Fatalf("textarea not cleared after send: %q", got.textarea.Value())
	}
	if cmd == nil {
		t.Fatal("send returned nil cmd")
	}
	msg := runCmd(cmd)
	res, ok := msg.(inputResultMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want inputResultMsg", msg)
	}
	if res.err != nil {
		t.Fatalf("input err=%v", res.err)
	}
	if res.resp.Dispatched != "prompt" {
		t.Fatalf("Dispatched=%q want prompt (empty behavior on idle runner)", res.resp.Dispatched)
	}
	rec := getRec()
	if len(rec.inputs) != 1 || rec.inputs[0].Message != "hello agent" {
		t.Fatalf("recorded=%+v", rec.inputs)
	}
}

// TestUpdate_FollowUpOnCtrlF: Ctrl-F sets streamingBehavior=follow_up.
func TestUpdate_FollowUpOnCtrlF(t *testing.T) {
	mux := http.NewServeMux()
	m, getRec := newTestModel(t, mux)
	m.textarea.SetValue("after you finish")

	_, cmd := m.Update(ctrlKey('f'))
	msg := runCmd(cmd)
	res := msg.(inputResultMsg)
	if res.resp.Dispatched != "follow_up" {
		t.Fatalf("Dispatched=%q want follow_up", res.resp.Dispatched)
	}
	rec := getRec()
	if rec.inputs[0].StreamingBehavior != "follow_up" {
		t.Fatalf("behavior=%q", rec.inputs[0].StreamingBehavior)
	}
}

// TestUpdate_SendEmptyIsNoop: hitting Ctrl-D with an empty textarea
// must not hit the daemon and must surface a "nothing to send" flash.
func TestUpdate_SendEmptyIsNoop(t *testing.T) {
	mux := http.NewServeMux()
	m, getRec := newTestModel(t, mux)
	out, cmd := m.Update(ctrlKey('d'))
	got := out.(Model)
	if cmd != nil {
		t.Fatalf("expected nil cmd for empty send, got %T", cmd)
	}
	if !strings.Contains(got.flash, "nothing to send") {
		t.Fatalf("flash=%q", got.flash)
	}
	if rec := getRec(); len(rec.inputs) != 0 {
		t.Fatalf("rec=%+v want zero", rec.inputs)
	}
}

// TestUpdate_AbortOnCtrlA: Ctrl-A fires Abort and flashes "abort sent".
func TestUpdate_AbortOnCtrlA(t *testing.T) {
	mux := http.NewServeMux()
	m, getRec := newTestModel(t, mux)
	_, cmd := m.Update(ctrlKey('a'))
	msg := runCmd(cmd)
	res, ok := msg.(abortResultMsg)
	if !ok {
		t.Fatalf("cmd produced %T", msg)
	}
	if res.err != nil {
		t.Fatalf("abort err=%v", res.err)
	}
	if rec := getRec(); rec.aborts != 1 {
		t.Fatalf("aborts=%d", rec.aborts)
	}
}

// TestUpdate_StreamEvent_AppendsToScrollback: a streamEventMsg of
// EventTypeMessage adds a rendered block to m.events.
func TestUpdate_StreamEvent_AppendsToScrollback(t *testing.T) {
	m, _ := newTestModel(t, http.NewServeMux())
	m.resize(120, 30)
	out, _ := m.Update(streamEventMsg{ev: client.Event{
		Type:    client.EventTypeMessage,
		EventID: 1,
		Message: &api.MessageEvent{Kind: "user_text", Text: "hi", TS: time.Now()},
	}})
	got := out.(Model)
	if len(got.events) != 1 {
		t.Fatalf("events=%d", len(got.events))
	}
	if got.events[0].id != 1 {
		t.Fatalf("event id=%d", got.events[0].id)
	}
}

// TestUpdate_StatusEvent_StreamingFromDaemon: the daemon is the
// authoritative source for the live `streaming` indicator. Any
// status frame with Streaming=true must flip m.streaming on, and
// any subsequent status frame with Streaming=false must flip it
// back off (e.g. when pi emits agent_end between turns). This
// replaces the legacy event-kind heuristic that lit up on
// assistant_text and never reliably went out.
func TestUpdate_StatusEvent_StreamingFromDaemon(t *testing.T) {
	m, _ := newTestModel(t, http.NewServeMux())
	out, _ := m.Update(streamEventMsg{ev: client.Event{
		Type:   client.EventTypeStatus,
		Status: &api.JobResponse{JobID: "job-test", Status: "running", Streaming: true},
	}})
	m = out.(Model)
	if !m.streaming {
		t.Fatal("streaming not flipped to true on status Streaming=true")
	}
	// Daemon samples IsStreaming() again after pi emits agent_end
	// and emits a fresh status frame. The TUI must reflect that
	// flip-to-idle so the status bar doesn't show "streaming"
	// forever (review on update.go:117).
	out, _ = m.Update(streamEventMsg{ev: client.Event{
		Type:   client.EventTypeStatus,
		Status: &api.JobResponse{JobID: "job-test", Status: "running", Streaming: false},
	}})
	m = out.(Model)
	if m.streaming {
		t.Fatal("streaming not flipped to false on status Streaming=false")
	}
}

// TestUpdate_StreamEvent_AssistantTextDoesNotFlipStreaming pins the
// new contract: transcript events do NOT drive the live indicator.
// Only status frames do.
func TestUpdate_StreamEvent_AssistantTextDoesNotFlipStreaming(t *testing.T) {
	m, _ := newTestModel(t, http.NewServeMux())
	out, _ := m.Update(streamEventMsg{ev: client.Event{
		Type:    client.EventTypeMessage,
		EventID: 1,
		Message: &api.MessageEvent{Kind: "assistant_text", Text: "thinking"},
	}})
	got := out.(Model)
	if got.streaming {
		t.Fatal("streaming must NOT flip on transcript events; daemon is source of truth")
	}
}

// TestUpdate_StatusEvent_AppliesAttachCount: a status event is
// folded into m.job and surfaces attach_count for the status bar.
func TestUpdate_StatusEvent_AppliesAttachCount(t *testing.T) {
	m, _ := newTestModel(t, http.NewServeMux())
	out, _ := m.Update(streamEventMsg{ev: client.Event{
		Type:    client.EventTypeStatus,
		EventID: 2,
		Status:  &api.JobResponse{JobID: "job-test", Status: "running", AttachCount: 3},
	}})
	got := out.(Model)
	if got.job == nil || got.job.AttachCount != 3 {
		t.Fatalf("job=%+v want AttachCount=3", got.job)
	}
}

// TestUpdate_TerminalMidSession_TriggersQuit: we attach to a live
// run (first status is non-terminal), the run goes terminal during
// the session, and the stream closes. The TUI must auto-quit so the
// operator isn't stranded.
func TestUpdate_TerminalMidSession_TriggersQuit(t *testing.T) {
	m, _ := newTestModel(t, http.NewServeMux())
	// First status: alive.
	out, _ := m.Update(streamEventMsg{ev: client.Event{
		Type:   client.EventTypeStatus,
		Status: &api.JobResponse{JobID: "job-test", Status: "running"},
	}})
	m = out.(Model)
	if m.TerminalAtConnect() {
		t.Fatal("terminalAtConnect must be false when first status is non-terminal")
	}
	// Run transitions to done.
	out, _ = m.Update(streamEventMsg{ev: client.Event{
		Type:   client.EventTypeDone,
		Status: &api.JobResponse{JobID: "job-test", Status: "done"},
	}})
	m = out.(Model)
	// Stream closes.
	out, cmd := m.Update(streamClosedMsg{})
	got := out.(Model)
	if !got.quitting {
		t.Fatal("quitting not set after mid-session terminal + streamClose")
	}
	if cmd == nil {
		t.Fatal("expected tea.Quit cmd")
	}
}

// TestUpdate_TerminalAtConnect_DoesNotAutoQuit: attaching to an
// already-terminal run is a read-only inspection mode. streamClose
// must NOT auto-quit — otherwise the AltScreen wipe leaves the
// operator staring at their old prompt with no trace of the run
// (regression spotted when the user attached to job-fda3c1 which
// was already done).
func TestUpdate_TerminalAtConnect_DoesNotAutoQuit(t *testing.T) {
	m, _ := newTestModel(t, http.NewServeMux())
	out, _ := m.Update(streamEventMsg{ev: client.Event{
		Type:   client.EventTypeStatus,
		Status: &api.JobResponse{JobID: "job-test", Status: "done"},
	}})
	m = out.(Model)
	if !m.TerminalAtConnect() {
		t.Fatal("terminalAtConnect must be true when first status is terminal")
	}
	// done event arrives next.
	out, _ = m.Update(streamEventMsg{ev: client.Event{
		Type:   client.EventTypeDone,
		Status: &api.JobResponse{JobID: "job-test", Status: "done"},
	}})
	m = out.(Model)
	out, cmd := m.Update(streamClosedMsg{})
	got := out.(Model)
	if got.quitting {
		t.Fatal("must NOT auto-quit when attached to an already-terminal run")
	}
	if cmd != nil {
		t.Fatalf("expected nil cmd in read-only mode, got %T", cmd)
	}
	if !strings.Contains(got.flash, "read-only") {
		t.Fatalf("flash must hint at read-only mode, got %q", got.flash)
	}
}

// TestUpdate_TerminalRun_BlocksInputDispatch: sending input while
// the run is terminal must NOT hit the daemon (which would 409) and
// must surface a clear flash.
func TestUpdate_TerminalRun_BlocksInputDispatch(t *testing.T) {
	mux := http.NewServeMux()
	m, getRec := newTestModel(t, mux)
	// Mark run terminal via a status event.
	out, _ := m.Update(streamEventMsg{ev: client.Event{
		Type:   client.EventTypeStatus,
		Status: &api.JobResponse{JobID: "job-test", Status: "done"},
	}})
	m = out.(Model)
	m.textarea.SetValue("hi")
	out, cmd := m.Update(ctrlKey('d'))
	got := out.(Model)
	if cmd != nil {
		t.Fatalf("expected nil cmd when run is terminal, got %T", cmd)
	}
	if !strings.Contains(got.flash, "cannot send input") {
		t.Fatalf("flash=%q", got.flash)
	}
	if rec := getRec(); len(rec.inputs) != 0 {
		t.Fatalf("rec=%+v want zero (no daemon round-trip on terminal)", rec.inputs)
	}
	// Same for follow_up and abort.
	_, fcmd := got.Update(ctrlKey('f'))
	if fcmd != nil {
		t.Fatal("follow_up cmd must be nil on terminal")
	}
	_, acmd := got.Update(ctrlKey('a'))
	if acmd != nil {
		t.Fatal("abort cmd must be nil on terminal")
	}
}

// TestUpdate_ResizeFlowsViewport: a WindowSizeMsg propagates into the
// viewport's dimensions. Strict assertion on geometry would couple
// us to bubbles internals; we just verify the model takes it.
func TestUpdate_ResizeFlowsViewport(t *testing.T) {
	m, _ := newTestModel(t, http.NewServeMux())
	out, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	got := out.(Model)
	if got.width != 200 || got.height != 60 {
		t.Fatalf("w=%d h=%d want 200/60", got.width, got.height)
	}
}

// TestUpdate_StreamErrorEvent_Flashes: an EventTypeError frame
// surfaces as a status-bar flash without exiting the program.
func TestUpdate_StreamErrorEvent_Flashes(t *testing.T) {
	m, _ := newTestModel(t, http.NewServeMux())
	out, _ := m.Update(streamEventMsg{ev: client.Event{
		Type: client.EventTypeError,
		Err:  context.DeadlineExceeded,
	}})
	got := out.(Model)
	if !strings.Contains(got.flash, "stream error") {
		t.Fatalf("flash=%q", got.flash)
	}
	if got.quitting {
		t.Fatal("error event should not quit the program")
	}
}
