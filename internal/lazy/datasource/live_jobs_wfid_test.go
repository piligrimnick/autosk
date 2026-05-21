package datasource_test

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
	"autosk/internal/store/doltlite"
)

// TestLive_JobsHonoursWorkflowID is the regression test for the
// review remark "Live.Jobs(): scope.WorkflowID has no effect on the
// Jobs panel when the daemon is up". Previously Live.Jobs only
// skipped rows whose WorkflowName decoded to empty — it never
// compared the decorated WorkflowID against f.WorkflowID, so
// highlighting one workflow in the Workflows panel didn't narrow
// the Jobs panel when the daemon was live.
//
// Setup: two step rows on two different workflows. The fake daemon
// returns two job rows pointing at them. Live.Jobs(WorkflowID=A)
// must return only the row whose step belongs to workflow A.
func TestLive_JobsHonoursWorkflowID(t *testing.T) {
	// --- fake daemon: returns both jobs unfiltered ---
	sock := filepath.Join(shortTmp(t), "wf.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.HealthResponse{OK: true, Workers: 1})
	})
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ListResponse{
			Jobs: []api.JobResponse{
				{JobID: "job-A", TaskID: "ask-aaaaaa", StepID: "st-A", Status: "running"},
				{JobID: "job-B", TaskID: "ask-bbbbbb", StepID: "st-B", Status: "running"},
			},
		})
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { _ = srv.Close() })

	// --- offline base: seed two workflows, two steps, two agents.
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(context.Background(), filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = ts.Close() })
	if err := ts.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db := ts.DB()
	ctx := context.Background()
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO agents(id, name, is_human, created_at) VALUES ('ag-A','agA',0,?), ('ag-B','agB',0,?)`, now, now); err != nil {
		t.Fatalf("agents: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at) VALUES ('wf-A','wA','','st-A',0,?), ('wf-B','wB','','st-B',0,?)`, now, now); err != nil {
		t.Fatalf("workflows: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES ('st-A','wf-A','do','ag-A',0), ('st-B','wf-B','do','ag-B',0)`); err != nil {
		t.Fatalf("steps: %v", err)
	}

	off, err := datasource.NewOffline(ts, dir, nil)
	if err != nil {
		t.Fatalf("offline: %v", err)
	}
	cli, err := client.New(client.Options{Sock: sock})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	live := datasource.NewLive(off, cli)

	// Unfiltered: both rows come back.
	all, err := live.Jobs(ctx, datasource.JobFilter{})
	if err != nil {
		t.Fatalf("Jobs(empty): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered Jobs returned %d, want 2", len(all))
	}

	// Filter by workflow A: only job-A survives.
	a, err := live.Jobs(ctx, datasource.JobFilter{WorkflowID: "wf-A"})
	if err != nil {
		t.Fatalf("Jobs(wf-A): %v", err)
	}
	if len(a) != 1 || a[0].JobID != "job-A" {
		t.Fatalf("Jobs(wf-A) returned %+v, want [job-A]", a)
	}

	// Filter by workflow B: only job-B survives.
	b, err := live.Jobs(ctx, datasource.JobFilter{WorkflowID: "wf-B"})
	if err != nil {
		t.Fatalf("Jobs(wf-B): %v", err)
	}
	if len(b) != 1 || b[0].JobID != "job-B" {
		t.Fatalf("Jobs(wf-B) returned %+v, want [job-B]", b)
	}

	// Filter by a workflow that has no jobs: empty result, no error.
	none, err := live.Jobs(ctx, datasource.JobFilter{WorkflowID: "wf-missing"})
	if err != nil {
		t.Fatalf("Jobs(wf-missing): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("Jobs(wf-missing) returned %d, want 0", len(none))
	}
}
