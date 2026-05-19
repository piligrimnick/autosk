package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/jesseduffield/gocui"

	"autosk/internal/agent"
	"autosk/internal/daemon/runstore"
	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/tui"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// fakeDS is a tiny in-memory Datasource the smoke tests drive
// directly so we don't need a daemon. It wraps an Offline for reads
// the TUI does against the project DB.
type fakeDS struct {
	*datasource.Offline
	streamed atomic.Bool
}

func (f *fakeDS) Healthz(_ context.Context) (datasource.Health, error) {
	return datasource.Health{Daemon: "down"}, nil
}

// TestLazy_SmokeDashboardLaunch verifies the TUI starts, the layout
// fires, and the seeded task appears in the Tasks panel.
//
// We construct a doltlite-backed tmpdb, seed one task + one job, run
// the TUI in a goroutine via the headless simulation screen, give it
// a moment to lay out, then inject 'q' to quit and assert the buffer
// of the Tasks view contains the task id.
func TestLazy_SmokeDashboardLaunch(t *testing.T) {
	if raceEnabled {
		t.Skip("skipping under -race: pre-existing race in test fixture's screen reads (see followup)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()

	dl := doltlite.New()
	if err := dl.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dl.Close()
	if err := dl.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	task, err := dl.CreateTask(ctx, store.Task{Title: "Refactor auth", Status: store.StatusNew, Priority: 1})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// Seed a synthetic workflow + step so we can attach a daemon_run.
	ag := agent.New(dl.DB())
	humanAgent, _ := ag.GetByName(ctx, "human")
	_ = humanAgent

	ds, err := datasource.NewOffline(dl, dir, nil)
	if err != nil {
		t.Fatalf("offline: %v", err)
	}
	fake := &fakeDS{Offline: ds}
	tui.SetDebugLogger(t.Logf)
	defer tui.SetDebugLogger(nil)

	// Run the TUI in a goroutine; force headless so it uses a
	// simulation screen we can drive from the test.
	done := make(chan error, 1)
	go func() {
		done <- tui.Run(ctx, tui.Options{
			Datasource:  fake,
			ProjectRoot: dir,
			Refresh:     50 * time.Millisecond,
			Headless:    true,
			Width:       120,
			Height:      40,
		})
	}()

	// Give the gocui main loop time to lay out and refresh. We poll
	// for up to 3s; each pass nudges the event loop by injecting an
	// innocuous resize so flush() fires and the simulation screen
	// gets painted.
	// Sanity: confirm the offline datasource can read tasks with a
	// fresh context (i.e. the cancellation we see in refreshAll is not
	// because the underlying store query is broken).
	tasks, err := ds.Tasks(context.Background(), datasource.DefaultTaskFilter())
	if err != nil {
		t.Fatalf("ds.Tasks (sanity): %v", err)
	}
	if len(tasks) == 0 {
		t.Fatalf("ds.Tasks (sanity): no tasks returned — seed did not stick?")
	}
	t.Logf("sanity Tasks: got %d (first=%s/%s)", len(tasks), tasks[0].ID, tasks[0].Title)

	if !waitFor(3*time.Second, func() bool {
		injectResize(120, 40)
		return findInScreen(task.ID)
	}) {
		// Dump screen for debugging.
		t.Logf("---- screen dump ----\n%s\n---- end ----", dumpScreen())
		injectKey('q')
		<-done
		t.Fatalf("Tasks panel never rendered task id %q", task.ID)
	}

	injectKey('q')
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned err: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatalf("TUI did not quit on q")
	}
}

// TestLazy_DeepLinkJob verifies --job opens the inspector directly.
func TestLazy_DeepLinkJob(t *testing.T) {
	if raceEnabled {
		t.Skip("skipping under -race: pre-existing race in test fixture's screen reads (see followup)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()

	dl := doltlite.New()
	if err := dl.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dl.Close()
	if err := dl.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	task, _ := dl.CreateTask(ctx, store.Task{Title: "Bump version", Status: store.StatusNew, Priority: 2})

	// Seed a daemon_run directly via SQL — the runstore needs a step
	// row but for this smoke test we just want a job id the TUI can
	// open.
	_, err := dl.DB().ExecContext(ctx, `INSERT INTO agents(id, name, is_human, created_at) VALUES ('ag-fake','x',0,?)`, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	_, err = dl.DB().ExecContext(ctx, `INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at) VALUES ('wf-fake','single:x','',?,1,?)`, "st-fake", time.Now().Unix())
	if err != nil {
		t.Fatalf("seed wf: %v", err)
	}
	_, err = dl.DB().ExecContext(ctx, `INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES ('st-fake','wf-fake','x','ag-fake',0)`)
	if err != nil {
		t.Fatalf("seed step: %v", err)
	}
	rs := runstore.New(dl.DB())
	r, err := rs.CreateRun(ctx, runstore.NewRun{TaskID: task.ID, StepID: "st-fake"})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// Mark it done so the default tab is Archive (no Live SSE needed).
	exit := 0
	if _, err := rs.MarkDone(ctx, r.JobID, exit, nil); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	ds, err := datasource.NewOffline(dl, dir, nil)
	if err != nil {
		t.Fatalf("offline: %v", err)
	}
	tui.SetDebugLogger(t.Logf)
	defer tui.SetDebugLogger(nil)

	done := make(chan error, 1)
	go func() {
		done <- tui.Run(ctx, tui.Options{
			Datasource:  &fakeDS{Offline: ds},
			ProjectRoot: dir,
			Refresh:     50 * time.Millisecond,
			Headless:    true,
			Width:       120,
			Height:      40,
			InitialJob:  r.JobID,
		})
	}()

	if !waitFor(3*time.Second, func() bool {
		injectResize(120, 40)
		return findInScreen(r.JobID) && findInScreen("Archive")
	}) {
		t.Logf("---- screen dump ----\n%s\n---- end ----", dumpScreen())
		injectKey('q')
		<-done
		t.Fatalf("inspector did not open on job %q (tab label Archive missing)", r.JobID)
	}

	injectKey('q')
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
	}
}

// waitFor polls f until it returns true or d elapses.
func waitFor(d time.Duration, f func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f()
}

// dumpScreen returns the full simulation-screen content as a string.
func dumpScreen() string {
	scr := gocui.Screen
	if scr == nil {
		return "(no screen)"
	}
	sim, ok := scr.(tcell.SimulationScreen)
	if !ok {
		return "(not simulation)"
	}
	cells, w, h := sim.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				b.WriteRune(' ')
			} else {
				b.WriteRune(c.Runes[0])
			}
		}
		b.WriteRune('\n')
	}
	return b.String()
}

// findInScreen scans the tcell simulation screen for the given
// substring across all cell rows.
func findInScreen(needle string) bool {
	scr := gocui.Screen
	if scr == nil {
		return false
	}
	sim, ok := scr.(tcell.SimulationScreen)
	if !ok {
		return false
	}
	cells, w, h := sim.GetContents()
	if len(cells) == 0 {
		return false
	}
	for y := 0; y < h; y++ {
		var row []rune
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				row = append(row, ' ')
			} else {
				row = append(row, c.Runes[0])
			}
		}
		if strings.Contains(string(row), needle) {
			return true
		}
	}
	return false
}

// injectKey pushes a rune-keypress into the gocui screen.
func injectKey(r rune) {
	scr := gocui.Screen
	if scr == nil {
		return
	}
	sim, ok := scr.(tcell.SimulationScreen)
	if !ok {
		return
	}
	sim.InjectKey(tcell.KeyRune, r, tcell.ModNone)
}

// injectResize nudges the event loop by emitting a resize event.
// gocui's MainLoop reads the next event and immediately runs flush(),
// which is the only point at which the simulation screen actually
// receives the layout's writes.
func injectResize(w, h int) {
	scr := gocui.Screen
	if scr == nil {
		return
	}
	sim, ok := scr.(tcell.SimulationScreen)
	if !ok {
		return
	}
	sim.SetSize(w, h)
}

