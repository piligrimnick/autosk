package transcript_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"autosk/internal/daemon/transcript"
)

// fixture is one realistic session.jsonl. Header + a user message + an
// assistant message with thinking + text + a toolCall + a toolResult,
// plus a thinking_level_change and an unknown entry type to exercise
// the KindOther fallback.
const fixture = `
{"type":"session","id":"sess-1","timestamp":"2026-05-17T10:00:00Z","cwd":"/abs","version":3}
{"type":"message","id":"e1","parentId":null,"timestamp":"2026-05-17T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"do the thing"}]}}
{"type":"thinking_level_change","id":"e2","parentId":"e1","timestamp":"2026-05-17T10:00:02Z","thinkingLevel":"high"}
{"type":"message","id":"e3","parentId":"e2","timestamp":"2026-05-17T10:00:03Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"thinking out loud"},{"type":"text","text":"ok, calling tool"},{"type":"toolCall","id":"tc1","name":"read","arguments":{"path":"/etc/hosts"}}]}}
{"type":"message","id":"e4","parentId":"e3","timestamp":"2026-05-17T10:00:04Z","message":{"role":"toolResult","toolCallId":"tc1","toolName":"read","isError":false,"content":[{"type":"text","text":"file contents"}]}}
{"type":"wat","id":"e5","parentId":"e4","timestamp":"2026-05-17T10:00:05Z","blob":"unknown"}
`

func writeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRead_ProjectsCanonicalShape(t *testing.T) {
	path := writeFixture(t)
	got, err := transcript.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []transcript.Kind{
		transcript.KindSession,
		transcript.KindUserText,
		transcript.KindThinkingLevel,
		transcript.KindAssistantThinking,
		transcript.KindAssistantText,
		transcript.KindToolCall,
		transcript.KindToolResult,
		transcript.KindOther,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d\n%+v", len(got), len(want), got)
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Errorf("event %d: got kind %q, want %q", i, got[i].Kind, k)
		}
	}
	// Spot-check projections.
	if got[1].Text != "do the thing" {
		t.Errorf("user text: %q", got[1].Text)
	}
	if got[4].Text != "ok, calling tool" {
		t.Errorf("assistant text: %q", got[4].Text)
	}
	if got[5].Name != "read" {
		t.Errorf("tool name: %q", got[5].Name)
	}
	if got[6].Text != "file contents" || got[6].Name != "read" {
		t.Errorf("tool result projection: %+v", got[6])
	}
	if got[6].IsError {
		t.Errorf("is_error: true, want false")
	}
}

func TestTail_ReturnsLastN(t *testing.T) {
	path := writeFixture(t)
	got, err := transcript.Tail(path, 3)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	// Should be the last 3 from the canonical sequence.
	wantTail := []transcript.Kind{transcript.KindToolCall, transcript.KindToolResult, transcript.KindOther}
	for i, k := range wantTail {
		if got[i].Kind != k {
			t.Errorf("tail[%d]: got %q, want %q", i, got[i].Kind, k)
		}
	}
}

func TestRead_MissingFile(t *testing.T) {
	_, err := transcript.Read("/no/such/file.jsonl")
	if !errors.Is(err, transcript.ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
}

func TestRead_EmptyPath(t *testing.T) {
	_, err := transcript.Read("")
	if !errors.Is(err, transcript.ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
}
