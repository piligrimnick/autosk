// Package rpcclient is the Go JSON-RPC client for autoskd (plan §4.1, §7.8).
//
// autoskd is the Rust daemon that owns .autosk/db; the Go CLI and lazy TUI talk
// to it over a single line-delimited JSON-RPC surface on a unix-domain socket.
// This package is the transport (one JSON object per line) plus the
// language-server-style auto-spawn connector: a caller that finds no daemon
// transparently spawns autoskd and waits for it to come up (see connector.go).
//
// Phase 1 exposes the READ surface (the methods the read-only lazy datasource
// needs). Writes, live job.subscribe tailing and TCP/auth land in later phases.
package rpcclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
)

// Options configures a Client. Zero-value fields fall back to sensible defaults
// so most callers only set Cwd (and rely on $AUTOSK_SOCK / the default path).
type Options struct {
	// Sock is the daemon UDS path. Empty → $AUTOSK_SOCK → ~/.autosk/daemon.sock.
	Sock string
	// Cwd is the project root carried in every request's selector. Empty →
	// os.Getwd().
	Cwd string
	// DBOverride is forwarded as the db_path selector when non-empty.
	DBOverride string
	// AutoskdBin overrides autoskd binary resolution for auto-spawn (else
	// $AUTOSKD_BIN, then alongside the calling binary, then PATH).
	AutoskdBin string
	// NoAutoSpawn disables the connect-or-spawn behaviour (connect only).
	NoAutoSpawn bool
}

// Client is a JSON-RPC client bound to one project selector (cwd/db).
type Client struct {
	conn *Connector
	cwd  string
	db   string
	id   atomic.Uint64
}

// New constructs a Client, resolving the socket path and cwd.
func New(opts Options) (*Client, error) {
	sock, err := resolveSock(opts.Sock)
	if err != nil {
		return nil, err
	}
	cwd, err := resolveCwd(opts.Cwd)
	if err != nil {
		return nil, err
	}
	return &Client{
		conn: &Connector{sock: sock, bin: opts.AutoskdBin, noSpawn: opts.NoAutoSpawn},
		cwd:  cwd,
		db:   opts.DBOverride,
	}, nil
}

// Sock returns the resolved socket path.
func (c *Client) Sock() string { return c.conn.sock }

// Cwd returns the project root carried in every request's selector.
func (c *Client) Cwd() string { return c.cwd }

// rpcRequest / rpcResponse mirror autosk-proto's envelope.
type rpcRequest struct {
	ID     uint64 `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RPCError is a structured daemon error (mirrors autosk-proto ErrorObject).
type RPCError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("autoskd rpc error %d: %s", e.Code, e.Message)
}

// Error codes mirrored from autosk-proto::rpc::error_codes.
const (
	CodeMethodNotFound  = -32601
	CodeProjectNotFound = 1001
	CodeInvalidProject  = 1002
	CodeNotFound        = 1003
	// CodeConflict marks a retryable "exists but not ready / not applicable"
	// condition (terminal run, or a queued run whose live runner has not
	// spawned yet). Mirrors the daemon's former HTTP 409.
	CodeConflict = 1004
)

// call dials the daemon (auto-spawning if needed), sends one request, and
// decodes the single response line. A fresh connection per call keeps the
// transport stateless and sidesteps response-correlation across concurrent
// callers; locally the connect cost is negligible.
func (c *Client) call(ctx context.Context, method string, params map[string]any, out any) error {
	conn, err := c.conn.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	req := rpcRequest{ID: c.id.Add(1), Method: method, Params: params}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("autoskd %s: write: %w", method, err)
	}
	var resp rpcResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("autoskd %s: read: %w", method, err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("autoskd %s: decode result: %w", method, err)
		}
	}
	return nil
}

// selector returns a params map carrying the project selector, merged with any
// method-specific extras.
func (c *Client) selector(extra map[string]any) map[string]any {
	p := make(map[string]any, len(extra)+2)
	if c.cwd != "" {
		p["cwd"] = c.cwd
	}
	if c.db != "" {
		p["db_path"] = c.db
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// IsAPIError reports whether err is (or wraps) an RPCError.
func IsAPIError(err error) (*RPCError, bool) {
	var e *RPCError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// resolveSock mirrors internal/daemon/client.resolveSock: explicit → $AUTOSK_SOCK
// → ~/.autosk/daemon.sock, absolute-pathed.
func resolveSock(s string) (string, error) {
	if s == "" {
		if env := os.Getenv("AUTOSK_SOCK"); env != "" {
			s = env
		}
	}
	if s == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve sock: %w", err)
		}
		s = filepath.Join(home, ".autosk", "daemon.sock")
	}
	abs, err := filepath.Abs(s)
	if err != nil {
		return "", fmt.Errorf("resolve sock: %w", err)
	}
	return abs, nil
}

func resolveCwd(c string) (string, error) {
	if c == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
		c = wd
	}
	abs, err := filepath.Abs(c)
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	return abs, nil
}

// dialUnix dials the UDS honouring the context.
func dialUnix(ctx context.Context, sock string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", sock)
}
