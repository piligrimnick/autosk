// Package agentnode spawns a Node.js bootstrapper for a custom-runner
// agent package.
//
// The executor talks to the bootstrapper through executor.PiRunner
// semantics, but the underlying process is a one-shot Node child:
//
//   - SendPrompt writes the JSON RunContextSeed to the child's stdin
//     and closes it; the child runs to completion.
//   - WaitForAgentEnd blocks until the Node process exits.
//   - There are no per-turn events to surface, so Events() returns a
//     pre-closed channel.
//   - GetState returns an empty SessionInfo (no pi session here).
//
// The executor sets MaxCorrections=0 for runs that dispatch to a node
// runner, so a missed step_signals row terminates the run after one
// iteration instead of attempting kickback (which can't work — the
// process is gone).
//
// Plan: docs/plans/20260518-Agent-Packages.md §5.2 / §6.
package agentnode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"autosk/internal/daemon/pi"
)

// Opts configures Spawn.
type Opts struct {
	// NodeBin overrides the node binary. Defaults to "node".
	NodeBin string
	// BootstrapPath is the absolute path to the bootstrapper script
	// (dist/bootstrap.js inside @autosk/agent-runtime).
	BootstrapPath string
	// PackageName is the name of the agent package being spawned.
	// Passed to the bootstrapper as --pkg.
	PackageName string
	// RunnerPath is the absolute path to the runner module inside the
	// package install dir. Passed to the bootstrapper as --runner.
	RunnerPath string
	// Cwd is the child's working directory. Must be absolute.
	Cwd string
	// Env, when non-nil, replaces the inherited environment. Use
	// os.Environ + your overrides if you only want to add variables.
	Env []string
	// UseTsxLoader controls whether to add --import tsx so the
	// bootstrapper can require .ts files. Defaults to true; set false
	// if the bootstrapper is shipped as plain JS only.
	UseTsxLoader bool
}

// Runner is one Node bootstrapper subprocess.
//
// All public methods are safe for concurrent use except SendPrompt
// (intended to be called exactly once by the executor).
type Runner struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdinMu sync.Mutex
	stdinOK atomic.Bool

	// stderrBuf accumulates the child's stderr so failures can include
	// it in the error message.
	stderrBuf bytesBuffer

	exitOnce sync.Once
	exitCode int
	exitErr  error
	doneCh   chan struct{}
}

// errWaitTimeout mirrors pi.IsWaitTimeout's sentinel so the executor's
// shutdown helper recognises both.
var errWaitTimeout = errors.New("agentnode: wait timed out before child exited")

// Spawn starts a Node child running the bootstrapper.
func Spawn(ctx context.Context, opts Opts) (*Runner, error) {
	if opts.BootstrapPath == "" {
		return nil, fmt.Errorf("agentnode.Spawn: empty BootstrapPath")
	}
	if opts.PackageName == "" {
		return nil, fmt.Errorf("agentnode.Spawn: empty PackageName")
	}
	if opts.RunnerPath == "" {
		return nil, fmt.Errorf("agentnode.Spawn: empty RunnerPath")
	}
	if _, err := os.Stat(opts.BootstrapPath); err != nil {
		return nil, fmt.Errorf("agentnode.Spawn: bootstrap missing at %s: %w", opts.BootstrapPath, err)
	}
	bin := opts.NodeBin
	if bin == "" {
		bin = "node"
	}
	args := []string{}
	if opts.UseTsxLoader {
		args = append(args, "--import", "tsx")
	}
	args = append(args,
		opts.BootstrapPath,
		"--pkg", opts.PackageName,
		"--runner", opts.RunnerPath,
	)

	cmd := exec.CommandContext(ctx, bin, args...)
	if opts.Cwd != "" {
		if !filepath.IsAbs(opts.Cwd) {
			return nil, fmt.Errorf("agentnode.Spawn: Cwd must be absolute, got %q", opts.Cwd)
		}
		cmd.Dir = opts.Cwd
	}
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	// Inherit stdout so user-visible runner output flows to the daemon's
	// stdout (captured into the transcript by the executor).
	cmd.Stdout = os.Stdout

	r := &Runner{doneCh: make(chan struct{})}
	cmd.Stderr = &r.stderrBuf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	r.cmd = cmd
	r.stdin = stdin
	r.stdinOK.Store(true)
	go r.reap()
	return r, nil
}

// reap waits for the child to exit and stores its status. Runs once
// per process.
func (r *Runner) reap() {
	err := r.cmd.Wait()
	r.exitErr = err
	if r.cmd.ProcessState != nil {
		r.exitCode = r.cmd.ProcessState.ExitCode()
	} else {
		r.exitCode = -1
	}
	close(r.doneCh)
}

// ---- PiRunner-compatible surface -----------------------------------------

// PID returns the Node child's pid, or 0 if the process is gone.
func (r *Runner) PID() int {
	if r.cmd == nil || r.cmd.Process == nil {
		return 0
	}
	return r.cmd.Process.Pid
}

// Events returns a pre-closed channel — node runners have no per-turn
// events the executor needs.
func (r *Runner) Events() <-chan pi.Event {
	c := make(chan pi.Event)
	close(c)
	return c
}

// GetState returns an empty SessionInfo. There's no pi session to
// surface.
func (r *Runner) GetState(_ context.Context) (pi.SessionInfo, error) {
	return pi.SessionInfo{}, nil
}

// SendPrompt writes the supplied payload (expected to be JSON-encoded
// RunContextSeed) to the child's stdin and closes it. Subsequent calls
// are no-ops — the child is one-shot.
func (r *Runner) SendPrompt(_ context.Context, payload string) error {
	r.stdinMu.Lock()
	defer r.stdinMu.Unlock()
	if !r.stdinOK.Load() || r.stdin == nil {
		return nil
	}
	if _, err := io.WriteString(r.stdin, payload); err != nil {
		return fmt.Errorf("write seed: %w", err)
	}
	// Close so the bootstrapper sees EOF and finishes reading.
	r.stdinOK.Store(false)
	err := r.stdin.Close()
	r.stdin = nil
	return err
}

// WaitForAgentEnd blocks until the child exits or ctx is cancelled.
// "agent_end" for a custom runner is "the process exited".
func (r *Runner) WaitForAgentEnd(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.doneCh:
		if r.exitErr != nil {
			return fmt.Errorf("runner exited with error: %w (stderr: %s)", r.exitErr, r.stderrBuf.snapshot())
		}
		return nil
	}
}

// Abort is best-effort: closes stdin so the runner sees EOF, then
// requests SIGTERM. Idempotent.
func (r *Runner) Abort(_ context.Context) error {
	_ = r.CloseStdin()
	return r.Terminate()
}

// CloseStdin closes the child's stdin. Idempotent.
func (r *Runner) CloseStdin() error {
	r.stdinMu.Lock()
	defer r.stdinMu.Unlock()
	if r.stdin == nil {
		return nil
	}
	err := r.stdin.Close()
	r.stdin = nil
	r.stdinOK.Store(false)
	return err
}

// Terminate sends SIGTERM. No-op if the process is gone.
func (r *Runner) Terminate() error {
	if r.cmd == nil || r.cmd.Process == nil {
		return nil
	}
	if err := r.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return nil
}

// Kill sends SIGKILL. No-op if the process is gone.
func (r *Runner) Kill() error {
	if r.cmd == nil || r.cmd.Process == nil {
		return nil
	}
	if err := r.cmd.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return nil
}

// Wait waits for the child to exit (bounded by grace). Returns the
// exit code. Idempotent.
func (r *Runner) Wait(ctx context.Context, grace time.Duration) (int, error) {
	r.exitOnce.Do(func() {
		if grace <= 0 {
			grace = time.Second
		}
		select {
		case <-r.doneCh:
			// Reap goroutine already populated exitErr/exitCode.
		case <-time.After(grace):
			r.exitErr = errWaitTimeout
		case <-ctx.Done():
			r.exitErr = ctx.Err()
		}
	})
	return r.exitCode, r.exitErr
}

// IsWaitTimeout reports whether err matches our wait-timeout sentinel.
func IsWaitTimeout(err error) bool { return errors.Is(err, errWaitTimeout) }

// ---- bytesBuffer ---------------------------------------------------------

// bytesBuffer is a sync-safe append-only sink for the child's stderr.
// Avoids pulling bytes.Buffer mutex semantics into the runner's API.
type bytesBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf = append(b.buf, p...)
	b.mu.Unlock()
	return len(p), nil
}

func (b *bytesBuffer) snapshot() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) > 4096 {
		return string(b.buf[len(b.buf)-4096:])
	}
	return string(b.buf)
}
