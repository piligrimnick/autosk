package server_test

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosk/internal/daemon/runstore"
)

// sseEvent is one parsed SSE frame.
type sseEvent struct {
	Event string
	ID    string
	Data  string
}

// readSSE reads SSE frames from r until ctx is done or done frame seen.
func readSSE(ctx context.Context, r *http.Response, sink chan<- sseEvent) {
	defer close(sink)
	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	cur := sseEvent{}
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Text()
		if line == "" {
			// dispatch
			if cur.Data != "" || cur.Event != "" {
				sink <- cur
			}
			cur = sseEvent{}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestStream_ReplayThenStatusThenDone(t *testing.T) {
	s := newStack(t, nil, "")
	r, _ := s.runs.CreateRun(context.Background(), runstore.NewRun{Prompt: "p", Cwd: s.cwd})

	// Seed a session.jsonl with one assistant event.
	jsonl := filepath.Join(s.cwd, "stream-session.jsonl")
	if err := os.WriteFile(jsonl, []byte(
		`{"type":"session","id":"s1","timestamp":"2026-05-17T10:00:00Z","cwd":"/x"}`+"\n"+
			`{"type":"message","id":"e1","parentId":null,"timestamp":"2026-05-17T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.runs.SetPISession(context.Background(), r.JobID, "s1", jsonl); err != nil {
		t.Fatal(err)
	}

	// Connect.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.ts.URL+"/v1/jobs/"+r.JobID+"/stream?limit=5", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	sink := make(chan sseEvent, 32)
	go readSSE(ctx, resp, sink)

	expectKinds := map[string]bool{}
	// Drive run to terminal in another goroutine.
	go func() {
		time.Sleep(120 * time.Millisecond)
		_, _ = s.runs.MarkRunning(context.Background(), r.JobID, 4242)

		// Append a new event to the session — should arrive on stream.
		f, _ := os.OpenFile(jsonl, os.O_APPEND|os.O_WRONLY, 0o644)
		_, _ = f.WriteString(`{"type":"message","id":"e2","parentId":"e1","timestamp":"2026-05-17T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"second"}]}}` + "\n")
		_ = f.Close()

		time.Sleep(700 * time.Millisecond)
		_, _ = s.runs.MarkDone(context.Background(), r.JobID, 0, runstore.ClosureDone)
	}()

	deadline := time.After(4 * time.Second)
	sawSecond := false
	sawDone := false
LOOP:
	for {
		select {
		case <-deadline:
			t.Fatalf("never saw 'second' (%t) and done (%t); events: %v", sawSecond, sawDone, expectKinds)
		case ev, ok := <-sink:
			if !ok {
				if !sawDone {
					t.Fatalf("stream closed before done; events: %v", expectKinds)
				}
				break LOOP
			}
			expectKinds[ev.Event] = true
			if ev.Event == "message" && strings.Contains(ev.Data, "\"second\"") {
				sawSecond = true
			}
			if ev.Event == "done" {
				sawDone = true
				break LOOP
			}
		}
	}
	if !sawSecond {
		t.Errorf("did not see appended 'second' assistant text")
	}
	for _, want := range []string{"message", "status", "done"} {
		if !expectKinds[want] {
			t.Errorf("did not observe SSE event %q", want)
		}
	}
}
