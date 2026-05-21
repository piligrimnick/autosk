package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/client"
	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/tui"
	"autosk/internal/store/doltlite"
)

// TestLazy_DaemonUp_RendersStreamingGlyph is the acceptance-checklist
// §9 item 1 regression: with the daemon up, the Jobs panel must
// render the live Streaming / AttachCount values from /v1/jobs (not
// the static DB values, which are always Streaming=false /
// AttachCount=0). The previous review's BLOCKER 1 was the
// pirunners-not-wired bug; this is the e2e that would have caught
// it.
//
// The test wires a fake daemon over UDS that returns one job with
// Streaming=true / AttachCount=3, points a Compose datasource at it,
// runs the TUI in headless mode, and asserts the rendered Jobs
// panel contains the "*live*" string + "(3)" attach count.
func TestLazy_DaemonUp_RendersStreamingGlyph(t *testing.T) {
	if raceEnabled {
		// The test fixture (findInScreen/injectResize/dumpScreen)
		// reads the package-level gocui.Screen variable while
		// gocui's MainLoop writes to it. Refactoring those helpers
		// to drive reads through g.Update closures is its own
		// task; until then we skip under -race so this test
		// doesn't broadcast the fixture issue across CI.  The
		// internal/lazy/... code under test is itself race-clean.
		t.Skip("skipping under -race: pre-existing race in test fixture's screen reads (see followup)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()

	// --- fake daemon over UDS ---
	sock := filepath.Join(shortLazyTmp(t), "lazy-up.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.HealthResponse{OK: true, Workers: 2})
	})
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ListResponse{
			Jobs: []api.JobResponse{
				{
					JobID: "job-live-1", TaskID: "ask-7a5601", Status: "running",
					Streaming: true, AttachCount: 3,
				},
			},
		})
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		_ = json.NewEncoder(w).Encode(api.JobResponse{
			JobID: id, TaskID: "ask-7a5601", Status: "running",
			Streaming: true, AttachCount: 3,
		})
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	defer srv.Close()

	// --- offline DB (no tasks, no jobs — the live source is what
	// populates the Jobs panel here) ---
	dl := doltlite.New()
	if err := dl.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dl.Close()
	if err := dl.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	off, err := datasource.NewOffline(dl, dir, nil)
	if err != nil {
		t.Fatalf("offline: %v", err)
	}
	cli, err := client.New(client.Options{Sock: sock})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ds := datasource.NewCompose(off, cli, 30*time.Millisecond)
	defer ds.Close()

	// Sanity: Compose's initial probe should have marked daemon=ok.
	if h := ds.Health(); h.Daemon != "ok" {
		t.Fatalf("daemon health=%q want ok before TUI start", h.Daemon)
	}

	tui.SetDebugLogger(t.Logf)
	defer tui.SetDebugLogger(nil)

	done := make(chan error, 1)
	go func() {
		done <- tui.Run(ctx, tui.Options{
			Datasource:  ds,
			ProjectRoot: dir,
			Refresh:     50 * time.Millisecond,
			Headless:    true,
			Width:       240, // wide enough that the Jobs panel inner area
			Height:      40,  // fits the full row (id + status + wf + age + (N) + *live*)
		})
	}()

	// Poll the screen until we see the live glyph + attach count.
	// 5s budget — initial probe + first refresh + render.
	ok := waitFor(5*time.Second, func() bool {
		injectResize(240, 40)
		return findInScreen("job-live") && findInScreen("(3)") && findInScreen("*live*")
	})
	if !ok {
		t.Logf("---- screen dump ----\n%s\n---- end ----", dumpScreen())
		injectKey('q')
		<-done
		t.Fatalf("Jobs panel did not render Streaming/AttachCount from live daemon")
	}

	injectKey('q')
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run err: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
	}
}

// shortLazyTmp mirrors shortTmp from internal/lazy/datasource — UDS
// socket paths are bounded at 104 chars on macOS so t.TempDir() is
// too long when run from a deep $TMPDIR.
func shortLazyTmp(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if len(d) < 80 {
		return d
	}
	return "/tmp"
}
