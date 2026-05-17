package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Opts configures Spawn.
type Opts struct {
	// PIBin is the binary to run. Default: "pi" (resolved via PATH).
	PIBin string
	// Cwd is the child's working directory. Must be absolute.
	Cwd string
	// Model is passed as `--model` when non-empty.
	Model string
	// Thinking is passed as `--thinking` when non-empty.
	Thinking string
	// SessionDir is passed as `--session-dir` when non-empty.
	SessionDir string
	// ExtraArgs is appended to the argv after the daemon-managed flags.
	ExtraArgs []string
	// Env, when non-nil, replaces the inherited environment. Use os.Environ
	// + your overrides if you only want to add variables.
	Env []string
	// EventBuffer sizes the Events() channel. Default 256.
	EventBuffer int
}

// ErrUnknownResponse is returned when a response references an id we did not
// send. Indicates a bug or version skew.
var ErrUnknownResponse = errors.New("pi: response id not registered")

// Runner manages one pi child process.
type Runner struct {
	opts    Opts
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser

	encMu sync.Mutex // protects writes to stdin

	events chan Event

	mu       sync.Mutex
	pending  map[string]chan Response
	closed   bool
	readErr  error
	readDone chan struct{}

	// turnEnds receives a token every time agent_end is observed.
	turnEnds chan struct{}

	// nextID is incremented for every outgoing command lacking an explicit id.
	nextID atomic.Uint64

	waitOnce sync.Once
	waitErr  error
	exitCode int
}

// Spawn starts a new pi child in `--mode rpc`.
//
// The returned Runner has a reader goroutine attached; consumers must
// either drain Events() or accept events may be dropped past the buffer
// capacity (default 256). Use Wait or Terminate/Kill to clean up.
func Spawn(ctx context.Context, opts Opts) (*Runner, error) {
	bin := opts.PIBin
	if bin == "" {
		bin = "pi"
	}
	args := []string{"--mode", "rpc"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Thinking != "" {
		args = append(args, "--thinking", opts.Thinking)
	}
	if opts.SessionDir != "" {
		args = append(args, "--session-dir", opts.SessionDir)
	}
	args = append(args, opts.ExtraArgs...)

	cmd := exec.CommandContext(ctx, bin, args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}

	bufSize := opts.EventBuffer
	if bufSize <= 0 {
		bufSize = 256
	}
	r := &Runner{
		opts:     opts,
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
		events:   make(chan Event, bufSize),
		pending:  make(map[string]chan Response),
		readDone: make(chan struct{}),
		turnEnds: make(chan struct{}, 32),
	}
	// Drain stderr to a sink so the pipe doesn't fill and block pi.
	go drainPipe(stderr)
	go r.readLoop()
	return r, nil
}

// PID returns the pi child's pid, or 0 if the process is gone.
func (r *Runner) PID() int {
	if r.cmd == nil || r.cmd.Process == nil {
		return 0
	}
	return r.cmd.Process.Pid
}

// Events streams normalised events from pi's stdout. Closed by the reader
// when stdout EOFs.
func (r *Runner) Events() <-chan Event { return r.events }

// SendCommand encodes c onto stdin. If c.ID is empty it is assigned a fresh
// monotonic id and the registered response channel is returned. The channel
// receives at most one value; on read error it is closed without value.
func (r *Runner) SendCommand(c Command) (<-chan Response, error) {
	if c.ID == "" {
		c.ID = fmt.Sprintf("d%d", r.nextID.Add(1))
	}
	ch := make(chan Response, 1)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("pi: runner closed")
	}
	r.pending[c.ID] = ch
	r.mu.Unlock()

	buf, err := json.Marshal(c)
	if err != nil {
		r.removePending(c.ID)
		return nil, fmt.Errorf("marshal command: %w", err)
	}
	r.encMu.Lock()
	defer r.encMu.Unlock()
	if _, err := r.stdin.Write(append(buf, '\n')); err != nil {
		r.removePending(c.ID)
		return nil, fmt.Errorf("write stdin: %w", err)
	}
	return ch, nil
}

// awaitResponse blocks until the response for the registered channel
// arrives, or ctx is done, or the reader closes.
func awaitResponse(ctx context.Context, ch <-chan Response) (Response, error) {
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return Response{}, errors.New("pi: response channel closed before reply")
		}
		return resp, nil
	}
}

// SendPrompt is a typed helper around SendCommand for `prompt`. It returns
// when pi acks preflight; the actual run completion is signalled by an
// agent_end event (use WaitForAgentEnd).
func (r *Runner) SendPrompt(ctx context.Context, message string) error {
	ch, err := r.SendCommand(Command{Type: "prompt", Message: message})
	if err != nil {
		return err
	}
	resp, err := awaitResponse(ctx, ch)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("pi prompt rejected: %s", resp.Error)
	}
	return nil
}

// GetState returns the current SessionInfo via `get_state`.
func (r *Runner) GetState(ctx context.Context) (SessionInfo, error) {
	ch, err := r.SendCommand(Command{Type: "get_state"})
	if err != nil {
		return SessionInfo{}, err
	}
	resp, err := awaitResponse(ctx, ch)
	if err != nil {
		return SessionInfo{}, err
	}
	if !resp.Success {
		return SessionInfo{}, fmt.Errorf("pi get_state failed: %s", resp.Error)
	}
	var info SessionInfo
	if err := json.Unmarshal(resp.Data, &info); err != nil {
		return SessionInfo{}, fmt.Errorf("decode session info: %w", err)
	}
	return info, nil
}

// Abort sends `{type:"abort"}` to pi to stop the current run. Returns when
// pi acknowledges or ctx is done.
func (r *Runner) Abort(ctx context.Context) error {
	ch, err := r.SendCommand(Command{Type: "abort"})
	if err != nil {
		return err
	}
	resp, err := awaitResponse(ctx, ch)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("pi abort failed: %s", resp.Error)
	}
	return nil
}

// WaitForAgentEnd blocks until the reader observes the next agent_end
// event, or ctx is done, or the reader exits.
func (r *Runner) WaitForAgentEnd(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.turnEnds:
		return nil
	case <-r.readDone:
		// Reader exited before agent_end. Surface its error if any.
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.readErr != nil {
			return r.readErr
		}
		return io.EOF
	}
}

// CloseStdin closes pi's stdin, asking it to shut down cleanly.
func (r *Runner) CloseStdin() error {
	if r.stdin == nil {
		return nil
	}
	err := r.stdin.Close()
	r.stdin = nil
	return err
}

// Terminate sends SIGTERM to the child. No-op if already gone.
func (r *Runner) Terminate() error {
	if r.cmd == nil || r.cmd.Process == nil {
		return nil
	}
	return r.cmd.Process.Signal(syscall.SIGTERM)
}

// Kill sends SIGKILL to the child. No-op if already gone.
func (r *Runner) Kill() error {
	if r.cmd == nil || r.cmd.Process == nil {
		return nil
	}
	return r.cmd.Process.Kill()
}

// Wait waits for the child to exit. If it does not exit within grace,
// returns the wait error without forcibly killing — callers should call
// Kill explicitly. Idempotent.
func (r *Runner) Wait(ctx context.Context, grace time.Duration) (int, error) {
	r.waitOnce.Do(func() {
		done := make(chan error, 1)
		go func() { done <- r.cmd.Wait() }()
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case err := <-done:
			r.waitErr = err
		case <-timer.C:
			r.waitErr = errWaitTimeout
		case <-ctx.Done():
			r.waitErr = ctx.Err()
		}
		if r.cmd.ProcessState != nil {
			r.exitCode = r.cmd.ProcessState.ExitCode()
		} else {
			r.exitCode = -1
		}
		// Best-effort: close stdin/stdout so any blocking reader can exit.
		_ = r.CloseStdin()
		<-r.readDone
	})
	return r.exitCode, r.waitErr
}

var errWaitTimeout = errors.New("pi: wait timed out before child exited")

// IsWaitTimeout reports whether err is the wait-timeout sentinel.
func IsWaitTimeout(err error) bool { return errors.Is(err, errWaitTimeout) }

// ---- reader loop ---------------------------------------------------------

func (r *Runner) readLoop() {
	defer close(r.readDone)
	defer close(r.events)
	scanner := bufio.NewScanner(r.stdout)
	// Long lines tolerated: 1 MiB. pi rarely emits anything close to that.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Copy so the buffer reuse in Scanner doesn't bite us.
		raw := append([]byte(nil), line...)
		var msg inboundMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			// Malformed line — treat as KindOther but log via stderr is too
			// noisy; preserve the raw bytes in the event payload.
			r.emit(Event{Kind: KindOther, Raw: raw, ReceivedAt: time.Now().UTC()})
			continue
		}
		r.handle(msg, raw)
	}
	r.mu.Lock()
	r.readErr = scanner.Err()
	r.closed = true
	// Fail any still-pending responses.
	for id, ch := range r.pending {
		close(ch)
		delete(r.pending, id)
	}
	r.mu.Unlock()
}

func (r *Runner) handle(msg inboundMessage, raw []byte) {
	now := time.Now().UTC()
	kind := classify(msg.Type)

	switch kind {
	case KindResponse:
		success := msg.Success != nil && *msg.Success
		resp := Response{
			ID:      msg.ID,
			Command: msg.Command,
			Success: success,
			Error:   msg.Error,
			Data:    msg.Data,
		}
		r.deliverResponse(resp)
		// Also surface to events so consumers can log it.
		r.emit(Event{Kind: kind, Raw: raw, ReceivedAt: now})
	case KindExtensionRequest:
		// Reply with cancellation for blocking dialogs so the agent never
		// hangs in headless mode. Fire-and-forget methods need no reply.
		r.replyToExtensionUI(msg, raw)
		r.emit(Event{Kind: kind, Raw: raw, ReceivedAt: now})
	case KindAgentEnd:
		// Push the event, then notify any waiter.
		r.emit(Event{Kind: kind, Raw: raw, ReceivedAt: now})
		select {
		case r.turnEnds <- struct{}{}:
		default:
			// Buffer full: drop the signal. WaitForAgentEnd waiters can
			// still observe the agent_end event itself; in practice the
			// 32-slot buffer is far more than the daemon needs.
		}
	default:
		r.emit(Event{Kind: kind, Raw: raw, ReceivedAt: now})
	}
}

func (r *Runner) emit(e Event) {
	// Non-blocking send so a slow consumer can't stall the reader.
	select {
	case r.events <- e:
	default:
	}
}

func (r *Runner) deliverResponse(resp Response) {
	r.mu.Lock()
	ch, ok := r.pending[resp.ID]
	if ok {
		delete(r.pending, resp.ID)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	ch <- resp
	close(ch)
}

func (r *Runner) removePending(id string) {
	r.mu.Lock()
	if ch, ok := r.pending[id]; ok {
		close(ch)
		delete(r.pending, id)
	}
	r.mu.Unlock()
}

// replyToExtensionUI dispatches our canned response back on stdin.
func (r *Runner) replyToExtensionUI(msg inboundMessage, raw []byte) {
	if msg.ID == "" {
		return
	}
	// Fire-and-forget methods need no reply.
	switch msg.Method {
	case "notify", "setStatus", "setWidget", "setTitle", "set_editor_text":
		return
	}
	resp := ExtensionUIResponse{
		Type:      "extension_ui_response",
		ID:        msg.ID,
		Cancelled: true,
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return
	}
	r.encMu.Lock()
	defer r.encMu.Unlock()
	if r.stdin == nil {
		return
	}
	_, _ = r.stdin.Write(append(buf, '\n'))
}

// drainPipe reads from r and discards. Best-effort; exits on first error or EOF.
func drainPipe(rc io.ReadCloser) {
	defer rc.Close()
	buf := make([]byte, 4096)
	for {
		_, err := rc.Read(buf)
		if err != nil {
			return
		}
	}
}

// processWasSignaled reports whether the wait error indicates the process
// was killed by a signal (useful for cancel paths).
func processWasSignaled(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			return ws.Signaled()
		}
	}
	return false
}

// runtimePidString is a small helper for log lines — kept here so callers
// don't import strconv just for the pid.
func runtimePidString(pid int) string {
	if pid <= 0 {
		return "?"
	}
	return strconv.Itoa(pid)
}

// processExists reports whether the running process is still around.
func processExists(p *os.Process) bool {
	if p == nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
