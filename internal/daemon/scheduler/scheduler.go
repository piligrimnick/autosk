// Package scheduler is the daemon's job queue + worker pool.
//
// Per plan §6 / docs/plans/20260518-Daemon-UDS-Plan.md §4.2: jobs are
// enqueued as qualified pairs (project, job_id), picked up by a fixed-
// size worker pool, and handed to an Executor that drives the per-project
// runner and transitions the run through MarkRunning → terminal.
//
// The scheduler itself no longer owns the restart-recovery sweep — that
// is now run per-project by internal/daemon/projectmgr when a project is
// opened for the first time. The scheduler also does not own a runstore
// handle (there is no globally-shared one anymore); the queue is project-
// agnostic and only carries opaque qualified ids.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// Job is a qualified job reference. Project is the canonical absolute
// project root (matches projectmgr.Key) and ID is the per-project
// daemon_runs.job_id.
type Job struct {
	Project string
	ID      string
}

// String renders a Job as "<project>::<id>" — used as the internal map
// key and for logging.
func (j Job) String() string { return j.Project + "::" + j.ID }

// Executor is the per-job lifecycle implementation. The daemon wires
// this as a closure that looks up the project in the manager and calls
// proj.Executor.Run(ctx, job.ID). Tests substitute a mock.
//
// Implementations are responsible for the full state machine: MarkRunning,
// MarkDone/MarkFailed/MarkCancelled. The scheduler observes the return
// value only for logging — terminal state must already be persisted.
type Executor interface {
	Run(ctx context.Context, job Job) error
}

// ExecutorFunc adapts a plain function to the Executor interface.
type ExecutorFunc func(ctx context.Context, job Job) error

// Run implements Executor.
func (f ExecutorFunc) Run(ctx context.Context, job Job) error { return f(ctx, job) }

// Config tunes the scheduler.
type Config struct {
	// Workers is the maximum number of concurrent jobs. <= 0 → 1.
	Workers int
	// QueueDepth is the buffered channel size. <= 0 → max(Workers, 16).
	QueueDepth int
	// Logger receives info/warn output. nil → slog.Default().
	Logger *slog.Logger
}

// Scheduler coordinates a worker pool against an Executor.
type Scheduler struct {
	exec Executor
	cfg  Config
	log  *slog.Logger

	queue chan Job

	startMu sync.Mutex
	started bool
	stopped chan struct{}

	cancelCtx context.CancelFunc
	rootCtx   context.Context

	workersWG sync.WaitGroup

	activeMu sync.Mutex
	active   map[string]context.CancelFunc // keyed by Job.String()
}

// Sentinel errors.
var (
	ErrNotStarted     = errors.New("scheduler: not started")
	ErrAlreadyStarted = errors.New("scheduler: already started")
	ErrJobNotActive   = errors.New("scheduler: job not active")
	ErrQueueFull      = errors.New("scheduler: queue full")
)

// New constructs a scheduler. The caller still has to call Start.
//
// The runstore is intentionally absent: restart recovery is owned by the
// project manager which opens its per-project runstore exactly once.
func New(exec Executor, cfg Config) *Scheduler {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.QueueDepth <= 0 {
		if cfg.Workers > 16 {
			cfg.QueueDepth = cfg.Workers
		} else {
			cfg.QueueDepth = 16
		}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		exec:    exec,
		cfg:     cfg,
		log:     log,
		queue:   make(chan Job, cfg.QueueDepth),
		stopped: make(chan struct{}),
		active:  make(map[string]context.CancelFunc),
	}
}

// Start launches the worker pool. Returns ErrAlreadyStarted on the
// second call.
func (s *Scheduler) Start(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return ErrAlreadyStarted
	}
	s.rootCtx, s.cancelCtx = context.WithCancel(context.Background())
	for i := 0; i < s.cfg.Workers; i++ {
		s.workersWG.Add(1)
		go s.workerLoop(i)
	}
	s.started = true
	return nil
}

// Stop drains the queue and waits for in-flight jobs to settle.
//
//  1. Closes the queue so no new jobs are accepted.
//  2. Cancels every active job context (so executors can observe and
//     unwind: Abort → SIGTERM → MarkCancelled).
//  3. Waits for workers to exit, or for graceCtx to expire.
func (s *Scheduler) Stop(graceCtx context.Context) error {
	s.startMu.Lock()
	if !s.started {
		s.startMu.Unlock()
		return ErrNotStarted
	}
	s.started = false
	s.cancelCtx()
	close(s.queue) // workers exit when queue drains
	s.startMu.Unlock()

	// Cancel every active job.
	s.activeMu.Lock()
	for _, cancel := range s.active {
		cancel()
	}
	s.activeMu.Unlock()

	done := make(chan struct{})
	go func() {
		s.workersWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		close(s.stopped)
		return nil
	case <-graceCtx.Done():
		close(s.stopped)
		return graceCtx.Err()
	}
}

// Stopped returns a channel that is closed once Stop completes.
func (s *Scheduler) Stopped() <-chan struct{} { return s.stopped }

// Enqueue places a qualified job in the queue. Returns ErrQueueFull if
// the buffered channel is at capacity (caller is responsible for back
// pressure handling at the HTTP/poller layer).
//
// startMu is held for the duration of the non-blocking send so Stop
// (which closes s.queue while holding the same lock) cannot race the
// send into a closed channel. The send itself is non-blocking, so the
// critical section is bounded by a single channel op + map lookup.
func (s *Scheduler) Enqueue(job Job) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if !s.started {
		return ErrNotStarted
	}
	if job.ID == "" {
		return fmt.Errorf("scheduler: empty job id")
	}
	select {
	case s.queue <- job:
		return nil
	default:
		return ErrQueueFull
	}
}

// Cancel signals the per-job context for the qualified job. The executor
// is expected to observe and unwind. Returns ErrJobNotActive if no such
// job is in flight (the caller can still PATCH the row directly if
// needed; the scheduler does not touch the DB on cancel).
func (s *Scheduler) Cancel(job Job) error {
	s.activeMu.Lock()
	cancel, ok := s.active[job.String()]
	s.activeMu.Unlock()
	if !ok {
		return ErrJobNotActive
	}
	cancel()
	return nil
}

// IsActive reports whether the named job is currently being executed.
func (s *Scheduler) IsActive(job Job) bool {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	_, ok := s.active[job.String()]
	return ok
}

// ActiveJobs returns a snapshot of the currently active jobs.
func (s *Scheduler) ActiveJobs() []Job {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	out := make([]Job, 0, len(s.active))
	for k := range s.active {
		// Decode "<project>::<id>" back into a Job. We do not bother
		// validating — the keys are only ever inserted by runOne.
		if idx := indexOfDoubleColon(k); idx >= 0 {
			out = append(out, Job{Project: k[:idx], ID: k[idx+2:]})
		}
	}
	return out
}

// indexOfDoubleColon finds the first "::" in s, or -1.
func indexOfDoubleColon(s string) int {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == ':' && s[i+1] == ':' {
			return i
		}
	}
	return -1
}

// ActiveCountByProject returns the number of in-flight jobs grouped by
// project key (used for the aggregated health endpoint).
func (s *Scheduler) ActiveCountByProject() map[string]int {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	out := make(map[string]int, len(s.active))
	for k := range s.active {
		if idx := indexOfDoubleColon(k); idx >= 0 {
			out[k[:idx]]++
		}
	}
	return out
}

func (s *Scheduler) workerLoop(idx int) {
	defer s.workersWG.Done()
	for {
		select {
		case <-s.rootCtx.Done():
			return
		case job, ok := <-s.queue:
			if !ok {
				return
			}
			s.runOne(idx, job)
		}
	}
}

func (s *Scheduler) runOne(workerIdx int, job Job) {
	jobCtx, cancel := context.WithCancel(s.rootCtx)
	key := job.String()
	s.activeMu.Lock()
	s.active[key] = cancel
	s.activeMu.Unlock()
	defer func() {
		s.activeMu.Lock()
		delete(s.active, key)
		s.activeMu.Unlock()
		cancel()
	}()

	s.log.Info("scheduler: run start", "worker", workerIdx, "job", key)
	err := s.exec.Run(jobCtx, job)
	if err != nil {
		s.log.Warn("scheduler: executor returned error", "job", key, "err", err)
	} else {
		s.log.Info("scheduler: run finished", "job", key)
	}
}
