package rpcclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
)

// Notification is one server→client notification frame. In v2 the daemon scopes
// `task-changed` to connections that issued task.subscribe, `project-changed` to
// connections that issued project.subscribe, and `session-changed` to
// connections that issued session.subscribeProject; this client issues all three
// (task.subscribe opens the project and starts its fs watcher) and treats every
// kind as a "re-fetch" signal. Params is the raw payload (kept opaque; callers
// refetch): `task-changed` carries {root, task}, `project-changed` carries
// {project}, `session-changed` carries {root, session}.
type Notification struct {
	Method string          // "task-changed" | "project-changed" | "session-changed"
	Params json.RawMessage // raw notification params (kept opaque; callers refetch)
}

// NoteStream is an active task/project notification subscription over a
// dedicated persistent connection. The daemon multiplexes the subscribe ack and
// the server→client `task-changed`/`project-changed` notifications onto this one
// line-delimited connection. Close terminates the subscription (best-effort
// task.unsubscribe + connection close). Mirrors JobStream (stream.go).
type NoteStream struct {
	events    <-chan Notification
	conn      net.Conn
	selector  map[string]any
	closeOnce sync.Once
	closed    chan struct{}
}

// Events is the stream of notification frames; it closes when the stream ends
// (daemon disconnect, a subscribe error, or Close).
func (s *NoteStream) Events() <-chan Notification { return s.events }

// Close terminates the subscription. Idempotent.
func (s *NoteStream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		// Best-effort unsubscribe so the daemon drops the hub registration
		// promptly; closing the connection is the hard backstop (the daemon's
		// per-connection reader breaks on EOF and unregisters).
		_ = json.NewEncoder(s.conn).Encode(rpcRequest{ID: 0, Method: "task.unsubscribe", Params: s.selector})
		_ = json.NewEncoder(s.conn).Encode(rpcRequest{ID: 0, Method: "project.unsubscribe", Params: s.selector})
		_ = json.NewEncoder(s.conn).Encode(rpcRequest{ID: 0, Method: "session.unsubscribeProject", Params: s.selector})
		_ = s.conn.Close()
		close(s.closed)
	})
	return nil
}

// Subscribe opens a persistent connection, subscribes to the project's
// task-changed AND project-changed notifications, and returns a stream of
// frames. The caller MUST Close the stream. autoskd is auto-spawned on first use
// (the connector handles dialing). v2 scopes the notification kinds to separate
// subscriptions (task.subscribe opens the project + its fs watcher;
// project.subscribe registers for registry-level pushes; session.subscribeProject
// registers for project-scoped session lifecycle pushes), so we issue all three.
func (c *Client) Subscribe(ctx context.Context) (*NoteStream, error) {
	conn, err := c.conn.Dial(ctx)
	if err != nil {
		return nil, err
	}
	subID := c.id.Add(1)
	if err := json.NewEncoder(conn).Encode(rpcRequest{
		ID:     subID,
		Method: "task.subscribe",
		Params: c.selector(nil),
	}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("autoskd task.subscribe: write: %w", err)
	}
	if err := json.NewEncoder(conn).Encode(rpcRequest{
		ID:     c.id.Add(1),
		Method: "project.subscribe",
		Params: c.selector(nil),
	}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("autoskd project.subscribe: write: %w", err)
	}
	if err := json.NewEncoder(conn).Encode(rpcRequest{
		ID:     c.id.Add(1),
		Method: "session.subscribeProject",
		Params: c.selector(nil),
	}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("autoskd session.subscribeProject: write: %w", err)
	}
	ch := make(chan Notification, 16)
	s := &NoteStream{
		events:   ch,
		conn:     conn,
		selector: c.selector(nil),
		closed:   make(chan struct{}),
	}
	go s.readLoop(ch, subID)
	// Honour caller cancellation: close the stream (and the connection) when the
	// context is done so a long-lived subscription is reaped with the TUI.
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.closed:
		}
	}()
	return s, nil
}

// readLoop demultiplexes the shared connection: `task-changed`/`project-changed`
// notifications are forwarded to the channel; the subscribe ack is ignored,
// except that an error response to our subscribe ends the stream (so the
// consumer's watch loop can fall back to its periodic re-sync). Self-reaps on
// exit (deferred Close) so a daemon EOF or a subscribe error releases the
// connection + the ctx-watcher goroutine without an external Close.
func (s *NoteStream) readLoop(ch chan<- Notification, subID uint64) {
	defer close(ch)
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
		switch raw.Method {
		case "task-changed", "project-changed", "session-changed":
			select {
			case ch <- Notification{Method: raw.Method, Params: raw.Params}:
			case <-s.closed:
				return
			}
		case "":
			// A response frame. The subscribe ack is ignored; an error response
			// (method unsupported / project unresolved) ends the stream.
			if raw.ID == subID && raw.Error != nil {
				return
			}
		}
	}
}
