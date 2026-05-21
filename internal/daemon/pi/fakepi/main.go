// fakepi is a tiny test stand-in for `pi --mode rpc`. It speaks the same
// JSON Lines protocol as the real binary for the subset the daemon relies
// on. Behavior is controlled by environment variables so tests don't need
// to recompile per case:
//
//	FAKEPI_SESSION_ID    — value returned in get_state.data.sessionId
//	FAKEPI_SESSION_FILE  — value returned in get_state.data.sessionFile
//	FAKEPI_AGENT_END_DELAY_MS — milliseconds to wait before emitting agent_end
//	FAKEPI_SCENARIO      — "ok" (default), "no_agent_end" (drop agent_end),
//	                       "prompt_error" (reply success=false to prompt),
//	                       "dialog" (emit a select extension_ui_request)
//	FAKEPI_HUGE_PAYLOAD_BYTES — when >0, emit an extra message_end carrying
//	                       a text block of approximately this many bytes
//	                       before agent_end (used by runner tests to
//	                       exercise the no-per-line-cap path). The exact
//	                       event type doesn't matter for the reader test;
//	                       message_end is convenient because the regular
//	                       turn already emits one and reusing the shape
//	                       keeps the fakepi switch small.
//	FAKEPI_INJECT_GARBAGE_LINE — when non-empty, write the literal value
//	                       (followed by '\n') as its own line on stdout
//	                       between message_end and turn_end. The runner's
//	                       reader is expected to surface it as a single
//	                       KindOther event and keep parsing subsequent
//	                       lines (used by runner tests to lock the
//	                       line-oriented resync contract).
//
// fakepi exits 0 on stdin EOF or SIGTERM.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type cmd struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

var (
	outMu   sync.Mutex
	writer  = bufio.NewWriter(os.Stdout)
	exiting = make(chan struct{})
)

func emit(v any) {
	outMu.Lock()
	defer outMu.Unlock()
	_ = json.NewEncoder(writer).Encode(v)
	_ = writer.Flush()
}

// emitRawLine writes s followed by '\n' to stdout, bypassing json.Encoder.
// Used by FAKEPI_INJECT_GARBAGE_LINE to feed deliberately malformed bytes
// to the runner's reader so tests can pin the line-oriented resync
// behaviour.
func emitRawLine(s string) {
	outMu.Lock()
	defer outMu.Unlock()
	_, _ = writer.WriteString(s)
	_ = writer.WriteByte('\n')
	_ = writer.Flush()
}

func main() {
	// Trap SIGTERM/SIGINT — flush any pending writes and exit cleanly.
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		<-sigC
		close(exiting)
		_ = writer.Flush()
		os.Exit(143)
	}()

	scenario := os.Getenv("FAKEPI_SCENARIO")
	sessID := envOr("FAKEPI_SESSION_ID", "sess-fake")
	sessFile := envOr("FAKEPI_SESSION_FILE", "/tmp/fakepi/session.jsonl")
	delay := envIntMS("FAKEPI_AGENT_END_DELAY_MS", 0)
	hugePayload := envInt("FAKEPI_HUGE_PAYLOAD_BYTES", 0)
	garbageLine := os.Getenv("FAKEPI_INJECT_GARBAGE_LINE")

	// JSON Lines on stdin. json.Decoder streams without the per-token
	// cap bufio.Scanner imposes — fine here because the daemon only
	// writes well-formed JSON to fakepi's stdin, so the no-resync
	// behaviour of json.Decoder is never observable. (The runner-side
	// reader uses bufio.Reader.ReadBytes for a different reason: pi's
	// stdout in the wild needs the resync property; see runner.go.)
	dec := json.NewDecoder(os.Stdin)
	for {
		var c cmd
		if err := dec.Decode(&c); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			emit(map[string]any{"type": "response", "id": "", "command": "?", "success": false, "error": fmt.Sprintf("parse: %v", err)})
			continue
		}
		handle(c, scenario, sessID, sessFile, delay, hugePayload, garbageLine)
	}
	// stdin closed → exit cleanly.
}

func handle(c cmd, scenario, sessID, sessFile string, delay time.Duration, hugePayload int, garbageLine string) {
	switch c.Type {
	case "get_state":
		emit(map[string]any{
			"type":    "response",
			"id":      c.ID,
			"command": "get_state",
			"success": true,
			"data": map[string]any{
				"sessionId":    sessID,
				"sessionFile":  sessFile,
				"messageCount": 0,
			},
		})
	case "prompt":
		if scenario == "prompt_error" {
			emit(map[string]any{"type": "response", "id": c.ID, "command": "prompt", "success": false, "error": "fake error"})
			return
		}
		// Ack preflight immediately.
		emit(map[string]any{"type": "response", "id": c.ID, "command": "prompt", "success": true})

		// Optional dialog scenario: fake an extension_ui_request and stop.
		if scenario == "dialog" {
			emit(map[string]any{"type": "extension_ui_request", "id": "dlg-1", "method": "select", "title": "pick", "options": []string{"a", "b"}})
			// then proceed normally so the test can confirm we don't hang.
		}

		// Simulate a turn.
		go func(prompt string) {
			emit(map[string]any{"type": "agent_start"})
			emit(map[string]any{"type": "turn_start"})
			emit(map[string]any{
				"type":    "message_start",
				"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "ack: " + prompt}}, "timestamp": time.Now().UnixMilli()},
			})
			emit(map[string]any{
				"type":    "message_end",
				"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "ack: " + prompt}}, "timestamp": time.Now().UnixMilli()},
			})
			if hugePayload > 0 {
				// Emit a single >hugePayload-byte JSON line to exercise
				// the runner's no-per-line-cap reader path. The text
				// block carries the bulk; with JSON escaping + envelope
				// the line is always >= hugePayload bytes.
				big := strings.Repeat("x", hugePayload)
				emit(map[string]any{
					"type":    "message_end",
					"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": big}}, "timestamp": time.Now().UnixMilli()},
				})
			}
			if garbageLine != "" {
				// Inject a non-JSON line between two valid frames. The
				// runner's reader must surface this as a single KindOther
				// event and keep parsing subsequent lines normally.
				emitRawLine(garbageLine)
			}
			emit(map[string]any{"type": "turn_end", "message": map[string]any{}, "toolResults": []any{}})

			if scenario == "no_agent_end" {
				return // intentionally never emit agent_end
			}
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-exiting:
					return
				}
			}
			emit(map[string]any{"type": "agent_end", "messages": []any{}})
		}(c.Message)
	case "abort":
		emit(map[string]any{"type": "response", "id": c.ID, "command": "abort", "success": true})
	case "extension_ui_response":
		// no-op
	default:
		emit(map[string]any{"type": "response", "id": c.ID, "command": c.Type, "success": false, "error": "unknown command"})
	}
}

func envOr(k, d string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return d
}

func envIntMS(k string, def int) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return time.Duration(def) * time.Millisecond
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return time.Duration(def) * time.Millisecond
	}
	return time.Duration(n) * time.Millisecond
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
