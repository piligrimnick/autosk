// Package client is a small, typed Go client for the autosk daemon's
// HTTP-over-UDS API. It is the transport layer of the in-process
// `autosk attach` TUI (internal/attach/tui) and is suitable for any
// other Go program that needs to read SSE / POST input / abort runs
// without re-deriving the wire shapes.
//
// What this package is NOT:
//
//   - It is not a drop-in replacement for the inline daemonClient in
//     cmd/autosk/daemon.go. That code is bound to cobra flags and
//     ad-hoc CLI output formatting; refactoring it is out of scope.
//     The CLI keeps its inline client; this package serves the TUI.
//   - It is not generic over HTTP transports. The constructor sets up
//     a unix-domain-socket dialer; non-UDS use cases would need a
//     different transport plumbing (out of scope today).
//
// All exported methods take a context.Context. Cancelling the
// context cleanly closes the underlying HTTP connection, which for a
// streaming call is also the daemon's signal to release the attach
// counter (see internal/daemon/server/sse.go).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"autosk/internal/daemon/api"
)

// Options configures Client. Zero-value fields fall back to sensible
// defaults so most callers only need Sock (or even just an empty
// Options).
type Options struct {
	// Sock is the unix-domain-socket path of the daemon. Empty falls
	// back to $AUTOSK_SOCK, then to $HOME/.autosk/daemon.sock.
	Sock string
	// Cwd is the project root attached to every request via the
	// X-Autosk-Cwd header. Empty falls back to os.Getwd().
	Cwd string
	// DBOverride is forwarded as X-Autosk-DB when non-empty. The TUI
	// rarely needs this but daemon tests benefit.
	DBOverride string
	// HTTPClient lets tests inject a custom http.Client (e.g. the
	// httptest UDS dialer in attach_test.go). When nil New constructs
	// one with a UDS DialContext and no timeout (SSE streams hold the
	// connection open).
	HTTPClient *http.Client
}

// Client talks to the daemon over a unix-domain socket.
type Client struct {
	sock string
	cwd  string
	db   string
	http *http.Client
}

// New constructs a Client. Returns an error only when the supplied
// Sock/Cwd cannot be absolute-pathed and there is no usable fallback;
// callers can ignore the error if they know all paths are absolute.
func New(opts Options) (*Client, error) {
	sock, err := resolveSock(opts.Sock)
	if err != nil {
		return nil, err
	}
	cwd, err := resolveCwd(opts.Cwd)
	if err != nil {
		return nil, err
	}
	cli := opts.HTTPClient
	if cli == nil {
		cli = newUDSHTTPClient(sock)
	}
	return &Client{
		sock: sock,
		cwd:  cwd,
		db:   opts.DBOverride,
		http: cli,
	}, nil
}

// Sock returns the resolved socket path. Useful for diagnostics.
func (c *Client) Sock() string { return c.sock }

// Cwd returns the resolved project root carried in X-Autosk-Cwd.
func (c *Client) Cwd() string { return c.cwd }

// HTTPClient exposes the underlying http.Client for tests and rare
// advanced callers (e.g. injecting a recording transport).
func (c *Client) HTTPClient() *http.Client { return c.http }

// resolveSock turns an Options.Sock value (possibly empty) into an
// absolute path, honouring the AUTOSK_SOCK env var and the default
// $HOME/.autosk/daemon.sock fallback.
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

// newUDSHTTPClient returns an http.Client wired to a UDS dialer.
// Timeout is zero (no deadline) because /stream is long-lived.
func newUDSHTTPClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 0,
	}
}

// ---- one-shot requests --------------------------------------------------

// GetJob fetches the daemon's current view of a job.
func (c *Client) GetJob(ctx context.Context, jobID string) (api.JobResponse, error) {
	var out api.JobResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/jobs/"+jobID, nil, &out); err != nil {
		return api.JobResponse{}, err
	}
	return out, nil
}

// SendInput posts operator-typed text into the daemon's pi runner.
// behavior may be "" (defaults to "steer" when streaming) or
// "follow_up" to queue the message after the current turn chain.
// Returns the dispatched pi command label ("prompt"|"steer"|
// "follow_up") plus the run id.
func (c *Client) SendInput(ctx context.Context, jobID, message, behavior string) (api.InputResponse, error) {
	var out api.InputResponse
	body := api.InputRequest{Message: message, StreamingBehavior: behavior}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/jobs/"+jobID+"/input", body, &out); err != nil {
		return api.InputResponse{}, err
	}
	return out, nil
}

// Abort tells the daemon to stop the in-flight pi turn for jobID.
// The run row stays non-terminal — cancel via DELETE for that.
func (c *Client) Abort(ctx context.Context, jobID string) (api.AbortResponse, error) {
	var out api.AbortResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/jobs/"+jobID+"/abort", nil, &out); err != nil {
		return api.AbortResponse{}, err
	}
	return out, nil
}

// doJSON executes a request with X-Autosk-Cwd attached and decodes a
// JSON response. Non-2xx replies are surfaced as APIError so callers
// can switch on status codes (e.g. 409 = "runner not registered yet").
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.do(ctx, method, path, body, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return decodeAPIError(resp.StatusCode, buf)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(buf, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

// do is the low-level request builder. extraHeaders is for the SSE
// path (Last-Event-ID). Body is JSON-marshalled when non-nil.
func (c *Client) do(ctx context.Context, method, path string, body any, extraHeaders http.Header) (*http.Response, error) {
	var br io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		br = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://autosk"+path, br)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cwd != "" {
		req.Header.Set("X-Autosk-Cwd", c.cwd)
	}
	if c.db != "" {
		req.Header.Set("X-Autosk-DB", c.db)
	}
	for k, vv := range extraHeaders {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	return c.http.Do(req)
}

// APIError is a structured daemon error: HTTP status, the human
// message, and the optional details map. Use errors.As to extract.
type APIError struct {
	Status  int
	Message string
	Details map[string]any
}

// Error implements error. Format mirrors the inline daemonClient's
// formatter so end-user output stays consistent.
func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("daemon (HTTP %d): %s", e.Status, e.Message)
	}
	return fmt.Sprintf("daemon (HTTP %d)", e.Status)
}

// IsAPIError reports whether err is (or wraps) an APIError and, when
// it is, hands the typed value to the caller. Tiny helper to keep
// switch-on-status code idiomatic.
func IsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

func decodeAPIError(status int, body []byte) error {
	var env api.ErrorResponse
	_ = json.Unmarshal(body, &env)
	if env.Error == "" {
		// Non-JSON or empty envelope — surface raw text.
		env.Error = string(body)
	}
	return &APIError{Status: status, Message: env.Error, Details: env.Details}
}

// streamRetryDelay is exposed for the reconnect-loop-free TUI tests
// (we don't implement reconnect in v1, but the variable is kept here
// so a future iteration has a single knob to flip).
//
//nolint:unused // kept as the documented extension point.
var streamRetryDelay = time.Second
