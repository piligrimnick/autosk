// SSE streaming primitives for the autosk daemon client.
//
// The daemon emits one of four SSE event kinds (see
// internal/daemon/server/sse.go): message, status, done, error.
// This file parses the raw text/event-stream into a typed Event union
// and exposes Stream() as the single entry point the TUI consumes.
//
// Design notes:
//
//   - Events are surfaced over a buffered channel. The Stream method
//     spawns one goroutine that owns the http.Response.Body and shuts
//     it down when the context cancels or the stream ends naturally.
//
//   - We carry the SSE event id forward as a Last-Event-ID hint so a
//     future reconnect can pick up where it left off. The hint is
//     exposed via StreamHandle.LastEventID. The current TUI does not
//     reconnect (see plan §scope — "no reconnect/backoff"), but
//     surfacing the cursor keeps that future iteration cheap.
//
//   - We split each event's `data:` line at the first byte and JSON-
//     decode the payload using the daemon's own api.* types so the
//     TUI never has to look at raw bytes.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"autosk/internal/daemon/api"
)

// StreamOptions configures Stream.
type StreamOptions struct {
	// Attach, when true, opens the stream with ?attach=true so the
	// daemon's attach counter is incremented for the lifetime of the
	// connection (see server/sse.go). The TUI always passes Attach=true.
	Attach bool
	// Full, when true, replays the entire transcript before tailing.
	// Mutually exclusive with Limit in semantics — when Full is set
	// the daemon ignores Limit.
	Full bool
	// Limit caps the initial replay size when Full is false. Zero
	// means "use the daemon's default of 20".
	Limit int
	// LastEventID, when >0, is sent as the Last-Event-ID header to
	// resume from a prior cursor without re-emitting events the
	// client already has. Currently unused by the TUI (no reconnect
	// in v1) but plumbed end-to-end.
	LastEventID int
}

// Event is one decoded SSE frame from the daemon.
//
// Exactly one of Message, Status, or Err is non-nil for any given
// event:
//
//   - Type=EventTypeMessage → Message is set.
//   - Type=EventTypeStatus  → Status is set.
//   - Type=EventTypeDone    → Status is set (carries the terminal snapshot).
//   - Type=EventTypeError   → Err is set; the stream is *not* closed
//     by this event alone, but a follow-up Done usually arrives.
//
// EventID mirrors the SSE `id:` field and is monotonic per stream;
// callers can save it as the next reconnect cursor.
type Event struct {
	Type    EventType
	EventID int
	Message *api.MessageEvent
	Status  *api.JobResponse
	Err     error
	// Raw is the unparsed `data:` payload — useful for diagnostics
	// and for forward-compat with unknown event types the daemon
	// might add later.
	Raw string
}

// EventType is the discriminator for Event.
type EventType string

const (
	EventTypeMessage EventType = "message"
	EventTypeStatus  EventType = "status"
	EventTypeDone    EventType = "done"
	EventTypeError   EventType = "error"
	// EventTypeUnknown covers `event:` values the daemon emits that
	// this client doesn't recognise — surfaced verbatim so the TUI
	// can decide whether to render or ignore.
	EventTypeUnknown EventType = "unknown"
)

// StreamHandle is the result of opening a stream. The TUI reads from
// Events and calls Close to detach early. Close is safe to call from
// any goroutine and is idempotent (subsequent calls are no-ops).
type StreamHandle struct {
	// Events is a buffered channel of decoded events. The producer
	// closes it when the daemon hangs up or the context cancels.
	Events <-chan Event

	resp      *http.Response
	cancel    context.CancelFunc
	closeOnce sync.Once
}

// Close cancels the underlying request context and waits for the
// producer goroutine to finish via the events-channel close. The
// guard against double-close is sync.Once so concurrent Close calls
// are safe (http.Response.Body double-Close is implementation
// defined; sync.Once gives us the contract we want).
func (h *StreamHandle) Close() {
	h.closeOnce.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
		if h.resp != nil && h.resp.Body != nil {
			_ = h.resp.Body.Close()
		}
	})
}

// Stream opens GET /v1/jobs/{id}/stream with the requested SSE
// options and returns a handle whose Events channel yields decoded
// frames. The caller MUST drain or Close the handle to release the
// connection (and, when Attach=true, the daemon's attach counter).
//
// Stream returns a non-nil error only when the initial HTTP request
// fails or the daemon answers with a non-2xx status before any SSE
// frame arrives. Mid-stream errors are surfaced as EventTypeError
// frames followed by channel-close.
func (c *Client) Stream(ctx context.Context, jobID string, opts StreamOptions) (*StreamHandle, error) {
	q := streamQuery(opts)
	streamCtx, cancel := context.WithCancel(ctx)
	hdr := http.Header{}
	if opts.LastEventID > 0 {
		hdr.Set("Last-Event-ID", strconv.Itoa(opts.LastEventID))
	}
	resp, err := c.do(streamCtx, http.MethodGet, "/v1/jobs/"+jobID+"/stream"+q, nil, hdr)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		return nil, decodeAPIError(resp.StatusCode, buf)
	}

	ch := make(chan Event, 32)
	go pumpStream(resp.Body, ch)

	return &StreamHandle{
		Events: ch,
		resp:   resp,
		cancel: cancel,
	}, nil
}

// streamQuery composes the SSE query string from StreamOptions.
// Exported-shape kept stable so attach_test.go-style fixtures can
// assert the URL without importing this file.
func streamQuery(opts StreamOptions) string {
	var parts []string
	if opts.Attach {
		parts = append(parts, "attach=true")
	}
	if opts.Full {
		parts = append(parts, "full=true")
	}
	if opts.Limit > 0 && !opts.Full {
		parts = append(parts, "limit="+strconv.Itoa(opts.Limit))
	}
	if len(parts) == 0 {
		return ""
	}
	return "?" + strings.Join(parts, "&")
}

// pumpStream reads the SSE body, decodes events, and forwards them
// on out. Closes out on EOF / context cancel / parse error.
func pumpStream(body io.ReadCloser, out chan<- Event) {
	defer close(out)
	defer body.Close()

	sc := bufio.NewScanner(body)
	// SSE frames can be large (full transcript replay puts the whole
	// MessageEvent into a single `data:` line). Generous buffer.
	sc.Buffer(make([]byte, 0, 1<<14), 1<<22)

	var (
		evType EventType = EventTypeMessage // SSE default per RFC
		evID   int
		evData strings.Builder
	)
	flush := func() {
		raw := evData.String()
		if raw == "" && evType == "" {
			return
		}
		ev := decodeEvent(evType, evID, raw)
		out <- ev
		evType = EventTypeMessage
		evID = 0
		evData.Reset()
	}

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// SSE comment / keepalive — ignore.
		case strings.HasPrefix(line, "event:"):
			evType = EventType(strings.TrimSpace(line[len("event:"):]))
		case strings.HasPrefix(line, "id:"):
			if n, err := strconv.Atoi(strings.TrimSpace(line[len("id:"):])); err == nil {
				evID = n
			}
		case strings.HasPrefix(line, "data:"):
			if evData.Len() > 0 {
				evData.WriteByte('\n')
			}
			evData.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	// Trailing event without a blank-line terminator (rare, but be lenient).
	if evData.Len() > 0 {
		flush()
	}
}

// decodeEvent turns a (type, id, data) tuple into a typed Event.
// Unrecognised event kinds are passed through as EventTypeUnknown so
// callers don't lose the data.
func decodeEvent(t EventType, id int, data string) Event {
	switch t {
	case EventTypeMessage:
		var m api.MessageEvent
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return Event{Type: EventTypeError, EventID: id, Err: fmt.Errorf("decode message: %w", err), Raw: data}
		}
		return Event{Type: EventTypeMessage, EventID: id, Message: &m, Raw: data}
	case EventTypeStatus, EventTypeDone:
		var s api.JobResponse
		if err := json.Unmarshal([]byte(data), &s); err != nil {
			return Event{Type: EventTypeError, EventID: id, Err: fmt.Errorf("decode %s: %w", t, err), Raw: data}
		}
		return Event{Type: t, EventID: id, Status: &s, Raw: data}
	case EventTypeError:
		// Daemon error frames are `{"error":"..."}` strings.
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(data), &e)
		msg := e.Error
		if msg == "" {
			msg = data
		}
		return Event{Type: EventTypeError, EventID: id, Err: fmt.Errorf("daemon stream error: %s", msg), Raw: data}
	default:
		return Event{Type: EventTypeUnknown, EventID: id, Raw: data}
	}
}
