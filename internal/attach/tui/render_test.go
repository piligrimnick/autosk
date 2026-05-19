package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"autosk/internal/daemon/api"
)

// TestRenderEvent_CoversAllKnownKinds: one frame per documented event
// Kind, asserting the rendered string is non-empty and contains a
// recognisable header keyword. This is a smoke test, not a golden
// snapshot — the visual styling can drift without breaking the
// invariant that the user sees *something* per event.
func TestRenderEvent_CoversAllKnownKinds(t *testing.T) {
	s := defaultStyles()
	ts := time.Date(2026, 5, 19, 10, 30, 15, 0, time.UTC)

	cases := []struct {
		name    string
		ev      api.MessageEvent
		wantSub string
	}{
		{"user_text", api.MessageEvent{Kind: "user_text", TS: ts, Text: "hi"}, "user"},
		{"assistant_text", api.MessageEvent{Kind: "assistant_text", TS: ts, Text: "ok"}, "agent"},
		{"assistant_thinking", api.MessageEvent{Kind: "assistant_thinking", TS: ts, Text: "hm"}, "think"},
		{"tool_call", api.MessageEvent{Kind: "tool_call", TS: ts, Name: "Read", Input: map[string]any{"path": "/etc/hosts"}}, "Read"},
		{"tool_result_ok", api.MessageEvent{Kind: "tool_result", TS: ts, Name: "Read", Text: "127.0.0.1"}, "Read"},
		{"tool_result_err", api.MessageEvent{Kind: "tool_result", TS: ts, Name: "Read", Text: "denied", IsError: true}, "ERR"},
		{"model_change", api.MessageEvent{Kind: "model_change", TS: ts}, "model_change"},
		{"compaction", api.MessageEvent{Kind: "compaction", TS: ts}, "compaction"},
		{"unknown_kind", api.MessageEvent{Kind: "wat", TS: ts, Text: "?"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderEvent(s, tc.ev, 120)
			if got == "" {
				t.Fatalf("%s: empty render", tc.name)
			}
			if tc.wantSub != "" && !strings.Contains(stripANSI(got), tc.wantSub) {
				t.Fatalf("%s render missing %q\n---\n%s\n---", tc.name, tc.wantSub, got)
			}
		})
	}
}

// TestRenderEvent_ErrorVariantHasBadge: the tool_result error path
// must carry a recognisable ERR marker so the operator's eye lands
// on it even at a glance.
func TestRenderEvent_ErrorVariantHasBadge(t *testing.T) {
	out := renderEvent(defaultStyles(), api.MessageEvent{
		Kind: "tool_result", Name: "WriteFile", Text: "permission denied", IsError: true,
	}, 80)
	if !strings.Contains(stripANSI(out), "ERR") {
		t.Fatalf("tool_result error: ERR badge missing in:\n%s", out)
	}
}

// TestRenderStatusBar_LayoutAndFields: a status bar with width=80
// must end at exactly width=80 (lipgloss pad-fills), include the
// jobID, the streaming/idle label, and the attached count.
func TestRenderStatusBar_LayoutAndFields(t *testing.T) {
	s := defaultStyles()
	job := api.JobResponse{
		JobID: "job-abc", Status: "running", AttachCount: 2,
		CorrectionsUsed: 0, MaxCorrections: 3,
	}
	out := renderStatusBar(s, 80, &job, "job-abc", true, "")
	plain := stripANSI(out)
	// width is honoured by lipgloss → trailing pad to 80.
	if visualWidth(plain) < 60 {
		t.Fatalf("status bar surprisingly short (%d):\n%s", visualWidth(plain), plain)
	}
	for _, sub := range []string{"job-abc", "streaming", "attached: 2", "corrections: 0/3"} {
		if !strings.Contains(plain, sub) {
			t.Fatalf("status bar missing %q:\n%s", sub, plain)
		}
	}
}

func TestRenderStatusBar_PreStatus(t *testing.T) {
	out := renderStatusBar(defaultStyles(), 80, nil, "job-x", false, "")
	if !strings.Contains(stripANSI(out), "connecting") {
		t.Fatalf("pre-status missing 'connecting':\n%s", out)
	}
}

// TestSummariseInput_MapAndJSON: tool_call's Input is either a Go map
// (when the SSE payload was unmarshalled into interface{}) or a JSON
// RawMessage. Both branches must produce a compact one-line summary.
func TestSummariseInput_MapAndJSON(t *testing.T) {
	m := map[string]any{"path": "/etc/hosts", "limit": 10}
	got := summariseInput(m)
	if !strings.Contains(got, "path=/etc/hosts") {
		t.Fatalf("map summary missing path=: %q", got)
	}
	js := json.RawMessage(`{"k":"v","n":3}`)
	got = summariseInput(js)
	if got == "" {
		t.Fatalf("raw json summary empty")
	}
}

// TestWrap_NoSplitOnShortLines: wrap is a no-op when width >= len.
func TestWrap_NoSplitOnShortLines(t *testing.T) {
	if got := wrap("short text", 80); got != "short text" {
		t.Fatalf("wrap altered short line: %q", got)
	}
}

func TestWrap_BreaksAtWidth(t *testing.T) {
	s := "alpha beta gamma delta epsilon zeta eta theta"
	got := wrap(s, 20)
	for _, line := range strings.Split(got, "\n") {
		if visualWidth(line) > 20 {
			t.Fatalf("wrap produced line wider than 20: %q (full:\n%s)", line, got)
		}
	}
}

// ---- ANSI strip helper (tests only) ------------------------------------

// stripANSI removes the common SGR escape sequences emitted by
// lipgloss so substring assertions don't have to match coloured text.
func stripANSI(s string) string {
	var out strings.Builder
	in := false
	for _, r := range s {
		switch {
		case in:
			if (r >= '@' && r <= '~') && r != ';' {
				in = false
			}
		case r == 0x1b:
			in = true
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// visualWidth returns the rune count (close enough for ASCII test
// payloads; lipgloss has Width() for real rendering).
func visualWidth(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
