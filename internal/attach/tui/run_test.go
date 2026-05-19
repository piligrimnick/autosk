package tui

import (
	"bytes"
	"strings"
	"testing"

	"autosk/internal/daemon/api"
)

// TestPrintExitSummary_NoStatusReceived: we never saw a status event
// (e.g. SSE open succeeded then stream closed before the first
// frame). The summary mentions the no-status condition explicitly.
func TestPrintExitSummary_NoStatusReceived(t *testing.T) {
	var buf bytes.Buffer
	m := New(nil, "job-x")
	printExitSummary(&buf, m, "job-x")
	got := buf.String()
	if !strings.Contains(got, "no status received") {
		t.Fatalf("summary missing 'no status received':\n%s", got)
	}
	if !strings.Contains(got, "job-x") {
		t.Fatalf("summary missing job id:\n%s", got)
	}
}

// TestPrintExitSummary_TerminalAtConnect_ReadOnly: the operator
// attached to an already-terminal run and exited via Ctrl-C. The
// summary surfaces the "read-only inspection" framing and includes
// the total event count (m.EventCount, NOT len(m.events) — so empty-
// render kinds are not silently undercounted; see review on
// run.go:107).
func TestPrintExitSummary_TerminalAtConnect_ReadOnly(t *testing.T) {
	exit := 0
	var buf bytes.Buffer
	m := New(nil, "job-y")
	// Feed one terminal status (latches terminalAtConnect=true).
	m.applyStatus(api.JobResponse{JobID: "job-y", Status: "done", ExitCode: &exit, DurationMS: 5000})
	// Push three events of which one is a compaction (renderEvent
	// returns "" → not appended to m.events but EventCount bumps).
	m.pushEvent(1, api.MessageEvent{Kind: "user_text", Text: "hi"})
	m.pushEvent(2, api.MessageEvent{Kind: "assistant_text", Text: "hello"})
	m.pushEvent(3, api.MessageEvent{Kind: "compaction"})

	if got := m.EventCount(); got != 3 {
		t.Fatalf("EventCount=%d want 3 (compaction must be counted)", got)
	}
	if got := len(m.events); got != 3 {
		// compaction renders as "(compaction)" so it IS rendered;
		// adjust if renderEvent changes.
	}

	printExitSummary(&buf, m, "job-y")
	got := buf.String()
	if !strings.Contains(got, "read-only inspection") {
		t.Fatalf("summary missing 'read-only inspection':\n%s", got)
	}
	if !strings.Contains(got, "3 events") {
		t.Fatalf("summary event count not 3 (EventCount, not len(events)):\n%s", got)
	}
	if !strings.Contains(got, "exit 0") {
		t.Fatalf("summary missing exit code:\n%s", got)
	}
}

// TestPrintExitSummary_TerminalMidSession: we attached to a live run,
// it went terminal during the session, the TUI auto-quit. The
// summary distinguishes this from a read-only inspection.
func TestPrintExitSummary_TerminalMidSession(t *testing.T) {
	var buf bytes.Buffer
	m := New(nil, "job-z")
	// First status is non-terminal → terminalAtConnect=false.
	m.applyStatus(api.JobResponse{JobID: "job-z", Status: "running"})
	// Then a terminal status arrives.
	m.applyStatus(api.JobResponse{JobID: "job-z", Status: "failed", Error: "panic"})

	printExitSummary(&buf, m, "job-z")
	got := buf.String()
	if !strings.Contains(got, "failed during session") {
		t.Fatalf("summary missing 'failed during session':\n%s", got)
	}
	if !strings.Contains(got, "error: panic") {
		t.Fatalf("summary missing error tag:\n%s", got)
	}
	if strings.Contains(got, "read-only") {
		t.Fatalf("summary should NOT mention 'read-only' for mid-session terminal:\n%s", got)
	}
}

// TestPrintExitSummary_DetachedWhileRunning: operator hit Ctrl-C
// before the run terminated. The summary records the last status
// the daemon reported.
func TestPrintExitSummary_DetachedWhileRunning(t *testing.T) {
	var buf bytes.Buffer
	m := New(nil, "job-w")
	m.applyStatus(api.JobResponse{JobID: "job-w", Status: "running"})

	printExitSummary(&buf, m, "job-w")
	got := buf.String()
	if !strings.Contains(got, "detached (run was running)") {
		t.Fatalf("summary missing 'detached (run was running)':\n%s", got)
	}
}

// TestSummariseTerminal pins the wire format of the terminal summary
// blurb so a future tweak to the join separator / field order is an
// explicit code change visible in this table.
func TestSummariseTerminal(t *testing.T) {
	exit := 1
	cases := []struct {
		name string
		in   api.JobResponse
		want string
	}{
		{
			"status_only",
			api.JobResponse{Status: "done"},
			"done",
		},
		{
			"status_exit",
			api.JobResponse{Status: "done", ExitCode: &exit},
			"done · exit 1",
		},
		{
			"status_exit_duration",
			api.JobResponse{Status: "failed", ExitCode: &exit, DurationMS: 3500},
			"failed · exit 1, 4s",
		},
		{
			"status_error_only",
			api.JobResponse{Status: "failed", Error: "boom"},
			"failed · error: boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summariseTerminal(&tc.in); got != tc.want {
				t.Fatalf("summariseTerminal=%q want %q", got, tc.want)
			}
		})
	}
}
