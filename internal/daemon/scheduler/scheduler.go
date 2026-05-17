// Package scheduler is the daemon's job queue + worker pool.
//
// Per plan §6: jobs are enqueued by job_id, picked up by a fixed-size
// worker pool, and handed to an Executor that drives the pi child and
// transitions the run through MarkRunning → terminal. The scheduler
// itself never touches pi directly; it only owns the queue, the
// per-job cancellation contexts, and the restart-recovery sweep.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"autosk/internal/daemon/runstore"
)

// Executor is the per-job lifecycle implementation. The default executor
// (package pirunner, wired up in cmd/autosk/daemon.go) spawns pi and runs
// the closure-verification loop. Tests substitute a mock.
//
// Implementations are responsible for the full state machine: MarkRunning,
// MarkDone/MarkFailed/MarkCancelled. The scheduler observes the return
// value only for logging — terminal state must already be persisted.
type Executor interface {
	Run(ctx context.Context, jobID string) error
}

// ExecutorFunc adapts a plain function to the Executor interface.
type ExecutorFunc func(ctx context.Context, jobID string) error

// Run implements Executor.
func (f ExecutorFunc) Run(ctx context.Context, jobID string) error { return f(ctx, jobID) }

// Config tunes the scheduler.
type Config struct {
	// Workers is the maximum number of concurrent jobs. <= 0 → 1.
	Workers int
	// QueueDepth is the buffered channel size. <= 0 → max(Workers, 16).
	QueueDepth int
	// Logger receives info/warn output. nil → slog.Default().
	Logger *slog.Logger
}

// Scheduler coordinates a worker pool against a runstore.
type Scheduler struct {
	runs *runstore.Store
	exec Executor
	cfg  Config
	log  *slog.Logger

	queue chan string

	startMu sync.Mutex
	started bool
	stopped chan struct{}

	cancelCtx context.CancelFunc
	rootCtx   context.Context

	workersWG sync.WaitGroup

	activeMu sync.Mutex
	active   map[string]context.CancelFunc
}

// Sentinel errors.
var (
	ErrNotStarted     = errors.New("scheduler: not started")
	ErrAlreadyStarted = errors.New("scheduler: already started")
	ErrJobNotActive   = errors.New("scheduler: job not active")
	ErrQueueFull      = errors.New("scheduler: queue full")
)

// New constructs a scheduler. The caller still has to call Start.
func New(runs *runstore.Store, exec Executor, cfg Config) *Scheduler {
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
		runs:    runs,
		exec:    exec,
		cfg:     cfg,
		log:     log,
		queue:   make(chan string, cfg.QueueDepth),
		stopped: make(chan struct{}),
		active:  make(map[string]context.CancelFunc),
	}
}

// Start runs the restart-recovery sweep and launches the worker pool.
// Returns ErrAlreadyStarted on the second call.
func (s *Scheduler) Start(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return ErrAlreadyStarted
	}
	n, err := s.runs.SweepRunningOnStartup(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: sweep on startup: %w", err)
	}
	if n > 0 {
		s.log.Warn("scheduler: rewrote stale running rows on startup", "count", n)
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
// 1. Closes the queue so no new jobs are accepted.
// 2. Cancels every active job context (so executors can observe and
//    unwind: Abort → SIGTERM → MarkCancelled).
// 3. Waits for workers to exit, or for graceCtx to expire.
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

// Enqueue places jobID in the queue. Returns ErrQueueFull if the buffered
// channel is at capacity (caller is responsible for backpressure handling
// at the HTTP layer).
func (s *Scheduler) Enqueue(jobID string) error {
	s.startMu.Lock()
	started := s.started
	s.startMu.Unlock()
	if !started {
		return ErrNotStarted
	}
	select {
	case s.queue <- jobID:
		return nil
	default:
		return ErrQueueFull
	}
}

// Cancel signals the per-job context for jobID. The executor is expected
// to observe and unwind. Returns ErrJobNotActive if no such job is in
// flight (the caller can still PATCH the row directly if needed; the
// scheduler does not touch the DB on cancel).
func (s *Scheduler) Cancel(jobID string) error {
	s.activeMu.Lock()
	cancel, ok := s.active[jobID]
	s.activeMu.Unlock()
	if !ok {
		return ErrJobNotActive
	}
	cancel()
	return nil
}

// IsActive reports whether the named job is currently being executed.
func (s *Scheduler) IsActive(jobID string) bool {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	_, ok := s.active[jobID]
	return ok
}

// ActiveJobs returns a snapshot of the currently active job ids.
func (s *Scheduler) ActiveJobs() []string {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	out := make([]string, 0, len(s.active))
	for id := range s.active {
		out = append(out, id)
	}
	return out
}

func (s *Scheduler) workerLoop(idx int) {
	defer s.workersWG.Done()
	for {
		select {
		case <-s.rootCtx.Done():
			return
		case jobID, ok := <-s.queue:
			if !ok {
				return
			}
			s.runOne(idx, jobID)
		}
	}
}

func (s *Scheduler) runOne(workerIdx int, jobID string) {
	jobCtx, cancel := context.WithCancel(s.rootCtx)
	s.activeMu.Lock()
	s.active[jobID] = cancel
	s.activeMu.Unlock()
	defer func() {
		s.activeMu.Lock()
		delete(s.active, jobID)
		s.activeMu.Unlock()
		cancel()
	}()

	s.log.Info("scheduler: run start", "worker", workerIdx, "job", jobID)
	err := s.exec.Run(jobCtx, jobID)
	if err != nil {
		s.log.Warn("scheduler: executor returned error", "job", jobID, "err", err)
	} else {
		s.log.Info("scheduler: run finished", "job", jobID)
	}
}
