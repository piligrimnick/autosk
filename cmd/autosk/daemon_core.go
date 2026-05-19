package main

import (
	"context"
	"fmt"
	"time"

	"autosk/internal/agent/pkgregistry"
	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/pirunners"
	"autosk/internal/daemon/projectmgr"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/daemon/server"
)

// daemonCoreConfig bundles the few inputs to buildDaemonCore. Kept in
// this file (and not inlined into newDaemonServeCmd) so a unit test
// can assert the production wiring carries the attach hubs through to
// both the project manager and the HTTP server.
type daemonCoreConfig struct {
	Reg            *pkgregistry.Registry
	Workers        int
	PIBin          string
	SessionDirRoot string
	Grace          time.Duration
	IdleTimeout    time.Duration
	PollInterval   time.Duration
}

// daemonCore is the in-process daemon: one scheduler + one project
// manager + one HTTP server, plus the daemon-wide attach hubs that
// the lazy TUI's Live tab depends on.
type daemonCore struct {
	Mgr         *projectmgr.Manager
	Sched       *scheduler.Scheduler
	Srv         *server.Server
	Runners     *pirunners.Registry
	Attachments *pirunners.Attachments
}

// buildDaemonCore is the production wiring for `autosk daemon serve`.
//
// The two pirunners hubs are constructed here and THREADED INTO BOTH
// projectmgr.Deps (so per-project executors can register their live
// pi runners and the executor's per-turn 'attached?' check can fire)
// AND server.Deps (so decorateRun returns the live Streaming /
// AttachCount on JobResponse, and /input + /abort can resolve the
// runner without 503-ing). The previous wiring missed both halves;
// without them the lazy TUI's Jobs panel rendered Streaming=false /
// AttachCount=0 forever and the Live tab's Ctrl-D / Ctrl-F / Ctrl-A
// all returned 503 "attach disabled: no runner registry".
func buildDaemonCore(cfg daemonCoreConfig) daemonCore {
	runners := pirunners.NewRegistry()
	attachments := pirunners.NewAttachments()

	var mgr *projectmgr.Manager
	sched := scheduler.New(scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
		proj, ok := mgr.Get(projectmgr.Key(job.Project))
		if !ok {
			return fmt.Errorf("project not loaded: %s", job.Project)
		}
		return proj.Executor.Run(ctx, job.ID)
	}), scheduler.Config{Workers: cfg.Workers})

	mgr = projectmgr.New(projectmgr.Deps{
		Sched:        sched,
		Packages:     cfg.Reg,
		PollInterval: cfg.PollInterval,
		Runners:      runners,
		Attachments:  attachments,
		ExecCfg: executor.Config{
			PIBin:          cfg.PIBin,
			SessionDirRoot: cfg.SessionDirRoot,
			Grace:          cfg.Grace,
			IdleTimeout:    cfg.IdleTimeout,
		},
	})

	srv := server.New(server.Deps{
		Projects:    mgr,
		Sched:       sched,
		Workers:     cfg.Workers,
		Runners:     runners,
		Attachments: attachments,
	})

	return daemonCore{
		Mgr:         mgr,
		Sched:       sched,
		Srv:         srv,
		Runners:     runners,
		Attachments: attachments,
	}
}
