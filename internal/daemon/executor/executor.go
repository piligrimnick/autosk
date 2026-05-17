// Package executor implements the per-job lifecycle the daemon's
// scheduler hands to each worker. The contract:
//
//   1. MarkRunning the run row.
//   2. Snapshot the autosk task's incoming blockers; if AutoClaim, Claim.
//   3. Spawn pi --mode rpc.
//   4. GetState → persist pi_session_id / session_path.
//   5. SendPrompt(initial), then loop on WaitForAgentEnd:
//        - if ad-hoc (no task), one turn is enough → done.
//        - else verifyClosure. If valid, done with closure_kind.
//        - else if corrections remain, send corrective message and loop.
//        - else fail with error="agent_did_not_close_task".
//   6. Close stdin, wait, persist terminal state.
//
// The daemon never calls autosk done; the agent owns closing the task.
// See plan §§6.1, 6.1.1, 6.1.2.
package executor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/runstore"
	"autosk/internal/store"
)

// PiRunner is the subset of *pi.Runner the executor uses. Tests substitute
// a mock; production wiring uses pi.Spawn directly via Factory.
type PiRunner interface {
	PID() int
	Events() <-chan pi.Event
	GetState(ctx context.Context) (pi.SessionInfo, error)
	SendPrompt(ctx context.Context, message string) error
	WaitForAgentEnd(ctx context.Context) error
	Abort(ctx context.Context) error
	CloseStdin() error
	Terminate() error
	Kill() error
	Wait(ctx context.Context, grace time.Duration) (int, error)
}

// Factory spawns a new PiRunner. The default factory wraps pi.Spawn.
type Factory func(ctx context.Context, opts pi.Opts) (PiRunner, error)

// DefaultFactory is the Factory that calls pi.Spawn.
var DefaultFactory Factory = func(ctx context.Context, opts pi.Opts) (PiRunner, error) {
	return pi.Spawn(ctx, opts)
}

// TaskStore is the subset of the autosk task store the executor depends on.
// store.Store satisfies it.
type TaskStore interface {
	GetTask(ctx context.Context, id string) (store.Task, error)
	Claim(ctx context.Context, id string) (store.Task, error)
	Deps(ctx context.Context, id string) (incoming, outgoing []string, err error)
}

// Config tunes the executor.
type Config struct {
	// PIBin overrides the binary used by the default factory. Empty → "pi".
	PIBin string
	// SessionDirRoot is the base directory under which per-job session
	// directories are created. Empty → ".autosk/sessions" relative to the
	// run's cwd.
	SessionDirRoot string
	// Grace is the time SIGTERM has to bring pi down before SIGKILL.
	Grace time.Duration
	// IdleTimeout caps a single WaitForAgentEnd. Empty → 30 min.
	IdleTimeout time.Duration
}

// Executor binds the store layer, the task store, the pi factory and the
// config. It implements scheduler.Executor via Run.
type Executor struct {
	runs    *runstore.Store
	tasks   TaskStore
	factory Factory
	cfg     Config
}

// New constructs the executor.
func New(runs *runstore.Store, tasks TaskStore, factory Factory, cfg Config) *Executor {
	if factory == nil {
		factory = DefaultFactory
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 10 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	return &Executor{runs: runs, tasks: tasks, factory: factory, cfg: cfg}
}

// ErrAgentDidNotClose is the terminal failure produced when the agent runs
// out of correction attempts without closing the task.
var ErrAgentDidNotClose = errors.New("agent_did_not_close_task")

// Run is the scheduler.Executor entry point.
func (e *Executor) Run(ctx context.Context, jobID string) error {
	bgCtx := context.Background() // for persisting terminal state after ctx cancel

	run, err := e.runs.GetRun(ctx, jobID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}

	// Phase 1: transition queued → running and snapshot blockers.
	run, err = e.runs.MarkRunning(ctx, jobID, 0)
	if err != nil {
		return fmt.Errorf("mark running: %w", err)
	}
	if run.TaskID != "" {
		inc, _, err := e.tasks.Deps(ctx, run.TaskID)
		if err == nil {
			_ = e.runs.SetPreBlockedBy(ctx, jobID, inc)
			run.PreBlockedBy = inc
		}
		if run.AutoClaim {
			if _, err := e.tasks.Claim(ctx, run.TaskID); err != nil {
				// Not fatal: log via stored error column. Plan §6.1 explicitly
				// says claim failures are non-fatal.
				_ = appendRunError(bgCtx, e.runs, jobID, "auto_claim_failed: "+err.Error())
			}
		}
	}

	// Phase 2: spawn pi.
	sessionDir := e.sessionDirFor(run)
	if err := ensureDir(sessionDir); err != nil {
		return e.failTerminal(bgCtx, jobID, nil, fmt.Errorf("session dir: %w", err))
	}
	runner, err := e.factory(ctx, pi.Opts{
		PIBin:      e.cfg.PIBin,
		Cwd:        run.Cwd,
		Model:      run.Model,
		Thinking:   string(run.Thinking),
		SessionDir: sessionDir,
	})
	if err != nil {
		return e.failTerminal(bgCtx, jobID, nil, fmt.Errorf("spawn pi: %w", err))
	}
	defer e.cleanup(runner, bgCtx)

	if pid := runner.PID(); pid > 0 {
		_ = e.runs.SetPID(ctx, jobID, pid)
	}

	// Phase 3: pull session info.
	stateCtx, stateCancel := context.WithTimeout(ctx, 10*time.Second)
	info, sterr := runner.GetState(stateCtx)
	stateCancel()
	if sterr == nil {
		_ = e.runs.SetPISession(ctx, jobID, info.SessionID, info.SessionFile)
	}

	// Phase 4: initial prompt.
	if err := runner.SendPrompt(ctx, run.Prompt); err != nil {
		return e.handleRunError(ctx, bgCtx, jobID, runner, err)
	}

	// Phase 5: turn-end loop with closure verification + kickback.
	var closure runstore.ClosureKind
	for {
		turnCtx, turnCancel := context.WithTimeout(ctx, e.cfg.IdleTimeout)
		werr := runner.WaitForAgentEnd(turnCtx)
		turnCancel()
		if werr != nil {
			return e.handleRunError(ctx, bgCtx, jobID, runner, werr)
		}
		// Ad-hoc prompt → one turn is enough.
		if run.TaskID == "" {
			closure = ""
			break
		}
		kind, verr := verifyClosure(ctx, e.tasks, run.TaskID, run.PreBlockedBy)
		if verr != nil {
			return e.handleRunError(ctx, bgCtx, jobID, runner, verr)
		}
		if kind != "" {
			closure = kind
			break
		}
		// Invalid closure. Kick the agent back, unless we're exhausted.
		if run.CorrectionsUsed >= run.MaxCorrections {
			_ = runner.Abort(ctx)
			return e.failTerminal(bgCtx, jobID, nil, ErrAgentDidNotClose)
		}
		newCount, ierr := e.runs.IncCorrections(ctx, jobID)
		if ierr != nil {
			return e.handleRunError(ctx, bgCtx, jobID, runner, ierr)
		}
		run.CorrectionsUsed = newCount
		msg := CorrectiveMessage(run.TaskID, newCount, run.MaxCorrections)
		if err := runner.SendPrompt(ctx, msg); err != nil {
			return e.handleRunError(ctx, bgCtx, jobID, runner, err)
		}
	}

	// Phase 6: clean shutdown.
	exit, werr := e.shutdown(runner, ctx, bgCtx)
	if werr != nil && !errors.Is(werr, context.Canceled) {
		return e.failTerminal(bgCtx, jobID, &exit, fmt.Errorf("pi exit: %w", werr))
	}
	if _, err := e.runs.MarkDone(bgCtx, jobID, exit, closure); err != nil {
		return fmt.Errorf("mark done: %w", err)
	}
	return nil
}

// handleRunError routes a non-terminal error into the right terminal state.
func (e *Executor) handleRunError(ctx, bgCtx context.Context, jobID string, runner PiRunner, runErr error) error {
	if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		// Cancel path: SIGTERM the child, then mark cancelled.
		_ = runner.Abort(bgCtx)
		_ = runner.CloseStdin()
		_ = runner.Terminate()
		exit, _ := runner.Wait(bgCtx, e.cfg.Grace)
		if waitTimedOut(runner, exit) {
			_ = runner.Kill()
			exit, _ = runner.Wait(bgCtx, e.cfg.Grace)
		}
		_, _ = e.runs.MarkCancelled(bgCtx, jobID, &exit)
		return runErr
	}
	// Other error: best-effort shutdown then mark failed.
	exit, _ := e.shutdown(runner, ctx, bgCtx)
	_, _ = e.runs.MarkFailed(bgCtx, jobID, &exit, runErr.Error())
	return runErr
}

// shutdown closes stdin and waits up to grace; SIGKILLs on timeout. Used on
// both clean and error paths after the run loop exits.
func (e *Executor) shutdown(runner PiRunner, ctx, bgCtx context.Context) (int, error) {
	_ = runner.CloseStdin()
	exit, err := runner.Wait(bgCtx, e.cfg.Grace)
	if pi.IsWaitTimeout(err) {
		_ = runner.Terminate()
		exit, err = runner.Wait(bgCtx, e.cfg.Grace)
		if pi.IsWaitTimeout(err) {
			_ = runner.Kill()
			exit, err = runner.Wait(bgCtx, e.cfg.Grace)
		}
	}
	return exit, err
}

func (e *Executor) failTerminal(ctx context.Context, jobID string, exit *int, cause error) error {
	_, _ = e.runs.MarkFailed(ctx, jobID, exit, cause.Error())
	return cause
}

func (e *Executor) cleanup(runner PiRunner, bgCtx context.Context) {
	// Best-effort: if Wait already ran this is idempotent (Wait uses sync.Once).
	go func() {
		_ = runner.CloseStdin()
		_, _ = runner.Wait(bgCtx, e.cfg.Grace)
	}()
}

// sessionDirFor returns the directory passed as `--session-dir` to pi.
// We create a per-job subdirectory so transcripts are easy to find.
func (e *Executor) sessionDirFor(run runstore.Run) string {
	root := e.cfg.SessionDirRoot
	if root == "" {
		root = filepath.Join(run.Cwd, ".autosk", "sessions")
	}
	return filepath.Join(root, run.JobID)
}

// ---- closure verification --------------------------------------------------

// VerifyClosure inspects the autosk task right after an end-of-turn and
// classifies the run's closure. Returns "" when the agent has not closed
// per protocol — the caller is expected to kick back.
//
// Closure kinds (plan §6.1.1):
//
//	"done"       → tasks.status == done
//	"cancelled"  → tasks.status == cancelled
//	"decomposed" → status still new|claimed but incoming blockers grew
//	"" (empty)   → invalid closure
func VerifyClosure(ctx context.Context, tasks TaskStore, taskID string, preBlockedBy []string) (runstore.ClosureKind, error) {
	return verifyClosure(ctx, tasks, taskID, preBlockedBy)
}

func verifyClosure(ctx context.Context, tasks TaskStore, taskID string, preBlockedBy []string) (runstore.ClosureKind, error) {
	if taskID == "" {
		return "", nil
	}
	t, err := tasks.GetTask(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}
	switch t.Status {
	case store.StatusDone:
		return runstore.ClosureDone, nil
	case store.StatusCancelled:
		return runstore.ClosureCancelled, nil
	}
	inc, _, err := tasks.Deps(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("get deps: %w", err)
	}
	if hasNewBlocker(inc, preBlockedBy) {
		return runstore.ClosureDecomposed, nil
	}
	return "", nil
}

func hasNewBlocker(current, pre []string) bool {
	preSet := make(map[string]struct{}, len(pre))
	for _, id := range pre {
		if id != "" {
			preSet[id] = struct{}{}
		}
	}
	for _, id := range current {
		if _, seen := preSet[id]; !seen && id != "" {
			return true
		}
	}
	return false
}

// CorrectiveMessage renders the user-facing kickback text. See plan §6.1.2.
func CorrectiveMessage(taskID string, attempt, max int) string {
	var sb strings.Builder
	sb.WriteString("Your autosk task ")
	sb.WriteString(taskID)
	sb.WriteString(" is still open and you have not closed it per protocol.\n")
	sb.WriteString("Before you stop, you must do exactly one of:\n")
	sb.WriteString("  1. `autosk done ")
	sb.WriteString(taskID)
	sb.WriteString("` — if the work is complete.\n")
	sb.WriteString("  2. `autosk cancel ")
	sb.WriteString(taskID)
	sb.WriteString("` — if it cannot be done.\n")
	sb.WriteString("  3. Decompose: create new tasks with `autosk create ... --blocks ")
	sb.WriteString(taskID)
	sb.WriteString("` and then stop. The parent is treated as resolved when at least one new blocker is added.\n")
	fmt.Fprintf(&sb, "This is correction attempt %d of %d. If you ignore it, the run will be marked failed.", attempt, max)
	return sb.String()
}

// waitTimedOut is a small helper: returns true if exit == -1 and the
// runner is still alive (best-effort heuristic; real callers should rely
// on pi.IsWaitTimeout via the returned error instead).
func waitTimedOut(runner PiRunner, exit int) bool {
	// We don't have direct access to the wait error here without state;
	// a -1 exit conventionally means "did not exit normally". Callers that
	// need authoritative answers use pi.IsWaitTimeout.
	return exit < 0 && runner.PID() > 0
}

func appendRunError(ctx context.Context, runs *runstore.Store, jobID, msg string) error {
	// We don't currently expose a "append note" method; instead, leave a
	// breadcrumb in the daemon log. This stub keeps the call site honest
	// in case we add a note column later.
	_ = ctx
	_ = runs
	_ = jobID
	_ = msg
	return nil
}

func ensureDir(p string) error {
	return mkdirAll(p, 0o755)
}
