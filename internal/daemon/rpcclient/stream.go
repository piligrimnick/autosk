package rpcclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"autosk/internal/daemon/api"
)

// JobEvent is one `job-event` notification frame (autosk-proto::wire::JobEvent).
// It mirrors the old SSE frame: message (with EventID), status, done, error.
type JobEvent struct {
	// Kind is "message" | "status" | "done" | "error".
	Kind    string            `json:"kind"`
	JobID   string            `json:"job_id"`
	EventID int64             `json:"event_id"`
	Event   *api.MessageEvent `json:"event"` // set when Kind == "message"
	Job     *api.JobResponse  `json:"job"`   // set when Kind == "status" | "done"
	Error   string            `json:"error"` // set when Kind == "error"
}

// SubscribeOptions mirror the job.subscribe replay-then-tail params (plan §4.1).
type SubscribeOptions struct {
	Attach      bool // bump the per-job attach counter (disarms idle-timeout)
	Full        bool // replay the whole archived transcript before tailing
	Limit       int  // replay only the last N events (ignored when Full)
	FromEventID int  // resume after this SSE id (Last-Event-ID semantics)
}

// JobStream is an active job.subscribe tail over a dedicated persistent
// connection. The daemon multiplexes the subscribe ack and the server→client
// `job-event` notifications onto this one line-delimited connection. Close
// terminates the subscription (best-effort job.unsubscribe + connection close).
type JobStream struct {
	events    <-chan JobEvent
	conn      net.Conn
	selector  map[string]any
	closeOnce sync.Once
	closed    chan struct{}
}

// Events is the stream of job-event frames; it closes when the stream ends
// (terminal job, daemon disconnect, or Close).
func (s *JobStream) Events() <-chan JobEvent { return s.events }

// Close terminates the subscription. Idempotent.
func (s *JobStream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		// Best-effort unsubscribe so the daemon drops the tail thread + attach
		// guard promptly; closing the connection is the hard backstop (the
		// daemon's per-connection reader breaks on EOF and cleans up).
		_ = json.NewEncoder(s.conn).Encode(rpcRequest{
			ID:     0,
			Method: "job.unsubscribe",
			Params: s.selector,
		})
		_ = s.conn.Close()
		close(s.closed)
	})
	return nil
}

// JobSubscribe opens a persistent connection, subscribes to a job's transcript,
// and returns a stream of job-event frames. The caller MUST Close the stream.
// autoskd is auto-spawned on first use (the connector handles dialing).
func (c *Client) JobSubscribe(ctx context.Context, jobID string, opts SubscribeOptions) (*JobStream, error) {
	conn, err := c.conn.Dial(ctx)
	if err != nil {
		return nil, err
	}
	extra := map[string]any{"job_id": jobID}
	if opts.Attach {
		extra["attach"] = true
	}
	if opts.Full {
		extra["full"] = true
	}
	if opts.Limit > 0 {
		extra["limit"] = opts.Limit
	}
	if opts.FromEventID > 0 {
		extra["from_event_id"] = opts.FromEventID
	}
	subID := c.id.Add(1)
	if err := json.NewEncoder(conn).Encode(rpcRequest{
		ID:     subID,
		Method: "job.subscribe",
		Params: c.selector(extra),
	}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("autoskd job.subscribe: write: %w", err)
	}
	ch := make(chan JobEvent, 64)
	s := &JobStream{
		events:   ch,
		conn:     conn,
		selector: c.selector(map[string]any{"job_id": jobID}),
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

// readLoop demultiplexes the shared connection: `job-event` notifications are
// forwarded to the channel; the subscribe ack (and unsubscribe ack) are
// ignored, except that an error response to our subscribe surfaces as a
// synthetic error frame so the consumer sees the failure.
func (s *JobStream) readLoop(ch chan<- JobEvent, subID uint64) {
	defer close(ch)
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
		if raw.Method == "job-event" {
			var ev JobEvent
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
			case ch <- JobEvent{Kind: "error", JobID: "", Error: raw.Error.Error()}:
			case <-s.closed:
			}
			return
		}
	}
}
