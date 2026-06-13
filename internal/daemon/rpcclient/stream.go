package rpcclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"autosk/internal/daemon/api"
)

// SessionEvent is one `session-event` notification frame (api.SessionEventParams).
// Kind is "message" | "status" | "done" | "error":
//   - message: Event carries one pi-format transcript line (also used for replay).
//   - status / done: Session carries the decorated session meta.
//   - error: Error carries the message.
type SessionEvent struct {
	Kind      string              `json:"kind"`
	SessionID string              `json:"session_id"`
	Event     *api.TranscriptLine `json:"event,omitempty"`
	Session   *api.SessionMeta    `json:"session,omitempty"`
	Error     string              `json:"error,omitempty"`
	Line      int                 `json:"line,omitempty"`
}

// SessionStream is an active session.subscribe tail over a dedicated persistent
// connection. The daemon multiplexes the subscribe ack and the server→client
// `session-event` notifications onto this one line-delimited connection. Close
// terminates the subscription (best-effort session.unsubscribe + connection
// close).
type SessionStream struct {
	events    <-chan SessionEvent
	conn      net.Conn
	selector  map[string]any
	closeOnce sync.Once
	closed    chan struct{}
}

// Events is the stream of session-event frames; it closes when the stream ends
// (terminal session, daemon disconnect, or Close).
func (s *SessionStream) Events() <-chan SessionEvent { return s.events }

// Close terminates the subscription. Idempotent.
func (s *SessionStream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		// Best-effort unsubscribe so the daemon drops the tail thread promptly;
		// closing the connection is the hard backstop (the daemon's
		// per-connection reader breaks on EOF and cleans up).
		_ = json.NewEncoder(s.conn).Encode(rpcRequest{ID: 0, Method: "session.unsubscribe", Params: s.selector})
		_ = s.conn.Close()
		close(s.closed)
	})
	return nil
}

// SessionSubscribe opens a persistent connection, subscribes to a session's
// transcript (replay-from fromLine, then tail), and returns a stream of
// session-event frames. The caller MUST Close the stream. autoskd is
// auto-spawned on first use (the connector handles dialing).
func (c *Client) SessionSubscribe(ctx context.Context, sessionID string, fromLine int) (*SessionStream, error) {
	conn, err := c.conn.Dial(ctx)
	if err != nil {
		return nil, err
	}
	extra := map[string]any{"id": sessionID}
	if fromLine > 0 {
		extra["from_line"] = fromLine
	}
	subID := c.id.Add(1)
	if err := json.NewEncoder(conn).Encode(rpcRequest{
		ID:     subID,
		Method: "session.subscribe",
		Params: c.selector(extra),
	}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("autoskd session.subscribe: write: %w", err)
	}
	ch := make(chan SessionEvent, 64)
	s := &SessionStream{
		events:   ch,
		conn:     conn,
		selector: c.selector(map[string]any{"id": sessionID}),
		closed:   make(chan struct{}),
	}
	go s.readLoop(ch, subID)
	// Honour caller cancellation: close the stream (and the connection) when the
	// context is done so a long-lived tail is reaped with the TUI view.
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.closed:
		}
	}()
	return s, nil
}

// readLoop demultiplexes the shared connection: `session-event` notifications
// are forwarded to the channel; the subscribe ack (and unsubscribe ack) are
// ignored, except that an error response to our subscribe surfaces as a
// synthetic error frame so the consumer sees the failure.
func (s *SessionStream) readLoop(ch chan<- SessionEvent, subID uint64) {
	defer close(ch)
	// Self-reap on exit so a daemon EOF or a subscribe-error response releases
	// the local connection + the ctx-watcher goroutine. Close() is idempotent.
	defer func() { _ = s.Close() }()
	dec := json.NewDecoder(s.conn)
	for {
		var raw struct {
			ID     uint64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Error  *RPCError       `json:"error"`
		}
		if err := dec.Decode(&raw); err != nil {
			return // EOF / connection closed
		}
		if raw.Method == "session-event" {
			var ev SessionEvent
			if err := json.Unmarshal(raw.Params, &ev); err != nil {
				continue
			}
			select {
			case ch <- ev:
			case <-s.closed:
				return
			}
			continue
		}
		// A response frame. Only the subscribe ack matters: surface its error.
		if raw.Method == "" && raw.ID == subID && raw.Error != nil {
			select {
			case ch <- SessionEvent{Kind: "error", Error: raw.Error.Error()}:
			case <-s.closed:
			}
			return
		}
	}
}
