package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/tui"
	"autosk/internal/store"
)

// fakeDS is a self-contained in-memory Datasource the smoke test drives
// directly so we don't need a daemon (or a doltlite store). It serves a fixed
// task set for reads and treats every other verb as an inert success — enough
// to exercise tui.Run's layout + Tasks-panel render path end-to-end.
type fakeDS struct {
	tasks []datasource.Task
}

func (f *fakeDS) Tasks(context.Context, datasource.TaskFilter) ([]datasource.Task, error) {
	return f.tasks, nil
}
func (f *fakeDS) GetTask(_ context.Context, id string) (datasource.Task, error) {
	for _, t := range f.tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return datasource.Task{}, nil
}
func (f *fakeDS) Sessions(context.Context, string) ([]datasource.Session, error) {
	return nil, nil
}
func (f *fakeDS) GetSession(context.Context, string) (datasource.Session, error) {
	return datasource.Session{}, nil
}
func (f *fakeDS) Workflows(context.Context) ([]datasource.Workflow, error) { return nil, nil }
func (f *fakeDS) Agents(context.Context) ([]datasource.Agent, error)       { return nil, nil }
func (f *fakeDS) Comments(context.Context, string) ([]datasource.Comment, error) {
	return nil, nil
}

func (f *fakeDS) Healthz(context.Context) (datasource.Health, error) {
	return datasource.Health{Daemon: "down"}, nil
}
func (f *fakeDS) CreateTask(context.Context, string, string) (string, error) { return "", nil }
func (f *fakeDS) TaskDone(context.Context, string) error                     { return nil }
func (f *fakeDS) TaskCancel(context.Context, string) error                   { return nil }
func (f *fakeDS) TaskReopen(context.Context, string) error                   { return nil }
func (f *fakeDS) UpdateTask(context.Context, string, *string, *string) error { return nil }
func (f *fakeDS) EnrollWorkflow(context.Context, string, string) error       { return nil }
func (f *fakeDS) EnrollAgent(context.Context, string, string) error          { return nil }
func (f *fakeDS) Resume(context.Context, string, string) error               { return nil }
func (f *fakeDS) Block(context.Context, string, string) error                { return nil }
func (f *fakeDS) Unblock(context.Context, string, string) error              { return nil }
func (f *fakeDS) AddComment(context.Context, string, string) error           { return nil }
func (f *fakeDS) AbortSession(context.Context, string) error                 { return nil }
func (f *fakeDS) SessionInput(context.Context, string, string, string) error { return nil }
func (f *fakeDS) Reconnect(context.Context) error                            { return nil }
func (f *fakeDS) SessionTranscript(context.Context, string) ([]datasource.LiveEvent, error) {
	return nil, nil
}
func (f *fakeDS) StreamSession(context.Context, string) (*datasource.LiveHandle, error) {
	return nil, datasource.ErrDaemonRequired
}

var _ datasource.Datasource = (*fakeDS)(nil)

// TestLazy_SmokeDashboardLaunch verifies the TUI starts, the layout
// fires, and the seeded task appears in the Tasks panel.
//
// We drive an in-memory fakeDS (no doltlite store at all) seeded with a
// single task and zero jobs; `dir` is used only as the ProjectRoot. We run
// the TUI in a goroutine via the headless simulation screen, give it a
// moment to lay out, then inject 'q' to quit and assert the buffer of the
// Tasks view contains the task id.
func TestLazy_SmokeDashboardLaunch(t *testing.T) {
	if raceEnabled {
		t.Skip("skipping under -race: pre-existing race in test fixture's screen reads (see followup)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()

	task := datasource.Task{
		ID:     "ask-a1b2c3",
		Title:  "Refactor auth",
		Status: store.StatusNew,
	}
	fake := &fakeDS{tasks: []datasource.Task{task}}
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
