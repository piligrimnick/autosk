// Package compactor runs the doltlite chunk-store garbage collector
// on a per-project schedule so the chunk-store's internal WAL never
// grows long enough to dominate query cost.
//
// Background. doltlite is a SQLite fork with a content-addressed
// prolly-tree storage layer. Every write appends new chunks; stale
// chunks are reclaimed only by an explicit `dolt_gc()` call. Until
// that call runs, every cursor open in any process attached to the
// database has to replay the chunk-store WAL on each transaction
// begin (csReplayWal → pread-per-frame + qsort merge). After ~5h of
// daemon activity in a busy project this loop saturates a core in
// the `autosk lazy` dashboard — see docs/daemon.md "100%-CPU lazy"
// for the full diagnosis.
//
// Lifecycle. One Compactor per loaded project, owned by projectmgr.
// Start launches a goroutine that ticks every Interval; Stop cancels
// the ticker and waits for the in-flight compaction (if any) to
// finish, bounded by graceCtx. Concurrency: doltlite is single-
// writer, so we never need to coordinate with the poller or executor
// here — the connection pool (db.SetMaxOpenConns(1)) serialises the
// `SELECT dolt_gc()` call against everything else in the process.
//
// Multi-project daemons get one Compactor per Project; the manager
// runs them in parallel. Tunable via the daemon's --gc-interval flag
// (0 disables; default 30m).
package compactor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"autosk/internal/store/doltlite"
)

// DefaultInterval is the gap between compactions when Config.Interval
// is zero. 30 minutes is a compromise between "the WAL never grows
// large enough to be expensive" and "compaction itself is cheap, so
// don't burn cycles on a quiet project".
const DefaultInterval = 30 * time.Minute

// Config tunes the compactor.
type Config struct {
	// Interval between compactions. ≤ 0 → DefaultInterval. Set
	// explicitly to 0 via the daemon's --gc-interval=0 to disable
	// compaction entirely (use the on-demand `autosk gc` CLI
	// instead).
	Interval time.Duration
	// ProjectKey identifies the project this compactor belongs to.
	// Used purely for log lines so operators can tell projects apart
	// in a multi-project daemon.
	ProjectKey string
	// Logger receives info/warn output. nil → slog.Default().
	Logger *slog.Logger
}

// Compactor runs `SELECT dolt_gc()` on a schedule.
type Compactor struct {
	store *doltlite.Store
	cfg   Config
	log   *slog.Logger

	startMu sync.Mutex
	started bool
	cancel  context.CancelFunc
	doneCh  chan struct{}

	// busy guards against an oddly slow GC overlapping the next
	// tick. Belt-and-suspenders: at 30m intervals against a GC that
	// finishes in <100ms this never trips, but we'd rather skip a
	// tick than queue compactions.
	busyMu sync.Mutex

	// ticks counts successful scheduled tick invocations (regardless
	// of whether they reclaimed anything). Exposed via Ticks() so
	// tests and the `autosk daemon status` introspection can prove
	// the loop is alive.
	ticks atomic.Uint64
}

// Sentinel errors.
var (
	ErrAlreadyStarted = errors.New("compactor: already started")
	ErrNotStarted     = errors.New("compactor: not started")
	ErrDisabled       = errors.New("compactor: disabled (interval=0)")
)

// New constructs a Compactor. cfg.Interval=0 means "disabled" — see
// the Start documentation.
func New(s *doltlite.Store, cfg Config) *Compactor {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Compactor{store: s, cfg: cfg, log: log}
}

// Interval returns the effective tick interval (DefaultInterval when
// the config left it unset). Returns 0 when the compactor has been
// explicitly disabled via NewWithDisabled / cfg.Interval < 0.
func (c *Compactor) Interval() time.Duration {
	if c.cfg.Interval < 0 {
		return 0
	}
	if c.cfg.Interval == 0 {
		return DefaultInterval
	}
	return c.cfg.Interval
}

// Disabled reports whether the compactor was constructed with a
// negative interval (operator opt-out via --gc-interval=-1 or the
// equivalent). Disabled compactors return ErrDisabled from Start so
// the caller can log a one-shot warning instead of silently doing
// nothing.
func (c *Compactor) Disabled() bool { return c.cfg.Interval < 0 }

// Ticks returns the number of successful scheduled compactions. The
// RunOnce path is intentionally excluded so callers can distinguish
// scheduler-driven work from on-demand work.
func (c *Compactor) Ticks() uint64 { return c.ticks.Load() }

// Start launches the compaction loop. The first compaction fires
// after Interval, not immediately: the project has presumably just
// opened and there's nothing to reclaim yet.
//
// Start is a no-op when the compactor was disabled at construction
// (returns ErrDisabled, which callers can ignore). Calling Start
// twice returns ErrAlreadyStarted.
func (c *Compactor) Start(ctx context.Context) error {
	if c.Disabled() {
		return ErrDisabled
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.started {
		return ErrAlreadyStarted
	}
	runCtx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.doneCh = make(chan struct{})
	c.started = true
	go c.loop(runCtx)
	return nil
}

// Stop cancels the compaction loop and waits for it to exit, or for
// graceCtx. Idempotent on already-stopped compactors (returns
// ErrNotStarted, which is safe to ignore in shutdown sequences).
func (c *Compactor) Stop(graceCtx context.Context) error {
	c.startMu.Lock()
	if !c.started {
		c.startMu.Unlock()
		return ErrNotStarted
	}
	c.started = false
	c.cancel()
	doneCh := c.doneCh
	c.startMu.Unlock()
	select {
	case <-doneCh:
		return nil
	case <-graceCtx.Done():
		return graceCtx.Err()
	}
}

// RunOnce fires a single compaction outside the scheduled loop. Used
// by the `autosk gc` CLI subcommand and by tests that want a
// deterministic GC without waiting for the ticker.
//
// Safe to call concurrently with Start; the busy mutex makes
// scheduled and on-demand calls serialise.
func (c *Compactor) RunOnce(ctx context.Context) (doltlite.CompactResult, error) {
	c.busyMu.Lock()
	defer c.busyMu.Unlock()
	return c.store.Compact(ctx)
}

// loop ticks every Interval until ctx is cancelled. The first tick
// is offset by Interval (not 0) so newly-opened projects don't pay
// for compaction during the daemon's startup burst.
func (c *Compactor) loop(ctx context.Context) {
	defer close(c.doneCh)
	t := time.NewTicker(c.Interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

// tick runs one compaction. Errors are logged but never propagated —
// a failed GC is not fatal (we'll retry next tick) and surfaceable
// via the daemon logs.
func (c *Compactor) tick(ctx context.Context) {
	// Skip if a previous tick is still running (shouldn't normally
	// happen — see busy guard rationale on the field).
	if !c.busyMu.TryLock() {
		c.log.Warn("compactor: skipping tick (previous still running)",
			"project", c.cfg.ProjectKey)
		return
	}
	defer c.busyMu.Unlock()
	res, err := c.store.Compact(ctx)
	if err != nil {
		c.log.Warn("compactor: dolt_gc failed",
			"project", c.cfg.ProjectKey, "err", err)
		return
	}
	c.ticks.Add(1)
	c.log.Info("compactor: dolt_gc",
		"project", c.cfg.ProjectKey,
		"removed", res.ChunksRemoved,
		"kept", res.ChunksKept,
		"duration", res.Duration.Round(time.Millisecond))
}
